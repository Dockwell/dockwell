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

// What a call that ran out of time says comes down to which time ran out: the
// control plane's, with no answer from the agent, or a wait of the agent's own on
// the server, which says when it answered and nothing of the call's time, however
// long the call had and however near its end the answer came. The agent's clock
// for the call never ends before the control plane's, so an answer that says its
// time ran out is always a wait of its own (#613's review, M1). An agent's plain
// words stay first, and each is still a call out of time.
func TestACallOutOfTimeSaysWhichTimeRanOut(t *testing.T) {
	s := &Session{name: "hz-1"}
	agentSaid := func() error {
		return &resource.ServerError{Server: "hz-1", Err: resource.Plainly("The step's time ran out before web started, so Dockwell stopped waiting.",
			outOfTime{"can't create service web in env_1: context deadline exceeded"})}
	}
	for _, tc := range []struct {
		name          string
		err           error
		timeout, left time.Duration
		want, not     []string
		plain         bool
	}{{
		name:    "no answer",
		err:     context.DeadlineExceeded,
		timeout: 10 * time.Minute,
		want:    []string{"hz-1's agent didn't answer within 10 min, so Dockwell stopped waiting.", "Check those on hz-1, then try again"},
	}, {
		name:    "a wait on the server, after 30 s of 10 min",
		err:     agentSaid(),
		timeout: 10 * time.Minute,
		left:    10*time.Minute - 30*time.Second,
		plain:   true,
		want: []string{
			"hz-1's agent answered after 30 s that a wait on the server ran out, so it stopped: can't create service web in env_1: context deadline exceeded.",
			"Check what it was waiting for on hz-1, then try again",
		},
		not: []string{"didn't answer", "10 min"},
	}, {
		name:    "a wait on the server, on a 1 s call",
		err:     agentSaid(),
		timeout: time.Second,
		left:    800 * time.Millisecond,
		plain:   true,
		want:    []string{"hz-1's agent answered within a second that a wait on the server ran out, so it stopped: "},
		not:     []string{"didn't answer", "1 s"},
	}, {
		name:    "a backup's wait on the server, 20 min before its 24 h end",
		err:     agentSaid(),
		timeout: 24 * time.Hour,
		left:    20 * time.Minute,
		plain:   true,
		want:    []string{"hz-1's agent answered after ", " that a wait on the server ran out, so it stopped: "},
		not:     []string{"didn't answer", "24 h", "half done"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.timedOut(tc.err, tc.timeout, time.Now().Add(tc.left))
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%v isn't a call out of time", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("it says %q, without %q", err, want)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(err.Error(), not) {
					t.Errorf("it says %q, with %q", err, not)
				}
			}
			if tc.plain {
				if plain, _ := resource.Explain(err); plain != "The step's time ran out before web started, so Dockwell stopped waiting." {
					t.Errorf("its plain words read %q", plain)
				}
			}
		})
	}
	other := errors.New("iptables said no")
	if got := s.timedOut(other, 10*time.Minute, time.Now()); !errors.Is(got, other) || got.Error() != other.Error() {
		t.Errorf("a failure that isn't time reads %v", got)
	}
}

// A caller that gave up gets context.Canceled back, even when the agent's answer
// that a wait of its own ran out comes after: the caller left, and nothing reads
// as time running out (#613's review, L3).
func TestACancelledCallerIsCancelledWhateverTheAgentSaysAfter(t *testing.T) {
	kinds := resource.NewRegistry(&firewall.Driver{})
	step := resource.Step{Ref: resource.Ref{Kind: firewall.Kind, Scope: "srv_1", Name: "slow"}, Decision: resource.Decision{Action: resource.ActionCreate}}
	a := NewAgents(kinds, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var s *Session
	s = a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(firewall.Kind)}, WritesReferences: true}, func(msg *agentv1.ConnectResponse) error {
		if c := msg.GetCall(); c != nil {
			cancel()
			s.deliver(&agentv1.CallResult{Id: c.GetId(), Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{
				Message: "can't create firewall.rule slow in srv_1: context deadline exceeded", OutOfTime: true,
			}}})
		}
		return nil
	})
	defer s.close()
	err := s.Apply(ctx, step, &resourcev1.FirewallRule{Port: 8080})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a caller that gave up gets %v, want context.Canceled", err)
	}
	if err != nil && (errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "wait on the server")) {
		t.Errorf("a caller that gave up reads %q, as time running out", err)
	}
}
