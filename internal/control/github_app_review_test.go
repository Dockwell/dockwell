// SPDX-License-Identifier: AGPL-3.0-only

package control

// The review of #253's slice 2 (dockwell-briefs/reviews/github-app-slice2-review.md),
// each test named for the finding it holds.

import (
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Review M1: a branch pushed back to a commit polling deployed before is a push
// too, as a rollback by force-push is, and deploys.
func TestReviewPollingDeploysABranchMovedBackToAnEarlierCommit(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	apps := a.f.server.apps
	a.gh.mu.Lock()
	a.gh.deliveries = `[{"id":2,"guid":"g2","event":"push","status_code":0}]`
	a.gh.mu.Unlock()
	apps.CheckDeliveries(ctx)

	second := a.commit("Second")
	apps.Poll(ctx)
	a.ended(t, a.outcome(t, a.web.GetId(), "poll-"+second, dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING).GetChangeId())
	third := a.commit("Third")
	apps.Poll(ctx)
	a.ended(t, a.outcome(t, a.web.GetId(), "poll-"+third, dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING).GetChangeId())
	if got := a.service(t, "", "web").GetSpec().GetBuild().GetGit().GetCommit(); got != third {
		t.Fatalf("web builds %s, not %s", got, third)
	}

	// Third was bad: main is force-pushed back to Second.
	a.repo.Ref("refs/heads/main", second)
	a.mu.Lock()
	a.head = second
	a.mu.Unlock()
	apps.Poll(ctx)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if a.service(t, "", "web").GetSpec().GetBuild().GetGit().GetCommit() == second {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("main names %s again, and web still builds %s: polling took the rollback for a commit it had deployed", second, third)
}
