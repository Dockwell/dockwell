// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/store"
)

// packages is a server's packages, which a fake host installs.
type packages struct {
	mu        sync.Mutex
	waiting   *resource.Install
	installed map[string]string
	installs  int
	// hold, when set, keeps an install going until it's closed.
	hold chan struct{}
}

func (p *packages) Plan(context.Context, bool) (*resource.Install, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiting == nil {
		return &resource.Install{}, nil
	}
	return &resource.Install{Packages: slices.Clone(p.waiting.Packages), Reboot: p.waiting.Reboot}, nil
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
	return left, false, nil
}

func (p *packages) Install(ctx context.Context, _ string, planned []resource.InstallPackage, say func(string)) error {
	p.mu.Lock()
	p.installs++
	hold := p.hold
	p.mu.Unlock()
	for _, pk := range planned {
		say("Setting up " + pk.Name + " (" + pk.To + ")")
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pk := range planned {
		p.installed[pk.Name] = pk.To
	}
	p.waiting = nil
	return nil
}

// addHostServer adds a server whose host installs from packages.
func (f *fullServer) addHostServer(t *testing.T, name string, p *packages) *testServer {
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
	}
	s.connect(t, f)
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
	s := f.addHostServer(t, "web-1", p)
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

func listChanges(t *testing.T, c dockwellv1connect.ChangeServiceClient) []*dockwellv1.Change {
	t.Helper()
	res, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.GetChanges()
}
