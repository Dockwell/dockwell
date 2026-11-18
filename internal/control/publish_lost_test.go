// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/json"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// loseNextPut has the stand-in write the tunnel's next configuration and lose the
// answer, as a crash between Cloudflare's write and Dockwell's own would.
func (s *publishStand) loseNextPut() {
	var lost atomic.Bool
	s.cf.LosePut = func(string) bool { return lost.CompareAndSwap(false, true) }
}

// failedApply applies a change whose step at Cloudflare fails, and waits for it to end.
func (s *publishStand) failedApply(t *testing.T, ch *dockwellv1.Change) {
	t.Helper()
	w := watch(t, s.person, ch.GetId())
	if _, err := applyChange(t, s.person, &dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("%s applied, though its write's answer was lost", ch.GetSummary())
	}
}

// settled waits until no change is applying, as a rollback goes on after its change
// says it failed, and a plan made meanwhile would be refused as stale.
func (s *publishStand) settled(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		listed, err := s.person.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(listed.Msg.GetChanges(), func(c *dockwellv1.Change) bool {
			return c.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLYING
		}) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a change is still applying")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// documentIs waits for the tunnel's document to read want, as a rollback ends after
// the change says it failed.
func (s *publishStand) documentIs(t *testing.T, want, why string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		doc, _ := s.cf.Document(webOne)
		if jsonEqual(t, doc, json.RawMessage(want)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s", why, doc)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A Move it to Dockwell whose write lands at Cloudflare and whose answer is lost
// keeps the rule made by hand, recorded before the write, so the rollback puts it
// back whole (the re-check's A-M2: this fails if the record follows the write).
func TestAMoveWhoseAnswerIsLostPutsTheRuleBack(t *testing.T) {
	t.Parallel()
	withHand := `{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},` + wikiByHand + `,{"service":"http_status:404"}]}`
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		withAppsPort(s)
		s.cf.SetDocument(webOne, json.RawMessage(withHand))
		s.loseNextPut()
		s.failedApply(t, propose(t, s.person, "Publish wiki", s.takingOver("wiki")))
		s.documentIs(t, withHand, "the rollback should put the rule made by hand back whole")
		s.settled(t)
		// And moved again, then removed, it's back as it was.
		s.applied(t, propose(t, s.person, "Publish wiki", s.takingOver("wiki")))
		s.applied(t, propose(t, s.person, "Remove wiki", s.removed("wiki")))
		s.documentIs(t, withHand, "removed, the rule made by hand should be back as it was")
	})
}

// A removal of a rule Dockwell took on whose write lands and whose answer is lost
// keeps the rule made by hand, recorded before the write, so the rollback puts it
// back whole, its options too (the re-check's A-M1).
func TestARemovalWhoseAnswerIsLostKeepsTheRuleMadeByHand(t *testing.T) {
	t.Parallel()
	byHand := `{"hostname":"wiki.example.com","service":"http://127.0.0.1:80","originRequest":{"noTLSVerify":true}}`
	withHand := `{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},` + byHand + `,{"service":"http_status:404"}]}`
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		s.cf.SetDocument(webOne, json.RawMessage(withHand))
		s.applied(t, propose(t, s.person, "Publish wiki", s.takingOver("wiki")))
		s.documentIs(t, withHand, "taking it on writes nothing")
		s.loseNextPut()
		s.failedApply(t, propose(t, s.person, "Remove wiki", s.removed("wiki")))
		s.documentIs(t, withHand, "the rollback should put the rule made by hand back whole")
		s.settled(t)
		// Removed again, then undone, it's back whole.
		done := s.applied(t, propose(t, s.person, "Remove wiki", s.removed("wiki")))
		undo, err := s.person.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: done.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		s.applied(t, undo.Msg.GetChange())
		s.documentIs(t, withHand, "undone, the rule made by hand should be back whole")
	})
}

// A route that stops publishing a name Dockwell published, keeping the domain,
// takes off what Dockwell wrote itself: its record and its zone rule. Removed after,
// nothing of it is left at Cloudflare or in the store (the re-check's A-M3).
func TestStoppingPublishingTakesOffWhatDockwellWrote(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		s.applied(t, propose(t, s.person, "Publish docs", s.published("docs", "docs.example.com")))
		if !s.holdsRule(t, "*.example.com") || len(s.cf.Records("example.com")) != 1 {
			t.Fatalf("published: %v %v", rulesOf(t, s.cf), s.cf.Records("example.com"))
		}
		stop := propose(t, s.person, "Stop publishing docs", s.unpublished("docs", "docs.example.com"))
		if steps := stepsOf(stop, dockwellv1.ProviderAction_PROVIDER_ACTION_DELETE_RECORD); len(steps) != 1 || steps[0].GetRouting() != "" {
			t.Fatalf("Dockwell's own record comes off, not let go: %v", providerSteps(stop))
		}
		if steps := stepsOf(stop, dockwellv1.ProviderAction_PROVIDER_ACTION_REMOVE_RULE); len(steps) != 1 {
			t.Fatalf("Dockwell's own zone rule comes off: %v", providerSteps(stop))
		}
		s.applied(t, stop)
		s.applied(t, propose(t, s.person, "Remove docs", s.removed("docs")))
		if got := s.cf.Records("example.com"); len(got) != 0 {
			t.Fatalf("records left at Cloudflare: %v", got)
		}
		if s.holdsRule(t, "*.example.com") || slices.Contains(rulesOf(t, s.cf), "*.example.com=http://127.0.0.1:80") {
			t.Fatalf("the zone rule is left: %v", rulesOf(t, s.cf))
		}
	})
}

// A route that took on a rule made by hand and stops publishing, keeping the domain,
// lets go of that rule, and of Dockwell's record the name still goes through, so the
// name keeps working; the store forgets both (H2's own case, the re-check's A-M3).
func TestStoppingPublishingLetsGoWhereAHandRuleNeedsIt(t *testing.T) {
	t.Parallel()
	byHand := `{"hostname":"wiki.example.com","service":"http://127.0.0.1:80"}`
	withHand := `{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},` + byHand + `,{"service":"http_status:404"}]}`
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		s.cf.SetDocument(webOne, json.RawMessage(withHand))
		s.applied(t, propose(t, s.person, "Publish wiki", s.takingOver("wiki")))
		stop := propose(t, s.person, "Stop publishing wiki", s.unpublished("wiki", "wiki.example.com"))
		if steps := stepsOf(stop, dockwellv1.ProviderAction_PROVIDER_ACTION_DELETE_RECORD); len(steps) != 1 || steps[0].GetRouting() != "Dockwell lets go of it, and it stays as it is" {
			t.Fatalf("the record the hand rule needs is let go: %v", providerSteps(stop))
		}
		s.applied(t, stop)
		s.documentIs(t, withHand, "the rule made by hand stays as it is")
		if got := s.cf.Records("example.com"); len(got) != 1 || got[0].Name != "wiki.example.com" {
			t.Fatalf("the name's record stays: %v", got)
		}
		if s.holdsRule(t, "wiki.example.com") {
			t.Fatal("the store still holds the rule it let go")
		}
	})
}
