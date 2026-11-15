// SPDX-License-Identifier: AGPL-3.0-only

package gitproviders

import (
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/github"
)

// An installation checks every branch each minute up to 30, and past that takes
// turns, each branch as often as the turns come round.
func TestPastThirtyBranchesTakeTurns(t *testing.T) {
	t.Parallel()
	picked, next, every := due(12, 0, pollCap)
	if len(picked) != 12 || next != 0 || every != time.Minute {
		t.Errorf("12 branches: %v, next %d, every %s", picked, next, every)
	}
	seen := map[int]int{}
	turn := 0
	for range 3 {
		picked, turn, every = due(75, turn, pollCap)
		if len(picked) != pollCap || every != 3*time.Minute {
			t.Fatalf("75 branches checks %d this minute, each every %s", len(picked), every)
		}
		for _, i := range picked {
			seen[i]++
		}
	}
	for i := range 75 {
		if seen[i] == 0 {
			t.Errorf("branch %d had no turn in 3 minutes", i)
		}
	}
	if len(seen) != 75 {
		t.Errorf("3 minutes reached %d of 75", len(seen))
	}
}

// A secondary limit waits as long as GitHub asked, then twice as long each time it
// comes again, to 15 minutes; few calls left wait for the hour; an answer that
// passed starts the next back-off over.
func TestPollingBacksOffAsGitHubAsks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	w := &watching{state: dockwellv1.GitHubDelivery_STATE_POLLING}
	w.slowDown(github.Rate{Limited: true, RetryAfter: 90 * time.Second}, now)
	if w.state != dockwellv1.GitHubDelivery_STATE_BACKING_OFF || !w.backoffUntil.Equal(now.Add(90*time.Second)) {
		t.Fatalf("a secondary limit backs off to %s, %v", w.backoffUntil, w.state)
	}
	steps := []time.Duration{3 * time.Minute, 6 * time.Minute, 12 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for _, want := range steps {
		w.slowDown(github.Rate{Limited: true}, now)
		if w.backoffStep != want {
			t.Errorf("the back-off is %s, want %s", w.backoffStep, want)
		}
	}
	w.slowDown(github.Rate{Limit: 5000, Remaining: 4000}, now)
	if w.backoffStep != 0 {
		t.Errorf("an answer that passed left the back-off at %s", w.backoffStep)
	}
	reset := now.Add(40 * time.Minute)
	w.state = dockwellv1.GitHubDelivery_STATE_POLLING
	w.slowDown(github.Rate{Limit: 5000, Remaining: 499, Reset: reset}, now)
	if w.state != dockwellv1.GitHubDelivery_STATE_BACKING_OFF || !w.backoffUntil.Equal(reset) {
		t.Errorf("a tenth of the hour's calls left waits until %s, %v", w.backoffUntil, w.state)
	}
}
