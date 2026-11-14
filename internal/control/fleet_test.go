// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/system"
	"github.com/dockwell/dockwell/internal/agent/system/systemtest"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// A fleet action is a change over a set of servers, wave by wave (ADR 0094).

// fleetServer is a server a fleet action runs on: its units on a stand of its own,
// its reboots by packages, and its agent's upgrades by upgrader.
type fleetServer struct {
	*testServer
	stand    *systemtest.Stand
	packages *packages
	upgrader *fakeUpgrader
}

// addFleetServer adds a server named name whose nginx.service runs, and which
// fails to start again when fails.
func (f *fullServer) addFleetServer(t *testing.T, name string, fails bool) *fleetServer {
	t.Helper()
	stand := systemtest.New(t, systemtest.Unit{Props: systemtest.Running("nginx.service"), Fails: fails})
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.EnrollLocal(t.Context(), csr, name)
	if err != nil {
		t.Fatal(err)
	}
	r := &system.Reader{Tools: &hosttool.Runner{Root: stand.Root}, Root: stand.Root}
	p := &packages{installed: map[string]string{}, boot: "boot-1"}
	up := &fakeUpgrader{own: "0.9.3"}
	s := &fleetServer{stand: stand, packages: p, upgrader: up, testServer: &testServer{
		id:      enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		system:  r,
		drivers: resource.NewRegistry(&host.Driver{Backend: p, Units: r, Upgrader: up}),
		dataDir: t.TempDir(),
		hello: func(h *agentv1.Hello) {
			p.mu.Lock()
			h.BootId = p.boot
			p.mu.Unlock()
			h.UpgradesItself = true
			h.Build.Version = "v" + up.Own()
		},
	}}
	s.connect(t, f)
	waitForHost(t, f, s.testServer)
	return s
}

// restarts reports whether the server's nginx.service was restarted.
func (s *fleetServer) restarts() bool {
	return slices.ContainsFunc(s.stand.Calls(), func(c string) bool { return strings.Contains(c, "restart") && strings.Contains(c, "nginx.service") })
}

// fakeUpgrader stands in for the agent's upgrader: it records what it was asked,
// and the test brings its agent back at the new version.
type fakeUpgrader struct {
	mu    sync.Mutex
	own   string
	asked []resource.AgentUpgradeAsked
	fail  error
}

func (u *fakeUpgrader) Own() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.own
}

func (u *fakeUpgrader) Upgrade(_ context.Context, ask resource.AgentUpgradeAsked) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.fail != nil {
		return u.fail
	}
	sum := sha256.Sum256(ask.Script)
	if hex.EncodeToString(sum[:]) != ask.ScriptSHA256 {
		return errors.New("the script isn't the one its plan named")
	}
	if !slices.ContainsFunc(u.asked, func(a resource.AgentUpgradeAsked) bool { return a.Change == ask.Change }) {
		u.asked = append(u.asked, ask)
	}
	return nil
}

func (u *fakeUpgrader) Forget(string) {}

func (u *fakeUpgrader) upgrades() []resource.AgentUpgradeAsked {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.asked)
}

// upgraded stands in for the script having run: the agent goes, and connects again
// at version.
func (s *fleetServer) upgraded(t *testing.T, f *fullServer, version string) {
	t.Helper()
	s.stop()
	waitConnected(t, f, s.id.ServerID, false)
	s.upgrader.mu.Lock()
	s.upgrader.own = version
	s.upgrader.mu.Unlock()
	s.connect(t, f)
}

func labelServers(t *testing.T, c dockwellv1connect.ChangeServiceClient, set string, servers ...*fleetServer) {
	t.Helper()
	ids := make([]string, 0, len(servers))
	for _, s := range servers {
		ids = append(ids, s.id.ServerID)
	}
	res, err := c.PlanServerLabels(t.Context(), connect.NewRequest(&dockwellv1.PlanServerLabelsRequest{ServerIds: ids, Set: []string{set}}))
	if err != nil {
		t.Fatal(err)
	}
	if done := actedOn(t, c, res.Msg.GetChange(), ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("labelling ended %v: %s", done.GetState(), done.GetError())
	}
}

// wavesOf are a plan's waves, by server name.
func wavesOf(c *dockwellv1.Change) [][]string {
	var out [][]string
	for _, w := range c.GetPlan().GetRollout().GetWaves() {
		out = append(out, w.GetServerNames())
	}
	return out
}

func idsOf(servers ...*fleetServer) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.id.ServerID)
	}
	return out
}

// A selector picks the servers by their labels, at planning; the plan holds the
// waves, a canary first and then the batch, by name; and the steps run wave by
// wave, each server's after the one before.
func TestAFleetActionRunsOverASelectorInWaves(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	charlie := f.addFleetServer(t, "charlie", false)
	alpha := f.addFleetServer(t, "alpha", false)
	bravo := f.addFleetServer(t, "bravo", false)
	other := f.addFleetServer(t, "delta", false)
	labelServers(t, c, "pool:web", alpha, bravo, charlie)
	labelServers(t, c, "pool:db", other)

	res, err := c.PlanSystemServiceAction(t.Context(), connect.NewRequest(&dockwellv1.PlanSystemServiceActionRequest{
		Selector: "pool:web", Name: "nginx.service", Action: dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_RESTART,
		Rollout: &dockwellv1.FleetRollout{BatchSize: 2},
	}))
	if err != nil {
		t.Fatal(err)
	}
	change := res.Msg.GetChange()
	if got := wavesOf(change); !slices.EqualFunc(got, [][]string{{"alpha"}, {"bravo", "charlie"}}, slices.Equal) {
		t.Fatalf("the waves are %v", got)
	}
	if change.GetSummary() != "Restart nginx.service on alpha, bravo and charlie" {
		t.Errorf("the summary reads %q", change.GetSummary())
	}
	done := actedOn(t, c, change, "")
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the restarts ended %v: %s", done.GetState(), done.GetError())
	}
	waves := map[string]int32{}
	var order []string
	steps := slices.Clone(done.GetSteps())
	slices.SortFunc(steps, func(a, b *dockwellv1.StepProgress) int {
		return a.GetStartTime().AsTime().Compare(b.GetStartTime().AsTime())
	})
	for _, st := range steps {
		waves[st.GetServerId()] = st.GetWave()
		order = append(order, st.GetServerId())
	}
	if !slices.Equal(order, idsOf(alpha, bravo, charlie)) {
		t.Errorf("the steps ran on %v, want alpha, bravo, charlie", order)
	}
	if waves[alpha.id.ServerID] != 1 || waves[bravo.id.ServerID] != 2 || waves[charlie.id.ServerID] != 2 {
		t.Errorf("the steps' waves are %v", waves)
	}
	if !alpha.restarts() || !bravo.restarts() || !charlie.restarts() || other.restarts() {
		t.Errorf("restarted: alpha %t, bravo %t, charlie %t, delta %t", alpha.restarts(), bravo.restarts(), charlie.restarts(), other.restarts())
	}

	// A selector no server matches, and one beside IDs, are refused.
	if _, err := c.PlanReboot(t.Context(), connect.NewRequest(&dockwellv1.PlanRebootRequest{Selector: "pool:cache"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a selector nothing matches = %v", err)
	}
	if _, err := c.PlanReboot(t.Context(), connect.NewRequest(&dockwellv1.PlanRebootRequest{Selector: "pool:web", ServerIds: idsOf(alpha)})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a selector beside IDs = %v", err)
	}
}

// By default, the first step that fails stops the change there: no server after it
// is touched, and what passed stays.
func TestAFleetActionStopsAtTheFirstFailure(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	alpha := f.addFleetServer(t, "alpha", false)
	bravo := f.addFleetServer(t, "bravo", true)
	charlie := f.addFleetServer(t, "charlie", false)

	res, err := c.PlanSystemServiceAction(t.Context(), connect.NewRequest(&dockwellv1.PlanSystemServiceActionRequest{
		ServerIds: idsOf(charlie, bravo, alpha), Name: "nginx.service", Action: dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_RESTART,
		Rollout: &dockwellv1.FleetRollout{NoCanary: true, BatchSize: 1},
	}))
	if err != nil {
		t.Fatal(err)
	}
	change := res.Msg.GetChange()
	if got := wavesOf(change); !slices.EqualFunc(got, [][]string{{"alpha"}, {"bravo"}, {"charlie"}}, slices.Equal) {
		t.Fatalf("the waves are %v", got)
	}
	done := actedOn(t, c, change, "")
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED {
		t.Fatalf("the change ended %v", done.GetState())
	}
	if !alpha.restarts() || charlie.restarts() {
		t.Errorf("restarted: alpha %t, charlie %t; want alpha only past bravo's failure", alpha.restarts(), charlie.restarts())
	}
	for _, st := range done.GetSteps() {
		if st.GetServerId() == charlie.id.ServerID && st.GetState() != dockwellv1.StepState_STEP_STATE_SKIPPED {
			t.Errorf("charlie's step is %v, want skipped", st.GetState())
		}
	}
}

// With keep_going, a failed step is recorded and the next server goes on; the
// change still fails, naming each server that did.
func TestAFleetActionThatKeepsGoingFailsAtTheEnd(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	alpha := f.addFleetServer(t, "alpha", true)
	bravo := f.addFleetServer(t, "bravo", false)
	charlie := f.addFleetServer(t, "charlie", true)

	res, err := c.PlanSystemServiceAction(t.Context(), connect.NewRequest(&dockwellv1.PlanSystemServiceActionRequest{
		ServerIds: idsOf(alpha, bravo, charlie), Name: "nginx.service", Action: dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_RESTART,
		Rollout: &dockwellv1.FleetRollout{KeepGoing: true},
	}))
	if err != nil {
		t.Fatal(err)
	}
	change := res.Msg.GetChange()
	if !change.GetPlan().GetRollout().GetKeepGoing() {
		t.Errorf("the plan's rollout reads %v", change.GetPlan().GetRollout())
	}
	done := actedOn(t, c, change, "")
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED {
		t.Fatalf("the change ended %v", done.GetState())
	}
	if !bravo.restarts() {
		t.Error("bravo wasn't restarted past alpha's failure")
	}
	if e := done.GetError(); !strings.Contains(e, "2 servers failed: alpha and charlie") {
		t.Errorf("the change's error reads %q", e)
	}
}

// A pause holds the next wave until it has passed, from when the last ended.
func TestAFleetActionPausesBetweenWaves(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	alpha := f.addFleetServer(t, "alpha", false)
	bravo := f.addFleetServer(t, "bravo", false)
	pause := 1500 * time.Millisecond

	res, err := c.PlanSystemServiceAction(t.Context(), connect.NewRequest(&dockwellv1.PlanSystemServiceActionRequest{
		ServerIds: idsOf(alpha, bravo), Name: "nginx.service", Action: dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_RESTART,
		Rollout: &dockwellv1.FleetRollout{Pause: durationpb.New(pause)},
	}))
	if err != nil {
		t.Fatal(err)
	}
	done := actedOn(t, c, res.Msg.GetChange(), "")
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the change ended %v: %s", done.GetState(), done.GetError())
	}
	var first, second *dockwellv1.StepProgress
	for _, st := range done.GetSteps() {
		switch st.GetServerId() {
		case alpha.id.ServerID:
			first = st
		case bravo.id.ServerID:
			second = st
		}
	}
	if gap := second.GetStartTime().AsTime().Sub(first.GetFinishTime().AsTime()); gap < pause-50*time.Millisecond {
		t.Errorf("the second wave started %s after the first ended, want the pause of %s", gap, pause)
	}

	// A pause past a day is refused.
	if _, err := c.PlanReboot(t.Context(), connect.NewRequest(&dockwellv1.PlanRebootRequest{
		ServerIds: idsOf(alpha, bravo), Rollout: &dockwellv1.FleetRollout{Pause: durationpb.New(25 * time.Hour)},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a pause of 25 hours = %v", err)
	}
}

// A server that's offline at planning isn't in the change, and the plan says so; a
// server that needs nothing takes no step and is in no wave.
func TestAFleetActionLeavesOutWhatsOfflineOrNeedsNothing(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	alpha := f.addFleetServer(t, "alpha", false)
	bravo := f.addFleetServer(t, "bravo", false)
	charlie := f.addFleetServer(t, "charlie", false)
	delta := f.addFleetServer(t, "delta", false)
	delta.stop()
	waitConnected(t, f, delta.id.ServerID, false)
	// charlie's nginx.service is stopped already.
	stopped, _, err := planUnit(t, c, charlie.testServer, "nginx.service", dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_STOP)
	if err != nil {
		t.Fatal(err)
	}
	if done := actedOn(t, c, stopped, ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("stopping charlie's nginx ended %v: %s", done.GetState(), done.GetError())
	}

	res, err := c.PlanSystemServiceAction(t.Context(), connect.NewRequest(&dockwellv1.PlanSystemServiceActionRequest{
		ServerIds: idsOf(alpha, bravo, charlie, delta), Name: "nginx.service", Action: dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_STOP,
	}))
	if err != nil {
		t.Fatal(err)
	}
	change := res.Msg.GetChange()
	if got := wavesOf(change); !slices.EqualFunc(got, [][]string{{"alpha"}, {"bravo"}}, slices.Equal) {
		t.Errorf("the waves are %v, want alpha then bravo", got)
	}
	if n := noteOf(change, "servers_offline"); n == nil || !slices.Equal(n.GetNames(), []string{"delta"}) || !strings.Contains(n.GetMessage(), "delta isn't connected") {
		t.Errorf("the offline note is %v", n)
	}
	if n := noteOf(change, "servers_unchanged"); n == nil || !slices.Equal(n.GetNames(), []string{"charlie"}) {
		t.Errorf("the note of what needs nothing is %v", n)
	}

	// A set of one has no waves.
	one, _, err := planUnit(t, c, alpha.testServer, "nginx.service", dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_RESTART)
	if err != nil {
		t.Fatal(err)
	}
	if one.GetPlan().GetRollout() != nil {
		t.Errorf("a set of one has the rollout %v", one.GetPlan().GetRollout())
	}
}

// The next wave waits for a server of the one before that's still coming back, as
// from a reboot: the change goes on as it reconnects, and no step runs twice.
func TestAWaveWaitsForAServerComingBack(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	alpha := f.addFleetServer(t, "alpha", false)
	bravo := f.addFleetServer(t, "bravo", false)

	res, err := c.PlanReboot(t.Context(), connect.NewRequest(&dockwellv1.PlanRebootRequest{ServerIds: idsOf(alpha, bravo)}))
	if err != nil {
		t.Fatal(err)
	}
	change := res.Msg.GetChange()
	if got := wavesOf(change); !slices.EqualFunc(got, [][]string{{"alpha"}, {"bravo"}}, slices.Equal) {
		t.Fatalf("the waves are %v", got)
	}
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	waitChange(t, c, change.GetId(), "alpha's reboot waits", func(ch *dockwellv1.Change) bool {
		return slices.ContainsFunc(ch.GetSteps(), func(st *dockwellv1.StepProgress) bool {
			return st.GetServerId() == alpha.id.ServerID && st.GetState() == dockwellv1.StepState_STEP_STATE_WAITING
		})
	})
	time.Sleep(200 * time.Millisecond)
	bravo.packages.mu.Lock()
	early := bravo.packages.reboots
	bravo.packages.mu.Unlock()
	if early != 0 {
		t.Fatal("bravo was asked to reboot while alpha was still coming back")
	}
	alpha.packages.reboot(t, f, alpha.testServer, "boot-2")
	if err := f.engine.Resume(t.Context()); err != nil && !strings.Contains(err.Error(), "reconnect") {
		t.Logf("resume after alpha: %v", err)
	}
	waitChange(t, c, change.GetId(), "bravo's reboot waits", func(ch *dockwellv1.Change) bool {
		return slices.ContainsFunc(ch.GetSteps(), func(st *dockwellv1.StepProgress) bool {
			return st.GetServerId() == bravo.id.ServerID && st.GetState() == dockwellv1.StepState_STEP_STATE_WAITING
		})
	})
	bravo.packages.reboot(t, f, bravo.testServer, "boot-2")
	if err := f.engine.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := waitChange(t, c, change.GetId(), "the reboots pass", func(ch *dockwellv1.Change) bool {
		return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED || ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_FAILED
	})
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the reboots ended %v: %s", done.GetState(), done.GetError())
	}
	for _, s := range []*fleetServer{alpha, bravo} {
		s.packages.mu.Lock()
		n := s.packages.reboots
		s.packages.mu.Unlock()
		if n != 1 {
			t.Errorf("%s was asked to reboot %d times, want once", s.id.ServerName, n)
		}
	}
}

// Upgrading agents runs the control plane's own install script, named in the plan
// by its SHA-256, on each server but the control server; it takes a passkey, the
// step passes once the agent is back at the version, and it can't be undone.
func TestAnAgentUpgradeRunsTheControlPlanesScript(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	control := f.addFleetServer(t, "control", false)
	alpha := f.addFleetServer(t, "alpha", false)
	bravo := f.addFleetServer(t, "bravo", false)
	current := f.addFleetServer(t, "current", false)
	current.upgraded(t, f, "0.9.4")
	f.engine.SetControlServer(control.id.ServerID)

	// A control plane that isn't a release has none to upgrade to.
	if _, err := c.PlanAgentUpgrade(t.Context(), connect.NewRequest(&dockwellv1.PlanAgentUpgradeRequest{ServerIds: idsOf(alpha)})); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "development build") {
		t.Fatalf("an upgrade from a development build = %v", err)
	}
	script := []byte("#!/bin/sh\necho installing\n")
	f.engine.SetAgentRelease(revision.AgentRelease{Version: "v0.9.4", Script: script, Mirror: "https://mirror.example.com/dockwell/releases"})
	sum := sha256.Sum256(script)

	// The control server alone is refused; with others, it's left out, saying so.
	if _, err := c.PlanAgentUpgrade(t.Context(), connect.NewRequest(&dockwellv1.PlanAgentUpgradeRequest{ServerIds: idsOf(control)})); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "control server") {
		t.Errorf("upgrading the control server's agent = %v", err)
	}
	// An agent at the version already has nothing to upgrade.
	if _, err := c.PlanAgentUpgrade(t.Context(), connect.NewRequest(&dockwellv1.PlanAgentUpgradeRequest{ServerIds: idsOf(current)})); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "runs 0.9.4 already") {
		t.Errorf("upgrading an agent at the version = %v", err)
	}

	res, err := c.PlanAgentUpgrade(t.Context(), connect.NewRequest(&dockwellv1.PlanAgentUpgradeRequest{ServerIds: idsOf(control, alpha, bravo, current)}))
	if err != nil {
		t.Fatal(err)
	}
	change := res.Msg.GetChange()
	if change.GetSummary() != "Upgrade the agents on alpha, bravo and current to 0.9.4" {
		t.Errorf("the summary reads %q", change.GetSummary())
	}
	if n := noteOf(change, "control_server"); n == nil || !slices.Equal(n.GetNames(), []string{"control"}) {
		t.Errorf("the control server's note is %v", n)
	}
	if n := noteOf(change, "servers_unchanged"); n == nil || !slices.Equal(n.GetNames(), []string{"current"}) {
		t.Errorf("the note of what needs nothing is %v", n)
	}
	if got := wavesOf(change); !slices.EqualFunc(got, [][]string{{"alpha"}, {"bravo"}}, slices.Equal) {
		t.Errorf("the waves are %v", got)
	}
	var upgrades int
	for _, r := range change.GetPlan().GetResources() {
		if u := r.GetAgentUpgrade(); u != nil {
			upgrades++
			if u.GetFrom() != "0.9.3" || u.GetTo() != "0.9.4" || u.GetScriptSha256() != hex.EncodeToString(sum[:]) {
				t.Errorf("an upgrade plans %v", u)
			}
		}
	}
	if upgrades != 2 || !change.GetPlan().GetHostAccess() {
		t.Errorf("the plan upgrades %d agents, host access %t", upgrades, change.GetPlan().GetHostAccess())
	}
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint()})); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "passkey") {
		t.Fatalf("an upgrade applied without a passkey = %v", err)
	}
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	waitChange(t, c, change.GetId(), "alpha's upgrade waits", func(ch *dockwellv1.Change) bool {
		return slices.ContainsFunc(ch.GetSteps(), func(st *dockwellv1.StepProgress) bool {
			return st.GetServerId() == alpha.id.ServerID && st.GetState() == dockwellv1.StepState_STEP_STATE_WAITING && strings.Contains(st.GetMessage(), "is upgrading")
		})
	})
	asked := alpha.upgrader.upgrades()
	if len(asked) != 1 || !bytes.Equal(asked[0].Script, script) || asked[0].Version != "0.9.4" || asked[0].Mirror != "https://mirror.example.com/dockwell/releases" || asked[0].Change != change.GetId() {
		t.Fatalf("alpha's agent was asked %v", asked)
	}
	if len(bravo.upgrader.upgrades()) != 0 {
		t.Fatal("bravo was upgraded while alpha's agent was still coming back")
	}
	// Its agent goes as the script restarts it, and comes back at the version: a
	// resume that planned it while it was still up waits for it, rather than fail.
	alpha.upgraded(t, f, "0.9.4")
	if err := f.engine.Resume(t.Context()); err != nil && !strings.Contains(err.Error(), "reconnect") {
		t.Logf("resume after alpha: %v", err)
	}
	waitChange(t, c, change.GetId(), "bravo's upgrade waits", func(ch *dockwellv1.Change) bool {
		return slices.ContainsFunc(ch.GetSteps(), func(st *dockwellv1.StepProgress) bool {
			return st.GetServerId() == bravo.id.ServerID && st.GetState() == dockwellv1.StepState_STEP_STATE_WAITING
		})
	})
	bravo.upgraded(t, f, "0.9.4")
	if err := f.engine.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := waitChange(t, c, change.GetId(), "the upgrades pass", func(ch *dockwellv1.Change) bool {
		return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED || ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_FAILED
	})
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the upgrades ended %v: %s", done.GetState(), done.GetError())
	}
	for _, st := range done.GetSteps() {
		if !strings.Contains(st.GetMessage(), "runs 0.9.4 now") {
			t.Errorf("a step says %q", st.GetMessage())
		}
	}
	if len(control.upgrader.upgrades()) != 0 || len(current.upgrader.upgrades()) != 0 {
		t.Error("the control server's or the current agent was asked to upgrade")
	}
	if _, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: change.GetId()})); err == nil || !strings.Contains(err.Error(), "can't be undone") {
		t.Errorf("undoing an upgrade = %v", err)
	}
}

// An agent whose script ended with it at its old version fails the step with what
// the script said, and an agent too old to upgrade itself isn't asked.
func TestAnAgentUpgradeThatFailsSaysWhy(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	alpha := f.addFleetServer(t, "alpha", false)
	f.engine.SetAgentRelease(revision.AgentRelease{Version: "0.9.4", Script: []byte("#!/bin/sh\n")})
	res, err := c.PlanAgentUpgrade(t.Context(), connect.NewRequest(&dockwellv1.PlanAgentUpgradeRequest{ServerIds: idsOf(alpha)}))
	if err != nil {
		t.Fatal(err)
	}
	change := res.Msg.GetChange()
	alpha.upgrader.mu.Lock()
	alpha.upgrader.fail = errors.New("the install script ended with status 1, and the agent still runs 0.9.3: Error: the release's signature doesn't check out")
	alpha.upgrader.mu.Unlock()
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	done := actedOn(t, c, change, proof)
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(done.GetError()+done.GetSteps()[0].GetDetail(), "signature doesn't check out") {
		t.Errorf("the upgrade ended %v: %s / %s", done.GetState(), done.GetError(), done.GetSteps()[0].GetDetail())
	}

	older := f.addFleetServer(t, "older", false)
	older.stop()
	waitConnected(t, f, older.id.ServerID, false)
	older.hello = func(h *agentv1.Hello) { h.UpgradesItself = false }
	older.connect(t, f)
	if _, err := c.PlanAgentUpgrade(t.Context(), connect.NewRequest(&dockwellv1.PlanAgentUpgradeRequest{ServerIds: idsOf(older)})); err == nil || !strings.Contains(err.Error(), "older than fleet upgrades") {
		t.Errorf("upgrading an agent that can't = %v", err)
	}
	// Beside one that can, it's left out, saying so.
	newer := f.addFleetServer(t, "newer", false)
	res, err = c.PlanAgentUpgrade(t.Context(), connect.NewRequest(&dockwellv1.PlanAgentUpgradeRequest{ServerIds: idsOf(older, newer)}))
	if err != nil {
		t.Fatal(err)
	}
	if n := noteOf(res.Msg.GetChange(), "older_agent"); n == nil || !slices.Equal(n.GetNames(), []string{"older"}) {
		t.Errorf("the older agent's note is %v", n)
	}
}
