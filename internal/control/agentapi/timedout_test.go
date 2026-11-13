// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

// What a call that ran out of time says comes down to which time ran out: the
// control plane's, with no answer from the agent; the call's own on the agent, which
// answers just before the control plane's deadline; or a wait of the agent's on the
// server, long before it, which says when it answered and nothing of the call's
// time. An agent's plain words stay first, and each is still a call out of time.
func TestACallOutOfTimeSaysWhichTimeRanOut(t *testing.T) {
	s := &Session{name: "hz-1"}
	const timeout = 10 * time.Minute
	agentSaid := func() error {
		return &resource.ServerError{Server: "hz-1", Err: resource.Plainly("The step's time ran out before web started, so Dockwell stopped waiting.",
			outOfTime{"can't create service web in env_1: context deadline exceeded"})}
	}
	for _, tc := range []struct {
		name      string
		err       error
		left      time.Duration
		want, not []string
		plain     bool
	}{{
		name: "no answer",
		err:  context.DeadlineExceeded,
		want: []string{"hz-1's agent didn't answer within 10 min, so Dockwell stopped waiting.", "Check those on hz-1, then try again"},
	}, {
		name:  "the call's own time, on the agent",
		err:   agentSaid(),
		left:  200 * time.Millisecond,
		plain: true,
		want:  []string{"hz-1's agent ran out of the 10 min this had, so it stopped: can't create service web in env_1: context deadline exceeded.", "Check those on hz-1, then try again"},
		not:   []string{"didn't answer"},
	}, {
		name:  "a wait on the server, after 30 s",
		err:   agentSaid(),
		left:  timeout - 30*time.Second,
		plain: true,
		want:  []string{"hz-1's agent answered after 30 s that a wait on the server ran out, so it stopped: can't create service web in env_1: context deadline exceeded.", "Check that on hz-1, then try again"},
		not:   []string{"didn't answer", "10 min"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.timedOut(tc.err, timeout, time.Now().Add(tc.left))
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
	if got := s.timedOut(other, timeout, time.Now()); !errors.Is(got, other) || got.Error() != other.Error() {
		t.Errorf("a failure that isn't time reads %v", got)
	}
}
