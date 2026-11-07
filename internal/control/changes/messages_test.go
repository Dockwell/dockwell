// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// server is a fake server: a firewall in memory behind its agent.
type server struct {
	fw     *firewall.Fake
	agent  resource.Agent
	online atomic.Bool
}

type fleet map[string]*server

func (f fleet) Agent(id string) (resource.Agent, bool) {
	s, ok := f[id]
	if !ok {
		return nil, false
	}
	return s.agent, s.online.Load()
}

// engineWith starts an engine, and a handler over it, on a store with the
// servers srv_1, srv_2, … named hz-1, hz-2, …, all connected.
func engineWith(t *testing.T, b store.Backend, servers int) (*Handler, *revision.Engine, fleet, *store.DB) {
	t.Helper()
	db, _ := storetest.Open(t, b)
	logger := slog.New(slog.DiscardHandler)
	if _, err := revision.Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	f := fleet{}
	now := time.Now()
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		for i := 1; i <= servers; i++ {
			id := fmt.Sprintf("srv_%d", i)
			if err := tx.CreateServer(t.Context(), store.Server{ID: id, Name: fmt.Sprintf("hz-%d", i), CreatedAt: now, CertSerial: id, CertNotAfter: now}); err != nil {
				return err
			}
			s := &server{fw: firewall.NewFake()}
			s.agent = resource.Local{Drivers: resource.NewRegistry(&firewall.Driver{Backend: s.fw})}
			s.online.Store(true)
			f[id] = s
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(&container.Driver{}, &firewall.Driver{}, &service.Driver{})
	e := revision.New(db, kinds, f, secretstest.Keeper(db, kinds), logger)
	return New(e, nil, db, logger), e, f, db
}

func rule(server, name string, port uint32) revision.Edit {
	return revision.Edit{Ref: resource.Ref{Kind: firewall.Kind, Scope: server, Name: name}, Spec: &resourcev1.FirewallRule{Port: port}}
}

func proposed(t *testing.T, e *revision.Engine, edits ...revision.Edit) string {
	t.Helper()
	p, err := e.Propose(t.Context(), revision.Proposal{Summary: "Open ports", Actor: "user:usr_mira", Edits: edits})
	if err != nil {
		t.Fatal(err)
	}
	return p.Change
}

func applied(t *testing.T, e *revision.Engine, edits ...revision.Edit) string {
	t.Helper()
	id := proposed(t, e, edits...)
	if err := e.Apply(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

// hooked is an agent whose plans and steps a test can hold up or fail.
type hooked struct {
	resource.Agent
	plan  func() error
	apply func(step resource.Step) error
}

func (h hooked) Plan(ctx context.Context, desired []resource.Desired) ([]resource.Step, error) {
	if h.plan != nil {
		if err := h.plan(); err != nil {
			return nil, err
		}
	}
	return h.Agent.Plan(ctx, desired)
}

func (h hooked) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if h.apply != nil {
		if err := h.apply(step); err != nil {
			return err
		}
	}
	return h.Agent.Apply(ctx, step, desired)
}

// fixVerbs start a fix. A message that starts with one leads with the fix,
// not the fact. ("edit" is left out: "edit 2 has no spec" names an edit.)
var fixVerbs = regexp.MustCompile(`^(apply|propose|plan|undo|check|add|delete|try|watch|run|sign|name|set|make|review|choose)\b`)

// wholeSentence checks a message as the API sends it: it starts with the
// fact, in lowercase as Go errors do (the panel capitalizes it), and its
// first sentence is a sentence, not a name cut off from one.
func wholeSentence(msg string) error {
	first, _, _ := strings.Cut(msg, ". ")
	switch {
	case msg == "":
		return errors.New("it's empty")
	case strings.ToLower(msg[:1]) != msg[:1]:
		return errors.New("it starts with a capital, which Go errors don't")
	case strings.HasSuffix(msg, "."):
		return errors.New("it ends with a period, which Go errors don't")
	case !strings.Contains(first, " "):
		return fmt.Errorf("its first sentence, %q, is a fragment", first)
	case fixVerbs.MatchString(msg):
		return errors.New("it starts with the fix, not the fact")
	}
	return nil
}

// Every error the engine and the handler return reaches the API whole: the
// fact, then the consequence, then the fix. None is cut down to a fragment
// by taking its kind off the front, as "hz-1. Apply again once its agent
// reconnects" was.
func TestErrorMessagesStateTheFactFirst(t *testing.T) {
	storetest.Each(t, testErrorMessagesStateTheFactFirst)
}

func testErrorMessagesStateTheFactFirst(t *testing.T, backend store.Backend) {
	h, e, f, db := engineWith(t, backend, 2)
	ctx := t.Context()
	type sent struct {
		what, reason, message string
		err                   error
	}
	var cases []sent
	expect := func(what, reason, message string, err error) {
		cases = append(cases, sent{what, reason, message, err})
	}

	_, err := e.Change(ctx, "chg_nope")
	expect("a change that isn't there", "not_found", "there's no change chg_nope. Check its ID, or list the changes to find it", err)

	_, err = h.edits(ctx, nil)
	expect("no edits", "invalid", "the change has no edits. Add one for each resource to change", err)
	ruleSpec := &resourcev1.Spec{Kind: &resourcev1.Spec_FirewallRule{FirewallRule: &resourcev1.FirewallRule{Port: 80}}}
	_, err = h.edits(ctx, []*dockwellv1.Edit{{Resource: &dockwellv1.ResourceRef{Name: "http"}, Spec: ruleSpec, Delete: true}})
	expect("a spec and a delete", "invalid", "edit 1 both sets a spec and deletes. Do one or the other", err)
	_, err = h.edits(ctx, []*dockwellv1.Edit{{Resource: &dockwellv1.ResourceRef{Name: "http"}, Spec: ruleSpec}})
	expect("a server-level edit with no server, of two", "choice", "there are 2 servers, hz-1 and hz-2, so say which one you mean", err)
	_, err = h.edits(ctx, []*dockwellv1.Edit{{Resource: &dockwellv1.ResourceRef{Scope: "hz-9", Name: "http"}, Spec: ruleSpec}})
	expect("a server that isn't there", "not_found", "there's no server hz-9 in the fleet", err)
	_, err = h.edits(ctx, []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Scope: "production", Name: "web"}, Project: "nowhere",
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}}}},
	}})
	expect("a project that isn't there", "not_found", "there's no project nowhere. The projects are default", err)
	_, err = e.Propose(ctx, revision.Proposal{Summary: "x", Actor: "user:usr_mira", Edits: []revision.Edit{rule("srv_1", "http", 0)}})
	expect("an invalid spec", "invalid", "firewall.rule http in srv_1 isn't valid: a rule's port runs from 1 to 65535, not 0", err)

	// http on hz-1: one plan applied, and another made before it.
	first, second := proposed(t, e, rule("srv_1", "http", 80)), proposed(t, e, rule("srv_1", "http", 81))
	if err := e.Apply(ctx, first); err != nil {
		t.Fatal(err)
	}
	expect("applying a plan made before another applied", "plan_stale",
		"http changed after this plan was made (in "+first+"), so the plan is out of date. Plan the change again",
		e.Apply(ctx, second))
	expect("applying a change twice", "not_planned",
		first+" is applied already, so it can't be applied again. Undo it, or propose a new change", e.Apply(ctx, first))
	expect("discarding an applied change", "not_planned", first+" is applied, and only a planned change can be discarded",
		e.Discard(ctx, first, revision.By{Actor: "user:usr_mira"}))
	planned := proposed(t, e, rule("srv_1", "https", 443))
	_, err = e.Undo(ctx, planned, revision.By{Actor: "user:usr_mira"})
	expect("undoing a planned change", "cant_undo", planned+" is planned, and only applied changes can be undone", err)
	later := applied(t, e, rule("srv_1", "http", 8080))
	_, err = e.Undo(ctx, first, revision.By{Actor: "user:usr_mira"})
	expect("undoing a change whose resource changed again", "cant_undo",
		"firewall.rule http in srv_1 changed again in "+later+", so "+first+" can't be undone whole. Undo "+later+" first, or edit http directly", err)

	// The server changed by hand since the plan.
	changed := proposed(t, e, rule("srv_1", "http", 8081))
	_ = f["srv_1"].fw.Put(ctx, "http", &resourcev1.FirewallRule{Port: 9})
	expect("applying once the server changed", "plan_changed",
		"the servers changed since this plan was made, so it no longer matches them. Plan the change again and review what changed",
		e.Apply(ctx, changed))

	// hz-2 is away.
	f["srv_2"].online.Store(false)
	away := proposed(t, e, rule("srv_2", "http", 80))
	expect("applying with a server away", "server_offline",
		"hz-2 isn't connected, so this change can't apply. Apply it again once its agent reconnects", e.Apply(ctx, away))
	_, err = e.Drift(ctx, "srv_2")
	expect("drift on a server that is away", "server_offline",
		"hz-2 isn't connected, so its drift can't be checked. Check it again once its agent reconnects", err)
	f["srv_2"].online.Store(true)

	// hz-2's agent can't plan.
	plain := f["srv_2"].agent
	f["srv_2"].agent = hooked{Agent: plain, plan: func() error { return errors.New("can't read the firewall just now") }}
	_, err = e.Propose(ctx, revision.Proposal{Summary: "x", Actor: "user:usr_mira", Edits: []revision.Edit{rule("srv_2", "ssh", 22)}})
	expect("an agent that can't plan", "agent_failed", "can't plan on hz-2: can't read the firewall just now", err)
	f["srv_2"].agent = plain

	// slow on hz-2 holds its step until released.
	running, release := make(chan struct{}, 1), make(chan struct{})
	f["srv_2"].agent = hooked{Agent: plain, apply: func(step resource.Step) error {
		if step.Ref.Name == "slow" {
			running <- struct{}{}
			<-release
		}
		return nil
	}}
	slow := proposed(t, e, rule("srv_2", "slow", 8080))
	if err := e.Start(ctx, slow, revision.By{Actor: "user:usr_mira"}); err != nil {
		t.Fatal(err)
	}
	<-running
	expect("applying a change that is being applied", "busy", slow+" is being applied already. Watch it to follow its steps", e.Apply(ctx, slow))
	again := proposed(t, e, rule("srv_2", "slow", 8080)) // the same spec again: only the claim stops it
	expect("applying while another change applies the resource", "busy",
		"another change is applying slow now, so this one can't start yet. Apply it again once that one ends ("+slow+")", e.Apply(ctx, again))
	close(release)
	e.Wait()

	stopped := revision.New(db, e.Kinds(), f, secretstest.Keeper(db, e.Kinds()), slog.New(slog.DiscardHandler))
	if err := stopped.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	last := proposed(t, e, rule("srv_1", "smtp", 25))
	expect("applying as the control plane stops", "stopping",
		"the control plane is stopping, so "+last+" wasn't applied. Apply it again once the control plane is back", stopped.Apply(ctx, last))

	for _, c := range cases {
		if c.err == nil {
			t.Errorf("%s: no error", c.what)
			continue
		}
		var cerr *connect.Error
		if !errors.As(h.rpcError(ctx, c.err), &cerr) {
			t.Fatalf("%s: rpcError gave no *connect.Error", c.what)
		}
		got, reason := cerr.Message(), cerr.Meta().Get(authapi.ReasonKey)
		if reason != c.reason || got != c.message {
			t.Errorf("%s: the API says %s, %q;\nwant %s, %q", c.what, reason, got, c.reason, c.message)
		}
		if err := wholeSentence(got); err != nil {
			t.Errorf("%s: %q doesn't read as a whole sentence: %v", c.what, got, err)
		}
	}
}

// A server-level edit that leaves out its server, once there are several,
// is a choice: the error names the servers, for the CLI to ask which.
func TestAServerIsAChoiceOnceThereAreTwo(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		h, _, _, _ := engineWith(t, b, 2)
		spec := &resourcev1.Spec{Kind: &resourcev1.Spec_FirewallRule{FirewallRule: &resourcev1.FirewallRule{Port: 80}}}
		_, err := h.edits(t.Context(), []*dockwellv1.Edit{{Resource: &dockwellv1.ResourceRef{Name: "http"}, Spec: spec}})
		var choice *scope.ChoiceError
		if !errors.As(err, &choice) || choice.Level != scope.ServerLevel || strings.Join(choice.Options, " ") != "hz-1 hz-2" {
			t.Fatalf("a firewall rule with no server, of two = %v, want a choice of hz-1 and hz-2", err)
		}
		if got, err := h.edits(t.Context(), []*dockwellv1.Edit{{Resource: &dockwellv1.ResourceRef{Scope: "hz-2", Name: "http"}, Spec: spec}}); err != nil ||
			got[0].Ref.Scope != "srv_2" {
			t.Errorf("a firewall rule on hz-2 = %+v, %v, want it on srv_2", got, err)
		}
	})
}

// ListDrift asks every server's agent at once, up to a limit, rather than
// one after another: each may take a minute. Here each agent answers only
// once all of them have been asked, so asking in turn would wait for ever.
// The results come in the servers' order all the same.
func TestDriftChecksServersSideBySide(t *testing.T) {
	storetest.Each(t, testDriftChecksServersSideBySide)
}

func testDriftChecksServersSideBySide(t *testing.T, backend store.Backend) {
	const servers = 3
	h, e, f, _ := engineWith(t, backend, servers)
	var edits []revision.Edit
	for i := 1; i <= servers; i++ {
		edits = append(edits, rule(fmt.Sprintf("srv_%d", i), "http", 80))
	}
	applied(t, e, edits...)
	var asked sync.WaitGroup
	asked.Add(servers)
	all := make(chan struct{})
	go func() { asked.Wait(); close(all) }()
	for id, s := range f {
		_ = s.fw.Put(t.Context(), "http", &resourcev1.FirewallRule{Port: 81}) // changed by hand
		s.agent = observing{Agent: s.agent, before: func(ctx context.Context) error {
			asked.Done()
			select {
			case <-all:
				return nil
			case <-time.After(3 * time.Second):
				return fmt.Errorf("%s's agent was asked alone: the others weren't asked while it answered", id)
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
	}
	res, err := h.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if u := res.Msg.GetUncheckedServers(); len(u) != 0 {
		t.Fatalf("unchecked servers = %v, want every one checked", u)
	}
	var order []string
	for _, d := range res.Msg.GetDrift() {
		order = append(order, d.GetServerId())
	}
	if strings.Join(order, " ") != "srv_1 srv_2 srv_3" {
		t.Errorf("drift came for %v, want each server once, in order", order)
	}
}

// observing is an agent that runs before ahead of each Observe.
type observing struct {
	resource.Agent
	before func(ctx context.Context) error
}

func (o observing) Observe(ctx context.Context, refs []resource.Ref) ([]proto.Message, error) {
	if err := o.before(ctx); err != nil {
		return nil, err
	}
	return o.Agent.Observe(ctx, refs)
}
