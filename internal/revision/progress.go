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
	// Message says what is happening, for people, or why the step failed,
	// in plain words.
	Message string `json:"message,omitempty"`
	// Activity is what the step's driver last said it is doing, while it
	// runs.
	Activity string `json:"activity,omitempty"`
	// Detail is what a server said when the step failed, when Message puts
	// that in plain words.
	Detail string `json:"detail,omitempty"`
	// Reason says what a failure comes down to, when the driver knows.
	Reason string `json:"reason,omitempty"`
	// Lost is true when the server went away during the step, so nothing can
	// say whether it ran until the server is back.
	Lost bool `json:"lost,omitempty"`
	// Note is what the step found that changed what it did, such as a pull that
	// found nothing newer.
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
	// Did is what the step did in the end, once it's done: Action says what was
	// planned, and a pull finds out as it goes. Nil until then, for a step that
	// failed, and for one a run that stopped partway had done already.
	Did *resource.Action `json:"did,omitempty"`
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

// ended says the record has a resource's step on a server as having ended.
func (p *Progress) ended(server, resourceID string) bool {
	i := p.index(server, resourceID)
	return i >= 0 && p.Steps[i].State.Final()
}

// Update is one event of a watched change: a step moved on, or the change
// did.
type Update struct {
	// Change is the change's ID.
	Change string
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
	dropped := make(chan struct{})
	e.pmu.Lock()
	if e.watchers[changeID] == nil {
		e.watchers[changeID] = map[chan Update]chan struct{}{}
	}
	e.watchers[changeID][ch] = dropped
	e.pmu.Unlock()
	go func() {
		// A watcher dropped for falling behind watches again with the same
		// ctx, so this ends then too, rather than waiting for ctx.
		select {
		case <-ctx.Done():
		case <-dropped:
			return
		}
		e.pmu.Lock()
		defer e.pmu.Unlock()
		e.dropWatcherLocked(changeID, ch)
	}()
	return ch
}

// everyChange is the key watchers of every change are filed under, which
// no change's ID can be.
const everyChange = "*"

// WatchAll follows every change, as Watch follows one: each update names
// its change. Read what it is about after calling WatchAll.
func (e *Engine) WatchAll(ctx context.Context) <-chan Update {
	return e.Watch(ctx, everyChange)
}

func (e *Engine) dropWatcherLocked(changeID string, ch chan Update) {
	ws := e.watchers[changeID]
	dropped, ok := ws[ch]
	if !ok {
		return
	}
	delete(ws, ch)
	close(ch)
	close(dropped)
	if len(ws) == 0 {
		delete(e.watchers, changeID)
	}
}

func (e *Engine) publishLocked(changeID string, u Update) {
	u.Change = changeID
	for _, key := range [...]string{changeID, everyChange} {
		for ch := range e.watchers[key] {
			select {
			case ch <- u:
			default:
				// Too far behind: drop it, and it watches again from a snapshot.
				e.dropWatcherLocked(key, ch)
			}
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
			return fault.New(ErrBusy, "another change is applying %s now, so this one can't start yet. Apply it again once that one ends (%s)", items[i].ref.Name, other)
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
	s.State, s.Message, s.Activity, s.Detail = state, message, "", ""
	step := *s
	c := p.clone()
	return &c, &Update{Index: i, Step: &step}
}

// finished ends a step that ran, saying what it did in the end.
func (e *Engine) finished(changeID string, i int, said string, did resource.Action) (*Progress, *Update) {
	p, u := e.step(changeID, i, StepDone, said)
	if p == nil {
		return nil, nil
	}
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if live, ok := e.live[changeID]; ok {
		live.Steps[i].Did = &did
	}
	p.Steps[i].Did, u.Step.Did = &did, &did
	return p, u
}

// failed is step for a step that failed, with what a person reads about it,
// what the server said, and what it comes down to.
func (e *Engine) failed(changeID string, i int, err error) (*Progress, *Update) {
	plain, detail := resource.Explain(err)
	reason, lost := resource.ReasonOf(err), errors.Is(err, resource.ErrLost)
	p, u := e.step(changeID, i, StepFailed, plain)
	if p == nil {
		return nil, nil
	}
	say := func(s *StepProgress) { s.Detail, s.Reason, s.Lost = detail, reason, lost }
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if live, ok := e.live[changeID]; ok && i < len(live.Steps) {
		say(&live.Steps[i])
	}
	say(&p.Steps[i])
	say(u.Step)
	return p, u
}

// progressed updates what a step that is running says, as its driver says
// how it is going, such as how much of an image it has pulled, and tells
// the change's watchers. It isn't recorded: the step's end is. A word that
// comes once the step is no longer running is dropped.
func (e *Engine) progressed(changeID string, i int, message string) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	p, ok := e.live[changeID]
	if !ok || i < 0 || i >= len(p.Steps) || p.Steps[i].State != StepRunning {
		return
	}
	p.Steps[i].Message, p.Steps[i].Activity = message, message
	step := p.Steps[i]
	e.publishLocked(changeID, Update{Index: i, Step: &step})
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
				Message: stepMessage(StepQueued, st, sp.Name),
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
			s.State, s.Message = StepDone, alreadyHasIt
		}
	}
	return p
}

// alreadyHasIt ends a step that found the server as it was to leave it.
const alreadyHasIt = "The server already has it"

// neverBegan reports a step on a server that the record says no run started: it
// waited for its server, which is written down as it happens, and has no start. That's
// the only thing that can say so. A step's start stays in memory until something
// records the step, so one that reads as queued after a crash may have got anywhere,
// and nobody can say.
func (p Progress) neverBegan(server string, ref resource.Ref) bool {
	return slices.ContainsFunc(p.Steps, func(s StepProgress) bool {
		return s.Server == server && s.Ref == ref && s.State == StepWaiting && s.Started.IsZero()
	})
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

// stepMessage is message for a step, in its own words where it restarts a
// resource, rather than replacing it for a new spec.
func stepMessage(state StepState, st *resource.Step, server string) string {
	if st.Restore != nil {
		what := fmt.Sprintf("%s %s on %s", st.Ref.Kind, st.Ref.Name, server)
		switch state {
		case StepQueued:
			return "Waiting to restore " + what
		case StepRunning:
			return "Restoring " + what
		case StepDone:
			return "Restored " + what
		case StepFailed, StepSkipped, StepWaiting:
		}
	}
	if st.Restart != "" {
		what := fmt.Sprintf("%s %s on %s", st.Ref.Kind, st.Ref.Name, server)
		switch state {
		case StepQueued:
			return "Waiting to restart " + what
		case StepRunning:
			return "Restarting " + what
		case StepDone:
			return "Restarted " + what
		case StepFailed, StepSkipped, StepWaiting:
		}
	}
	return message(state, st.Action, st.Ref, server)
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
	// MadeRevisions says it wrote a revision of anything.
	MadeRevisions bool
	// RolledBackBy is the rollback of this change, if it failed and one was
	// made; UndoneBy the applied change that undid it.
	RolledBackBy, UndoneBy string
	// Rollback says how that rollback is going, nil when there is none.
	Rollback *Rollback
}

// What a rollback's state can be: it runs on after the change it rolls back
// has failed, and only its own record can say how it went.
const (
	RollbackApplying = "applying" // its steps are running
	RollbackWaiting  = "waiting"  // a step waits for its server
	RollbackApplied  = "applied"  // every step ran, so what was there before is back
	RollbackFailed   = "failed"   // a step failed, so what was there before may not be back
)

// Rollback is how the rollback of a failed change is going, for whoever has
// to say what runs now.
type Rollback struct {
	// ID is the rollback's change.
	ID string
	// State is one of the Rollback states.
	State string
	// Steps counts its steps, without one that found the server had what it was
	// to put back. None means the servers still had what was there before, as
	// the failed change stopped before it changed them.
	Steps int
	// WaitingFor names the servers it waits for, in RollbackWaiting.
	WaitingFor []string
	// Restored names the resources its steps put back. What isn't here didn't
	// change, so it never stopped.
	Restored []string
	// Error says why it failed, in plain words, and Detail what the server
	// said, in RollbackFailed.
	Error, Detail string
}

// rollbackOf reads how a rollback is going from its record, and its live
// progress while it runs here.
func (e *Engine) rollbackOf(c *store.Change) (*Rollback, error) {
	var plan struct {
		Servers []struct {
			Server, Name, Unplanned string
			Steps                   []struct {
				Ref     resource.Ref
				Rollout *resource.RolloutPlan
			}
		}
	}
	if err := json.Unmarshal(c.Plan, &plan); err != nil {
		return nil, fmt.Errorf("can't read a stored plan: %w", err)
	}
	var progress Progress
	if p := e.progress(c.ID); p != nil {
		progress = *p
	} else {
		var err error
		if progress, err = decodeProgress(c.Progress); err != nil {
			return nil, err
		}
	}
	// The plan says what a step was to do, which it made before anyone could ask the
	// server, and only the step can say whether it had to.
	foundDone := func(server string, ref resource.Ref) bool {
		return slices.ContainsFunc(progress.Steps, func(s StepProgress) bool {
			return s.Server == server && s.Ref == ref && s.Did != nil && *s.Did == resource.ActionNone
		})
	}
	r := &Rollback{ID: c.ID, State: RollbackApplying}
	names := make(map[string]string, len(plan.Servers))
	for i := range plan.Servers {
		names[plan.Servers[i].Server] = plan.Servers[i].Name
		for _, step := range plan.Servers[i].Steps {
			if foundDone(plan.Servers[i].Server, step.Ref) {
				continue
			}
			r.Steps++
			// A rolling deploy that failed leaves what ran serving, and the step only takes
			// away what failed, so it has put nothing back.
			if step.Rollout.AlreadyRuns() {
				continue
			}
			if !slices.Contains(r.Restored, step.Ref.Name) {
				r.Restored = append(r.Restored, step.Ref.Name)
			}
		}
	}
	switch c.State {
	case store.ChangeApplied:
		r.State = RollbackApplied
	case store.ChangeFailed:
		r.State, r.Error = RollbackFailed, c.Error
		if i := slices.IndexFunc(progress.Steps, func(s StepProgress) bool { return s.State == StepFailed }); i >= 0 {
			r.Detail = progress.Steps[i].Detail
		}
	default:
		wait := func(name string) {
			r.State = RollbackWaiting
			if !slices.Contains(r.WaitingFor, name) {
				r.WaitingFor = append(r.WaitingFor, name)
			}
		}
		for i := range progress.Steps {
			if s := &progress.Steps[i]; s.State == StepWaiting {
				wait(names[s.Server])
			}
		}
		// A part its agent couldn't plan has no steps yet, and waits all the same.
		for i := range plan.Servers {
			if plan.Servers[i].Unplanned != "" {
				wait(plan.Servers[i].Name)
			}
		}
	}
	return r, nil
}

// Brief is a plan in brief: its fingerprint, its counts, whether it is
// destructive or gives host access, why Dockwell made it, if it did, and
// whether it is a restart.
type Brief struct {
	Fingerprint string
	Counts      Counts
	Destructive bool
	HostAccess  bool
	Intent      *Intent // what it is for, when the method that proposed it knows
	Because     string
	Restart     bool
	Restore     bool // it puts a volume back from a backup
}

// Brief sums up the plan.
func (p *Plan) Brief() Brief {
	return Brief{
		Fingerprint: p.Fingerprint, Counts: p.Counts(), Destructive: p.Destructive(), HostAccess: p.HostAccess(), Intent: p.Intent, Because: p.Because,
		Restart: slices.ContainsFunc(p.Resources, func(r ResourcePlan) bool { return r.Restart }),
		Restore: slices.ContainsFunc(p.Resources, func(r ResourcePlan) bool { return r.Restore != nil }),
	}
}

// brief reads a stored plan in brief, leaving out the rest: its field
// changes, which are most of it.
func brief(b []byte) (Brief, error) {
	var stored struct {
		Fingerprint string
		Intent      *Intent
		Because     string
		Resources   []struct {
			Action     resource.Action
			HostAccess []resource.Access
			Restart    bool
			Restore    *resource.RestoreSource
		}
		Servers []struct {
			Steps []struct {
				Destroys bool
				Warnings []resource.Warning
				Rollout  *resource.RolloutPlan
			}
		}
	}
	if err := json.Unmarshal(b, &stored); err != nil {
		return Brief{}, fmt.Errorf("can't read a stored plan: %w", err)
	}
	p := Plan{Fingerprint: stored.Fingerprint, Intent: stored.Intent, Because: stored.Because, Resources: make([]ResourcePlan, len(stored.Resources)), Servers: make([]ServerPlan, len(stored.Servers))}
	for i := range stored.Resources {
		p.Resources[i].Action, p.Resources[i].HostAccess, p.Resources[i].Restart = stored.Resources[i].Action, stored.Resources[i].HostAccess, stored.Resources[i].Restart
		p.Resources[i].Restore = stored.Resources[i].Restore
	}
	for i := range stored.Servers {
		for _, st := range stored.Servers[i].Steps {
			p.Servers[i].Steps = append(p.Servers[i].Steps, resource.Step{Decision: resource.Decision{Destroys: st.Destroys, Warnings: st.Warnings, Rollout: st.Rollout}})
		}
	}
	return p.Brief(), nil
}

// EndedAt is when a change that failed did, from its steps: the time it began
// applying is all the change itself keeps, which a long deploy takes well away from
// when it failed. The zero time for a change that hasn't failed, or a change whose
// steps don't say.
func (r *Record) EndedAt() time.Time {
	if r.State != store.ChangeFailed {
		return time.Time{}
	}
	var ended time.Time
	for i := range r.Progress.Steps {
		if f := r.Progress.Steps[i].Finished; f.After(ended) {
			ended = f
		}
	}
	return ended
}

// UndoDeadline is until when an applied change can be undone; zero for
// one that isn't applied.
func (r *Record) UndoDeadline() time.Time {
	// A change that made no revision, as a pull that found nothing newer, has
	// nothing to undo, and offering to would only be refused. A restore makes none
	// either, but what it replaced is there to put back.
	if r.State != store.ChangeApplied || r.AppliedAt.IsZero() || !r.MadeRevisions && !r.Brief.Restore {
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
		out, err = e.Records(ctx, tx, cs)
		return err
	})
	return out, err
}

// Records completes changes read with tx, without their items, as Changes
// does: each with its plan in brief, its progress, and what rolled it back
// or undid it.
func (e *Engine) Records(ctx context.Context, tx *store.Tx, changes []store.Change) ([]*Record, error) {
	out := make([]*Record, len(changes))
	for i := range changes {
		r := &Record{Change: changes[i]}
		var err error
		if r.Brief, err = brief(changes[i].Plan); err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, e.complete(ctx, tx, out)
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
	made, err := tx.ChangesWithRevisions(ctx, ids)
	if err != nil {
		return err
	}
	for id := range made {
		byID[id].MadeRevisions = true
	}
	for i := range reverting {
		rc := &reverting[i]
		r := byID[rc.Reverts]
		switch {
		case r == nil:
		case IsRollback(*rc):
			r.RolledBackBy = rc.ID
			if r.Rollback, err = e.rollbackOf(rc); err != nil {
				return err
			}
		case rc.State == store.ChangeApplied:
			r.UndoneBy = rc.ID
		}
	}
	return nil
}
