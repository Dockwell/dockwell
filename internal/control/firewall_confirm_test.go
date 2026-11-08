// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/access"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/agent/nftables"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// table is Dockwell's nftables table in memory: what was last loaded.
type table struct {
	mu     sync.Mutex
	loaded *nftables.Table
	loads  int
}

func (t *table) Load(_ context.Context, nt nftables.Table) (nftables.Loaded, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loaded, t.loads = &nt, t.loads+1
	return nftables.Loaded{Present: true, Fingerprint: "fp"}, nil
}

func (t *table) Delete(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loaded = nil
	return nil
}

func (*table) Probe(context.Context) error { return nil }

// pins are the table's pins now, with their ports and sources.
func (t *table) pins() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loaded == nil {
		return ""
	}
	var out []string
	for _, r := range t.loaded.Pins {
		out = append(out, fmt.Sprintf("%s %v from %v", r.Name, r.Ports, r.Sources))
	}
	return strings.Join(out, "; ")
}

// rules are the rule names the table holds now.
func (t *table) rules() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loaded == nil {
		return nil
	}
	var out []string
	for _, list := range [][]nftables.Rule{t.loaded.Allows, t.loaded.Denies} {
		for _, r := range list {
			out = append(out, r.Name)
		}
	}
	return out
}

// addFirewallServer adds a server whose agent keeps its firewall under the timer,
// with SSH on 22 from 203.0.113.7, in a table in memory.
func (f *fullServer) addFirewallServer(t *testing.T, name string, timer time.Duration) (*testServer, *table) {
	t.Helper()
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.EnrollLocal(t.Context(), csr, name)
	if err != nil {
		t.Fatal(err)
	}
	tbl := &table{}
	m := &access.Manager{
		Dir: t.TempDir(), Table: tbl, Timer: timer,
		Pins: func(context.Context) (access.Pins, error) {
			return access.Pins{SSHPorts: []uint16{22}, SSHFrom: []netip.Prefix{netip.MustParsePrefix("203.0.113.7/32")}}, nil
		},
	}
	if err := m.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := &testServer{
		id:      enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		drivers: resource.NewRegistry(&firewall.Driver{Backend: m}, &firewall.PolicyDriver{Backend: m}),
		access:  m,
		dataDir: t.TempDir(),
	}
	s.connect(t, f)
	return s, tbl
}

// waitingFor waits for a change to wait for its confirmation on a server other
// than those done, and returns what it waits for: a server just confirmed may
// show its step running for a moment yet.
func waitingFor(t *testing.T, c dockwellv1connect.ChangeServiceClient, changeID string, done ...string) *dockwellv1.ChangeConfirmation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: changeID}))
		if err != nil {
			t.Fatal(err)
		}
		if w := res.Msg.GetChange().GetConfirmation(); w != nil && !slices.Contains(done, w.GetServerId()) {
			return w
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never waited for its confirmation", changeID)
	return nil
}

// confirmation is the request that confirms what a change waits for.
func confirmation(changeID string, w *dockwellv1.ChangeConfirmation) *connect.Request[dockwellv1.ConfirmChangeRequest] {
	return connect.NewRequest(&dockwellv1.ConfirmChangeRequest{Id: changeID, ServerId: w.GetServerId(), Deadline: w.GetDeadline()})
}

func ruleOn(server, name string, port uint32) *dockwellv1.Edit {
	e := ruleEdit(name, port)
	e.Resource.Scope = server
	return e
}

// A change to a server's firewall loads at once, waits for the person who
// applied it to confirm, saying until when and on which SSH port to test, and
// ends applied once they do, from the session they applied it in (ADR 0052).
func TestAFirewallChangeWaitsForItsConfirmation(t *testing.T) {
	storetest.Each(t, testAFirewallChangeWaitsForItsConfirmation)
}

func testAFirewallChangeWaitsForItsConfirmation(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	srv, tbl := f.addFirewallServer(t, "hz-1", time.Minute)
	c := changeClient(b, b.origin)

	// Two rules in one change: both load, and one confirmation keeps both.
	open := propose(t, c, "Open http and https", ruleOn(srv.id.ServerID, "http", 80), ruleOn(srv.id.ServerID, "https", 443))
	// It takes the person's passkey, as a change to who reaches a server does.
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: open.GetId()})); reasonOf(err) != "needs_step_up" {
		t.Errorf("applying a firewall change with no passkey = %v, want needs_step_up", err)
	}
	w := watch(t, c, open.GetId())
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: open.GetId()}); err != nil {
		t.Fatal(err)
	}
	wait := waitingFor(t, c, open.GetId())
	if wait.GetServerId() != srv.id.ServerID || len(wait.GetSshPorts()) != 1 || wait.GetSshPorts()[0] != 22 ||
		time.Until(wait.GetDeadline().AsTime()) < 30*time.Second || time.Until(wait.GetDeadline().AsTime()) > time.Minute {
		t.Errorf("waits for %v", wait)
	}
	if got := strings.Join(tbl.rules(), " "); got != "rule http rule https" {
		t.Errorf("the table holds %q while it waits, want both rules", got)
	}
	if got := tbl.pins(); got != "pin ssh [22] from [203.0.113.7/32]" {
		t.Errorf("the table pins %q while it waits", got)
	}

	// Nobody else confirms it: an API token can't, nor anyone outside the session.
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	if _, err := script.ConfirmChange(t.Context(), confirmation(open.GetId(), wait)); reasonOf(err) != "not_yours" {
		t.Errorf("a token confirming = %v, want not_yours", err)
	}

	// The same person, signed in elsewhere, isn't where they applied it from.
	elsewhere := newBrowser(t, b.origin, f.api, nil)
	elsewhere.auth = b.auth // the same synced passkey
	elsewhere.signIn(t)
	if _, err := changeClient(elsewhere, b.origin).ConfirmChange(t.Context(), confirmation(open.GetId(), wait)); reasonOf(err) != "not_yours" {
		t.Errorf("confirming from another session = %v, want not_yours", err)
	}

	// A confirmation names its server, and the deadline it answers.
	if _, err := c.ConfirmChange(t.Context(), connect.NewRequest(&dockwellv1.ConfirmChangeRequest{Id: open.GetId()})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a confirmation naming no server = %v", err)
	}
	other := &dockwellv1.ChangeConfirmation{ServerId: "srv_other", Deadline: wait.GetDeadline()}
	if _, err := c.ConfirmChange(t.Context(), confirmation(open.GetId(), other)); reasonOf(err) != "not_waiting" {
		t.Errorf("a confirmation for another server = %v, want not_waiting", err)
	}
	stale := &dockwellv1.ChangeConfirmation{ServerId: srv.id.ServerID, Deadline: timestamppb.New(wait.GetDeadline().AsTime().Add(-time.Second))}
	if _, err := c.ConfirmChange(t.Context(), confirmation(open.GetId(), stale)); reasonOf(err) != "not_waiting" {
		t.Errorf("a confirmation of another deadline = %v, want not_waiting", err)
	}

	res, err := c.ConfirmChange(t.Context(), confirmation(open.GetId(), wait))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetChange().GetId() != open.GetId() {
		t.Errorf("confirmed %v", res.Msg.GetChange())
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED || got.GetConfirmation() != nil {
		t.Errorf("a confirmed change ends %v", got)
	}
	if got := strings.Join(tbl.rules(), " "); got != "rule http rule https" {
		t.Errorf("the table holds %q once confirmed", got)
	}
	// A confirmation that comes again finds nothing to confirm.
	if _, err := c.ConfirmChange(t.Context(), confirmation(open.GetId(), wait)); err == nil {
		t.Error("a change confirmed twice")
	}
}

// Nobody confirms in time: the server puts back its firewall as it was, the
// change fails and rolls back, and a confirmation that comes after is refused.
func TestAFirewallChangeNobodyConfirmsRollsBack(t *testing.T) {
	storetest.Each(t, testAFirewallChangeNobodyConfirmsRollsBack)
}

func testAFirewallChangeNobodyConfirmsRollsBack(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	srv, tbl := f.addFirewallServer(t, "hz-1", 400*time.Millisecond)
	c := changeClient(b, b.origin)
	open := propose(t, c, "Open http", ruleOn(srv.id.ServerID, "http", 80))
	w := watch(t, c, open.GetId())
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: open.GetId()}); err != nil {
		t.Fatal(err)
	}
	wait := waitingFor(t, c, open.GetId())
	got := waitWatch(t, w).last()
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(got.GetError(), "Nobody confirmed the change") {
		t.Errorf("an unconfirmed change ends %v: %s", got.GetState(), got.GetError())
	}
	if rules := tbl.rules(); len(rules) != 0 {
		t.Errorf("the table holds %v after nobody confirmed", rules)
	}
	if _, err := c.ConfirmChange(t.Context(), confirmation(open.GetId(), wait)); reasonOf(err) != "not_waiting" {
		t.Errorf("a late confirmation = %v, want not_waiting", err)
	}
}

// While one change waits on a server, another to its firewall is refused as
// busy; a change to two servers' firewalls waits on each in turn, for each
// server's confirmation, and a confirmation sent again for the first server
// never confirms the second.
func TestOneFirewallChangeWaitsAtATime(t *testing.T) {
	storetest.Each(t, testOneFirewallChangeWaitsAtATime)
}

func testOneFirewallChangeWaitsAtATime(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1, _ := f.addFirewallServer(t, "hz-1", time.Minute)
	hz2, _ := f.addFirewallServer(t, "hz-2", time.Minute)
	c := changeClient(b, b.origin)

	both := propose(t, c, "Open http on both", ruleOn(hz1.id.ServerID, "http", 80), ruleOn(hz2.id.ServerID, "http", 80))
	w := watch(t, c, both.GetId())
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: both.GetId()}); err != nil {
		t.Fatal(err)
	}
	first := waitingFor(t, c, both.GetId())
	if _, err := c.ConfirmChange(t.Context(), confirmation(both.GetId(), first)); err != nil {
		t.Fatal(err)
	}
	second := waitingFor(t, c, both.GetId(), first.GetServerId())
	if _, err := c.ConfirmChange(t.Context(), confirmation(both.GetId(), first)); reasonOf(err) != "not_waiting" {
		t.Errorf("the first server's confirmation sent again while the second waits = %v, want not_waiting", err)
	}
	if _, err := c.ConfirmChange(t.Context(), confirmation(both.GetId(), second)); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Errorf("a change to two servers, confirmed on each, ends %v", got.GetState())
	}

	https := propose(t, c, "Open https", ruleOn(hz1.id.ServerID, "https", 443))
	ssh := propose(t, c, "Open ssh", ruleOn(hz1.id.ServerID, "ssh", 22))
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: https.GetId()}); err != nil {
		t.Fatal(err)
	}
	wait := waitingFor(t, c, https.GetId())
	_, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: ssh.GetId()})
	if err == nil || !strings.Contains(err.Error(), "this server's firewall") {
		t.Errorf("a second change while one waits = %v, want it busy", err)
	}
	if _, err := c.ConfirmChange(t.Context(), confirmation(https.GetId(), wait)); err != nil {
		t.Fatal(err)
	}
}

// A change to two servers runs each server's firewall steps together, so a
// server's timer covers its own steps alone, and a person who takes a while to
// test one server loses nothing on another: the change ends with both holding
// all of it (ADR 0052).
func TestEachServersFirewallStepsRunTogether(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	x, tx := f.addFirewallServer(t, "hz-1", time.Minute)
	y, ty := f.addFirewallServer(t, "hz-2", time.Minute)
	tables := map[string]*table{x.id.ServerID: tx, y.id.ServerID: ty}
	c := changeClient(b, b.origin)

	open := propose(t, c, "Open 8080", ruleOn(x.id.ServerID, "old", 8080), ruleOn(y.id.ServerID, "old", 8080))
	w := watch(t, c, open.GetId())
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: open.GetId()}); err != nil {
		t.Fatal(err)
	}
	var done []string
	for range 2 {
		wait := waitingFor(t, c, open.GetId(), done...)
		if _, err := c.ConfirmChange(t.Context(), confirmation(open.GetId(), wait)); err != nil {
			t.Fatal(err)
		}
		done = append(done, wait.GetServerId())
	}
	waitWatch(t, w)

	del := func(server, name string) *dockwellv1.Edit {
		return &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(firewall.Kind), Scope: server, Name: name}, Delete: true}
	}
	move := propose(t, c, "Move 8080 to 9090",
		del(x.id.ServerID, "old"), del(y.id.ServerID, "old"),
		ruleOn(x.id.ServerID, "new", 9090), ruleOn(y.id.ServerID, "new", 9090))
	w = watch(t, c, move.GetId())
	// Deleting an allow may lock you out, so the plan asks for its fingerprint.
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: move.GetId(), Fingerprint: move.GetPlan().GetFingerprint()}); err != nil {
		t.Fatal(err)
	}
	// Each server waits once it has run all its steps, and before the other
	// has run any: deletes don't wait for every server's builds.
	done = nil
	for range 2 {
		wait := waitingFor(t, c, move.GetId(), done...)
		for server, tbl := range tables {
			want := "rule old"
			if server == wait.GetServerId() || slices.Contains(done, server) {
				want = "rule new"
			}
			if got := strings.Join(tbl.rules(), " "); got != want {
				t.Errorf("as %s waits, %s holds %q, want %q", wait.GetServerId(), server, got, want)
			}
		}
		if _, err := c.ConfirmChange(t.Context(), confirmation(move.GetId(), wait)); err != nil {
			t.Fatal(err)
		}
		done = append(done, wait.GetServerId())
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Errorf("the change ends %v: %s", got.GetState(), got.GetError())
	}
}

// The person confirms the first server and goes away: the second's timer runs
// out, the change fails, and its rollback takes the first server back too.
func TestASecondServerThatTimesOutTakesTheFirstBack(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	// The first server's confirmation comes at once; the second's never does.
	x, tx := f.addFirewallServer(t, "hz-1", 3*time.Second)
	y, ty := f.addFirewallServer(t, "hz-2", 3*time.Second)
	c := changeClient(b, b.origin)
	both := propose(t, c, "Open 8080 on both", ruleOn(x.id.ServerID, "web", 8080), ruleOn(y.id.ServerID, "web", 8080))
	w := watch(t, c, both.GetId())
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: both.GetId()}); err != nil {
		t.Fatal(err)
	}
	first := waitingFor(t, c, both.GetId())
	if _, err := c.ConfirmChange(t.Context(), confirmation(both.GetId(), first)); err != nil {
		t.Fatal(err)
	}
	got := waitWatch(t, w).last()
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(got.GetError(), "Nobody confirmed the change") {
		t.Errorf("the change ends %v: %s", got.GetState(), got.GetError())
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(tx.rules())+len(ty.rules()) > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if len(tx.rules())+len(ty.rules()) > 0 {
		t.Errorf("after the rollback the servers hold %v and %v", tx.rules(), ty.rules())
	}
}

// A rule that would close SSH while nobody's SSH session says whose address to
// keep is refused when it's planned, and so is one that would deny the control
// plane's own port.
func TestARuleThatWouldCutAccessIsRefused(t *testing.T) {
	storetest.Each(t, testARuleThatWouldCutAccessIsRefused)
}

func testARuleThatWouldCutAccessIsRefused(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	srv, _ := f.addFirewallServer(t, "hz-1", time.Minute)
	srv.access.(*access.Manager).Pins = func(context.Context) (access.Pins, error) {
		return access.Pins{SSHPorts: []uint16{22}, Own: []uint16{7600}}, nil
	}
	c := changeClient(b, b.origin)
	deny := func(name string, port uint32) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(firewall.Kind), Scope: srv.id.ServerID, Name: name},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_FirewallRule{FirewallRule: &resourcev1.FirewallRule{
				Port: port, Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY,
			}}},
		}
	}
	for port, want := range map[uint32]string{22: "no SSH session is open", 7600: "which the control plane on this server listens on"} {
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Close it", Edits: []*dockwellv1.Edit{deny("close", port)}}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("denying %d = %v, want it refused, saying %q", port, err, want)
		}
	}
	policy := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(firewall.PolicyKind), Scope: srv.id.ServerID, Name: firewall.PolicyName},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_FirewallPolicy{FirewallPolicy: &resourcev1.FirewallPolicy{DenyEverythingElse: true}}},
	}
	if _, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Close the rest", Edits: []*dockwellv1.Edit{policy}})); err == nil || !strings.Contains(err.Error(), "no SSH session is open") {
		t.Errorf("deny everything else with no SSH session = %v", err)
	}
}

// A change's firewall steps run after its other builds and before its deletes,
// so nothing is removed before the server waits for its confirmation: a change
// nobody confirms rolls back with what it would have removed still there
// (ADR 0052).
func TestFirewallStepsRunBeforeDeletes(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addServer(t, true)
	c := changeClient(b, b.origin)
	if got := apply(t, c, propose(t, c, "Run web", webEdit("nginx:1.27", nil))); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("running web: %v", got.GetState())
	}
	swap := propose(t, c, "Swap web for a rule", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "container", Name: "web"}, Delete: true}, ruleEdit("http", 80))
	w := watch(t, c, swap.GetId())
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: swap.GetId(), Fingerprint: swap.GetPlan().GetFingerprint()}); err != nil {
		t.Fatal(err)
	}
	got := waitWatch(t, w).last()
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the swap ends %v: %s", got.GetState(), got.GetError())
	}
	var rule, web *dockwellv1.StepProgress
	for _, s := range got.GetSteps() {
		switch s.GetResource().GetName() {
		case "http":
			rule = s
		case "web":
			web = s
		}
	}
	if rule == nil || web == nil {
		t.Fatalf("steps %v", got.GetSteps())
	}
	if rule.GetFinishTime().AsTime().After(web.GetStartTime().AsTime()) {
		t.Errorf("web's delete started at %v, before the rule's step ended at %v", web.GetStartTime().AsTime(), rule.GetFinishTime().AsTime())
	}
}
