// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// applyAsRoot applies a plan that's as good as root, with a passkey for it.
func applyAsRoot(t *testing.T, b *browser, change *dockwellv1.Change) {
	t.Helper()
	c := changeClient(b, b.origin)
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	w := watch(t, c, change.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{
		Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof,
	})); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, w)
}

// A stolen session could apply a plan that's as good as root, and keep a way in to
// the server after the session is gone. So one takes a passkey, just now, for that
// change, as well as a person and a typed fingerprint.
func TestAPlanAsGoodAsRootTakesAPasskey(t *testing.T) {
	storetest.Each(t, testAPlanAsGoodAsRootTakesAPasskey)
}

func testAPlanAsGoodAsRootTakesAPasskey(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	root := importCompose(t, cc, "services:\n  app:\n    image: ghcr.io/acme/shop:1.4\n    privileged: true\n", nil, false)
	if !root.GetPlan().GetHostAccess() {
		t.Fatalf("privileged mode plans no host access: %v", root.GetPlan())
	}
	apply := func(fingerprint, proof string) error {
		t.Helper()
		_, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: root.GetId(), Fingerprint: fingerprint, Proof: proof}))
		return err
	}
	planned := func() {
		t.Helper()
		got, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: root.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if got.Msg.GetChange().GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
			t.Fatalf("the change is %v", got.Msg.GetChange().GetState())
		}
	}
	fingerprint := root.GetPlan().GetFingerprint()
	action := dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE

	// Without one it asks, having checked everything else, and nothing runs.
	if err := apply(fingerprint, ""); reasonOf(err) != "needs_step_up" || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("with no proof = %v", err)
	}
	planned()
	// What else would refuse it comes first, and leaves a proof unspent.
	proof := stepUpFor(t, b, action, root.GetId())
	if err := apply("not-this-plan", proof); reasonOf(err) != "plan_changed" {
		t.Fatalf("with another fingerprint = %v", err)
	}
	if err := apply("", proof); reasonOf(err) != "needs_confirmation" {
		t.Fatalf("with no fingerprint = %v", err)
	}
	// One for another change, or for something else, doesn't do, and it's spent.
	for what, other := range map[string]string{
		"another change":    stepUpFor(t, b, action, "chg_another"),
		"revealing instead": stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_REVEAL_SECRET, root.GetId()),
	} {
		if err := apply(fingerprint, other); reasonOf(err) != "needs_step_up" {
			t.Fatalf("with a proof for %s = %v", what, err)
		}
		planned()
		if err := apply(fingerprint, other); reasonOf(err) != "needs_step_up" {
			t.Fatalf("with that proof again = %v", err)
		}
	}

	w := watch(t, c, root.GetId())
	if err := apply(fingerprint, proof); err != nil {
		t.Fatalf("with its proof = %v", err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the change = %v", got)
	}
}

// What stops short of root asks no more of a person than it did, or the prompt would
// stop meaning anything. Keeping what a service has asks nothing at all.
func TestAPlanShortOfRootTakesNoPasskey(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	folder := importCompose(t, cc, nasFile(":ro"), nil, false)
	if !folder.GetPlan().GetHostAccess() {
		t.Fatalf("a bind volume plans no host access: %v", folder.GetPlan())
	}
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: folder.GetId(), Fingerprint: folder.GetPlan().GetFingerprint()})); err != nil {
		t.Fatalf("a plan short of root = %v", err)
	}
}

// The words are for a person, whoever reads them first.
func TestAskingForAPasskeySaysSo(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	root := importCompose(t, cc, "services:\n  app:\n    image: ghcr.io/acme/shop:1.4\n    privileged: true\n", nil, false)
	_, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: root.GetId(), Fingerprint: root.GetPlan().GetFingerprint()}))
	if err == nil || !strings.Contains(err.Error(), "confirm with your passkey") {
		t.Fatalf("got %v", err)
	}
}
