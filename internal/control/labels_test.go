// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
	"github.com/dockwell/dockwell/internal/ticket"
)

// hostLabels is a server's labels as its host's latest revision has them.
func hostLabels(t *testing.T, f *fullServer, server string) map[string]string {
	t.Helper()
	var labels map[string]string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(host.Kind), server, host.Name)
		if err != nil {
			return err
		}
		spec, err := f.engine.Kinds().Decode(host.Kind, r.Spec)
		if err != nil {
			return err
		}
		labels = spec.(*resourcev1.ServerHost).GetLabels()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return labels
}

// waitLabels waits for the fleet to give a server labels, as its stream would.
func waitLabels(t *testing.T, servers dockwellv1connect.ServerServiceClient, id string, want map[string]string) {
	t.Helper()
	var got map[string]string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		res, err := servers.GetServer(t.Context(), connect.NewRequest(&dockwellv1.GetServerRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		if got = res.Msg.GetServer().GetLabels(); maps.Equal(got, want) {
			return
		}
	}
	t.Fatalf("the fleet gives the labels %v, want %v", got, want)
}

// selected is the names of the servers a selector lists.
func selected(t *testing.T, servers dockwellv1connect.ServerServiceClient, selector string) ([]string, error) {
	t.Helper()
	res, err := servers.ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{Selector: selector}))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, s := range res.Msg.GetServers() {
		names = append(names, s.GetName())
	}
	slices.Sort(names)
	return names, nil
}

// A change of a server's labels alone is a revision of its host whose plan has no
// step on the server and says so (ADR 0091), so it applies with the server online
// or not. The fleet gives the labels at once, a selector lists by them, drift never
// reads them as missing, and a reboot's edit of the host keeps them.
func TestALabelChangeTakesNoStepOnTheServer(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	servers := dockwellv1connect.NewServerServiceClient(b, b.origin)
	p := &packages{installed: map[string]string{}, boot: "boot-1"}
	web := f.addHostServer(t, p)
	other := f.addNamedHostServer(t, "hz-1", &packages{installed: map[string]string{}, boot: "boot-1"}, "")

	plan := func(set, remove []string, ids ...string) (*dockwellv1.Change, error) {
		res, err := c.PlanServerLabels(t.Context(), connect.NewRequest(&dockwellv1.PlanServerLabelsRequest{ServerIds: ids, Set: set, Remove: remove}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange(), nil
	}
	noStep := func(ch *dockwellv1.Change, names ...string) {
		t.Helper()
		for _, s := range ch.GetPlan().GetServers() {
			if len(s.GetSteps()) > 0 {
				t.Errorf("%q has steps on %s: %v", ch.GetSummary(), s.GetServerName(), s.GetSteps())
			}
		}
		i := slices.IndexFunc(ch.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool { return n.GetReason() == "labels_only" })
		if i < 0 {
			t.Fatalf("%q's plan doesn't say it's labels alone: %v", ch.GetSummary(), ch.GetPlan().GetNotes())
		}
		n := ch.GetPlan().GetNotes()[i]
		if !slices.Equal(n.GetNames(), names) || !n.GetNamed() || !strings.Contains(n.GetMessage(), "no step runs on") {
			t.Errorf("the note reads %v, want it to name %v", n, names)
		}
		// Each host it labels is an update, with what changes of its labels, so no surface
		// reads the plan as one that changes nothing.
		for _, r := range ch.GetPlan().GetResources() {
			if r.GetResource().GetKind() != string(host.Kind) {
				continue
			}
			if r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_UPDATE || len(r.GetChanges()) == 0 ||
				!strings.HasPrefix(r.GetChanges()[0].GetPath(), "labels[") {
				t.Errorf("the host of %s plans %v, %v", r.GetResource().GetScope(), r.GetAction(), r.GetChanges())
			}
		}
	}

	ch, err := plan([]string{"region:eu", "pool:web"}, nil, web.id.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if ch.GetSummary() != "Label web-1 region:eu pool:web" {
		t.Errorf("the change says %q", ch.GetSummary())
	}
	noStep(ch, "web-1")
	if ch.GetPlan().GetHostAccess() {
		t.Error("a change of labels takes host access")
	}
	if got := apply(t, c, ch); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the labels' change is %v", got.GetState())
	}
	if got := hostLabels(t, f, web.id.ServerID); !maps.Equal(got, map[string]string{"region": "eu", "pool": "web"}) {
		t.Errorf("the host's labels are %v", got)
	}
	waitLabels(t, servers, web.id.ServerID, map[string]string{"region": "eu", "pool": "web"})

	// A selector lists by them.
	for selector, want := range map[string][]string{
		"":                    {"hz-1", "web-1"},
		"region:eu":           {"web-1"},
		"pool:web region:eu":  {"web-1"},
		"region":              {"web-1"},
		"!region":             {"hz-1"},
		"region:us":           nil,
		"pool:web !region:eu": nil,
	} {
		if got, err := selected(t, servers, selector); err != nil || !slices.Equal(got, want) {
			t.Errorf("the selector %q lists %v, %v, want %v", selector, got, err, want)
		}
	}
	if _, err := selected(t, servers, "region:eu:west"); reasonOf(err) != "invalid" {
		t.Errorf("a selector with a bad value = %v", err)
	}

	// Drift never reads them as missing on the server, which never has them.
	drift, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range drift.Msg.GetDrift() {
		if d.GetResource().GetKind() == string(host.Kind) {
			t.Errorf("the host reads as drifted: %v", d)
		}
	}

	// A reboot's edit of the host keeps them: its plan changes nothing of the host.
	reboot, _ := planReboot(t, c, web)
	for _, r := range reboot.GetPlan().GetResources() {
		if r.GetResource().GetKind() == string(host.Kind) && len(r.GetChanges()) > 0 {
			t.Errorf("the reboot changes the host: %v", r.GetChanges())
		}
	}
	if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: reboot.GetId()})); err != nil {
		t.Fatal(err)
	}

	// Offline, a change of them plans and applies the same, on both servers at once.
	web.stop()
	waitConnected(t, f, web.id.ServerID, false)
	ch, err = plan([]string{"region:us"}, []string{"pool"}, web.id.ServerID, other.id.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	noStep(ch, "hz-1", "web-1")
	if got := apply(t, c, ch); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("with web-1 offline, the labels' change is %v", got.GetState())
	}
	waitLabels(t, servers, web.id.ServerID, map[string]string{"region": "us"})
	waitLabels(t, servers, other.id.ServerID, map[string]string{"region": "us"})

	// Undone, it's labels alone too.
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: ch.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	noStep(undo.Msg.GetChange(), "hz-1", "web-1")
	apply(t, c, undo.Msg.GetChange())
	waitLabels(t, servers, web.id.ServerID, map[string]string{"region": "eu", "pool": "web"})
	waitLabels(t, servers, other.id.ServerID, nil)

	// What it can't take is refused before anything plans.
	for name, tc := range map[string]struct {
		set, remove []string
		says        string
	}{
		"a bad key":           {set: []string{"reg!on:eu"}, says: "letters, digits"},
		"a key set and gone":  {set: []string{"region:eu"}, remove: []string{"region"}, says: "both set and removed"},
		"nothing":             {says: "say a label to set"},
		"a value with spaces": {set: []string{"region:eu west"}, says: "letters, digits"},
		"what it has":         {set: []string{"region:eu"}, says: "has those labels already"},
	} {
		if _, err := plan(tc.set, tc.remove, web.id.ServerID); reasonOf(err) != "invalid" || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := plan([]string{"region:eu"}, nil, "srv_nobody"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a server that isn't there: %v", err)
	}
}

// An edit that reboots, installs or acts on a unit takes the host as it is, so an
// edit that also says a spec is refused, and none can lose its labels.
func TestAStepOnAHostKeepsItsSpec(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	web := f.addHostServer(t, &packages{installed: map[string]string{}, boot: "boot-1"})
	ref := resource.Ref{Kind: host.Kind, Scope: web.id.ServerID, Name: host.Name}
	for name, ed := range map[string]revision.Edit{
		"a reboot":   {Ref: ref, Spec: &resourcev1.ServerHost{}, Reboot: true},
		"an install": {Ref: ref, Spec: &resourcev1.ServerHost{}, Install: &resource.InstallAsked{All: true}},
		"a unit's":   {Ref: ref, Spec: &resourcev1.ServerHost{}, Unit: &resource.UnitAsked{Name: "nginx.service", Action: resource.UnitRestart}},
	} {
		_, err := f.engine.Propose(t.Context(), revision.Proposal{Summary: name, Actor: revision.SystemActor, Via: revision.ViaAutomatic, Edits: []revision.Edit{ed}})
		if err == nil || !strings.Contains(err.Error(), "Leave the spec out") {
			t.Errorf("%s that says a spec = %v", name, err)
		}
	}
}

func TestATicketsLabelsGoOnTheServerItAdds(t *testing.T) {
	storetest.Each(t, testATicketsLabelsGoOnTheServerItAdds)
}

// A ticket carries labels for the server that joins with it, which the change that
// adds its host gives it (ADR 0091). The ticket's value doesn't hold them, and a
// label it can't take is refused before the passkey is spent.
func testATicketsLabelsGoOnTheServerItAdds(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	servers := dockwellv1connect.NewServerServiceClient(b, b.origin)
	if err := f.ident.SetAgentURL(t.Context(), "https://"+f.agentAddr); err != nil {
		t.Fatal(err)
	}
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_ADD_SERVER, "")
	create := func(labels ...string) (*dockwellv1.CreateJoinTicketResponse, error) {
		res, err := servers.CreateJoinTicket(t.Context(), connect.NewRequest(&dockwellv1.CreateJoinTicketRequest{Proof: proof, Labels: labels}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	if _, err := create("region:eu", "-pool:web"); reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "start and end") {
		t.Fatalf("a ticket with a bad label = %v", err)
	}
	made, err := create("region:eu", "pool:web")
	if err != nil {
		t.Fatalf("after a bad label, the same proof makes a ticket = %v", err)
	}
	want := map[string]string{"region": "eu", "pool": "web"}
	if !maps.Equal(made.GetTicket().GetLabels(), want) {
		t.Errorf("the ticket gives %v", made.GetTicket().GetLabels())
	}
	read, err := servers.GetJoinTicket(t.Context(), connect.NewRequest(&dockwellv1.GetJoinTicketRequest{Id: made.GetTicket().GetId()}))
	if err != nil || !maps.Equal(read.Msg.GetTicket().GetLabels(), want) {
		t.Errorf("read again, the ticket gives %v, %v", read.Msg.GetTicket().GetLabels(), err)
	}
	decoded, err := ticket.Decode(made.GetValue())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(decoded.String(), "region") {
		t.Errorf("the ticket's value holds its labels: %v", decoded)
	}

	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.Join(t.Context(), decoded.GetTokenId(), decoded.GetSecret(), csr, "web-2", "198.51.100.20")
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{
		id:      enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		drivers: resource.NewRegistry(&host.Driver{Backend: &packages{installed: map[string]string{}}}),
		dataDir: t.TempDir(),
	}
	s.connect(t, f)
	waitLabels(t, servers, s.id.ServerID, want)
	if got := hostLabels(t, f, s.id.ServerID); !maps.Equal(got, want) {
		t.Errorf("web-2's host has %v", got)
	}
}
