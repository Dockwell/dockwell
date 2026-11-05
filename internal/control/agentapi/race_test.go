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
		s = a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(firewall.Kind)}}, func(msg *agentv1.ConnectResponse) error {
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
	s = a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(firewall.Kind)}}, func(msg *agentv1.ConnectResponse) error {
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

// fresh copies a result, so each call gets its own.
func fresh(r *agentv1.CallResult) *agentv1.CallResult {
	return &agentv1.CallResult{Result: r.GetResult()}
}
