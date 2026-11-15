// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// narrowedToken makes an API token for the person signed in to b, narrowed as
// req says, and returns its secret.
func narrowedToken(t *testing.T, b *browser, req *dockwellv1.CreateTokenRequest) (string, *dockwellv1.Token) {
	t.Helper()
	req.Name, req.Proof = "CI narrowed", stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, "")
	if req.Scope == dockwellv1.TokenScope_TOKEN_SCOPE_UNSPECIFIED {
		req.Scope = dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE
	}
	res, err := dockwellv1connect.NewTokenServiceClient(b, b.origin, connect.WithProtoJSON()).CreateToken(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("CreateToken narrowed to %v as %v: %v", req.GetProjects(), req.GetRole(), err)
	}
	return res.Msg.GetSecret(), res.Msg.GetToken()
}

// A token narrowed to a project reaches it alone, whoever made it: what's in
// another project isn't there for it, lists hold only its project's, servers read
// by name and state, and what's the install's own is refused with token_places,
// audited once. Within its project, an owner's token acts as an admin would
// (ADR 0071).
func TestATokenNarrowedToAProjectReachesItAlone(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		resourceOf := func(ch *dockwellv1.Change, kind string) string {
			t.Helper()
			for _, r := range ch.GetPlan().GetResources() {
				if r.GetResource().GetKind() == kind {
					return r.GetId()
				}
			}
			t.Fatalf("%s makes no %s: %v", ch.GetSummary(), kind, ch.GetPlan().GetResources())
			return ""
		}
		app := func(env, name string, vars map[string]string) *dockwellv1.Edit {
			return &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Scope: env, Name: name},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
					Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "ghcr.io/acme/shop:1.4", Env: vars}},
				}}},
			}
		}
		web := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
		webID := resourceOf(web, "service")
		blogID := resourceOf(apply(t, c, propose(t, c, "Make blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		})), "project")
		staging := resourceOf(apply(t, c, propose(t, c, "Add staging to blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: blogID, Name: "staging"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
		})), "environment")
		apply(t, c, propose(t, c, "Run api", app(staging, "api", nil)))

		secret, made := narrowedToken(t, b, &dockwellv1.CreateTokenRequest{Projects: []string{"blog"}})
		if got := made.GetProjects(); len(got) != 1 || got[0].GetId() != blogID || got[0].GetName() != "blog" {
			t.Errorf("the token says it acts in %v, want blog by its ID and name", got)
		}
		script := bearer{token: secret, rt: b.client.Transport}
		scripted := changeClient(script, b.origin)
		services := dockwellv1connect.NewServiceServiceClient(script, b.origin, connect.WithProtoJSON())

		_, err := services.ListServiceRevisions(t.Context(), connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: webID}))
		if connect.CodeOf(err) != connect.CodeNotFound || reasonOf(err) != "not_found" {
			t.Errorf("another project's service, by ID: %v, want not found", err)
		}
		listed, err := services.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, s := range listed.Msg.GetServices() {
			names = append(names, s.GetResource().GetName())
		}
		if !slices.Equal(names, []string{"api"}) {
			t.Errorf("the token lists services %v, want its project's alone", names)
		}

		// Servers are named on the install: by name and state alone, as below an admin.
		servers, err := dockwellv1connect.NewServerServiceClient(script, b.origin, connect.WithProtoJSON()).
			ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{}))
		if err != nil || len(servers.Msg.GetServers()) == 0 {
			t.Fatalf("the token listing servers: %v, %v", servers, err)
		}
		if s := servers.Msg.GetServers()[0]; s.GetName() == "" || s.GetInventory() != nil || s.GetStats() != nil {
			t.Errorf("the owner's narrowed token reads a server as %v, want its name and state alone", s)
		}

		// The install's own is refused, saying where the token acts, and audited once.
		settings := dockwellv1connect.NewSettingsServiceClient(script, b.origin, connect.WithProtoJSON())
		for range 2 {
			_, err = settings.GetControlPlane(t.Context(), connect.NewRequest(&dockwellv1.GetControlPlaneRequest{}))
			if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "token_places" || !strings.Contains(err.Error(), "acts only in blog") {
				t.Errorf("the token reading the control plane: %v, want token_places naming blog", err)
			}
		}
		err = f.store.View(t.Context(), func(tx *store.Tx) error {
			evs, err := tx.ListEvents(t.Context(), 1_000)
			var refusals []string
			for _, e := range evs {
				if e.Action == "role.refuse" {
					refusals = append(refusals, e.Detail)
				}
			}
			if len(refusals) != 1 || !strings.Contains(refusals[0], "GetControlPlane, which is the install's own") ||
				!strings.Contains(refusals[0], "acts only in blog") {
				t.Errorf("role.refuse = %q, want the one refused call, once, naming blog", refusals)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		// In its project it plans and applies, as the owner would there.
		planned, err := scripted.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Change api", Edits: []*dockwellv1.Edit{app(staging, "api", map[string]string{"A": "1"})},
		}))
		if err != nil {
			t.Fatalf("the token planning in its project: %v", err)
		}
		if _, err := scripted.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{
			Id: planned.Msg.GetChange().GetId(), Fingerprint: planned.Msg.GetChange().GetPlan().GetFingerprint(),
		})); err != nil {
			t.Errorf("the token applying in its project: %v", err)
		}
		// A plan that names no project lands in its one: production is blog's own, not the default project's.
		elsewhere, err := scripted.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Run web in production", Edits: []*dockwellv1.Edit{app("production", "web", nil)},
		}))
		if err != nil {
			t.Fatalf("the token planning in production by name: %v", err)
		}
		for _, r := range elsewhere.Msg.GetChange().GetPlan().GetResources() {
			if p := envProject(t, f, r.GetResource().GetScope()); p != blogID {
				t.Errorf("the token's plan for production is in %s, want blog's own (%s)", p, blogID)
			}
		}
	})
}

// A token capped at developer plans in a protected environment, and its change
// waits for an admin, while its maker's uncapped token applies there; capped at
// viewer, it only reads. What it's refused says the cap is why (ADR 0071).
func TestATokenCappedAtARoleActsAsThatRole(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))

		developer, made := narrowedToken(t, b, &dockwellv1.CreateTokenRequest{Role: dockwellv1.TokenRole_TOKEN_ROLE_DEVELOPER})
		if made.GetRole() != dockwellv1.TokenRole_TOKEN_ROLE_DEVELOPER || len(made.GetProjects()) != 0 {
			t.Errorf("the token says it acts as %v in %v, want a developer everywhere", made.GetRole(), made.GetProjects())
		}
		viewer, _ := narrowedToken(t, b, &dockwellv1.CreateTokenRequest{Role: dockwellv1.TokenRole_TOKEN_ROLE_VIEWER})
		uncapped := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		deploy := func(secret string) error {
			t.Helper()
			client := changeClient(bearer{token: secret, rt: b.client.Transport}, b.origin)
			planned, err := client.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
				Summary: "Deploy web", Edits: []*dockwellv1.Edit{runImage("ghcr.io/acme/shop:1.5")},
			}))
			if err != nil {
				return err
			}
			_, err = client.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{
				Id: planned.Msg.GetChange().GetId(), Fingerprint: planned.Msg.GetChange().GetPlan().GetFingerprint(),
			}))
			return err
		}

		// Production is protected by its name.
		if err := deploy(developer); connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "needs_role" {
			t.Errorf("a developer-capped token applying in production: %v, want it to wait for an admin", err)
		}
		if err := deploy(uncapped); err != nil {
			t.Errorf("the owner's uncapped token applying in production: %v", err)
		}
		err := deploy(viewer)
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "this API token acts as a viewer at most") {
			t.Errorf("a viewer-capped token planning: %v, want it refused for its cap", err)
		}
		_, err = dockwellv1connect.NewSettingsServiceClient(bearer{token: developer, rt: b.client.Transport}, b.origin, connect.WithProtoJSON()).
			GetControlPlane(t.Context(), connect.NewRequest(&dockwellv1.GetControlPlaneRequest{}))
		if reasonOf(err) != "needs_role" || !strings.Contains(err.Error(), "this API token acts as a developer at most") {
			t.Errorf("a developer-capped token reading the control plane: %v, want needs_role for its cap", err)
		}
	})
}

// A token is narrowed only within what its maker holds when it's made: a project
// they hold nothing in, one that isn't there, or a cap at their own role are
// refused. Once its maker loses its project, it reaches nothing (ADR 0071).
func TestATokenIsNarrowedWithinWhatItsMakerHolds(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		c := changeClient(b, b.origin)
		blog := apply(t, c, propose(t, c, "Make blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		}))
		blogID := blog.GetPlan().GetResources()[0].GetId()
		hold := aMember(t, f)
		hold(store.Grant{ProjectID: blogID, Role: store.RoleDeveloper})

		tokens := dockwellv1connect.NewTokenServiceClient(b, b.origin, connect.WithProtoJSON())
		refused := func(what string, req *dockwellv1.CreateTokenRequest, says string) {
			t.Helper()
			req.Name, req.Scope = "CI", dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE
			req.Proof = stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, "")
			_, err := tokens.CreateToken(t.Context(), connect.NewRequest(req))
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), says) {
				t.Errorf("%s: %v, want it refused saying %q", what, err, says)
			}
		}
		// Out of reach answers as missing does, in the same words, as the project's typed (ADR 0071).
		refused("a project they hold nothing in", &dockwellv1.CreateTokenRequest{Projects: []string{"default"}}, "there's no project default. Name one you hold")
		refused("a project that isn't there", &dockwellv1.CreateTokenRequest{Projects: []string{"nowhere"}}, "there's no project nowhere")
		refused("a cap at their own role", &dockwellv1.CreateTokenRequest{Role: dockwellv1.TokenRole_TOKEN_ROLE_DEVELOPER},
			"you're a developer, so a token that acts as a developer at most acts as you do already")

		secret, _ := narrowedToken(t, b, &dockwellv1.CreateTokenRequest{Projects: []string{blogID}})
		changes := changeClient(bearer{token: secret, rt: b.client.Transport}, b.origin)
		if _, err := changes.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{})); err != nil {
			t.Errorf("the token reading its project's changes: %v", err)
		}
		hold(onInstall(store.RoleViewer))
		_, err := changes.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
		if err != nil {
			t.Errorf("the token, its maker a viewer of the whole install now: %v, want it reading blog as a viewer", err)
		}
		hold()
		_, err = changes.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
		if reasonOf(err) != "needs_role" || !strings.Contains(err.Error(), "this API token's projects are gone, or its maker holds nothing in them") {
			t.Errorf("the token after its maker lost its project: %v, want it refused, saying why", err)
		}
	})
}

// A deleted project stays on a token by its ID and reaches nothing: the token
// reads it as gone, the plan to remove its maker too, a new token can't be made
// for it, and a token whose projects are all deleted is refused as one whose
// projects are gone. Undoing the deletion brings back the same project, and the
// token's reach in it (ADR 0071).
func TestATokenWhoseProjectIsDeletedReachesNothing(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newInviteStand(t, backend)
		ctx := t.Context()
		owner := changeClient(s.owner, s.origin)
		blogID := apply(t, owner, propose(t, owner, "Make blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		})).GetPlan().GetResources()[0].GetId()
		ada, adaID := s.joins(t, "ada@example.com", "Ada Lovelace", dockwellv1.Role_ROLE_DEVELOPER)
		if _, err := s.invite(t, "bo@example.com", dockwellv1.Role_ROLE_DEVELOPER, "blog", blogID); err != nil {
			t.Fatal(err)
		}
		secret, made := narrowedToken(t, ada, &dockwellv1.CreateTokenRequest{Projects: []string{"blog"}})
		script := changeClient(bearer{token: secret, rt: ada.client.Transport}, s.origin)
		if _, err := script.ListChanges(ctx, connect.NewRequest(&dockwellv1.ListChangesRequest{})); err != nil {
			t.Fatalf("the token reading blog's changes: %v", err)
		}

		deleted := apply(t, owner, propose(t, owner, "Delete blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"}, Delete: true,
		}))
		if deleted.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("deleting blog ended %v: %v", deleted.GetState(), deleted.GetError())
		}

		// Its project reads as gone, by its ID with no name, on API tokens, in the plan to
		// remove ada, and on an invite to it.
		tokens := dockwellv1connect.NewTokenServiceClient(ada, s.origin, connect.WithProtoJSON())
		listed, err := tokens.ListTokens(ctx, connect.NewRequest(&dockwellv1.ListTokensRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		gone := func(what string, got []*dockwellv1.TokenProject) {
			t.Helper()
			if len(got) != 1 || got[0].GetId() != blogID || got[0].GetName() != "" {
				t.Errorf("%s lists the token in %v, want blog by its ID alone, as gone", what, got)
			}
		}
		i := slices.IndexFunc(listed.Msg.GetTokens(), func(tok *dockwellv1.Token) bool { return tok.GetId() == made.GetId() })
		if i < 0 {
			t.Fatalf("API tokens lists %v, without ada's", listed.Msg.GetTokens())
		}
		gone("API tokens", listed.Msg.GetTokens()[i].GetProjects())
		plan, err := s.members.PlanMemberRemoval(ctx, connect.NewRequest(&dockwellv1.PlanMemberRemovalRequest{UserId: adaID}))
		if err != nil {
			t.Fatal(err)
		}
		i = slices.IndexFunc(plan.Msg.GetTokens(), func(tok *dockwellv1.RemovalToken) bool { return tok.GetId() == made.GetId() })
		if i < 0 {
			t.Fatalf("the plan to remove ada lists %v, without her token", plan.Msg.GetTokens())
		}
		gone("The plan to remove ada", plan.Msg.GetTokens()[i].GetProjects())

		members, err := s.members.ListMembers(ctx, connect.NewRequest(&dockwellv1.ListMembersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		i = slices.IndexFunc(members.Msg.GetInvites(), func(inv *dockwellv1.Invite) bool { return inv.GetEmail() == "bo@example.com" })
		if i < 0 {
			t.Fatalf("Members lists invites %v, without bo's", members.Msg.GetInvites())
		}
		if inv := members.Msg.GetInvites()[i]; inv.GetProjectId() != blogID || inv.GetProjectName() != "" {
			t.Errorf("Members lists bo's invite in %q (%s), want blog by its ID alone, as gone", inv.GetProjectName(), inv.GetProjectId())
		}

		// A new token for it is refused as for a project that isn't there.
		_, err = tokens.CreateToken(ctx, connect.NewRequest(&dockwellv1.CreateTokenRequest{
			Name: "CI", Scope: dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE, Projects: []string{blogID},
			Proof: stepUpFor(t, ada, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "there's no project "+blogID) {
			t.Errorf("a token for blog once it's deleted: %v, want it refused as for one that isn't there", err)
		}

		// The token, its one project deleted, reaches nothing, saying why.
		_, err = script.ListChanges(ctx, connect.NewRequest(&dockwellv1.ListChangesRequest{}))
		if reasonOf(err) != "needs_role" || !strings.Contains(err.Error(), "this API token's projects are gone") {
			t.Errorf("the token once blog is deleted: %v, want it refused as one whose projects are gone", err)
		}

		// Undoing the deletion brings blog back by the same ID, and the token's reach in it.
		undo, err := owner.UndoChange(ctx, connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: deleted.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if back := apply(t, owner, undo.Msg.GetChange()); back.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("undoing blog's deletion ended %v: %v", back.GetState(), back.GetError())
		}
		if _, err := script.ListChanges(ctx, connect.NewRequest(&dockwellv1.ListChangesRequest{})); err != nil {
			t.Errorf("the token once blog's deletion is undone: %v, want it reading blog again", err)
		}
	})
}
