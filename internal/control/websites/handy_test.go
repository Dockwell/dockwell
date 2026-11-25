// SPDX-License-Identifier: AGPL-3.0-only

package websites

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
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

type fakeSiteAgent struct{}

func (fakeSiteAgent) Plan(_ context.Context, desired []resource.Desired) ([]resource.Step, error) {
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

func (fakeSiteAgent) Apply(context.Context, resource.Step, proto.Message) error { return nil }

func (fakeSiteAgent) Observe(context.Context, []resource.Ref) ([]proto.Message, error) {
	return nil, nil
}

type fakeSiteAgents struct{}

func (fakeSiteAgents) Agent(string) (resource.Agent, bool) { return fakeSiteAgent{}, true }

func setupWebsitesHandler(t *testing.T) (*Handler, *changes.Handler, *revision.Engine, *store.DB) {
	t.Helper()
	db, _ := storetest.Open(t, store.SQLite)
	logger := slog.New(slog.DiscardHandler)
	if _, err := revision.Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		for _, id := range []string{"srv_1", "srv_2"} {
			if err := tx.CreateServer(t.Context(), store.Server{ID: id, Name: id, CreatedAt: now, CertSerial: id, CertNotAfter: now}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(&service.Driver{}, &route.Driver{}, &network.Driver{}, &variable.Type{}, &variable.SecretType{})
	keeper := secretstest.Keeper(db, kinds)
	eng := revision.New(db, kinds, fakeSiteAgents{}, keeper, logger)
	ch := changes.New(eng, nil, db, logger)
	h := New(ch, eng, db, keeper, nil, logger)
	return h, ch, eng, db
}

func callerCtx(t *testing.T, grants ...store.Grant) context.Context {
	t.Helper()
	ctx := authapi.As(t.Context(), auth.Client{}, auth.Principal{User: store.User{ID: "usr_1", Email: "u@example.com"}})
	return place.WithCaller(ctx, place.Caller{Grants: grants})
}

func TestVisitorNotesDescribeEverydayChanges(t *testing.T) {
	t.Parallel()
	prev := &resourcev1.Website{
		Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/site"}},
	}
	pausedNext := &resourcev1.Website{
		Serves:    &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/site"}},
		Paused:    true,
		PauseMode: resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_MAINTENANCE,
	}
	var pauseMsgs []string
	for _, n := range visitorNotes("env_1", prev, pausedNext) {
		pauseMsgs = append(pauseMsgs, n.Message)
	}
	if !strings.Contains(strings.Join(pauseMsgs, "\n"), "maintenance page") {
		t.Fatalf("expected pause note, got %v", pauseMsgs)
	}

	rulesNext := &resourcev1.Website{
		Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/site"}},
		Rules: &resourcev1.WebsiteRules{
			BasicAuth: &resourcev1.WebsiteBasicAuth{
				Users: []*resourcev1.WebsiteBasicAuthUser{{Username: "alice", Secret: "WEBSITE_AUTH_ALICE"}},
			},
			Cors:           &resourcev1.WebsiteCors{Enabled: true, AllowedOrigins: []string{"https://app.example.com"}},
			RateLimit:      &resourcev1.WebsiteRateLimit{Enabled: true, RequestsPerMinute: 120, Burst: 20},
			PathRules:      []*resourcev1.WebsitePathRule{{From: "/old", To: "/new", Kind: resourcev1.WebsitePathRuleKind_WEBSITE_PATH_RULE_KIND_REDIRECT, Permanent: true}},
			SubpathProxies: []*resourcev1.WebsiteSubpathProxy{{Path: "/api/*", Url: "http://192.0.2.10:3000"}},
			Headers:        []*resourcev1.WebsiteHeader{{Name: "X-Frame-Options", Value: "DENY"}},
			StaticCache:    &resourcev1.WebsiteStaticCache{Enabled: true, MaxAgeSeconds: 86400},
		},
		CustomCaddy: "respond /healthz 200",
	}
	var ruleMsgs []string
	for _, n := range visitorNotes("env_1", prev, rulesNext) {
		ruleMsgs = append(ruleMsgs, n.Message)
	}
	joined := strings.Join(ruleMsgs, "\n")
	for _, want := range []string{
		"Basic Auth",
		"CORS",
		"120 requests/min",
		"redirect and rewrite",
		"/api/*",
		"response header",
		"Static files are cached",
		"custom Caddy directives",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected visitor notes to mention %q, got:\n%s", want, joined)
		}
	}
}

// TestPrepareBasicAuthSecretsKeepsHandMadeSecrets verifies L6: removing a Basic Auth
// user deletes only that user's managed secrets and preserves hand-made WEBSITE_AUTH_* secrets.
func TestPrepareBasicAuthSecretsKeepsHandMadeSecrets(t *testing.T) {
	t.Parallel()
	prev := &resourcev1.Website{
		Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/site"}},
		Rules: &resourcev1.WebsiteRules{
			BasicAuth: &resourcev1.WebsiteBasicAuth{
				Users: []*resourcev1.WebsiteBasicAuthUser{
					{Username: "alice", Secret: "WEBSITE_AUTH_ALICE"},
					{Username: "bob", Secret: "WEBSITE_AUTH_BOB"},
				},
			},
		},
	}
	next := &resourcev1.Website{
		Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/site"}},
		Rules: &resourcev1.WebsiteRules{
			BasicAuth: &resourcev1.WebsiteBasicAuth{
				Users: []*resourcev1.WebsiteBasicAuthUser{
					{Username: "bob", Secret: "WEBSITE_AUTH_BOB"},
				},
			},
		},
	}
	existing := []store.Resource{
		{Name: "WEBSITE_AUTH_ALICE"},
		{Name: "WEBSITE_AUTH_ALICE_HASH"},
		{Name: "WEBSITE_AUTH_BOB"},
		{Name: "WEBSITE_AUTH_BOB_HASH"},
		{Name: "WEBSITE_AUTH_CUSTOM_HANDMADE"},
	}
	edits, err := prepareBasicAuthSecrets("env_1", prev, next, nil, existing)
	if err != nil {
		t.Fatalf("prepareBasicAuthSecrets = %v", err)
	}
	var deleted []string
	for _, e := range edits {
		if e.GetDelete() {
			deleted = append(deleted, e.GetResource().GetName())
		}
	}
	slices.Sort(deleted)
	want := []string{"WEBSITE_AUTH_ALICE", "WEBSITE_AUTH_ALICE_HASH"}
	if !slices.Equal(deleted, want) {
		t.Fatalf("deleted = %v, want %v (must keep WEBSITE_AUTH_CUSTOM_HANDMADE)", deleted, want)
	}
}

// TestPreviewWebsiteServerAccessCheck verifies L2: PreviewWebsite with server_id set
// refuses callers who don't hold admin access on a project running on that server.
func TestPreviewWebsiteServerAccessCheck(t *testing.T) {
	t.Parallel()
	h, _, eng, db := setupWebsitesHandler(t)
	var projID, envID string
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		tr, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		projID = tr.Projects[0].ID
		envID = tr.Projects[0].Environments[0].ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Place a service in projID on srv_1.
	p, err := eng.Propose(t.Context(), revision.Proposal{
		Summary: "add svc",
		Actor:   "user:usr_1",
		Edits: []revision.Edit{{
			Ref:     resource.Ref{Kind: service.Kind, Scope: envID, Name: "app"},
			Spec:    &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}},
			Servers: []string{"srv_1"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Apply(t.Context(), p.Change); err != nil {
		t.Fatal(err)
	}

	req := &dockwellv1.PreviewWebsiteRequest{
		ServerId: "srv_1",
		Website: &resourcev1.Website{
			Serves:      &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/site"}},
			CustomCaddy: "respond /healthz 200",
		},
	}

	// Viewer on install is refused before probing or calling any agent.
	viewerCtx := callerCtx(t, store.Grant{Role: store.RoleViewer})
	if _, err := h.PreviewWebsite(viewerCtx, connect.NewRequest(req)); err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer PreviewWebsite(server_id) = %v, want PermissionDenied", err)
	}

	// Project admin on srv_1 is NOT refused for srv_1 (only fails on missing agent in this unit harness).
	projAdminCtx := callerCtx(t, store.Grant{ProjectID: projID, Role: store.RoleAdmin})
	if _, err := h.PreviewWebsite(projAdminCtx, connect.NewRequest(req)); err == nil || connect.CodeOf(err) == connect.CodePermissionDenied {
		t.Fatalf("project admin on srv_1 got %v, want non-PermissionDenied", err)
	}

	// Project admin on srv_1 IS refused when probing srv_2 (no project on srv_2).
	reqOther := proto.CloneOf(req)
	reqOther.ServerId = "srv_2"
	if _, err := h.PreviewWebsite(projAdminCtx, connect.NewRequest(reqOther)); err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("project admin probing srv_2 = %v, want PermissionDenied", err)
	}
}

// TestCustomCaddyValidationPathsM2 verifies M2:
//   - UpdateWebsite while paused still runs server-side Caddy adapter validation on the unpaused config,
//   - BatchUpdateWebsites validates custom_caddy when resuming,
//   - Raw project edits through ChangeService.ProposeChange cannot add or modify custom_caddy.
func TestCustomCaddyValidationPathsM2(t *testing.T) {
	t.Parallel()
	h, ch, eng, db := setupWebsitesHandler(t)
	adminCtx := callerCtx(t, store.Grant{Role: store.RoleAdmin})

	// Create a normal website without custom_caddy.
	created, err := h.CreateWebsite(adminCtx, connect.NewRequest(&dockwellv1.CreateWebsiteRequest{
		Domain:  "m2.example.com",
		Servers: []string{"srv_1"},
		Website: &resourcev1.Website{
			Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/m2"}},
		},
	}))
	if err != nil {
		t.Fatalf("CreateWebsite: %v", err)
	}
	if err := eng.Apply(t.Context(), created.Msg.GetChange().GetId()); err != nil {
		t.Fatal(err)
	}
	list, err := h.ListWebsites(adminCtx, connect.NewRequest(&dockwellv1.ListWebsitesRequest{}))
	if err != nil || len(list.Msg.GetWebsites()) != 1 {
		t.Fatalf("ListWebsites: %v, %v", list, err)
	}
	siteInfo := list.Msg.GetWebsites()[0]

	// 1. UpdateWebsite while paused with custom_caddy MUST attempt server validation
	//    (since h.agents is nil, it refuses instead of silently accepting paused config).
	_, err = h.UpdateWebsite(adminCtx, connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{
		Id: siteInfo.GetId(),
		Website: &resourcev1.Website{
			Serves:      &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/m2"}},
			Paused:      true,
			CustomCaddy: "respond /healthz 200",
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "validated by Caddy") {
		t.Fatalf("UpdateWebsite(paused + custom_caddy) = %v, want adapter validation requirement", err)
	}

	// 2. Raw project edit through ChangeService.ProposeChange adding custom_caddy MUST be refused.
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
	_, err = ch.ProposeChange(adminCtx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "bypass custom caddy via raw project edit",
		Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: string(scope.ProjectKind), Scope: orgID, Name: siteInfo.GetName()},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{
				DisplayName: "m2.example.com",
				Website: &resourcev1.Website{
					Serves:      &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/m2"}},
					CustomCaddy: "respond /evil 200",
				},
			}}},
		}},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "WebsiteService") {
		t.Fatalf("raw ProposeChange with custom_caddy = %v, want InvalidArgument naming WebsiteService", err)
	}

	// 3. If a paused site already has custom_caddy stored, BatchUpdateWebsites(paused=false)
	//    MUST run Caddy adapter validation before proposing resume.
	pEdit, err := eng.Propose(t.Context(), revision.Proposal{
		Summary: "seed paused custom_caddy",
		Actor:   "user:usr_1",
		Edits: []revision.Edit{
			{
				Ref: resource.Ref{Kind: scope.ProjectKind, Scope: orgID, Name: siteInfo.GetName()},
				Spec: &resourcev1.Project{
					DisplayName: "m2.example.com",
					Website: &resourcev1.Website{
						Serves:      &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/m2"}},
						Paused:      true,
						CustomCaddy: "respond /healthz 200",
					},
				},
			},
			{
				Ref:     resource.Ref{Kind: service.Kind, Scope: siteInfo.GetEnvironmentId(), Name: "web"},
				Spec:    &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "caddy:2"}}},
				Servers: []string{"srv_1"},
			},
			{
				Ref:     resource.Ref{Kind: route.Kind, Scope: siteInfo.GetEnvironmentId(), Name: "site"},
				Spec:    &resourcev1.Route{Domain: "m2.example.com", Service: "web", Port: 8080},
				Servers: []string{"srv_1"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Apply(t.Context(), pEdit.Change); err != nil {
		t.Fatal(err)
	}
	_, err = h.BatchUpdateWebsites(adminCtx, connect.NewRequest(&dockwellv1.BatchUpdateWebsitesRequest{
		WebsiteIds: []string{siteInfo.GetId()},
		Paused:     proto.Bool(false),
	}))
	if err == nil || !strings.Contains(err.Error(), "validated by Caddy") {
		t.Fatalf("BatchUpdateWebsites(resume with custom_caddy) = %v, want adapter validation requirement", err)
	}
}

// TestSubpathProxyServiceIDProposeAndRoleCheck verifies L1:
//   - a viewer of Project B cannot publish Project B's service on Project A's website,
//   - a developer of Project B (and admin of Project A) succeeds through engine.Propose,
//   - a raw cross-env NetworkMember without a matching website subpath proxy is still refused.
func TestSubpathProxyServiceIDProposeAndRoleCheck(t *testing.T) {
	t.Parallel()
	h, _, eng, db := setupWebsitesHandler(t)
	installAdminCtx := callerCtx(t, store.Grant{Role: store.RoleAdmin})

	// Project B is the default project; add a non-protected staging environment and deploy "api" on srv_1.
	var projBID, envBID string
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		tr, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		projBID = tr.Projects[0].ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pEnv, err := eng.Propose(t.Context(), revision.Proposal{
		Summary: "add staging in project B",
		Actor:   "user:usr_1",
		Edits: []revision.Edit{{
			Ref:  resource.Ref{Kind: scope.EnvironmentKind, Scope: projBID, Name: "staging"},
			Spec: &resourcev1.Environment{},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Apply(t.Context(), pEnv.Change); err != nil {
		t.Fatal(err)
	}
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		tr, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		for _, e := range tr.Projects[0].Environments {
			if e.Name == "staging" {
				envBID = e.ID
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pAPI, err := eng.Propose(t.Context(), revision.Proposal{
		Summary: "add api in project B",
		Actor:   "user:usr_1",
		Edits: []revision.Edit{{
			Ref:     resource.Ref{Kind: service.Kind, Scope: envBID, Name: "api"},
			Spec:    &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "ghcr.io/acme/api:1"}}},
			Servers: []string{"srv_1"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Apply(t.Context(), pAPI.Change); err != nil {
		t.Fatal(err)
	}
	var apiSvcID string
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(service.Kind), envBID, "api")
		if err != nil {
			return err
		}
		apiSvcID = r.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Create website A on srv_1.
	created, err := h.CreateWebsite(installAdminCtx, connect.NewRequest(&dockwellv1.CreateWebsiteRequest{
		Domain:  "site-a.example.com",
		Servers: []string{"srv_1"},
		Website: &resourcev1.Website{
			Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/site-a"}},
		},
	}))
	if err != nil {
		t.Fatalf("CreateWebsite: %v", err)
	}
	if err := eng.Apply(t.Context(), created.Msg.GetChange().GetId()); err != nil {
		t.Fatal(err)
	}
	sitesResp, err := h.ListWebsites(installAdminCtx, connect.NewRequest(&dockwellv1.ListWebsitesRequest{}))
	if err != nil || len(sitesResp.Msg.GetWebsites()) != 1 {
		t.Fatalf("ListWebsites: %v, %v", sitesResp, err)
	}
	siteA := sitesResp.Msg.GetWebsites()[0]

	updateReq := &dockwellv1.UpdateWebsiteRequest{
		Id: siteA.GetId(),
		Website: &resourcev1.Website{
			Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/site-a"}},
			Rules: &resourcev1.WebsiteRules{
				SubpathProxies: []*resourcev1.WebsiteSubpathProxy{{
					Path:        "/api",
					ServiceId:   apiSvcID,
					ServicePort: 8080,
				}},
			},
		},
	}

	// 1. Admin on Project A + Viewer on Project B -> MUST be refused with PermissionDenied.
	viewerOfB := callerCtx(t,
		store.Grant{ProjectID: siteA.GetId(), Role: store.RoleAdmin},
		store.Grant{ProjectID: projBID, Role: store.RoleViewer},
	)
	if _, err := h.UpdateWebsite(viewerOfB, connect.NewRequest(updateReq)); err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer of target project B got %v, want PermissionDenied", err)
	}

	// 2. Install admin (holds developer+ on Project B and admin on Project A) -> MUST succeed through engine.Propose!
	res, err := h.UpdateWebsite(installAdminCtx, connect.NewRequest(updateReq))
	if err != nil {
		t.Fatalf("UpdateWebsite with subpath service_id failed Propose: %v", err)
	}
	if res.Msg.GetChange().GetId() == "" {
		t.Fatal("expected planned change ID")
	}

	// 2b. Verify resolveSubpathMembers accepts developer on non-protected Project B staging.
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		tr, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		devOfB := callerCtx(t,
			store.Grant{ProjectID: siteA.GetId(), Role: store.RoleAdmin},
			store.Grant{ProjectID: projBID, Role: store.RoleDeveloper},
		)
		members, err := h.resolveSubpathMembers(devOfB, tx, tr, updateReq.GetWebsite(), []string{"srv_1"})
		if err != nil {
			t.Fatalf("resolveSubpathMembers(developer of B) = %v", err)
		}
		if len(members) != 1 || members[0].GetEnvironment() != envBID || members[0].GetService() != "api" {
			t.Fatalf("unexpected subpath members: %v", members)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// 3. Raw cross-env NetworkMember on a non-subpath network MUST still be refused by reach.go.
	_, err = eng.Propose(t.Context(), revision.Proposal{
		Summary: "raw cross-env network member without link or subpath",
		Actor:   "user:usr_1",
		Edits: []revision.Edit{{
			Ref: resource.Ref{Kind: network.Kind, Scope: siteA.GetEnvironmentId(), Name: "extra"},
			Spec: &resourcev1.Network{
				Members: []*resourcev1.NetworkMember{{Environment: envBID, Service: "api", Alias: "hijack"}},
			},
			Servers: []string{"srv_1"},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "no link") {
		t.Fatalf("raw cross-env NetworkMember should be refused, got %v", err)
	}
}
