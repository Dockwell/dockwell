// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// slowFirewall puts the rule "slow" in place only once released, or not at
// all if its call is cancelled first.
type slowFirewall struct {
	*firewall.Fake
	started chan struct{}
	release chan struct{}
}

func (f *slowFirewall) Put(ctx context.Context, name string, rule *resourcev1.FirewallRule) error {
	if name == "slow" {
		f.started <- struct{}{}
		select {
		case <-f.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.Fake.Put(ctx, name, rule)
}

// sent keeps the results a stream sent back.
type sent struct {
	mu      sync.Mutex
	results map[uint64]*agentv1.CallResult
}

func (s *sent) send(m *agentv1.ConnectRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[m.GetResult().GetId()] = m.GetResult()
	return nil
}

func (s *sent) result(id uint64) *agentv1.CallResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.results[id]
}

func applyCall(t *testing.T, id uint64, name string) *agentv1.Call {
	t.Helper()
	ref := resource.Ref{Kind: firewall.Kind, Scope: "srv_A", Name: name}
	rule := &resourcev1.FirewallRule{Port: 8080}
	if err := (&firewall.Driver{}).Validate(rule); err != nil {
		t.Fatal(err)
	}
	spec, err := resource.Encode(rule)
	if err != nil {
		t.Fatal(err)
	}
	step := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}
	return &agentv1.Call{Id: id, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Apply{Apply: &agentv1.ApplyCall{
		Step: resourcepb.Step(step, ""), Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(ref), Spec: spec},
	}}}
}

// A stream that closes refuses the calls that arrive from then on, saying
// so, and runs none of them; the calls it took on finish, and their results
// go back. Stopping then waits for what still runs, and no call starts
// while it waits, however many arrive.
func TestCallsCloseBeforeTheyStop(t *testing.T) {
	fw := &slowFirewall{Fake: firewall.NewFake(), started: make(chan struct{}, 1), release: make(chan struct{})}
	out := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{
		Logger: slog.New(slog.DiscardHandler), Drivers: resource.NewRegistry(&firewall.Driver{Backend: fw}),
	}, out.send)

	c.start(applyCall(t, 1, "slow"))
	<-fw.started
	c.close() // moving to a renewed certificate
	c.start(applyCall(t, 2, "late"))
	if r := out.result(2); !r.GetError().GetDraining() || r.GetError().GetMessage() == "" {
		t.Errorf("a call that arrived as the stream closed got %v, want it refused as draining", r)
	}
	if rule, _ := fw.Rule(t.Context(), "late"); rule != nil {
		t.Error("a refused call ran")
	}
	if !c.busy() {
		t.Fatal("closing stopped the call under way")
	}
	close(fw.release)
	deadline := time.Now().Add(5 * time.Second)
	for c.busy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r := out.result(1); r.GetApply() == nil {
		t.Errorf("the call under way ended with %v, want it applied", r)
	}

	// Calls keep arriving while the stream stops, as from a receiver that
	// hasn't stopped yet: none starts once stop waits.
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() { c.start(applyCall(t, uint64(10+i), "rule")) })
	}
	c.stop()
	wg.Wait()
	for i := range 100 {
		if r := out.result(uint64(10 + i)); r == nil || !r.GetError().GetDraining() {
			t.Fatalf("call %d, after the stream closed, ended with %v; want it refused", 10+i, r)
		}
	}
}

// Stopping cancels the calls still running, and waits for them.
func TestStoppingCancelsWhatStillRuns(t *testing.T) {
	fw := &slowFirewall{Fake: firewall.NewFake(), started: make(chan struct{}, 1), release: make(chan struct{})}
	out := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{
		Logger: slog.New(slog.DiscardHandler), Drivers: resource.NewRegistry(&firewall.Driver{Backend: fw}),
	}, out.send)
	c.start(applyCall(t, 1, "slow"))
	<-fw.started
	c.stop()
	if c.busy() || out.result(1).GetError() == nil || fw.Len() != 0 {
		t.Errorf("after stopping, busy %t, result %v, %d rules; want the call cancelled", c.busy(), out.result(1), fw.Len())
	}
}
