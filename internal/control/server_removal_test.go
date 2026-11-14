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
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// An admin takes a server out of the fleet with a plan that says what becomes of
// what runs there, applied with a passkey, which no token applies: the server is
// gone from the fleet, its agent's stream ends at once, and a removed server can't
// be removed again (ADR 0090).
func TestAnAdminRemovesAServer(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addServerNamed(t, "hz-1", nil, nil)
	f.addServerNamed(t, "hz-2", nil, nil)
	second := f.lastDocker.id.ServerID
	waitConnected(t, f, second, true)
	person := changeClient(b, b.origin)
	imported, err := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()).ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{
		Project: scope.DefaultProject, Compose: "services:\n  web:\n    image: ghcr.io/acme/shop:1.4\n", Servers: []string{second},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := apply(t, person, imported.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("web on hz-2: %v", got)
	}

	res, err := person.PlanRemoveServer(t.Context(), connect.NewRequest(&dockwellv1.PlanRemoveServerRequest{ServerId: second}))
	if err != nil {
		t.Fatal(err)
	}
	ch := res.Msg.GetChange()
	p := ch.GetPlan()
	if p.GetRemoves().GetServerId() != second || p.GetRemoves().GetServerName() != "hz-2" || !p.GetDestructive() {
		t.Fatalf("the plan removes %v, destructive %v", p.GetRemoves(), p.GetDestructive())
	}
	if !slices.ContainsFunc(p.GetNotes(), func(n *dockwellv1.PlanNote) bool {
		return n.GetReason() == "stays_on_server" && strings.Contains(n.GetMessage(), "the app web")
	}) || !slices.ContainsFunc(p.GetNotes(), func(n *dockwellv1.PlanNote) bool { return n.GetReason() == "removal_final" }) {
		t.Errorf("the plan's notes: %v", p.GetNotes())
	}

	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	if _, err := script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: p.GetFingerprint()})); reasonOf(err) != "needs_session" {
		t.Errorf("a token applied a removal: %v", err)
	}
	if _, err := person.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: p.GetFingerprint()})); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "passkey") {
		t.Fatalf("an admin applied a removal without a passkey: %v", err)
	}
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, ch.GetId())
	w := watch(t, person, ch.GetId())
	if _, err := person.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: p.GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the removal: %v", got)
	}
	waitConnected(t, f, second, false)

	sc := dockwellv1connect.NewServerServiceClient(b, b.origin, connect.WithProtoJSON())
	listed, err := sc.ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(listed.Msg.GetServers(), func(s *dockwellv1.Server) bool { return s.GetId() == second }) {
		t.Error("the fleet still lists hz-2")
	}
	if _, err := person.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: ch.GetId()})); err == nil || !strings.Contains(err.Error(), "can't be undone") {
		t.Errorf("undoing a removal: %v", err)
	}
	if _, err := person.PlanRemoveServer(t.Context(), connect.NewRequest(&dockwellv1.PlanRemoveServerRequest{ServerId: second})); err == nil {
		t.Error("a removed server was planned to be removed again")
	}
}

// Removing a server is the install's admin's at every step (ADR 0090): a developer
// over everything on it, in an environment no protection holds, can't plan, apply
// or discard a removal, marked compromised or not, with their passkey or without.
func TestOnlyAnAdminActsOnARemoval(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addServerNamed(t, "hz-1", nil, nil)
	f.addServerNamed(t, "hz-2", nil, nil)
	second := f.lastDocker.id.ServerID
	waitConnected(t, f, second, true)
	person := changeClient(b, b.origin)
	apply(t, person, propose(t, person, "Make blog", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
	}))
	blog := envProjectByName(t, f, "blog")
	apply(t, person, propose(t, person, "Add staging to blog", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: blog, Name: "staging"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
	}))
	imported, err := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()).ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{
		Project: "blog", Environment: "staging", Compose: "services:\n  web:\n    image: ghcr.io/acme/shop:1.4\n", Servers: []string{second},
	}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, person, imported.Msg.GetChange())

	// The owner, someone else now, plans both.
	hold := aMember(t, f)
	hold(onInstall(store.RoleDeveloper))
	owner := place.WithCaller(t.Context(), place.Caller{UserID: "usr_keeper", Grants: []store.Grant{{UserID: "usr_keeper", ProjectID: store.Install, Role: store.RoleOwner}}})
	plan := func(compromised bool) *dockwellv1.Change {
		t.Helper()
		p, err := f.engine.RemoveServer(owner, revision.RemoveServer{Server: second, Compromised: compromised}, revision.By{Actor: "user:usr_keeper", Via: revision.ViaPanel})
		if err != nil {
			t.Fatal(err)
		}
		got, err := person.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: p.Change}))
		if err != nil {
			t.Fatal(err)
		}
		return got.Msg.GetChange()
	}
	removal, compromised := plan(false), plan(true)
	_, err = person.PlanRemoveServer(t.Context(), connect.NewRequest(&dockwellv1.PlanRemoveServerRequest{ServerId: second}))
	needsRole(t, "a developer planning a removal", err)
	for _, ch := range []*dockwellv1.Change{removal, compromised} {
		_, err = person.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}))
		needsRole(t, "a developer applying "+ch.GetSummary(), err)
		proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, ch.GetId())
		_, err = person.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint(), Proof: proof}))
		needsRole(t, "a developer applying "+ch.GetSummary()+" with their passkey", err)
		_, err = person.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: ch.GetId()}))
		needsRole(t, "a developer discarding "+ch.GetSummary(), err)
	}
	waitConnected(t, f, second, true)
	var revoked bool
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		s, err := tx.GetServer(t.Context(), second)
		revoked = s.RevokedAt != nil
		return err
	}); err != nil || revoked {
		t.Fatalf("after a developer's tries, hz-2 is revoked %t, %v", revoked, err)
	}

	// An admin discards the owner's plans.
	hold(onInstall(store.RoleAdmin))
	for _, ch := range []*dockwellv1.Change{removal, compromised} {
		if _, err := person.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: ch.GetId()})); err != nil {
			t.Errorf("an admin discarding %s: %v", ch.GetSummary(), err)
		}
	}
}

// A server with nothing on it, as one that has just joined, is removed with a plan
// that edits nothing and says it leaves (ADR 0090).
func TestAnEmptyServerIsRemoved(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addServerNamed(t, "hz-1", nil, nil)
	f.addServerNamed(t, "hz-2", nil, nil)
	second := f.lastDocker.id.ServerID
	waitConnected(t, f, second, true)
	person := changeClient(b, b.origin)
	res, err := person.PlanRemoveServer(t.Context(), connect.NewRequest(&dockwellv1.PlanRemoveServerRequest{ServerId: second}))
	if err != nil {
		t.Fatalf("planning to remove an empty server: %v", err)
	}
	ch := res.Msg.GetChange()
	p := ch.GetPlan()
	if p.GetRemoves().GetServerId() != second || len(p.GetServers()) != 1 || !p.GetServers()[0].GetRemoved() || p.GetServers()[0].GetServerName() != "hz-2" {
		t.Fatalf("the plan removes %v, on %v", p.GetRemoves(), p.GetServers())
	}
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, ch.GetId())
	w := watch(t, person, ch.GetId())
	if _, err := person.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: p.GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the removal: %v", got)
	}
	waitConnected(t, f, second, false)
	var revoked bool
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		s, err := tx.GetServer(t.Context(), second)
		revoked = s.RevokedAt != nil
		return err
	}); err != nil || !revoked {
		t.Errorf("hz-2 is revoked %t, %v", revoked, err)
	}
}

// envProjectByName is a project's ID, by its name.
func envProjectByName(t *testing.T, f *fullServer, name string) string {
	t.Helper()
	var id string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		tree, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			if p.Name == name {
				id = p.ID
			}
		}
		return nil
	}); err != nil || id == "" {
		t.Fatalf("no project %s: %v", name, err)
	}
	return id
}
