// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// fakeKindDriver wraps a Type so the fake agent accepts create/delete steps in
// revision engine tests without talking to Docker.
type fakeKindDriver struct {
	resource.Type
	state map[resource.Ref]proto.Message
}

func (d fakeKindDriver) Observe(_ context.Context, ref resource.Ref) (proto.Message, error) {
	return d.state[ref], nil
}

func (d fakeKindDriver) Plan(cur, want proto.Message, _ []resource.Change) (resource.Decision, error) {
	switch {
	case cur == nil && want != nil:
		return resource.Decision{Action: resource.ActionCreate}, nil
	case cur != nil && want == nil:
		return resource.Decision{Action: resource.ActionDelete, Destroys: true}, nil
	default:
		return resource.Decision{Action: resource.ActionNone}, nil
	}
}

func (d fakeKindDriver) Judge(_ context.Context, _ resource.Ref, cur, want proto.Message, ch []resource.Change) (resource.Decision, error) {
	return d.Plan(cur, want, ch)
}

func (d fakeKindDriver) Apply(_ context.Context, step resource.Step, desired proto.Message) error {
	if desired == nil {
		delete(d.state, step.Ref)
	} else {
		d.state[step.Ref] = desired
	}
	return nil
}

func (d fakeKindDriver) Verify(context.Context, resource.Ref, proto.Message) error { return nil }

// H3: Delete is refused while any app uses the database by any path:
// (1) link, (2) catalog-made extra database.user whose secret an app uses
// directly or via a derived secret template, (3) CreateDatabaseUser user whose
// secret an app uses, and (4) a secret whose connection URL/DSN names the database.
func TestDeleteDatabaseRefusedAcrossAllAppUsagePaths(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testDeleteDatabaseRefusedAcrossAllAppUsagePaths)
}

func testDeleteDatabaseRefusedAcrossAllAppUsagePaths(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	userDrv := fakeKindDriver{Type: &database.UserDriver{}, state: map[resource.Ref]proto.Message{}}
	logicalDrv := fakeKindDriver{Type: &database.LogicalDriver{}, state: map[resource.Ref]proto.Message{}}
	e.kinds = e.kinds.With(link.Type{}, userDrv, logicalDrv)
	e.sealer = secretstest.Keeper(db, e.kinds)
	loc := f["srv_A"].agent.(resource.Local)
	loc.Drivers = loc.Drivers.With(userDrv, logicalDrv)
	f["srv_A"].agent = loc
	env := production(t, db)

	// Set up a PostgreSQL database service plus four databases representing each path.
	pgSvc := Edit{
		Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: "pg"},
		Spec: &resourcev1.Service{
			Runs: &resourcev1.Service_App{App: &resourcev1.Container{
				Image: "postgres:17",
				Env:   map[string]string{"POSTGRES_USER": "postgres"},
			}},
			Secrets: map[string]string{"POSTGRES_PASSWORD": "PG_ADMIN_PASSWORD"},
		},
		Servers: []string{"srv_A"},
	}

	apply(t, e, propose(t, e, "Set up database and four app usage paths",
		secretEdit(env, "PG_ADMIN_PASSWORD", "admin-pw"),
		pgSvc,

		// Path 1: logical database used via link.
		Edit{
			Ref:     resource.Ref{Kind: database.LogicalKind, Scope: env, Name: "pg-linked_db"},
			Spec:    &resourcev1.LogicalDatabase{Service: "pg", DatabaseName: "linked_db", Owner: "postgres"},
			Servers: []string{"srv_A"},
		},
		Edit{
			Ref: resource.Ref{Kind: linkKind, Scope: env, Name: "web-linked"},
			Spec: &resourcev1.Link{
				Label:        "Database",
				Service:      "pg",
				Engine:       resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL,
				Version:      "17",
				Accepts:      []*resourcev1.LinkAccept{{Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL}},
				Sets:         []*resourcev1.LinkSet{{Name: "DB_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}},
				DatabaseName: "linked_db",
			},
		},

		// Path 2: catalog-made extra database.user whose secret is used directly by catalog-web.
		secretEdit(env, "CATALOG_EXTRA_PASSWORD", "extra-pw"),
		Edit{
			Ref:     resource.Ref{Kind: database.UserKind, Scope: env, Name: "catalog_app"},
			Spec:    &resourcev1.DatabaseUser{Service: "pg", Password: "CATALOG_EXTRA_PASSWORD", DatabaseName: "catalog_db"},
			Servers: []string{"srv_A"},
		},
		user(env, "catalog-web", "srv_A", map[string]string{"DB_PASS": "CATALOG_EXTRA_PASSWORD"}, nil),

		// Path 3: CreateDatabaseUser user whose secret is referenced through a derived URL template secret.
		secretEdit(env, "CREATED_USER_PASSWORD", "created-pw"),
		Edit{
			Ref:     resource.Ref{Kind: database.UserKind, Scope: env, Name: "created_user"},
			Spec:    &resourcev1.DatabaseUser{Service: "pg", Password: "CREATED_USER_PASSWORD"},
			Servers: []string{"srv_A"},
		},
		Edit{
			Ref: resource.Ref{Kind: variable.SecretKind, Scope: env, Name: "CREATED_DSN"},
			Spec: &resourcev1.Secret{
				From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Template{Template: &resourcev1.ValueTemplate{
					Text:  "postgres://created_user:${CREATED_USER_PASSWORD}@pg:5432/created_user",
					Names: []*resourcev1.TemplateName{{Name: "CREATED_USER_PASSWORD", Secret: true}},
				}}},
			},
		},
		user(env, "created-api", "srv_A", map[string]string{"DATABASE_URL": "CREATED_DSN"}, nil),

		// Path 4: logical database named only inside an app's connection-string secret (no link, no shared user secret).
		Edit{
			Ref:     resource.Ref{Kind: database.LogicalKind, Scope: env, Name: "pg-secret_db"},
			Spec:    &resourcev1.LogicalDatabase{Service: "pg", DatabaseName: "secret_db", Owner: "postgres"},
			Servers: []string{"srv_A"},
		},
		secretEdit(env, "CUSTOM_DB_URL", "postgres://reader:pw@pg:5432/secret_db?sslmode=disable"),
		user(env, "worker-app", "srv_A", map[string]string{"DATABASE_URL": "CUSTOM_DB_URL"}, nil),
	))

	// Path 1 refused.
	if _, err := e.Propose(t.Context(), Proposal{Summary: "Delete linked_db", Edits: []Edit{{Ref: resource.Ref{Kind: database.LogicalKind, Scope: env, Name: "pg-linked_db"}}}}); err == nil || !strings.Contains(err.Error(), "web-linked") {
		t.Fatalf("Path 1 (link): got %v, want refusal naming web-linked", err)
	}

	// Path 2 refused (catalog extra user's password secret used by catalog-web).
	if _, err := e.Propose(t.Context(), Proposal{Summary: "Delete catalog_app", Edits: []Edit{{Ref: resource.Ref{Kind: database.UserKind, Scope: env, Name: "catalog_app"}}}}); err == nil || !strings.Contains(err.Error(), "catalog-web") {
		t.Fatalf("Path 2 (catalog extra user): got %v, want refusal naming catalog-web", err)
	}

	// Path 3 refused (CreateDatabaseUser secret used transitively through derived secret template by created-api).
	if _, err := e.Propose(t.Context(), Proposal{Summary: "Delete created_user", Edits: []Edit{{Ref: resource.Ref{Kind: database.UserKind, Scope: env, Name: "created_user"}}}}); err == nil || !strings.Contains(err.Error(), "created-api") {
		t.Fatalf("Path 3 (CreateDatabaseUser transitive secret): got %v, want refusal naming created-api", err)
	}

	// Path 4 refused (secret naming database in connection URL used by worker-app).
	if _, err := e.Propose(t.Context(), Proposal{Summary: "Delete secret_db", Edits: []Edit{{Ref: resource.Ref{Kind: database.LogicalKind, Scope: env, Name: "pg-secret_db"}}}}); err == nil || !strings.Contains(err.Error(), "worker-app") || !strings.Contains(err.Error(), "CUSTOM_DB_URL") {
		t.Fatalf("Path 4 (secret naming DB): got %v, want refusal naming worker-app and CUSTOM_DB_URL", err)
	}

	// Once worker-app stops using CUSTOM_DB_URL in the same change, deleting pg-secret_db succeeds.
	if _, err := e.Propose(t.Context(), Proposal{Summary: "Disconnect worker-app and delete secret_db", Edits: []Edit{
		user(env, "worker-app", "srv_A", nil, nil),
		{Ref: resource.Ref{Kind: database.LogicalKind, Scope: env, Name: "pg-secret_db"}},
	}}); err != nil {
		t.Fatalf("deleting pg-secret_db after detaching worker-app failed: %v", err)
	}
}
