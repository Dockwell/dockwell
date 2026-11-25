// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
	"github.com/dockwell/dockwell/internal/website"
)

type stubBackupAgent struct{}

func (stubBackupAgent) Plan(_ context.Context, desired []resource.Desired) ([]resource.Step, error) {
	out := make([]resource.Step, len(desired))
	for i := range desired {
		act := resource.ActionCreate
		if desired[i].Spec == nil {
			act = resource.ActionDelete
		}
		out[i] = resource.Step{Ref: desired[i].Ref, Decision: resource.Decision{Action: act}}
	}
	return out, nil
}

func (stubBackupAgent) Apply(context.Context, resource.Step, proto.Message) error { return nil }

func (stubBackupAgent) Observe(context.Context, []resource.Ref) ([]proto.Message, error) {
	return nil, nil
}

type stubBackupAgents struct{}

func (stubBackupAgents) Agent(string) (resource.Agent, bool) { return stubBackupAgent{}, true }

// TestWebsiteFolderRestoreGuardsM5 verifies M5:
//   - restoring a website backup after the website moved to /srv/a2 is refused with the fact,
//   - restoring when another project mounts an overlapping path is refused,
//   - restoring after the website is deleted is refused.
func TestWebsiteFolderRestoreGuardsM5(t *testing.T) {
	t.Parallel()
	db, _ := storetest.Open(t, store.SQLite)
	logger := slog.New(slog.DiscardHandler)
	if _, err := revision.Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.CreateServer(t.Context(), store.Server{ID: "srv_1", Name: "srv-1", CreatedAt: now, CertSerial: "s1", CertNotAfter: now})
	}); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(&service.Driver{}, &route.Driver{}, &network.Driver{})
	keeper := secretstest.Keeper(db, kinds)
	eng := revision.New(db, kinds, stubBackupAgents{}, keeper, logger)
	ch := changes.New(eng, nil, db, logger)
	h := &Handler{changes: ch, engine: eng, db: db, keeper: keeper, logger: logger}

	var orgID string
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		tr, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		orgID = tr.Organization.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	makeSite := func(name, hostPath string) (projID, envID, webSvcID string) {
		t.Helper()
		var made []revision.Edit
		var env string
		if err := db.View(t.Context(), func(tx *store.Tx) error {
			var err error
			made, env, err = eng.NewProject(t.Context(), tx, orgID, name, "")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		made[0].Spec = &resourcev1.Project{DisplayName: name}
		edits := append(slices.Clone(made), revision.Edit{
			Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: webServiceName},
			Spec: &resourcev1.Service{
				Runs: &resourcev1.Service_App{App: &resourcev1.Container{
					Image:  "caddy:2",
					Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: hostPath}, Target: website.FolderTarget}},
				}},
			},
			Servers: []string{"srv_1"},
		})
		p, err := eng.Propose(t.Context(), revision.Proposal{Summary: "create " + name, Actor: "user:usr_1", Edits: edits})
		if err != nil {
			t.Fatal(err)
		}
		if err := eng.Apply(t.Context(), p.Change); err != nil {
			t.Fatal(err)
		}
		var webRes store.Resource
		if err := db.View(t.Context(), func(tx *store.Tx) error {
			var err error
			webRes, err = tx.GetResource(t.Context(), string(service.Kind), env, webServiceName)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return made[0].ID, env, webRes.ID
	}

	projA, envA, webSvcA := makeSite("site-a", "/srv/a")
	bkp := store.Backup{
		ID:            "bkp_site_a",
		EnvironmentID: envA,
		Volume:        webServiceName,
		VolumeID:      webSvcA,
		ServerID:      "srv_1",
		State:         store.BackupDone,
		Actor:         "user:usr_1",
		StartedAt:     now,
		FinishedAt:    &now,
		WebsiteID:     projA,
		WebsitePath:   "/srv/a",
	}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.CreateBackup(t.Context(), bkp); err != nil {
			return err
		}
		return tx.FinishBackup(t.Context(), bkp)
	}); err != nil {
		t.Fatal(err)
	}

	ctx := authapi.As(t.Context(), auth.Client{}, auth.Principal{User: store.User{ID: "usr_1", Email: "a@example.com"}})

	// 1. While site-a still serves /srv/a with no overlap, verifyWebsiteRestoreTarget accepts /srv/a.
	gotPath, err := h.verifyWebsiteRestoreTarget(ctx, &bkp)
	if err != nil || gotPath != "/srv/a" {
		t.Fatalf("verifyWebsiteRestoreTarget(valid) = %q, %v", gotPath, err)
	}

	// 2. Another project B mounts /srv/a (or a subfolder /srv/a/sub): restore MUST be refused.
	projB, envB, _ := makeSite("site-b", "/srv/a/sub")
	_, err = h.RestoreBackup(ctx, connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: bkp.ID}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("expected overlap refusal, got %v", err)
	}

	// Remove site-b so we can test site-a moving to /srv/a2 cleanly.
	delB, err := eng.Propose(t.Context(), revision.Proposal{
		Summary: "delete site-b",
		Actor:   "user:usr_1",
		Edits: []revision.Edit{
			{Ref: resource.Ref{Kind: service.Kind, Scope: envB, Name: webServiceName}},
			{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: projB, Name: "production"}},
			{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: orgID, Name: "site-b"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Apply(t.Context(), delB.Change); err != nil {
		t.Fatal(err)
	}

	// 3. Move site-a to /srv/a2: restoring the /srv/a backup MUST be refused naming both paths.
	moveA, err := eng.Propose(t.Context(), revision.Proposal{
		Summary: "move site-a to /srv/a2",
		Actor:   "user:usr_1",
		Edits: []revision.Edit{{
			Ref: resource.Ref{Kind: service.Kind, Scope: envA, Name: webServiceName},
			Spec: &resourcev1.Service{
				Runs: &resourcev1.Service_App{App: &resourcev1.Container{
					Image:  "caddy:2",
					Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/a2"}, Target: website.FolderTarget}},
				}},
			},
			Servers: []string{"srv_1"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Apply(t.Context(), moveA.Change); err != nil {
		t.Fatal(err)
	}
	_, err = h.RestoreBackup(ctx, connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: bkp.ID}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "/srv/a") || !strings.Contains(err.Error(), "/srv/a2") {
		t.Fatalf("expected folder-changed refusal naming /srv/a and /srv/a2, got %v", err)
	}

	// 4. Delete site-a completely: restoring MUST be refused with website_gone.
	delA, err := eng.Propose(t.Context(), revision.Proposal{
		Summary: "delete site-a",
		Actor:   "user:usr_1",
		Edits: []revision.Edit{
			{Ref: resource.Ref{Kind: service.Kind, Scope: envA, Name: webServiceName}},
			{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: projA, Name: "production"}},
			{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: orgID, Name: "site-a"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Apply(t.Context(), delA.Change); err != nil {
		t.Fatal(err)
	}
	_, err = h.RestoreBackup(ctx, connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: bkp.ID}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("expected website-gone refusal, got %v", err)
	}
}
