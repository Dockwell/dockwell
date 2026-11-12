// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/checks"
)

// standing is a runner's checks as a test sets them.
type standing []checks.Status

func (s standing) Statuses() []checks.Status { return s }
func (standing) Loaded() bool                { return true }

// A check that's down says what its newest failed run said: not a run that
// couldn't start, nor a pass on its way back up.
func TestADownCheckQuotesItsNewestFailedRun(t *testing.T) {
	spec := &resourcev1.AlertCheck{Address: "https://shop.example.com/", DownAfter: 2}
	run := func(ok, skipped bool, said string) *dockwellv1.AlertCheckRun {
		return &dockwellv1.AlertCheckRun{Time: timestamppb.New(t0), Ok: ok, Skipped: skipped, Said: said}
	}
	s := &signals{e: &Evaluator{Checks: standing{{
		ID: "chk_1", Name: "shop", Spec: spec, State: dockwellv1.AlertCheckState_ALERT_CHECK_STATE_DOWN, Since: t0,
		Runs: []*dockwellv1.AlertCheckRun{
			run(true, false, ""),
			run(false, true, "skipped: every slot was taken"),
			run(false, false, "502, outside 200 to 399"),
			run(false, false, "no answer within 10s"),
		},
	}}}}
	got := s.checksDown()
	if len(got) != 1 || got[0].verdict != holds || !strings.Contains(got[0].detail, ": 502, outside 200 to 399, 2 runs in a row") {
		t.Errorf("a down check says %+v", got)
	}
}

// What each state of a check says of its alert (ADR 0066): failing and unreadable
// keep what's open, and only up clears it.
func TestWhatEachStateOfACheckSaysOfItsAlert(t *testing.T) {
	spec := &resourcev1.AlertCheck{Address: "https://shop.example.com/"}
	for _, tc := range []struct {
		name    string
		status  checks.Status
		verdict verdict
	}{
		{"up", checks.Status{State: dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP}, fine},
		{"down", checks.Status{State: dockwellv1.AlertCheckState_ALERT_CHECK_STATE_DOWN}, holds},
		{"failing", checks.Status{State: dockwellv1.AlertCheckState_ALERT_CHECK_STATE_FAILING}, nearly},
		{"waiting", checks.Status{State: dockwellv1.AlertCheckState_ALERT_CHECK_STATE_WAITING}, unknown},
		{"up when last read, unreadable now", checks.Status{State: dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP, Problem: checks.Unreadable}, unknown},
	} {
		tc.status.ID, tc.status.Spec = "chk_1", spec
		s := &signals{e: &Evaluator{Checks: standing{tc.status}}}
		if got := s.checksDown(); len(got) != 1 || got[0].verdict != tc.verdict {
			t.Errorf("%s: %+v, want %v", tc.name, got, tc.verdict)
		}
	}
}

// A catalog source that follows a branch holds its alert while the last look left
// it short of the branch's commit, from the first look that failed; one at its
// branch's commit clears it, and one not looked at since a start keeps what's open
// (ADR 0077).
func TestWhatAFollowingCatalogSaysOfItsAlert(t *testing.T) {
	since := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	s := &signals{e: &Evaluator{Catalogs: func(context.Context) []Following {
		return []Following{
			{Name: "community", Branch: "main", Looked: true, Problem: "None of the entries at 89abcdef0123 pass Dockwell's checks. It stays at 0123456789ab.", Since: since},
			{Name: "dockwell", Branch: "main", Looked: true},
			{Name: "fresh", Branch: "main"},
		}
	}}}
	got := s.catalogsStuck(t.Context())
	if len(got) != 3 {
		t.Fatalf("findings: %+v", got)
	}
	if got[0].verdict != holds || !got[0].since.Equal(since) || got[0].title != "The catalog community can't follow main" ||
		!strings.Contains(got[0].detail, "It stays at 0123456789ab. What it offers stays as it was") || got[0].path != "/catalog/sources" {
		t.Errorf("a stuck catalog: %+v", got[0])
	}
	if got[1].verdict != fine || got[2].verdict != unknown {
		t.Errorf("a catalog at its branch: %v; one not looked at: %v", got[1].verdict, got[2].verdict)
	}
	if (&signals{e: &Evaluator{}}).catalogsStuck(t.Context()) != nil {
		t.Error("with no follower, it watched something")
	}
}
