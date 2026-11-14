// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// Waves is how a change runs over its servers, wave by wave (ADR 0094): each wave
// starts once every step of the one before has passed and the pause after it is
// over, and a step that fails stops the change there unless KeepGoing. Within a
// wave, servers go one after another, as every change's steps do.
type Waves struct {
	// Order is each wave's servers, by ID, in the order they go.
	Order [][]string `json:",omitempty"`
	// Pause is how long to wait between one wave's end and the next's start.
	Pause time.Duration `json:",omitempty"`
	// KeepGoing goes on to the next server after a step fails. The change still
	// fails, naming each server that did.
	KeepGoing bool `json:",omitempty"`
}

// Rollout is how a proposal asks its change to run over its servers (ADR 0094): the
// servers in the order they go, a canary of one first, then waves of Batch, or of
// a quarter of them rounded up when Batch is 0. The plan makes its Waves of those
// servers that take a step, so a canary is one that acts.
type Rollout struct {
	Servers   []string
	Canary    bool
	Batch     int
	Pause     time.Duration
	KeepGoing bool
}

// planWith is what a plan is made with beyond its items: the server a removal
// takes out of the fleet (ADR 0090), and the rollout a change was proposed with
// or the waves it was planned in (ADR 0094).
type planWith struct {
	removal *Removal
	rollout *Rollout
	waves   *Waves
}

// wavesOf is how plan runs over the servers that take a step, in the order the
// rollout gives them: as with.waves says for a change planned before, or as
// with.rollout asks, with the servers it asked of that take none. Fewer than two
// servers have no waves.
func (with planWith) wavesOf(plan *Plan) (waves *Waves, idle []string) {
	if with.rollout == nil {
		return with.waves, nil
	}
	r := with.rollout
	acting := make([]string, 0, len(r.Servers))
	for _, s := range r.Servers {
		if slices.ContainsFunc(plan.Servers, func(sp ServerPlan) bool { return sp.Server == s && len(sp.Steps) > 0 }) {
			acting = append(acting, s)
		} else {
			idle = append(idle, s)
		}
	}
	return wavesOf(acting, r.Canary, r.Batch, r.Pause, r.KeepGoing), idle
}

// wavesOf splits servers, in the order given, into waves: one server on its own
// first when canary, then waves of batch servers, or of a quarter of them rounded
// up when batch is 0. Fewer than two servers have no waves.
func wavesOf(servers []string, canary bool, batch int, pause time.Duration, keepGoing bool) *Waves {
	if len(servers) < 2 {
		return nil
	}
	rest := slices.Clone(servers)
	w := &Waves{Pause: pause, KeepGoing: keepGoing}
	if canary {
		w.Order = append(w.Order, rest[:1])
		rest = rest[1:]
	}
	if batch <= 0 {
		batch = (len(servers) + 3) / 4
	}
	for len(rest) > 0 {
		n := min(batch, len(rest))
		w.Order = append(w.Order, rest[:n])
		rest = rest[n:]
	}
	return w
}

// waveOf is the wave server is in, from 1, or 0 for none.
func (w *Waves) waveOf(server string) int {
	if w == nil {
		return 0
	}
	for i, wave := range w.Order {
		if slices.Contains(wave, server) {
			return i + 1
		}
	}
	return 0
}

// fingerprint is what the plan's fingerprint covers of the waves.
func (w *Waves) fingerprint() string {
	if w == nil {
		return ""
	}
	return fmt.Sprintf("waves %v|%s|%t\n", w.Order, w.Pause, w.KeepGoing)
}

// wavesIn is the waves change c runs by, as its plan says, or nil.
func wavesIn(c store.Change) *Waves {
	p, err := DecodePlan(c.Plan)
	if err != nil {
		return nil
	}
	return p.Waves
}

// endedBefore is when the last step of the waves before wave ended, as progress has
// it: the latest end of any of their servers' steps, or zero where none has. A wave
// found done after a later one, as a resume finds it, still counts.
func (w *Waves) endedBefore(wave int, progress *Progress) time.Time {
	var at time.Time
	if w == nil || wave < 2 || progress == nil {
		return at
	}
	for i := range progress.Steps {
		s := &progress.Steps[i]
		if before := w.waveOf(s.Server); before > 0 && before < wave && s.Finished.After(at) {
			at = s.Finished
		}
	}
	return at
}

// pauseUntil waits until at, or until ctx ends or the engine stops, which it
// reports as false.
func (e *Engine) pauseUntil(ctx context.Context, at time.Time) bool {
	for {
		left := at.Sub(e.now())
		if left <= 0 {
			return true
		}
		if e.isStopping() {
			return false
		}
		t := time.NewTimer(min(left, time.Second))
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
}

// wentOn is a step that failed in a run that went on past it, by its place in the
// change's progress, with what it failed with.
type wentOn struct {
	index int
	err   error
}

// failGoneOn fails change c if any of its steps failed, as its progress records them
// across every run of it, naming each server that did: a change that went on past a
// failure (ADR 0094). It returns nil when none did. A failure of this run keeps its
// own error; one from a run before has what its step recorded.
func (e *Engine) failGoneOn(ctx context.Context, c store.Change, now []wentOn) error {
	progress := e.progress(c.ID)
	if progress == nil {
		return nil
	}
	names := map[string]string{}
	if p, err := DecodePlan(c.Plan); err == nil {
		for i := range p.Servers {
			names[p.Servers[i].Server] = p.Servers[i].Name
		}
	}
	var servers []string
	first := -1
	for i := range progress.Steps {
		s := &progress.Steps[i]
		if s.State != StepFailed {
			continue
		}
		if first < 0 {
			first = i
		}
		if name := cmp.Or(names[s.Server], s.Server); !slices.Contains(servers, name) {
			servers = append(servers, name)
		}
	}
	if first < 0 {
		return nil
	}
	s := &progress.Steps[first]
	name := cmp.Or(names[s.Server], s.Server)
	cause := resource.Plainly(s.Message, errors.New(cmp.Or(s.Detail, s.Message)))
	if j := slices.IndexFunc(now, func(f wentOn) bool { return f.index == first }); j >= 0 {
		cause = now[j].err
	}
	plain, _ := resource.Explain(stepFailure(name, cause))
	if len(servers) > 1 {
		plain = fmt.Sprintf("%d servers failed: %s. The first: %s", len(servers), human.List(servers), plain)
	}
	return e.fail(ctx, c, s.Ref, cause, plain)
}
