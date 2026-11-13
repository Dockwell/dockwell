// SPDX-License-Identifier: AGPL-3.0-only

package resource_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
)

// What stopped a step is told from the step's own context, however deep under it
// the failure comes, and under however many waits of its own: the step's time, a
// wait inside it with the step's time left, or the step called off. With no step
// marked, the context it's given is the step's (#612).
func TestAStepsStopIsToldFromItsOwnContext(t *testing.T) {
	expired := func(ctx context.Context) context.Context {
		out, cancel := context.WithTimeout(ctx, time.Nanosecond)
		t.Cleanup(cancel)
		<-out.Done()
		return out
	}
	called := func(ctx context.Context) context.Context {
		out, cancel := context.WithCancel(ctx)
		cancel()
		return out
	}
	alive := func(ctx context.Context) context.Context {
		out, cancel := context.WithTimeout(ctx, time.Minute)
		t.Cleanup(cancel)
		return out
	}
	timedOut := fmt.Errorf("can't read it: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want resource.Stop
	}{
		{"the step's time", resource.WithStep(expired(t.Context())), timedOut, resource.StepTime},
		{"the step's time, under a wait of its own", alive(resource.WithStep(expired(t.Context()))), timedOut, resource.StepTime},
		{"a wait inside the step, with its time left", expired(resource.WithStep(alive(t.Context()))), timedOut, resource.InnerWait},
		{"a wait inside a wait inside the step", expired(alive(resource.WithStep(alive(t.Context())))), timedOut, resource.InnerWait},
		{"a deadline from below, with the step's time left", resource.WithStep(alive(t.Context())), timedOut, resource.InnerWait},
		{"called off", resource.WithStep(called(t.Context())), fmt.Errorf("can't read it: %w", context.Canceled), resource.CalledOff},
		{"called off, under a wait of its own", alive(resource.WithStep(called(t.Context()))), timedOut, resource.CalledOff},
		{"not a wait", resource.WithStep(expired(t.Context())), errors.New("iptables said no"), resource.NotStopped},
		{"no step marked, its time ran out", expired(t.Context()), timedOut, resource.StepTime},
		{"no step marked, called off", called(t.Context()), context.Canceled, resource.CalledOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resource.StopOf(tc.ctx, tc.err); got != tc.want {
				t.Errorf("it reads %v, want %v", got, tc.want)
			}
		})
	}
}

// stopping is a firewall whose rule stops as stop says, in the step's context.
type stopping struct {
	*firewall.Fake
	stop func(ctx context.Context) error
}

func (s stopping) Put(ctx context.Context, _ string, _ *resourcev1.FirewallRule) error {
	return s.stop(ctx)
}

// A step a driver couldn't carry out says which wait ended, when one did: the
// step's own time, a wait inside it with the step's time left, as Docker not
// answering in time is, or the step called off. Only the first reads as the step's
// time running out (#612).
func TestAStepThatStoppedSaysWhatStoppedIt(t *testing.T) {
	ref := resource.Ref{Kind: firewall.Kind, Scope: "srv_1", Name: "http"}
	step := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}
	for _, tc := range []struct {
		name       string
		stop       func(ctx context.Context) error
		step       func() (context.Context, context.CancelFunc)
		want, not  string
		stillAWait error
	}{{
		name: "the step's time",
		stop: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		step: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(t.Context(), 20*time.Millisecond)
		},
		want:       "The step's time ran out before Dockwell could create http, so it stopped waiting. Check it on this server, then try again.",
		stillAWait: context.DeadlineExceeded,
	}, {
		name: "a wait inside it",
		stop: func(ctx context.Context) error {
			wait, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
			defer cancel()
			<-wait.Done()
			return fmt.Errorf("iptables didn't answer: %w", wait.Err())
		},
		step: func() (context.Context, context.CancelFunc) { return context.WithTimeout(t.Context(), time.Minute) },
		want: "Dockwell couldn't create http, as a wait on this server ran out with the step's own time left: iptables didn't answer: context deadline exceeded. " +
			"Check it on this server, then try again.",
		not:        "The step's time",
		stillAWait: context.DeadlineExceeded,
	}, {
		name: "called off",
		stop: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		step: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(t.Context())
			time.AfterFunc(20*time.Millisecond, cancel)
			return ctx, cancel
		},
		want: "Dockwell stopped before it could create http, as the step was called off: its change was cancelled, or the control plane stopped waiting for it. " +
			"Run the change again if you still want it.",
		not:        "time ran out",
		stillAWait: context.Canceled,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			l := resource.Local{Drivers: resource.NewRegistry(&firewall.Driver{Backend: stopping{Fake: firewall.NewFake(), stop: tc.stop}})}
			ctx, cancel := tc.step()
			defer cancel()
			err := l.Apply(ctx, step, &resourcev1.FirewallRule{Port: 80})
			plain, _ := resource.Explain(err)
			if plain != tc.want {
				t.Errorf("it reads %q, want %q", plain, tc.want)
			}
			if tc.not != "" && strings.Contains(plain, tc.not) {
				t.Errorf("it reads %q, with %q", plain, tc.not)
			}
			if !errors.Is(err, tc.stillAWait) {
				t.Errorf("%v isn't %v any more", err, tc.stillAWait)
			}
		})
	}
}
