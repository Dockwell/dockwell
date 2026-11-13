// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
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
