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
		want: "Dockwell can't create http, as a wait on this server ran out with the step's own time left: iptables didn't answer: context deadline exceeded. " +
			"Check what http waits on, on this server, then try again.",
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
		want: "Dockwell stopped before it could create http, as the step was called off: its change was cancelled, the control plane stopped waiting, or the agent stopped. " +
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

// ownWait is a firewall whose rule waits on something of its own that runs out,
// and that says so the way a driver does, from what stopped it (StopOf), on the
// context of its own wait: only the step Local.Apply marks tells that wait from the
// step's time (#615's review, M1).
type ownWait struct{ *firewall.Fake }

func (ownWait) Put(ctx context.Context, _ string, _ *resourcev1.FirewallRule) error {
	wait, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	<-wait.Done()
	words := map[resource.Stop]string{
		resource.StepTime:  "The step's time ran out before the rule went in.",
		resource.InnerWait: "The firewall didn't answer in time, with the step's own time left.",
		resource.CalledOff: "The step was called off before the rule went in.",
	}
	return resource.Plainly(words[resource.StopOf(wait, wait.Err())], wait.Err())
}

// Local.Apply marks the step's own context, so a driver's wait of its own that ran
// out reads as that wait, not as the step's time.
func TestADriversOwnWaitReadsAsItsOwnThroughApply(t *testing.T) {
	l := resource.Local{Drivers: resource.NewRegistry(&firewall.Driver{Backend: ownWait{Fake: firewall.NewFake()}})}
	step := resource.Step{Ref: resource.Ref{Kind: firewall.Kind, Scope: "srv_1", Name: "http"}, Decision: resource.Decision{Action: resource.ActionCreate}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	err := l.Apply(ctx, step, &resourcev1.FirewallRule{Port: 80})
	if plain, _ := resource.Explain(err); plain != "The firewall didn't answer in time, with the step's own time left." {
		t.Errorf("it reads %q, want the driver's own wait", plain)
	}
}

// What stopped a step goes by when its failure happened, where the driver noted
// it before a cleanup that outlasted the step: an inner wait that ran out reads as
// that wait, though the step ended while its leftovers were cleaned up, and a
// failure from before the step was called off isn't the step's being called off
// (#615's review, L2).
func TestAStepsStopGoesByWhenItFailed(t *testing.T) {
	inner := fmt.Errorf("docker can't start web: %w", context.DeadlineExceeded)
	step, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	marked := resource.WithStep(step)
	failed := resource.FailedAt(inner, time.Now())
	<-step.Done() // the cleanup outlasts the step
	if got := resource.StopOf(marked, failed); got != resource.InnerWait {
		t.Errorf("a wait that ran out before the step did reads %v, want an inner wait", got)
	}
	if got := resource.StopOf(marked, inner); got != resource.StepTime {
		t.Errorf("with no note of when, it reads %v, as of now, the step's time", got)
	}
	if failed.Error() != inner.Error() || !errors.Is(failed, context.DeadlineExceeded) {
		t.Errorf("a noted failure reads %q, %v", failed, failed)
	}

	called, callOff := context.WithCancel(t.Context())
	marked = resource.WithStep(called)
	failed = resource.FailedAt(inner, time.Now().Add(-5*time.Second))
	callOff()
	time.Sleep(10 * time.Millisecond) // for the step's note of when it was called off
	if got := resource.StopOf(marked, failed); got != resource.InnerWait {
		t.Errorf("a wait that ran out before the step was called off reads %v, want an inner wait", got)
	}
	if got := resource.StopOf(marked, fmt.Errorf("can't read it: %w", context.Canceled)); got != resource.CalledOff {
		t.Errorf("a failure as the step was called off reads %v, want called off", got)
	}
}

// A step whose context ended for a cause of its own isn't read as its time or as
// called off: its cause's words say what (#615's review, L3).
func TestAStepEndedForACauseOfItsOwnIsNotAStop(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errors.New("the server is being replaced"))
	if got := resource.StopOf(resource.WithStep(ctx), context.Canceled); got != resource.NotStopped {
		t.Errorf("it reads %v, want not stopped", got)
	}
	timed, stop := context.WithTimeoutCause(t.Context(), time.Nanosecond, errors.New("the drain ran out"))
	defer stop()
	<-timed.Done()
	if got := resource.StopOf(resource.WithStep(timed), context.DeadlineExceeded); got != resource.NotStopped {
		t.Errorf("a deadline with a cause of its own reads %v, want not stopped", got)
	}
}
