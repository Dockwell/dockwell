// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// StepState is how a step of an applying change is going.
type StepState int

// Step states.
const (
	StepQueued  StepState = iota + 1 // waiting for the steps before it
	StepRunning                      // running now
	StepDone                         // done, and verified
	StepFailed                       // failed; the message says why
	StepSkipped                      // not run, because a step before it failed
	StepWaiting                      // waiting for its server to reconnect
)

var stepStateNames = [...]string{"", "queued", "running", "done", "failed", "skipped", "waiting"}

func (s StepState) String() string {
	if s > 0 && int(s) < len(stepStateNames) {
		return stepStateNames[s]
	}
	return fmt.Sprintf("state(%d)", int(s))
}

// Final reports a state a step doesn't leave in this run.
func (s StepState) Final() bool { return s == StepDone || s == StepFailed || s == StepSkipped }

// StepProgress is one step of an applying change and how it is going.
type StepProgress struct {
	Server     string          `json:"server"`
	ResourceID string          `json:"resource_id"`
	Ref        resource.Ref    `json:"ref"`
	Action     resource.Action `json:"action"`
	State      StepState       `json:"state"`
	// Message says what is happening, for people, or why the step failed.
	Message  string    `json:"message,omitempty"`
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
}

// Progress is how each step of an applying change is going: the plan's
// steps, server by server, each in the order it runs there.
type Progress struct {
	Steps []StepProgress `json:"steps"`
}

func (p *Progress) clone() Progress { return Progress{Steps: slices.Clone(p.Steps)} }

// index finds a step by its server and resource, or -1.
func (p *Progress) index(server, resourceID string) int {
	return slices.IndexFunc(p.Steps, func(s StepProgress) bool { return s.Server == server && s.ResourceID == resourceID })
}

// Update is one event of a watched change: a step moved on, or the change
// did.
type Update struct {
	// State is the change's state, when it moved on; empty for a step.
	State string
	// Progress is the change's progress when its state moved on, if it
	// runs here. Reading the change later may find it further on.
	Progress *Progress
	// Index is where Step is in the change's progress.
	Index int
	Step  *StepProgress
}

// watcherBuffer is how far a watcher may fall behind before it is
// dropped; it watches again and gets a fresh snapshot.
const watcherBuffer = 256

// Watch follows a change: it returns a channel of updates from now on,
// which closes when ctx is done, or when the watcher falls too far behind
// and should watch again. Read the change after calling Watch, and apply
// each update to it, so nothing is missed between the two.
func (e *Engine) Watch(ctx context.Context, changeID string) <-chan Update {
	ch := make(chan Update, watcherBuffer)
	e.pmu.Lock()
	if e.watchers[changeID] == nil {
		e.watchers[changeID] = map[chan Update]struct{}{}
	}
	e.watchers[changeID][ch] = struct{}{}
	e.pmu.Unlock()
	go func() {
		<-ctx.Done()
		e.pmu.Lock()
		defer e.pmu.Unlock()
		e.dropWatcherLocked(changeID, ch)
	}()
	return ch
}

func (e *Engine) dropWatcherLocked(changeID string, ch chan Update) {
	ws := e.watchers[changeID]
	if _, ok := ws[ch]; !ok {
		return
	}
	delete(ws, ch)
	close(ch)
	if len(ws) == 0 {
		delete(e.watchers, changeID)
	}
}

func (e *Engine) publishLocked(changeID string, u Update) {
	for ch := range e.watchers[changeID] {
		select {
		case ch <- u:
		default:
			// Too far behind: drop it, and it watches again from a snapshot.
			e.dropWatcherLocked(changeID, ch)
		}
	}
}

// published tells a change's watchers its state moved on.
func (e *Engine) published(changeID, state string) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	u := Update{State: state}
	if p, ok := e.live[changeID]; ok {
		c := p.clone()
		u.Progress = &c
	}
	e.publishLocked(changeID, u)
}

// track starts tracking the progress of a change that runs in this
// process, from its progress so far.
func (e *Engine) track(changeID string, p Progress) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	e.live[changeID] = &p
}

// own holds a change in this process, and reports false if it was held
// already.
func (e *Engine) own(changeID string) bool {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.owned[changeID] {
		return false
	}
	e.owned[changeID] = true
	return true
}

// ownOrResumeLater is own for Resume: a change held already is running
// here, or being resumed, so Resume runs again once that change lets go,
// in case it ends waiting for a server that is back by then.
func (e *Engine) ownOrResumeLater(changeID string) bool {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.owned[changeID] {
		e.waiters[changeID] = true
		return false
	}
	e.owned[changeID] = true
	return true
}

// claim claims the items' resources for a change held here, unless another
// change holds one. A rollback takes over the claims of the change it
// rolls back. With resumeLater, a refusal asks for Resume once the change
// holding the resource lets go.
func (e *Engine) claim(changeID, takesOver string, items []item, resumeLater bool) error {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	for i := range items {
		if other, ok := e.busy[items[i].id]; ok && other != changeID && other != takesOver {
			if resumeLater {
				e.waiters[other] = true
			}
			return fault.New(ErrBusy, "%s is applying %s now, so this change can't start yet. Apply it once %s ends", other, items[i].ref, other)
		}
	}
	for i := range items {
		e.busy[items[i].id] = changeID
	}
	return nil
}

// letGo ends a change's hold here: it isn't owned any more, its progress
// isn't live, and its resources are free. If Resume skipped something
// because this change held it, Resume runs again now, in the background:
// what it waited for is free. Only the change Resume waited on wakes it,
// so a change that lets go having done nothing, such as one waiting for a
// server that is away, never wakes Resume for another.
func (e *Engine) letGo(ctx context.Context, changeID string) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	delete(e.owned, changeID)
	delete(e.live, changeID)
	for id, c := range e.busy {
		if c == changeID {
			delete(e.busy, id)
		}
	}
	if !e.waiters[changeID] {
		return
	}
	delete(e.waiters, changeID)
	if e.waking {
		e.rerun = true
		return
	}
	if !e.enter() {
		return // stopping: the next start resumes
	}
	e.waking = true
	ctx = context.WithoutCancel(ctx)
	go func() {
		defer e.exit()
		for {
			if err := e.resume(ctx); err != nil {
				e.logger.WarnContext(ctx, "can't finish a change left applying yet; Dockwell tries again when a server reconnects",
					slog.String("error", err.Error()))
			}
			e.pmu.Lock()
			again := e.rerun
			e.rerun, e.waking = false, again
			e.pmu.Unlock()
			if !again {
				return
			}
		}
	}()
}

// resumeAfter asks for Resume once a change held here lets go.
func (e *Engine) resumeAfter(changeID string) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	e.waiters[changeID] = true
}

// progress returns the live progress of a change running in this process,
// or nil.
func (e *Engine) progress(changeID string) *Progress {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	p, ok := e.live[changeID]
	if !ok {
		return nil
	}
	c := p.clone()
	return &c
}

// step moves one step on in a change's live progress. It returns the
// progress as it now stands, to record, and the update to tell the
// change's watchers with announce once it is on record; nil for both if
// the change or the step isn't tracked.
func (e *Engine) step(changeID string, i int, state StepState, message string) (*Progress, *Update) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	p, ok := e.live[changeID]
	if !ok || i < 0 || i >= len(p.Steps) {
		return nil, nil
	}
	s := &p.Steps[i]
	now := e.now()
	switch {
	case state == StepRunning:
		s.Started, s.Finished = now, time.Time{}
	case state.Final():
		s.Finished = now
	}
	s.State, s.Message = state, message
	step := *s
	c := p.clone()
	return &c, &Update{Index: i, Step: &step}
}

// announce tells a change's watchers that a step moved on; nil tells them
// nothing.
func (e *Engine) announce(changeID string, u *Update) {
	if u == nil {
		return
	}
	e.pmu.Lock()
	defer e.pmu.Unlock()
	e.publishLocked(changeID, *u)
}

// newProgress lists a plan's steps, all queued.
func newProgress(plan *Plan, items []item) Progress {
	ids := map[resource.Ref]string{}
	for i := range items {
		ids[items[i].ref] = items[i].id
	}
	var p Progress
	for i := range plan.Servers {
		sp := &plan.Servers[i]
		for j := range sp.Steps {
			st := &sp.Steps[j]
			p.Steps = append(p.Steps, StepProgress{
				Server: sp.Server, ResourceID: ids[st.Ref], Ref: st.Ref, Action: st.Action, State: StepQueued,
				Message: message(StepQueued, st.Action, st.Ref, sp.Name),
			})
		}
	}
	return p
}

// resumed carries a change's recorded progress over to a fresh plan of
// what remains: each step still to do is queued again, a step that is no
// longer needed is done, and a step the first plan didn't have is added.
func resumed(before Progress, plan *Plan, items []item) Progress {
	p := before.clone()
	fresh := newProgress(plan, items)
	todo := map[int]bool{}
	for j := range fresh.Steps {
		st := &fresh.Steps[j]
		i := p.index(st.Server, st.ResourceID)
		if i < 0 {
			p.Steps = append(p.Steps, *st)
			i = len(p.Steps) - 1
		} else {
			p.Steps[i] = *st
		}
		todo[i] = true
	}
	for i := range p.Steps {
		s := &p.Steps[i]
		if !todo[i] && !s.State.Final() {
			s.State, s.Message = StepDone, "The server already has it"
		}
	}
	return p
}

// verbs words each action for a step's messages: to do, doing, done.
var verbs = map[resource.Action][3]string{
	resource.ActionCreate:  {"create", "Creating", "Created"},
	resource.ActionUpdate:  {"update", "Updating", "Updated"},
	resource.ActionReplace: {"replace", "Replacing", "Replaced"},
	resource.ActionDelete:  {"delete", "Deleting", "Deleted"},
}

// message says what a step is doing, for people, such as "Replacing
// service web on hz-1".
func message(state StepState, action resource.Action, ref resource.Ref, server string) string {
	v, ok := verbs[action]
	if !ok {
		v = [3]string{"check", "Checking", "Checked"}
	}
	what := fmt.Sprintf("%s %s on %s", ref.Kind, ref.Name, server)
	switch state {
	case StepQueued:
		return "Waiting to " + v[0] + " " + what
	case StepRunning:
		return v[1] + " " + what
	case StepDone:
		return v[2] + " " + what
	case StepSkipped:
		return "Not run, because a step before it failed"
	case StepWaiting:
		return "Waiting for " + server + " to reconnect"
	case StepFailed:
		// A failed step's message is its error.
	}
	return ""
}

// DecodePlan reads a plan as the store keeps it.
func DecodePlan(b []byte) (*Plan, error) {
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("can't read a stored plan: %w", err)
	}
	return &p, nil
}

// decodeProgress reads progress as the store keeps it; none is empty.
func decodeProgress(b []byte) (Progress, error) {
	var p Progress
	if len(b) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("can't read a change's stored progress: %w", err)
	}
	return p, nil
}

// Record is a change with its plan and progress, as the API shows it.
type Record struct {
	store.Change
	// Plan is the plan in full. Changes leaves it out: a list has Brief.
	Plan *Plan
	// Brief is what a list shows of the plan.
	Brief    Brief
	Progress Progress
	// RolledBackBy is the rollback of this change, if it failed and one was
	// made; UndoneBy the applied change that undid it.
	RolledBackBy, UndoneBy string
}

// Brief is a plan in brief: its fingerprint, its counts, and whether it is
// destructive.
type Brief struct {
	Fingerprint string
	Counts      Counts
	Destructive bool
}

// Brief sums up the plan.
func (p *Plan) Brief() Brief {
	return Brief{Fingerprint: p.Fingerprint, Counts: p.Counts(), Destructive: p.Destructive()}
}

// brief reads a stored plan in brief, leaving out the rest: its field
// changes, which are most of it.
func brief(b []byte) (Brief, error) {
	var stored struct {
		Fingerprint string
		Resources   []struct{ Action resource.Action }
		Servers     []struct {
			Steps []struct {
				Destroys bool
				Warnings []resource.Warning
			}
		}
	}
	if err := json.Unmarshal(b, &stored); err != nil {
		return Brief{}, fmt.Errorf("can't read a stored plan: %w", err)
	}
	p := Plan{Fingerprint: stored.Fingerprint, Resources: make([]ResourcePlan, len(stored.Resources)), Servers: make([]ServerPlan, len(stored.Servers))}
	for i := range stored.Resources {
		p.Resources[i].Action = stored.Resources[i].Action
	}
	for i := range stored.Servers {
		for _, st := range stored.Servers[i].Steps {
			p.Servers[i].Steps = append(p.Servers[i].Steps, resource.Step{Decision: resource.Decision{Destroys: st.Destroys, Warnings: st.Warnings}})
		}
	}
	return p.Brief(), nil
}

// UndoDeadline is until when an applied change can be undone; zero for
// one that isn't applied.
func (r *Record) UndoDeadline() time.Time {
	if r.State != store.ChangeApplied || r.AppliedAt.IsZero() {
		return time.Time{}
	}
	return r.AppliedAt.Add(UndoWindow)
}

// Change reads a change with its plan in full, and its progress: live
// while it runs in this process, as recorded otherwise.
func (e *Engine) Change(ctx context.Context, changeID string) (*Record, error) {
	var r *Record
	err := e.db.View(ctx, func(tx *store.Tx) error {
		c, err := tx.GetChange(ctx, changeID)
		if err != nil {
			return err
		}
		r = &Record{Change: c}
		if r.Plan, err = DecodePlan(c.Plan); err != nil {
			return err
		}
		r.Brief = r.Plan.Brief()
		return e.complete(ctx, tx, []*Record{r})
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, noSuchChange(changeID)
		}
		return nil, err
	}
	return r, nil
}

// Changes reads a page of changes, newest first, each with its plan in
// brief rather than in full.
func (e *Engine) Changes(ctx context.Context, page store.ChangePage, limit int) ([]*Record, error) {
	var out []*Record
	err := e.db.View(ctx, func(tx *store.Tx) error {
		cs, err := tx.ListChanges(ctx, page, limit)
		if err != nil {
			return err
		}
		for i := range cs {
			r := &Record{Change: cs[i]}
			if r.Brief, err = brief(cs[i].Plan); err != nil {
				return err
			}
			out = append(out, r)
		}
		return e.complete(ctx, tx, out)
	})
	return out, err
}

// complete reads each record's progress, and what rolled it back or undid
// it, with one query for them all.
func (e *Engine) complete(ctx context.Context, tx *store.Tx, records []*Record) error {
	ids := make([]string, len(records))
	byID := make(map[string]*Record, len(records))
	for i, r := range records {
		ids[i], byID[r.ID] = r.ID, r
		var err error
		if p := e.progress(r.ID); p != nil {
			r.Progress = *p
		} else if r.Progress, err = decodeProgress(r.Change.Progress); err != nil {
			return err
		}
	}
	reverting, err := tx.Reverting(ctx, ids)
	if err != nil {
		return err
	}
	for i := range reverting {
		rc := &reverting[i]
		r := byID[rc.Reverts]
		switch {
		case r == nil:
		case IsRollback(*rc):
			r.RolledBackBy = rc.ID
		case rc.State == store.ChangeApplied:
			r.UndoneBy = rc.ID
		}
	}
	return nil
}
