// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/agent/desired"
)

// round is one scenario's run: how things stood as it began, and what it did, which
// the invariants are checked against.
type round struct {
	start  time.Time
	before map[string][]box
	// seen are the reconcile events there were as it began.
	seen map[string]bool
	// changes are the changes it made, by the service they change.
	changes map[string]string
	// removed are the services whose container it removed on purpose.
	removed map[string]bool
	// restarted says Docker was killed, so it started containers again itself.
	restarted bool
	// expect is how many reconcile events of each action and service it should
	// leave, as "made web"; any other made or started is one too many.
	expect map[string]int
	// journal is what the agent had written down before the hand-back.
	journal []desired.Entry
}

func (s *stand) begin(ctx context.Context) (*round, error) {
	r := &round{start: time.Now(), changes: map[string]string{}, removed: map[string]bool{}, expect: map[string]int{}, seen: map[string]bool{}}
	var err error
	if r.before, err = s.boxes(ctx); err != nil {
		return nil, err
	}
	events, err := s.reconciled(ctx)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		r.seen[e.ID] = true
	}
	s.probe.take()
	return r, nil
}

// noteJournal keeps what the agent has written down, before it hands it back.
func (s *stand) noteJournal(r *round) error {
	entries, err := s.journal()
	if err != nil {
		return err
	}
	r.journal = append(r.journal, entries...)
	return nil
}

// check says which invariants don't hold now (docs/testing/chaos.md).
func (s *stand) check(ctx context.Context, r *round) []string {
	var bad []string
	if c, err := s.connection(ctx); err != nil || c != connected {
		return []string{fmt.Sprintf("chaos-1 isn't connected: %s %v", c, err)}
	}
	now, err := s.boxes(ctx)
	if err != nil {
		return []string{err.Error()}
	}
	live, err := s.live(ctx)
	if err != nil {
		return []string{err.Error()}
	}
	for _, svc := range services {
		if state := live[svc].ChangeState; state != applied && state != failed {
			bad = append(bad, fmt.Sprintf("2. %s's live change is %s, not ended", svc, state))
		}
		boxes := now[svc]
		if len(boxes) != 1 {
			bad = append(bad, fmt.Sprintf("2. Docker has %d containers of %s, want one: a server holds part of a change, or a twin", len(boxes), svc))
			continue
		}
		b, was := boxes[0], r.before[svc]
		// 1. Nothing running is removed or replaced.
		if len(was) == 1 && was[0].Running && !r.removed[svc] && r.changes[svc] == "" && b.ID != was[0].ID {
			bad = append(bad, fmt.Sprintf("1. %s's container was replaced: %s, then %s", svc, short(was[0].ID), short(b.ID)))
		}
		// 3. Nothing a person stopped is started.
		if svc == "held" {
			if b.Running {
				bad = append(bad, "3. held, which a person stopped, runs")
			} else if len(was) == 1 && !b.Finished.Equal(was[0].Finished) {
				bad = append(bad, fmt.Sprintf("3. held, which a person stopped, was started and stopped again: it stopped at %s, now at %s", was[0].Finished, b.Finished))
			}
		} else if !b.Running {
			bad = append(bad, fmt.Sprintf("1. %s doesn't run (%s)", svc, b.Error))
		}
		// 2 and 5. Each runs what the store says it should. A revision's label can
		// lag, as a rollback to what runs has no step; the image can't.
		if b.Image != live[svc].Image {
			bad = append(bad, fmt.Sprintf("2. %s runs %s, and the store says %s (revision %d, %s %s)", svc, b.Image, live[svc].Image, live[svc].Revision, live[svc].Change, live[svc].ChangeState))
		}
	}
	// 2. No change ends applied with a server holding part of it.
	for svc, id := range r.changes {
		st, err := s.changeState(ctx, id)
		switch {
		case err != nil:
			bad = append(bad, err.Error())
		case st != applied && st != failed:
			bad = append(bad, fmt.Sprintf("2. the change to %s is %s, not ended", svc, st))
		case st == applied && len(now[svc]) == 1 && (now[svc][0].Change != id || now[svc][0].Revision != strconv.FormatInt(live[svc].Revision, 10)):
			b := now[svc][0]
			bad = append(bad, fmt.Sprintf("2. the change to %s, %s, says applied, and its container runs %s, made by %s at revision %s", svc, id, b.Image, b.Change, b.Revision))
		}
	}
	// 5. The store and the agent agree.
	if d, err := s.drift(ctx); err != nil || d != "" {
		bad = append(bad, fmt.Sprintf("5. the control plane says chaos-1 drifted: %s %v", d, err))
	}
	// 4. Every reconcile reaches Activity once.
	bad = append(bad, s.checkReconciled(ctx, r)...)
	return bad
}

func (s *stand) checkReconciled(ctx context.Context, r *round) []string {
	events, err := s.reconciled(ctx)
	if err != nil {
		return []string{err.Error()}
	}
	got := map[string]int{}
	waits := map[string]int{}
	for _, e := range events {
		if r.seen[e.ID] || e.Reconciled == nil {
			continue
		}
		what := strings.TrimPrefix(e.Action, "server.reconciled_")
		key := what + " " + e.Reconciled.Name
		if what == "waiting" {
			waits[key+": "+e.Reconciled.Reason]++
			continue
		}
		got[key]++
	}
	var bad []string
	for _, key := range slices.Sorted(maps.Keys(r.expect)) {
		if got[key] != r.expect[key] {
			bad = append(bad, fmt.Sprintf("4. Activity says %q %d times, want %d", key, got[key], r.expect[key]))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(got)) {
		if _, ok := r.expect[key]; !ok {
			bad = append(bad, fmt.Sprintf("4. Activity says %q %d times, and nothing in this scenario should have", key, got[key]))
		}
	}
	for key, n := range waits {
		if n > 1 {
			bad = append(bad, fmt.Sprintf("4. Activity says %q %d times, want once", key, n))
		}
	}
	// What the agent wrote down before the hand-back is all there.
	for i := range r.journal {
		e := &r.journal[i]
		key := e.What + " " + e.Ref.Name
		if e.What == desired.Waiting {
			if waits[key+": "+e.Reason] == 0 {
				bad = append(bad, fmt.Sprintf("4. the agent wrote down %q (%s), and Activity doesn't say it", key, e.Reason))
			}
		} else if got[key] == 0 {
			bad = append(bad, fmt.Sprintf("4. the agent wrote down %q, and Activity doesn't say it", key))
		}
	}
	return bad
}

// settle waits for every invariant to hold, for at most 60 s once the fault is
// put right, then reads the agent's records with it stopped, and starts it again.
func (s *stand) settle(ctx context.Context, r *round) error {
	if err := s.waitChecks(ctx, r); err != nil {
		return err
	}
	if err := s.checkRecords(ctx); err != nil {
		return err
	}
	// The record check reopens the stream. Allow its journal and any duplicate
	// hand-back to arrive before declaring that each receipt was kept once.
	if err := sleep(ctx, time.Second); err != nil {
		return err
	}
	if err := s.waitChecks(ctx, r); err != nil {
		return err
	}
	if fails := s.probe.take(); len(fails) > 0 {
		return fmt.Errorf("web went unanswered %d times, outside what the scenario broke on purpose; the first: %s", len(fails), fails[0])
	}
	return nil
}

func (s *stand) waitChecks(ctx context.Context, r *round) error {
	var last []string
	deadline := time.Now().Add(60 * time.Second)
	settled := false
	for {
		if last = s.check(ctx, r); len(last) == 0 {
			if settled {
				return nil
			}
			settled = true // a failed change may still be scheduling its rollback
		} else {
			settled = false
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the invariants don't hold 60 s on:\n  %s", strings.Join(last, "\n  "))
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

// checkRecords stops chaos-1's agent, reads its records with its key, and starts
// it again: they hold each service, as the store has it, and nothing else. A record
// can be a revision behind, as a rollback to what runs has no step and the next
// fill brings it up; it can't be ahead, nor would it put back another image.
func (s *stand) checkRecords(ctx context.Context) error {
	s.agent.stop()
	recs, err := s.records()
	if rerr := s.restartAgent(ctx, true); rerr != nil && err == nil {
		err = rerr
	}
	if err != nil {
		return err
	}
	live, err := s.live(ctx)
	if err != nil {
		return err
	}
	var bad []string
	for _, svc := range services {
		r, ok := recs[svc]
		switch {
		case !ok:
			bad = append(bad, "5. chaos-1's agent keeps no record of "+svc)
		case r.Image != live[svc].Image:
			bad = append(bad, fmt.Sprintf("5. chaos-1's agent would put %s back as %s, and the store says %s", svc, r.Image, live[svc].Image))
		case r.Revision > live[svc].Revision:
			bad = append(bad, fmt.Sprintf("5. chaos-1's agent keeps %s at revision %d, ahead of the store's %d", svc, r.Revision, live[svc].Revision))
		}
	}
	if len(recs) != len(services) {
		bad = append(bad, fmt.Sprintf("5. chaos-1's agent keeps %d records, want %d", len(recs), len(services)))
	}
	if len(bad) > 0 {
		return errors.New(strings.Join(bad, "\n  "))
	}
	return nil
}

func short(id string) string { return id[:min(len(id), 12)] }
