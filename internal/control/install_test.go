// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	agentupdates "github.com/dockwell/dockwell/internal/agent/updates"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/store"
)

// packages is a server's packages, which a fake host installs as the agent does:
// in a unit of its own that outlives whoever started it, followed until it ends.
type packages struct {
	mu        sync.Mutex
	waiting   *resource.Install
	installed map[string]string
	// installs counts the installs started, follows each call that started or
	// followed one.
	installs, follows int
	// hold, when set, keeps an install going until it's closed.
	hold chan struct{}
	// ended is closed as the running install's unit ends; nil while none runs.
	ended chan struct{}
	// needs says what each package pulls in with it, and older plans past the
	// names asked, as an agent from before they could be picked does.
	needs map[string][]string
	older bool
	// asked are the names each plan asked for.
	asked [][]string
	// boot is the boot the server is on, marked the change and boot a reboot was
	// asked on, and reboots how many it was asked for.
	boot           string
	marked, onBoot string
	reboots        int
	// failed is what a reboot command that failed said, while the server stays up.
	failed string
}

func (p *packages) Rebooted(_ context.Context, change string) (host.RebootState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.marked != change:
		return host.RebootNone, nil
	case p.onBoot != p.boot:
		return host.RebootDone, nil
	case p.failed != "":
		return host.RebootFailed, nil
	}
	return host.RebootAsked, nil
}

func (p *packages) Reboot(_ context.Context, change string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.marked == change {
		if p.onBoot == p.boot && p.failed != "" {
			said := p.failed
			p.marked, p.failed = "", ""
			return errors.New("the reboot command failed: " + said)
		}
		return nil
	}
	p.marked, p.onBoot = change, p.boot
	p.reboots++
	return nil
}

// reboot stands in for the server coming back on another boot: its agent goes,
// and connects again saying the new boot.
func (p *packages) reboot(t *testing.T, f *fullServer, s *testServer, boot string) {
	t.Helper()
	s.stop()
	p.mu.Lock()
	p.boot = boot
	p.mu.Unlock()
	s.connect(t, f)
}

func (p *packages) Plan(_ context.Context, _ bool, names []string) (*resource.Install, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, names)
	if p.waiting == nil {
		return &resource.Install{}, nil
	}
	in := *p.waiting
	in.Packages = slices.Clone(p.waiting.Packages)
	if len(names) == 0 || p.older {
		return &in, nil
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
		for _, d := range p.needs[n] {
			want[d] = true
		}
	}
	in.Packages = slices.DeleteFunc(in.Packages, func(pk resource.InstallPackage) bool { return !want[pk.Name] })
	in.Asked = names
	return &in, nil
}

func (p *packages) Left(_ context.Context, _ string, planned []resource.InstallPackage) ([]resource.InstallPackage, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var left []resource.InstallPackage
	for _, pk := range planned {
		if p.installed[pk.Name] != pk.To {
			left = append(left, pk)
		}
	}
	return left, p.ended != nil, nil
}

func (p *packages) Install(ctx context.Context, _ string, planned []resource.InstallPackage, say func(string)) error {
	p.mu.Lock()
	p.follows++
	if p.ended == nil {
		done := true
		for _, pk := range planned {
			done = done && p.installed[pk.Name] == pk.To
		}
		if done {
			// What a run that ended unobserved installed reads done, as on the agent.
			p.mu.Unlock()
			return nil
		}
		p.installs++
		ended, hold := make(chan struct{}), p.hold
		p.ended = ended
		go func() {
			if hold != nil {
				<-hold
			}
			p.mu.Lock()
			for _, pk := range planned {
				p.installed[pk.Name] = pk.To
			}
			p.waiting, p.ended = nil, nil
			p.mu.Unlock()
			close(ended)
		}()
	}
	ended := p.ended
	p.mu.Unlock()
	for _, pk := range planned {
		say("Setting up " + pk.Name + " (" + pk.To + ")")
	}
	select {
	case <-ended:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// addHostServer adds web-1, whose host installs from packages.
func (f *fullServer) addHostServer(t *testing.T, p *packages) *testServer {
	t.Helper()
	return f.addHostServerReading(t, p, "")
}

// addHostServerReading adds web-1, whose host installs from packages, and whose
// agent reads its updates from the stand at root, unless root is empty.
func (f *fullServer) addHostServerReading(t *testing.T, p *packages, root string) *testServer {
	t.Helper()
	return f.addNamedHostServer(t, "web-1", p, root)
}

// addNamedHostServer is addHostServerReading for a server with another name.
func (f *fullServer) addNamedHostServer(t *testing.T, name string, p *packages, root string) *testServer {
	t.Helper()
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.EnrollLocal(t.Context(), csr, name)
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{
		id:      enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		drivers: resource.NewRegistry(&host.Driver{Backend: p}),
		dataDir: t.TempDir(),
		// Its agent says the boot the packages say it's on, so a test can reboot it.
		hello: func(h *agentv1.Hello) {
			p.mu.Lock()
			defer p.mu.Unlock()
			h.BootId = p.boot
		},
	}
	if root != "" {
		s.updates = agentupdates.New(agentupdates.Options{Root: root})
	}
	s.connect(t, f)
	// The control plane makes the server's host as its agent connects: a plan
	// made before that lands would read as out of date.
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
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server's host wasn't made")
		}
	}
	return s
}

// Installing updates is a change with a plan and no revision (ADR 0064): the
// agent's simulation, pulled-in packages included, in a plan that's as good as
// root, which a person applies with a passkey and a token can't; its lines are
// its progress; and it can't be undone.
func TestInstallingUpdatesIsAPlanAPersonAppliesWithAPasskey(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	p := &packages{
		installed: map[string]string{"openssl": "3.0.13-0ubuntu3.4"},
		waiting: &resource.Install{Packages: []resource.InstallPackage{
			{Name: "openssl", From: "3.0.13-0ubuntu3.4", To: "3.0.13-0ubuntu3.5", Security: true},
			{Name: "libssl-extra", To: "3.0.13-0ubuntu3.5", Security: true},
		}},
	}
	p.hold = make(chan struct{})
	s := f.addHostServer(t, p)
	c := changeClient(b, b.origin)

	// No API token plans or applies it: a person does, signed in.
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := dockwellv1connect.NewChangeServiceClient(bearer{token: token, rt: b.client.Transport}, b.origin, connect.WithProtoJSON())
	if _, err := script.PlanInstallUpdates(t.Context(), connect.NewRequest(&dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}})); connect.CodeOf(err) != connect.CodePermissionDenied && connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("a token planning an install = %v", err)
	}

	res, err := c.PlanInstallUpdates(t.Context(), connect.NewRequest(&dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}}))
	if err != nil {
		t.Fatal(err)
	}
	change := res.Msg.GetChange()
	if change.GetSummary() != "Install security updates on web-1" || !change.GetPlan().GetHostAccess() {
		t.Errorf("the plan reads %q, host access %t", change.GetSummary(), change.GetPlan().GetHostAccess())
	}
	var install *dockwellv1.PackageInstall
	for _, r := range change.GetPlan().GetResources() {
		if r.GetResource().GetKind() == string(host.Kind) {
			install = r.GetInstall()
		}
	}
	if len(install.GetPackages()) != 2 || install.GetPackages()[1].GetName() != "libssl-extra" || install.GetPackages()[1].GetInstalled() != "" {
		t.Fatalf("the plan installs %v, want openssl and what it pulls in", install)
	}

	// A token can't apply a person's plan: it takes a passkey.
	if _, err := script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint()})); err == nil {
		t.Fatal("a token applied an install")
	}
	// Nor a person without one.
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint()})); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "passkey") {
		t.Fatalf("an install applied without a passkey = %v", err)
	}
	if p.installs != 0 {
		t.Fatal("something was installed before the plan was applied")
	}

	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	// What the install prints is its step's progress, as it comes.
	waitChange(t, c, change.GetId(), "the install says what it's doing", func(ch *dockwellv1.Change) bool {
		return slices.ContainsFunc(ch.GetSteps(), func(st *dockwellv1.StepProgress) bool { return strings.HasPrefix(st.GetActivity(), "Setting up ") })
	})
	close(p.hold)
	done := waitChange(t, c, change.GetId(), "the install applies", func(ch *dockwellv1.Change) bool {
		return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED || ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_FAILED
	})
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", done.GetState(), done.GetError())
	}
	p.mu.Lock()
	installs, installed := p.installs, p.installed["libssl-extra"]
	p.mu.Unlock()
	if installs != 1 || installed != "3.0.13-0ubuntu3.5" {
		t.Errorf("installed %d times, libssl-extra at %q", installs, installed)
	}

	// An upgrade stays installed: undo says so.
	if _, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: change.GetId()})); err == nil || !strings.Contains(err.Error(), "can't be undone") {
		t.Errorf("undoing an install = %v", err)
	}

	// With nothing left waiting, nothing is planned, and no change is left behind.
	before := len(listChanges(t, c))
	if _, err := c.PlanInstallUpdates(t.Context(), connect.NewRequest(&dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}})); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "nothing to install") {
		t.Errorf("planning with nothing waiting = %v", err)
	}
	if after := listChanges(t, c); len(after) != before {
		for _, ch := range after {
			if ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
				t.Errorf("a plan with nothing to install was left: %v", ch.GetSummary())
			}
		}
	}
}

// A plan that needs a reboot after, or restarts Docker, applies: the apply plans
// again, and what it needs after reads as the plan said, so the fingerprint holds.
func TestAnInstallThatRebootsOrRestartsDockerApplies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		waiting *resource.Install
	}{
		{"a reboot", &resource.Install{Reboot: true, Packages: []resource.InstallPackage{
			{Name: "linux-image-6.8.0-52-generic", To: "6.8.0-52.53", Security: true, Reboot: true},
			{Name: "openssl", From: "3.0.13-0ubuntu3.4", To: "3.0.13-0ubuntu3.5", Security: true},
		}}},
		{"Docker restarting", &resource.Install{DockerRestarts: true, KeepsRunning: true, Packages: []resource.InstallPackage{
			{Name: "docker-ce", From: "5:27.4.0-1~ubuntu.24.04~noble", To: "5:27.4.1-1~ubuntu.24.04~noble", Security: true, Docker: true},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startFull(t, store.SQLite, nil)
			b := signedIn(t, f)
			p := &packages{installed: map[string]string{}, waiting: tc.waiting}
			s := f.addHostServer(t, p)
			c := changeClient(b, b.origin)
			res, err := c.PlanInstallUpdates(t.Context(), connect.NewRequest(&dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}}))
			if err != nil {
				t.Fatal(err)
			}
			change := res.Msg.GetChange()
			proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
			if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof})); err != nil {
				t.Fatalf("applying a plan with %s: %v", tc.name, err)
			}
			done := waitChange(t, c, change.GetId(), "the install applies", func(ch *dockwellv1.Change) bool {
				return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED || ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_FAILED
			})
			if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
				t.Fatalf("the install with %s ended %v: %s", tc.name, done.GetState(), done.GetError())
			}
		})
	}
}

// An install that takes longer than any step's time goes on to the end and
// applies: its step has hours of its own, so it isn't failed while it still runs.
func TestAnInstallLongerThanAnyStepsTimeApplies(t *testing.T) {
	was := agentapi.ApplyTimeout
	agentapi.ApplyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { agentapi.ApplyTimeout = was })
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	p := &packages{installed: map[string]string{}, waiting: &resource.Install{Packages: []resource.InstallPackage{
		{Name: "linux-modules-extra-6.8.0-52-generic", To: "6.8.0-52.53", Security: true, Reboot: true},
	}, Reboot: true}}
	p.hold = make(chan struct{})
	s := f.addHostServer(t, p)
	c := changeClient(b, b.origin)
	res, err := c.PlanInstallUpdates(t.Context(), connect.NewRequest(&dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}}))
	if err != nil {
		t.Fatal(err)
	}
	change := res.Msg.GetChange()
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	// Five times what any step gets, and it still runs.
	time.Sleep(5 * agentapi.ApplyTimeout)
	close(p.hold)
	done := waitChange(t, c, change.GetId(), "the install applies", func(ch *dockwellv1.Change) bool {
		return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED || ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_FAILED
	})
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("an install longer than a step's %s ended %v: %s", agentapi.ApplyTimeout, done.GetState(), done.GetError())
	}
}

// The control plane restarts while an install runs: the install goes on in its
// unit, and the change, resumed, follows it to the end, starting nothing again.
func TestAnInstallIsFollowedAgainAfterTheControlPlaneRestarts(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	p := &packages{installed: map[string]string{}, waiting: &resource.Install{Packages: []resource.InstallPackage{
		{Name: "openssl", From: "3.0.13-0ubuntu3.4", To: "3.0.13-0ubuntu3.5", Security: true},
	}}}
	p.hold = make(chan struct{})
	s := f.addHostServer(t, p)
	c := changeClient(b, b.origin)
	res, err := c.PlanInstallUpdates(t.Context(), connect.NewRequest(&dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}}))
	if err != nil {
		t.Fatal(err)
	}
	change := res.Msg.GetChange()
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	waitChange(t, c, change.GetId(), "the install runs", func(ch *dockwellv1.Change) bool {
		return slices.ContainsFunc(ch.GetSteps(), func(st *dockwellv1.StepProgress) bool { return strings.HasPrefix(st.GetActivity(), "Setting up ") })
	})

	_, letIn := f.restart(t, func(*Server) {})
	// dockwell run resumes what's left applying as each server reconnects.
	engine := f.engine
	f.agents.OnConnect(func(string) { _ = engine.Resume(context.WithoutCancel(t.Context())) })
	letIn()
	// The install ends only once the resumed change follows it.
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		p.mu.Lock()
		follows := p.follows
		p.mu.Unlock()
		if follows >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing followed the install after the restart")
		}
	}
	close(p.hold)
	done := waitChange(t, c, change.GetId(), "the resumed install ends", func(ch *dockwellv1.Change) bool {
		return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED || ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_FAILED
	})
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("after a restart, the install ended %v: %s", done.GetState(), done.GetError())
	}
	p.mu.Lock()
	installs := p.installs
	p.mu.Unlock()
	if installs != 1 {
		t.Errorf("the install started %d times, want once", installs)
	}
}

func listChanges(t *testing.T, c dockwellv1connect.ChangeServiceClient) []*dockwellv1.Change {
	t.Helper()
	res, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.GetChanges()
}

// Updating one package plans it and what it needs, and no other update waiting;
// the plan says which was asked for, and applies as any install does.
func TestUpdatingOnePackagePlansItAndWhatItNeeds(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	p := &packages{
		installed: map[string]string{"openssl": "3.0.13-0ubuntu3.4", "libssl3t64": "3.0.13-0ubuntu3.4", "curl": "8.5.0-2ubuntu10.4"},
		waiting: &resource.Install{Packages: []resource.InstallPackage{
			{Name: "curl", From: "8.5.0-2ubuntu10.4", To: "8.5.0-2ubuntu10.6"},
			{Name: "libssl3t64", From: "3.0.13-0ubuntu3.4", To: "3.0.13-0ubuntu3.5", Security: true},
			{Name: "openssl", From: "3.0.13-0ubuntu3.4", To: "3.0.13-0ubuntu3.5", Security: true},
		}},
		needs: map[string][]string{"openssl": {"libssl3t64"}},
	}
	s := f.addHostServer(t, p)
	c := changeClient(b, b.origin)

	res, err := c.PlanInstallUpdates(t.Context(), connect.NewRequest(&dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}, Packages: []string{"openssl", "openssl"}}))
	if err != nil {
		t.Fatal(err)
	}
	change := res.Msg.GetChange()
	if change.GetSummary() != "Update openssl on web-1" {
		t.Errorf("the change reads %q", change.GetSummary())
	}
	var install *dockwellv1.PackageInstall
	for _, r := range change.GetPlan().GetResources() {
		if r.GetResource().GetKind() == string(host.Kind) {
			install = r.GetInstall()
		}
	}
	var planned []string
	for _, pk := range install.GetPackages() {
		planned = append(planned, pk.GetName())
	}
	if !slices.Equal(planned, []string{"libssl3t64", "openssl"}) || !slices.Equal(install.GetAsked(), []string{"openssl"}) {
		t.Fatalf("the plan installs %v, asked %v; want openssl with libssl3t64, which it needs", planned, install.GetAsked())
	}
	p.mu.Lock()
	asked := p.asked[len(p.asked)-1]
	p.mu.Unlock()
	if !slices.Equal(asked, []string{"openssl"}) {
		t.Errorf("the agent was asked for %v, want openssl once", asked)
	}
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	done := waitChange(t, c, change.GetId(), "the update applies", func(ch *dockwellv1.Change) bool {
		return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED || ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_FAILED
	})
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the update ended %v: %s", done.GetState(), done.GetError())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.installed["openssl"] != "3.0.13-0ubuntu3.5" || p.installed["curl"] != "8.5.0-2ubuntu10.4" {
		t.Errorf("after updating openssl: openssl at %q, curl at %q; want curl left as it was", p.installed["openssl"], p.installed["curl"])
	}

	// Three or fewer are named in the change, more counted.
	for _, tc := range []struct {
		packages []string
		want     string
	}{
		{[]string{"curl", "openssl"}, "Update curl and openssl on web-1"},
		{[]string{"a", "b", "c", "d"}, "Update 4 packages on web-1"},
	} {
		p.waiting = &resource.Install{Packages: []resource.InstallPackage{
			{Name: "a", From: "1", To: "2"}, {Name: "b", From: "1", To: "2"}, {Name: "c", From: "1", To: "2"}, {Name: "curl", From: "1", To: "2"}, {Name: "d", From: "1", To: "2"}, {Name: "openssl", From: "1", To: "2"},
		}}
		p.needs = nil
		p.mu.Unlock()
		res, err := c.PlanInstallUpdates(t.Context(), connect.NewRequest(&dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}, Packages: tc.packages}))
		p.mu.Lock()
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Msg.GetChange().GetSummary(); got != tc.want {
			t.Errorf("updating %v reads %q, want %q", tc.packages, got, tc.want)
		}
	}
}

// A package list is names, and only names: one with a shell's characters, or one
// asked for beside every update, is refused before any server is asked.
func TestPackagesToUpdateAreNamesOnly(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	p := &packages{installed: map[string]string{}, waiting: &resource.Install{Packages: []resource.InstallPackage{{Name: "openssl", From: "1", To: "2"}}}}
	s := f.addHostServer(t, p)
	c := changeClient(b, b.origin)
	for _, tc := range []struct {
		name string
		req  *dockwellv1.PlanInstallUpdatesRequest
	}{
		{"a shell's characters", &dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}, Packages: []string{"openssl; rm -rf /"}}},
		{"a flag", &dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}, Packages: []string{"--allow-downgrades"}}},
		{"nothing", &dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}, Packages: []string{" "}}},
		{"beside every update", &dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}, Packages: []string{"openssl"}, All: true}},
	} {
		if _, err := c.PlanInstallUpdates(t.Context(), connect.NewRequest(tc.req)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("packages with %s = %v, want refused as invalid", tc.name, err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.asked) != 0 {
		t.Errorf("the agent was asked to plan %v", p.asked)
	}
}

// An agent from before packages could be picked reads past the names and plans
// the security fixes: its plan is refused, so nobody applies more than they picked.
func TestAnOlderAgentIsRefusedPickedPackages(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	p := &packages{
		installed: map[string]string{},
		waiting: &resource.Install{Packages: []resource.InstallPackage{
			{Name: "curl", From: "1", To: "2"},
			{Name: "openssl", From: "1", To: "2", Security: true},
		}},
		older: true,
	}
	s := f.addHostServer(t, p)
	c := changeClient(b, b.origin)
	_, err := c.PlanInstallUpdates(t.Context(), connect.NewRequest(&dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}, Packages: []string{"curl"}}))
	if err == nil || !strings.Contains(err.Error(), "not packages picked one by one") {
		t.Fatalf("an older agent's plan for curl = %v, want it refused", err)
	}
	for _, ch := range listChanges(t, c) {
		if ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
			t.Errorf("a refused plan was left: %v", ch.GetSummary())
		}
	}
}

// While an install runs, the server's updates name its change, so the Updates tab
// follows it, and aren't read halfway through; once it ends, the next look reads
// them again, whatever their age, so what it installed is gone from the list.
func TestTheUpdatesTabFollowsAnInstallAndReadsAgainAfter(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	root := aptStand(t, 0)
	p := &packages{installed: map[string]string{}, waiting: &resource.Install{Packages: []resource.InstallPackage{
		{Name: "openssl", From: "3.0.13-0ubuntu3.4", To: "3.0.13-0ubuntu3.5", Security: true},
	}}}
	p.hold = make(chan struct{})
	s := f.addHostServerReading(t, p, root)
	c := changeClient(b, b.origin)
	u := dockwellv1connect.NewUpdateServiceClient(b, b.origin, connect.WithProtoJSON())
	look := func() *dockwellv1.ServerUpdates {
		t.Helper()
		res, err := u.GetServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.GetServerUpdatesRequest{Id: s.id.ServerID}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetUpdates()
	}
	reads := func() int { return standCalls(t, root, "apt-get -q -o Debug::NoLocking") }
	if got := look(); got.GetInstallChangeId() != "" || reads() != 1 {
		t.Fatalf("before any install: install %q, %d reads", got.GetInstallChangeId(), reads())
	}

	res, err := c.PlanInstallUpdates(t.Context(), connect.NewRequest(&dockwellv1.PlanInstallUpdatesRequest{ServerIds: []string{s.id.ServerID}, Packages: []string{"openssl"}}))
	if err != nil {
		t.Fatal(err)
	}
	change := res.Msg.GetChange()
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	waitChange(t, c, change.GetId(), "the install runs", func(ch *dockwellv1.Change) bool {
		return slices.ContainsFunc(ch.GetSteps(), func(st *dockwellv1.StepProgress) bool { return strings.HasPrefix(st.GetActivity(), "Setting up ") })
	})
	if got := look(); got.GetInstallChangeId() != change.GetId() || len(got.GetPackages()) != 8 {
		t.Errorf("while it installs: install %q with %d packages, want %s with what was read before", got.GetInstallChangeId(), len(got.GetPackages()), change.GetId())
	}
	if _, err := u.CheckServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.CheckServerUpdatesRequest{Id: s.id.ServerID})); err != nil {
		t.Fatal(err)
	}
	if n := reads(); n != 1 {
		t.Errorf("read the updates %d times while an install ran, want none past the first", n-1)
	}
	close(p.hold)
	waitChange(t, c, change.GetId(), "the install applies", func(ch *dockwellv1.Change) bool {
		return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED
	})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		got := look()
		if got.GetInstallChangeId() == "" && reads() == 2 && !got.GetChecking() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the install: install %q, %d reads, checking %t; want it read again once", got.GetInstallChangeId(), reads(), got.GetChecking())
		}
	}
}
