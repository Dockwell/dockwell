// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/system"
	"github.com/dockwell/dockwell/internal/agent/system/systemtest"
	"github.com/dockwell/dockwell/internal/control/servers"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/store"
)

// addUnitServer adds web-1, a systemd server whose host acts on the stand's units
// with the agent's own reader (ADR 0082).
func (f *fullServer) addUnitServer(t *testing.T, stand *systemtest.Stand, hello func(*agentv1.Hello)) *testServer {
	t.Helper()
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.EnrollLocal(t.Context(), csr, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	r := &system.Reader{Tools: &hosttool.Runner{Root: stand.Root}, Root: stand.Root}
	s := &testServer{
		id:      enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		system:  r,
		drivers: resource.NewRegistry(&host.Driver{Units: r}),
		dataDir: t.TempDir(),
		hello:   hello,
	}
	s.connect(t, f)
	waitForHost(t, f, s)
	return s
}

// waitForHost waits for the control plane to make s's host, as it does once the
// agent connects: a plan made before would read as out of date.
func waitForHost(t *testing.T, f *fullServer, s *testServer) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var made bool
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			hosts, err := tx.ListKind(t.Context(), string(host.Kind))
			made = slices.ContainsFunc(hosts, func(r store.Resource) bool { return r.Scope == s.id.ServerID })
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if made {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the server's host wasn't made")
		}
	}
}

func planUnit(t *testing.T, c dockwellv1connect.ChangeServiceClient, s *testServer, name string, action dockwellv1.SystemServiceAction) (*dockwellv1.Change, *dockwellv1.HostUnit, error) {
	t.Helper()
	res, err := c.PlanSystemServiceAction(t.Context(), connect.NewRequest(&dockwellv1.PlanSystemServiceActionRequest{ServerIds: []string{s.id.ServerID}, Name: name, Action: action}))
	if err != nil {
		return nil, nil, err
	}
	change := res.Msg.GetChange()
	for _, r := range change.GetPlan().GetResources() {
		if r.GetUnit() != nil {
			return change, r.GetUnit(), nil
		}
	}
	t.Fatalf("the plan acts on no unit: %v", change.GetPlan())
	return nil, nil, nil
}

func actedOn(t *testing.T, c dockwellv1connect.ChangeServiceClient, change *dockwellv1.Change, proof string) *dockwellv1.Change {
	t.Helper()
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	return waitChange(t, c, change.GetId(), "the action ends", func(ch *dockwellv1.Change) bool {
		return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED || ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_FAILED
	})
}

// Acting on a unit is a change with a plan and no revision (ADR 0082): restarting
// one says what goes with it and applies without a passkey; it can't be undone;
// disabling one is undone by enabling it again; what stands as asked is refused
// with why; and a token can't plan one.
func TestActingOnAUnitIsAChangeWithAPlan(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	stand := systemtest.New(t,
		systemtest.Unit{Props: systemtest.Running("php8.3-fpm.service", "RequiredBy", "nginx.service")},
		systemtest.Unit{Props: systemtest.Running("nginx.service")},
	)
	s := f.addUnitServer(t, stand, nil)
	c := changeClient(b, b.origin)

	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := dockwellv1connect.NewChangeServiceClient(bearer{token: token, rt: b.client.Transport}, b.origin, connect.WithProtoJSON())
	if _, err := script.PlanSystemServiceAction(t.Context(), connect.NewRequest(&dockwellv1.PlanSystemServiceActionRequest{
		ServerIds: []string{s.id.ServerID}, Name: "nginx.service", Action: dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_RESTART,
	})); connect.CodeOf(err) != connect.CodePermissionDenied && connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("a token planning a restart = %v", err)
	}

	change, unit, err := planUnit(t, c, s, "php8.3-fpm.service", dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_RESTART)
	if err != nil {
		t.Fatal(err)
	}
	if change.GetSummary() != "Restart php8.3-fpm.service on web-1" || !slices.Equal(unit.GetWith(), []string{"nginx.service"}) || unit.GetConfirm() || !unit.GetWasRunning() {
		t.Errorf("the plan reads %q, %v", change.GetSummary(), unit)
	}
	done := actedOn(t, c, change, "")
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED || !strings.Contains(done.GetSteps()[0].GetMessage(), "Restarted php8.3-fpm.service on web-1") {
		t.Fatalf("the restart ended %v: %s, %q", done.GetState(), done.GetError(), done.GetSteps()[0].GetMessage())
	}
	if _, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: change.GetId()})); err == nil || !strings.Contains(err.Error(), "can't be undone") {
		t.Errorf("undoing a restart = %v", err)
	}

	// Disabled at boot, then enabled again by undoing it.
	change, _, err = planUnit(t, c, s, "nginx.service", dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_DISABLE)
	if err != nil {
		t.Fatal(err)
	}
	if change.GetSummary() != "Stop nginx.service starting at boot on web-1" {
		t.Errorf("the disable reads %q", change.GetSummary())
	}
	if done := actedOn(t, c, change, ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the disable ended %v: %s", done.GetState(), done.GetError())
	}
	if got := stand.Prop(t, "nginx.service", "UnitFileState"); got != "disabled" {
		t.Fatalf("nginx.service reads %s at boot", got)
	}
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: change.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	back := undo.Msg.GetChange()
	if !strings.HasPrefix(back.GetSummary(), "Undo: ") {
		t.Errorf("the undo reads %q", back.GetSummary())
	}
	if done := actedOn(t, c, back, ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the undo ended %v: %s", done.GetState(), done.GetError())
	}
	if got := stand.Prop(t, "nginx.service", "UnitFileState"); got != "enabled" {
		t.Errorf("after the undo nginx.service reads %s at boot", got)
	}

	// Starting what runs changes nothing, so nothing is planned.
	_, _, err = planUnit(t, c, s, "nginx.service", dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_START)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "nginx.service already runs on web-1") {
		t.Errorf("starting what runs = %v", err)
	}
	list, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range list.Msg.GetChanges() {
		if ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
			t.Errorf("%s is left planned: %q", ch.GetId(), ch.GetSummary())
		}
	}
}

// What Dockwell needs is guarded (ADR 0082): stopping Docker or SSH is refused by
// the name asked, and by an alias on the server; restarting SSH takes a passkey;
// restarting Docker the typed code too.
func TestWhatDockwellNeedsIsGuarded(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	stand := systemtest.New(t,
		systemtest.Unit{Props: systemtest.Running("ssh.service", "Names", "ssh.service sshd.service")},
		systemtest.Unit{Props: systemtest.Running("docker.service")},
		systemtest.Unit{Props: systemtest.Running("ssh-alias.service", "Names", "ssh-alias.service")},
	)
	s := f.addUnitServer(t, stand, nil)
	c := changeClient(b, b.origin)

	_, _, err := planUnit(t, c, s, "docker.service", dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_STOP)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "every app on web-1 runs in Docker") {
		t.Errorf("stopping Docker = %v", err)
	}
	_, _, err = planUnit(t, c, s, "sshd.service", dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_MASK)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "how you reach web-1 over SSH") {
		t.Errorf("masking SSH = %v", err)
	}

	change, unit, err := planUnit(t, c, s, "sshd.service", dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_RESTART)
	if err != nil {
		t.Fatal(err)
	}
	if unit.GetName() != "ssh.service" || unit.GetGuard() != dockwellv1.SystemServiceGuard_SYSTEM_SERVICE_GUARD_SSH || unit.GetConfirm() {
		t.Errorf("restarting SSH reads %v", unit)
	}
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint()})); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "passkey") {
		t.Fatalf("restarting SSH without a passkey = %v", err)
	}
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	if done := actedOn(t, c, change, proof); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("restarting SSH ended %v: %s", done.GetState(), done.GetError())
	}
	if !slices.ContainsFunc(stand.Calls(), func(c string) bool { return c == "sshd -t" }) {
		t.Errorf("SSH restarted without checking its config: %q", stand.Calls())
	}

	_, docker, err := planUnit(t, c, s, "docker.service", dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_RESTART)
	if err != nil {
		t.Fatal(err)
	}
	if !docker.GetConfirm() || !docker.GetDockerRestarts() {
		t.Errorf("restarting Docker reads %v", docker)
	}
}

// An agent older than the control plane would ignore the action, so none is planned
// through it.
func TestAnOlderAgentIsntAskedToActOnAUnit(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	stand := systemtest.New(t, systemtest.Unit{Props: systemtest.Running("nginx.service")})
	s := f.addUnitServer(t, stand, func(h *agentv1.Hello) { h.ActsOnUnits = false })
	_, _, err := planUnit(t, changeClient(b, b.origin), s, "nginx.service", dockwellv1.SystemServiceAction_SYSTEM_SERVICE_ACTION_RESTART)
	if err == nil || !strings.Contains(err.Error(), "older than the control plane") {
		t.Errorf("planning through an older agent = %v", err)
	}
}

// A unit's journal is read a page at a time, and reading it goes on record once an
// hour for a reader and a server; a failed unit's row says why, read once.
func TestTheSystemLogIsReadOnRecordAndAFailureSaysWhy(t *testing.T) {
	was := servers.SystemEvery
	servers.SystemEvery = 100 * time.Millisecond
	t.Cleanup(func() { servers.SystemEvery = was })
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	stand := systemtest.New(t, systemtest.Unit{
		Props: systemtest.Running("nginx.service", "ActiveState", "failed", "SubState", "failed", "Result", "exit-code"),
		Journal: []string{
			systemtest.Entry("c2", "init.scope", "1", "nginx.service: Failed with result 'exit-code'."),
			systemtest.Entry("c1", "nginx.service", "4242", "nginx: [emerg] bind() to 0.0.0.0:80 failed"),
		},
	})
	s := f.addUnitServer(t, stand, nil)
	sc := dockwellv1connect.NewServerServiceClient(b, b.origin, connect.WithProtoJSON())

	for range 2 {
		res, err := sc.ReadSystemLog(t.Context(), connect.NewRequest(&dockwellv1.ReadSystemLogRequest{ServerId: s.id.ServerID, Unit: "nginx.service"}))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Msg.GetLines()) != 2 || res.Msg.GetSource() != dockwellv1.SystemLogSource_SYSTEM_LOG_SOURCE_JOURNAL {
			t.Fatalf("the page reads %v", res.Msg)
		}
	}
	reads := countLogEvents(t, f, "the system log on web-1")
	if reads != 1 {
		t.Errorf("reading the system log twice went on record %d times, want once", reads)
	}

	w, err := sc.WatchSystemServices(t.Context(), connect.NewRequest(&dockwellv1.WatchSystemServicesRequest{ServerId: s.id.ServerID}))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := range 3 {
		if !w.Receive() {
			t.Fatal(w.Err())
		}
		list := w.Msg().GetList()
		if !list.GetActs() {
			t.Error("the list says the agent doesn't act on units")
		}
		for _, u := range list.GetServices() {
			if u.GetName() == "nginx.service" && !slices.Equal(u.GetWhy(), []string{"nginx: [emerg] bind() to 0.0.0.0:80 failed"}) {
				t.Errorf("read %d: nginx.service's why reads %q", i, u.GetWhy())
			}
		}
	}
	journals := 0
	for _, c := range stand.Calls() {
		if strings.HasPrefix(c, "journalctl") {
			journals++
		}
	}
	if journals != 3 {
		t.Errorf("the journal was read %d times: twice for the page and once for why, want 3", journals)
	}
}
