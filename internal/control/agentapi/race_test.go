// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
)

// The agent's own error for a call that ran out of time, and the call's
// end, can both be ready when the call wakes to read them: it says the
// agent didn't answer in time either way, however Go picks between them.
// A result that succeeded stands, as the step ran.
func TestACallOutOfTimeSaysSoWhicheverEndComesFirst(t *testing.T) {
	kinds := resource.NewRegistry(&firewall.Driver{})
	step := resource.Step{Ref: resource.Ref{Kind: firewall.Kind, Scope: "srv_1", Name: "slow"}, Decision: resource.Decision{Action: resource.ActionCreate}}
	rule := &resourcev1.FirewallRule{Port: 8080}
	run := func(answer *agentv1.CallResult) error {
		a := NewAgents(kinds, slog.New(slog.DiscardHandler))
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Millisecond)
		defer cancel()
		var s *Session
		// The agent answers once the call's time has run out, before the
		// call goes on to wait for its result: both are ready at once.
		s = a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(firewall.Kind)}, WritesReferences: true}, func(msg *agentv1.ConnectResponse) error {
			if c := msg.GetCall(); c != nil {
				<-ctx.Done()
				r := fresh(answer)
				r.Id = c.GetId()
				s.deliver(r)
			}
			return nil
		})
		defer s.close()
		return s.Apply(ctx, step, rule)
	}
	failed := &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{
		Message: "can't create firewall.rule slow in srv_1: context deadline exceeded",
	}}}
	succeeded := &agentv1.CallResult{Result: &agentv1.CallResult_Apply{Apply: &agentv1.ApplyResult{}}}
	// Go picks at random, so a wrong pick shows within a few tries.
	for range 40 {
		if err := run(failed); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "didn't answer within") {
			t.Fatalf("a call that failed as its time ran out = %v, want it to say the agent didn't answer in time", err)
		}
		if err := run(succeeded); err != nil {
			t.Fatalf("a call that succeeded as its time ran out = %v, want it applied", err)
		}
	}
}

// An agent's answer to a logs call and its stream's end can both be ready when the
// call wakes to read them, as when whoever reads is slow to take the last lines and
// the agent goes just after. What it answered stands, however Go picks: a read that
// finished doesn't read as an agent that went. An agent that said it was moving
// stopped the read to do so, so the reader carries on, answer or no answer.
func TestALogsCallTakesTheAgentsAnswerAsItsStreamEnds(t *testing.T) {
	kinds := resource.NewRegistry(&firewall.Driver{})
	run := func(answer *agentv1.CallResult, moving bool) error {
		a := NewAgents(kinds, slog.New(slog.DiscardHandler))
		calls := make(chan *agentv1.Call, 1)
		s := a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(firewall.Kind)}, WritesReferences: true}, func(msg *agentv1.ConnectResponse) error {
			if c := msg.GetCall(); c != nil {
				calls <- c
			}
			return nil
		})
		defer s.close()
		var id uint64
		result := make(chan error, 1)
		go func() {
			result <- s.Logs(t.Context(), &agentv1.LogsCall{}, func(*agentv1.SendLogsRequest) error {
				if moving {
					s.drain()
				}
				if answer != nil {
					r := fresh(answer)
					r.Id = id
					s.deliver(r)
				}
				s.close()
				return nil
			})
		}()
		c := <-calls
		id = c.GetId()
		a.uploadFor("srv_1", c.GetLogs().GetStream()).messages <- &agentv1.SendLogsRequest{}
		return <-result
	}
	finished := &agentv1.CallResult{Result: &agentv1.CallResult_Logs{Logs: &agentv1.LogsResult{}}}
	failed := &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: "Docker isn't running"}}}
	stopped := &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: "moving to a new stream", Draining: true}}}
	// Go picks at random, so a wrong pick shows within a few tries.
	for range 40 {
		if err := run(finished, false); err != nil {
			t.Fatalf("a read that finished as its stream ended = %v, want it finished", err)
		}
		var server *resource.ServerError
		if err := run(failed, false); !errors.As(err, &server) {
			t.Fatalf("a read that failed as its stream ended = %v, want what the agent said", err)
		}
		if err := run(stopped, true); !errors.Is(err, ErrMoving) || !errors.Is(err, resource.ErrNotConnected) {
			t.Fatalf("a read the agent stopped to move = %v, want it to say it can carry on", err)
		}
		// The agent can stop a read before it has said it's moving.
		if err := run(stopped, false); !errors.Is(err, ErrMoving) {
			t.Fatalf("a read stopped ahead of the word that its agent moves = %v, want it to say it can carry on", err)
		}
		if err := run(nil, true); !errors.Is(err, ErrMoving) {
			t.Fatalf("a read whose agent moved before it answered = %v, want it to say it can carry on", err)
		}
		if err := run(nil, false); !errors.Is(err, ErrLost) || errors.Is(err, ErrMoving) {
			t.Fatalf("a read whose agent went with no word = %v, want it lost", err)
		}
	}
}

// A read waits for an agent that said it's moving, for no longer than it's given, and
// for nothing else.
func TestAReadWaitsForAMoveAndNothingElse(t *testing.T) {
	const forever = time.Hour
	kinds := resource.NewRegistry(&firewall.Driver{})
	open := func(a *Agents) *Session {
		return a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(firewall.Kind)}}, func(*agentv1.ConnectResponse) error { return nil })
	}
	type got struct {
		session *Session
		ok      bool
		took    time.Duration
	}
	read := func(t *testing.T, ctx context.Context, a *Agents, within time.Duration) got {
		t.Helper()
		out := make(chan got, 1)
		start := time.Now()
		go func() {
			s, ok := a.SessionPastMove(ctx, "srv_1", within)
			out <- got{s, ok, time.Since(start)}
		}()
		select {
		case g := <-out:
			return g
		case <-time.After(5 * time.Second):
			t.Fatal("the read waited for what it shouldn't")
			return got{}
		}
	}
	fresh := func() *Agents { return NewAgents(kinds, slog.New(slog.DiscardHandler)) }

	t.Run("a connected server is there", func(t *testing.T) {
		a := fresh()
		s := open(a)
		defer s.close()
		if g := read(t, t.Context(), a, forever); !g.ok || g.session != s {
			t.Errorf("got %+v, want its stream", g)
		}
	})
	t.Run("a server that never connected isn't waited for", func(t *testing.T) {
		if g := read(t, t.Context(), fresh(), forever); g.ok {
			t.Errorf("got %+v, want it not connected", g)
		}
	})
	t.Run("an agent that went with no word isn't waited for", func(t *testing.T) {
		a := fresh()
		open(a).close()
		if g := read(t, t.Context(), a, forever); g.ok {
			t.Errorf("got %+v, want it not connected", g)
		}
	})
	t.Run("an agent moving is waited for, over its old stream's end", func(t *testing.T) {
		a := fresh()
		old := open(a)
		old.drain()
		next := make(chan *Session, 1)
		go func() {
			time.Sleep(50 * time.Millisecond)
			old.close()
			time.Sleep(50 * time.Millisecond)
			next <- open(a)
		}()
		g := read(t, t.Context(), a, forever)
		s := <-next
		defer s.close()
		if !g.ok || g.session != s {
			t.Errorf("got %+v, want its next stream", g)
		}
	})
	t.Run("a read begun between two streams waits for the next", func(t *testing.T) {
		a := fresh()
		old := open(a)
		old.drain()
		old.close()
		next := make(chan *Session, 1)
		go func() {
			time.Sleep(50 * time.Millisecond)
			next <- open(a)
		}()
		g := read(t, t.Context(), a, forever)
		s := <-next
		defer s.close()
		if !g.ok || g.session != s {
			t.Errorf("got %+v, want its next stream", g)
		}
	})
	t.Run("an agent that doesn't come back is out of time", func(t *testing.T) {
		a := fresh()
		old := open(a)
		old.drain()
		old.close()
		const within = 200 * time.Millisecond
		if g := read(t, t.Context(), a, within); g.ok || g.took < within-20*time.Millisecond {
			t.Errorf("got %+v, want it not connected after %s", g, within)
		}
	})
	t.Run("a move that can't finish yet is out of time too", func(t *testing.T) {
		a := fresh()
		old := open(a)
		defer old.close()
		old.drain()
		const within = 200 * time.Millisecond
		if g := read(t, t.Context(), a, within); g.ok || g.took < within-20*time.Millisecond {
			t.Errorf("got %+v, want it not connected after %s", g, within)
		}
	})
	t.Run("the time runs from the stream's end, not the read", func(t *testing.T) {
		a := fresh()
		old := open(a)
		old.drain()
		old.close()
		const within = 2 * time.Second
		time.Sleep(1500 * time.Millisecond)
		if g := read(t, t.Context(), a, within); g.ok || g.took > 1500*time.Millisecond {
			t.Errorf("got %+v, want it not connected in what's left of %s", g, within)
		}
	})
	t.Run("a move long past isn't waited for", func(t *testing.T) {
		a := fresh()
		old := open(a)
		old.drain()
		old.close()
		a.mu.Lock()
		a.moved["srv_1"] = time.Now().Add(-forever)
		a.mu.Unlock()
		if g := read(t, t.Context(), a, time.Minute); g.ok {
			t.Errorf("got %+v, want it not connected", g)
		}
	})
	t.Run("a stream that opens clears the move", func(t *testing.T) {
		a := fresh()
		old := open(a)
		old.drain()
		old.close()
		open(a).close() // and this one goes with no word
		if g := read(t, t.Context(), a, forever); g.ok {
			t.Errorf("got %+v, want it not connected", g)
		}
	})
	t.Run("a reader that left stops waiting", func(t *testing.T) {
		a := fresh()
		old := open(a)
		old.drain()
		old.close()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if g := read(t, ctx, a, forever); g.ok {
			t.Errorf("got %+v, want nothing", g)
		}
	})
}

// A rollout's call gets what its plan says it may take, on top of what any step
// gets, or a long start period would read as an agent that didn't answer, and the
// agent would stop a step that was going as it should.
func TestACallGetsTheTimeItsPlanTakes(t *testing.T) {
	kinds := resource.NewRegistry(&firewall.Driver{})
	step := resource.Step{Ref: resource.Ref{Kind: firewall.Kind, Scope: "srv_1", Name: "slow"}, Decision: resource.Decision{
		Action: resource.ActionReplace, Rollout: &resource.RolloutPlan{Rolling: true, HealthyWithin: 30 * time.Minute, Drain: time.Minute, StopGrace: 10 * time.Second},
	}}
	a := NewAgents(kinds, slog.New(slog.DiscardHandler))
	var s *Session
	var timeout time.Duration
	s = a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(firewall.Kind)}, WritesReferences: true}, func(msg *agentv1.ConnectResponse) error {
		if c := msg.GetCall(); c != nil {
			timeout = c.GetTimeout().AsDuration()
			s.deliver(&agentv1.CallResult{Id: c.GetId(), Result: &agentv1.CallResult_Apply{Apply: &agentv1.ApplyResult{}}})
		}
		return nil
	})
	defer s.close()
	if err := s.Apply(t.Context(), step, &resourcev1.FirewallRule{Port: 8080}); err != nil {
		t.Fatal(err)
	}
	if want := ApplyTimeout + step.Budget() - time.Minute; timeout < want {
		t.Errorf("the agent got %s for a step that may take %s beyond the %s any step gets", timeout, step.Budget(), ApplyTimeout)
	}
}

// An install's call gets hours on top of what any step gets: its download, then an
// install that builds kernel modules on a small server, which ten minutes would
// cut short while it goes on (ADR 0064).
func TestAnInstallsCallGetsItsHours(t *testing.T) {
	kinds := resource.NewRegistry(&firewall.Driver{})
	step := resource.Step{Ref: resource.Ref{Kind: firewall.Kind, Scope: "srv_1", Name: "host"}, Decision: resource.Decision{
		Action: resource.ActionUpdate, Install: &resource.Install{Packages: []resource.InstallPackage{{Name: "openssl", To: "3.0.13-0ubuntu3.5"}}},
	}}
	a := NewAgents(kinds, slog.New(slog.DiscardHandler))
	var s *Session
	var timeout time.Duration
	s = a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(firewall.Kind)}, WritesReferences: true}, func(msg *agentv1.ConnectResponse) error {
		if c := msg.GetCall(); c != nil {
			timeout = c.GetTimeout().AsDuration()
			s.deliver(&agentv1.CallResult{Id: c.GetId(), Result: &agentv1.CallResult_Apply{Apply: &agentv1.ApplyResult{}}})
		}
		return nil
	})
	defer s.close()
	if err := s.Apply(t.Context(), step, &resourcev1.FirewallRule{Port: 8080}); err != nil {
		t.Fatal(err)
	}
	if want := ApplyTimeout + resource.InstallBudget - time.Minute; timeout < want {
		t.Errorf("an install's call got %s, want at least %s", timeout, want)
	}
}

// fresh copies a result, so each call gets its own.
func fresh(r *agentv1.CallResult) *agentv1.CallResult {
	return &agentv1.CallResult{Result: r.GetResult()}
}
