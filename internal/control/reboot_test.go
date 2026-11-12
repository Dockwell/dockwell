// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// planReboot plans rebooting s, and returns the change with its host's plan.
func planReboot(t *testing.T, c dockwellv1connect.ChangeServiceClient, s *testServer) (*dockwellv1.Change, *dockwellv1.HostReboot) {
	t.Helper()
	res, err := c.PlanReboot(t.Context(), connect.NewRequest(&dockwellv1.PlanRebootRequest{ServerIds: []string{s.id.ServerID}}))
	if err != nil {
		t.Fatal(err)
	}
	change := res.Msg.GetChange()
	for _, r := range change.GetPlan().GetResources() {
		if r.GetResource().GetKind() == string(host.Kind) && r.GetReboot() != nil {
			return change, r.GetReboot()
		}
	}
	t.Fatalf("the plan reboots nothing: %v", change.GetPlan())
	return nil, nil
}

// Rebooting a server is a change with a plan and no revision (ADR 0064): as good as
// root, so a person applies it with a passkey and a token can't; its step waits for
// the server and passes once its agent connects from another boot, keeping its time,
// which the next plan says; the reboot is asked once; and it can't be undone.
func TestARebootPassesOnceTheServerIsBackOnAnotherBoot(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	p := &packages{installed: map[string]string{}, boot: "boot-1"}
	s := f.addHostServer(t, p)
	c := changeClient(b, b.origin)

	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := dockwellv1connect.NewChangeServiceClient(bearer{token: token, rt: b.client.Transport}, b.origin, connect.WithProtoJSON())
	if _, err := script.PlanReboot(t.Context(), connect.NewRequest(&dockwellv1.PlanRebootRequest{ServerIds: []string{s.id.ServerID}})); connect.CodeOf(err) != connect.CodePermissionDenied && connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("a token planning a reboot = %v", err)
	}

	change, reboot := planReboot(t, c, s)
	if change.GetSummary() != "Reboot web-1" || !change.GetPlan().GetHostAccess() || reboot.GetControlServer() || reboot.GetLastTook() != nil {
		t.Errorf("the plan reads %q, host access %t, %v", change.GetSummary(), change.GetPlan().GetHostAccess(), reboot)
	}
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint()})); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "passkey") {
		t.Fatalf("a reboot applied without a passkey = %v", err)
	}
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	// The server was asked, and the step waits for it to come back.
	waiting := waitChange(t, c, change.GetId(), "the reboot waits for its server", func(ch *dockwellv1.Change) bool {
		return slices.ContainsFunc(ch.GetSteps(), func(st *dockwellv1.StepProgress) bool {
			return st.GetState() == dockwellv1.StepState_STEP_STATE_WAITING && strings.Contains(st.GetMessage(), "is restarting")
		})
	})
	if waiting.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLYING {
		t.Fatalf("while its server restarts, the change is %v", waiting.GetState())
	}
	// Planned again on the same boot, as a resume does, it isn't asked twice.
	if err := f.engine.Resume(t.Context()); err != nil && !strings.Contains(err.Error(), "reconnect") {
		t.Logf("resume on the same boot: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	p.reboot(t, f, s, "boot-2")
	if err := f.engine.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := waitChange(t, c, change.GetId(), "the reboot passes", func(ch *dockwellv1.Change) bool {
		return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED || ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_FAILED
	})
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the reboot ended %v: %s", done.GetState(), done.GetError())
	}
	step := done.GetSteps()[0]
	took := step.GetFinishTime().AsTime().Sub(step.GetStartTime().AsTime())
	if !strings.Contains(step.GetMessage(), "is back") || took < 200*time.Millisecond {
		t.Errorf("the step says %q and took %s, want back after the wait", step.GetMessage(), took)
	}
	p.mu.Lock()
	reboots := p.reboots
	p.mu.Unlock()
	if reboots != 1 {
		t.Errorf("the server was asked to reboot %d times, want once", reboots)
	}
	if _, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: change.GetId()})); err == nil || !strings.Contains(err.Error(), "can't be undone") {
		t.Errorf("undoing a reboot = %v", err)
	}
	// The next plan says how long the last reboot took.
	_, again := planReboot(t, c, s)
	if again.GetLastTook().AsDuration() != took.Truncate(time.Millisecond) && again.GetLastTook().AsDuration().Round(time.Millisecond) != took.Round(time.Millisecond) {
		t.Errorf("the next plan says the last reboot took %s, want %s", again.GetLastTook().AsDuration(), took)
	}
}

// The control server's reboot says so in its plan, as the panel and the API stop
// with it.
func TestTheControlServersRebootSaysThePanelStops(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	p := &packages{installed: map[string]string{}, boot: "boot-1"}
	s := f.addHostServer(t, p)
	f.engine.SetControlServer(s.id.ServerID)
	_, reboot := planReboot(t, changeClient(b, b.origin), s)
	if !reboot.GetControlServer() {
		t.Errorf("the control server's reboot reads %v", reboot)
	}
}

// A server that doesn't come back fails its reboot's change at RebootLimit, by the
// step's recorded start, and nothing else waits for it.
func TestARebootWhoseServerDoesntComeBackFails(t *testing.T) {
	was := revision.RebootLimit
	revision.RebootLimit = 300 * time.Millisecond
	t.Cleanup(func() { revision.RebootLimit = was })
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	p := &packages{installed: map[string]string{}, boot: "boot-1"}
	s := f.addHostServer(t, p)
	c := changeClient(b, b.origin)
	change, _ := planReboot(t, c, s)
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	waitChange(t, c, change.GetId(), "the reboot waits", func(ch *dockwellv1.Change) bool {
		return slices.ContainsFunc(ch.GetSteps(), func(st *dockwellv1.StepProgress) bool {
			return st.GetState() == dockwellv1.StepState_STEP_STATE_WAITING
		})
	})
	s.stop()
	// Before the limit, nothing fails it.
	if err := f.engine.FailLostReboots(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := getChange(t, c, change.GetId()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLYING {
		t.Fatalf("before the limit the change is %v", got.GetState())
	}
	time.Sleep(revision.RebootLimit)
	if err := f.engine.FailLostReboots(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := getChange(t, c, change.GetId())
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(got.GetError(), "hasn't come back after its reboot") {
		t.Errorf("past the limit the change is %v: %s", got.GetState(), got.GetError())
	}
}

func getChange(t *testing.T, c dockwellv1connect.ChangeServiceClient, id string) *dockwellv1.Change {
	t.Helper()
	res, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.GetChange()
}

// An agent that doesn't say which boot it's on can't be told back from a reboot, so
// a reboot isn't planned through it.
func TestAnAgentWithNoBootIsRefusedAReboot(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	p := &packages{installed: map[string]string{}}
	s := f.addHostServer(t, p)
	_, err := changeClient(b, b.origin).PlanReboot(t.Context(), connect.NewRequest(&dockwellv1.PlanRebootRequest{ServerIds: []string{s.id.ServerID}}))
	if err == nil || !strings.Contains(err.Error(), "doesn't say which boot it's on") {
		t.Fatalf("a reboot through an agent with no boot = %v", err)
	}
}

// A reboot command that fails leaves the server up: the next look, as dockwell run
// takes every RebootCheckEvery while the server is connected on the boot it left,
// fails the step at once with what the command said, keeping when it started, and
// until then a look leaves the step waiting with its start as it was.
func TestARebootThatFailsIsSaidAtTheNextLook(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	p := &packages{installed: map[string]string{}, boot: "boot-1"}
	s := f.addHostServer(t, p)
	c := changeClient(b, b.origin)
	change, _ := planReboot(t, c, s)
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	waiting := waitChange(t, c, change.GetId(), "the reboot waits", func(ch *dockwellv1.Change) bool {
		return slices.ContainsFunc(ch.GetSteps(), func(st *dockwellv1.StepProgress) bool {
			return st.GetState() == dockwellv1.StepState_STEP_STATE_WAITING
		})
	})
	started := waiting.GetSteps()[0].GetStartTime().AsTime()

	// Still on its way down: a look asks again, reboots nothing more, and waits on.
	if waits, err := f.engine.RebootsWaiting(t.Context()); err != nil || !waits {
		t.Fatalf("a reboot waiting on a connected server reads %t, %v", waits, err)
	}
	if err := f.engine.Resume(t.Context()); err != nil && !strings.Contains(err.Error(), "reconnect") {
		t.Logf("resume: %v", err)
	}
	again := waitChange(t, c, change.GetId(), "the reboot still waits", func(ch *dockwellv1.Change) bool {
		return ch.GetSteps()[0].GetState() == dockwellv1.StepState_STEP_STATE_WAITING
	})
	if got := again.GetSteps()[0].GetStartTime().AsTime(); !got.Equal(started) {
		t.Errorf("a look moved the step's start from %s to %s", started, got)
	}

	p.mu.Lock()
	p.failed = "systemctl reboot ended with status 1: Failed to reboot: Access denied"
	p.mu.Unlock()
	if err := f.engine.Resume(t.Context()); err != nil {
		t.Logf("resume: %v", err)
	}
	done := waitChange(t, c, change.GetId(), "the reboot fails", func(ch *dockwellv1.Change) bool {
		return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_FAILED || ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED
	})
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(done.GetError()+done.GetSteps()[0].GetDetail(), "Access denied") {
		t.Fatalf("after its command failed the reboot ended %v: %s / %s", done.GetState(), done.GetError(), done.GetSteps()[0].GetDetail())
	}
	if got := done.GetSteps()[0].GetStartTime().AsTime(); !got.Equal(started) {
		t.Errorf("the failed step's start moved from %s to %s", started, got)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reboots != 1 {
		t.Errorf("the server was asked to reboot %d times, want once", p.reboots)
	}
}
