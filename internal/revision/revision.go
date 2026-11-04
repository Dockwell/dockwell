// SPDX-License-Identifier: AGPL-3.0-only

// Package revision is the one path every change to managed state takes:
// propose edits, plan them against the servers, apply the plan, and undo
// it later. Applying writes immutable revisions; undo and rollback write
// new ones. It also finds drift, where a server no longer matches its
// latest revision. See docs/adr/0009-resources-revisions-and-plans.md.
package revision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// UndoWindow is how long a change can be undone, as the plan sheet
// promises.
const UndoWindow = 7 * 24 * time.Hour

// SystemActor makes the changes Dockwell makes on its own, such as
// rollbacks.
const SystemActor = "dockwell"

// Errors callers can act on.
var (
	// ErrStale means a resource changed after the plan was made; plan
	// again.
	ErrStale = errors.New("the plan is out of date")
	// ErrPlanChanged means the servers no longer match the plan; review it
	// again.
	ErrPlanChanged = errors.New("the servers changed since this plan was made")
	// ErrNotPlanned means the change was already applied, or failed.
	ErrNotPlanned = errors.New("the change isn't waiting to be applied")
	// ErrCantUndo means the change can't be undone safely.
	ErrCantUndo = errors.New("the change can't be undone")
	// ErrOffline means a server the plan touches isn't connected.
	ErrOffline = errors.New("a server in the plan isn't connected")
	// ErrInvalid means an edit doesn't make sense.
	ErrInvalid = errors.New("the change isn't valid")
)

// Agents finds each server's agent.
type Agents interface {
	// Agent returns a server's agent, and whether it is connected now.
	Agent(server string) (resource.Agent, bool)
}

// Engine plans, applies and undoes changes.
type Engine struct {
	db     *store.DB
	kinds  *resource.Registry
	agents Agents
	logger *slog.Logger
	now    func() time.Time
	// One apply at a time keeps base revisions honest. Locks per resource
	// come when applies take long enough to matter.
	mu sync.Mutex
}

// New returns an engine over db, using kinds to read and check specs, and
// agents to reach servers.
func New(db *store.DB, kinds *resource.Registry, agents Agents, logger *slog.Logger) *Engine {
	return &Engine{db: db, kinds: kinds, agents: agents, logger: logger, now: time.Now}
}

// Edit sets one resource's desired spec; a nil Spec deletes it.
type Edit struct {
	Ref  resource.Ref
	Spec proto.Message
}

// Proposal is a set of edits someone wants to make.
type Proposal struct {
	Summary string // what it is for, in the actor's words
	Actor   string
	Edits   []Edit
}

// Plan is what applying a change would do.
type Plan struct {
	Change  string // the planned change's ID, to apply or discard
	Summary string
	Reverts string // the change this undoes or rolls back, if any
	// Servers lists each server with something to do, in proposal order.
	Servers []ServerPlan
	// Revisions is how many resources get a new revision. A plan can have
	// revisions without steps, such as adopting drift.
	Revisions   int
	Fingerprint string
}

// ServerPlan is the part of a plan on one server.
type ServerPlan struct {
	Server string
	// Offline means the server isn't connected, so the plan is based on
	// its last known desired state.
	Offline bool
	// Downtime adds up the steps' downtime, as they run one at a time.
	Downtime time.Duration
	Steps    []resource.Step
}

// Empty reports whether applying would change nothing at all.
func (p *Plan) Empty() bool { return len(p.Servers) == 0 && p.Revisions == 0 }

// Steps lists every step, server by server.
func (p *Plan) Steps() []resource.Step {
	var out []resource.Step
	for _, s := range p.Servers {
		out = append(out, s.Steps...)
	}
	return out
}

// item is one edit with what it starts from.
type item struct {
	ref     resource.Ref
	base    int           // the latest revision now; 0 if none
	current proto.Message // the latest spec; nil if none or deleted
	spec    proto.Message // what is wanted; nil deletes
	encoded []byte
}

// changes reports whether the item makes a new revision.
func (it item) changes() bool {
	cur, err := encodeOrNil(it.current)
	return err != nil || it.base == 0 || !bytes.Equal(cur, it.encoded) || (it.current == nil) != (it.spec == nil)
}

// Propose plans a proposal and records it as a planned change, ready to
// apply.
func (e *Engine) Propose(ctx context.Context, p Proposal) (*Plan, error) {
	return e.propose(ctx, p, "")
}

func (e *Engine) propose(ctx context.Context, p Proposal, reverts string) (*Plan, error) {
	if len(p.Edits) == 0 {
		return nil, fmt.Errorf("%w: it has no edits", ErrInvalid)
	}
	items := make([]item, 0, len(p.Edits))
	seen := map[resource.Ref]bool{}
	err := e.db.View(ctx, func(tx *store.Tx) error {
		for _, ed := range p.Edits {
			if seen[ed.Ref] {
				return fmt.Errorf("%w: it edits %s twice; combine the edits", ErrInvalid, ed.Ref)
			}
			seen[ed.Ref] = true
			it, err := e.load(ctx, tx, ed.Ref)
			if err != nil {
				return err
			}
			if ed.Spec != nil {
				it.spec = proto.Clone(ed.Spec)
				if err := e.kinds.Validate(ed.Ref.Kind, it.spec); err != nil {
					return fmt.Errorf("%w: %s: %w", ErrInvalid, ed.Ref, err)
				}
				if it.encoded, err = resource.Encode(it.spec); err != nil {
					return err
				}
			} else if it.current == nil {
				return fmt.Errorf("%w: there's no %s to delete", ErrInvalid, ed.Ref)
			}
			items = append(items, it)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	plan, err := e.plan(ctx, items)
	if err != nil {
		return nil, err
	}
	plan.Change, plan.Summary, plan.Reverts = id.New(id.Change), p.Summary, reverts
	record, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	c := store.Change{
		ID: plan.Change, Summary: p.Summary, Actor: p.Actor, State: store.ChangePlanned, Reverts: reverts,
		Fingerprint: plan.Fingerprint, Plan: record, CreatedAt: e.now(),
	}
	for _, it := range items {
		c.Items = append(c.Items, store.ChangeItem{Kind: string(it.ref.Kind), Server: it.ref.Server, Name: it.ref.Name, BaseRevision: it.base, Spec: it.encoded})
	}
	if err := e.db.Update(ctx, func(tx *store.Tx) error { return tx.CreateChange(ctx, c) }); err != nil {
		return nil, err
	}
	return plan, nil
}

// load reads where a resource stands now.
func (e *Engine) load(ctx context.Context, tx *store.Tx, ref resource.Ref) (item, error) {
	it := item{ref: ref}
	r, err := tx.GetResource(ctx, string(ref.Kind), ref.Server, ref.Name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return it, nil
	case err != nil:
		return it, err
	}
	it.base = r.Revision
	if r.Spec != nil {
		if it.current, err = e.kinds.Decode(ref.Kind, r.Spec); err != nil {
			return it, err
		}
	}
	return it, nil
}

// plan asks each server's agent what the items would take there. A server
// that isn't connected is planned from its last known desired state.
func (e *Engine) plan(ctx context.Context, items []item) (*Plan, error) {
	var servers []string
	byServer := map[string][]item{}
	for _, it := range items {
		if _, ok := byServer[it.ref.Server]; !ok {
			servers = append(servers, it.ref.Server)
		}
		byServer[it.ref.Server] = append(byServer[it.ref.Server], it)
	}

	plan := &Plan{}
	fp := sha256.New()
	for _, it := range items {
		if it.changes() {
			plan.Revisions++
		}
		sum := sha256.Sum256(it.encoded)
		fmt.Fprintf(fp, "item %s|%s|%s|%d|%x\n", it.ref.Kind, it.ref.Server, it.ref.Name, it.base, sum)
	}
	for _, server := range servers {
		its := byServer[server]
		sp := ServerPlan{Server: server}
		var steps []resource.Step
		agent, online := e.agents.Agent(server)
		if online {
			desired := make([]resource.Desired, len(its))
			for i, it := range its {
				desired[i] = resource.Desired{Ref: it.ref, Spec: it.spec}
			}
			var err error
			if steps, err = agent.Plan(ctx, desired); err != nil {
				return nil, fmt.Errorf("can't plan on %s: %w", server, err)
			}
		} else {
			sp.Offline = true
			for _, it := range its {
				d, err := e.kinds.Driver(it.ref.Kind)
				if err != nil {
					return nil, err
				}
				step, err := resource.PlanStep(d, it.ref, it.current, it.spec)
				if err != nil {
					return nil, err
				}
				steps = append(steps, step)
			}
		}
		steps = order(steps)
		for i := range steps {
			s := &steps[i]
			if s.Action == resource.ActionNone {
				continue
			}
			sp.Steps = append(sp.Steps, *s)
			sp.Downtime += s.Downtime
			fmt.Fprintf(fp, "step %s|%s|%d|%d|%d|%t\n", s.Ref, s.Action, s.Effect, s.Downtime, len(s.Changes), s.Destroys)
			for _, c := range s.Changes {
				fmt.Fprintf(fp, "  %s|%d|%s|%s\n", c.Path, c.Op, c.From, c.To)
			}
		}
		if len(sp.Steps) > 0 {
			plan.Servers = append(plan.Servers, sp)
		}
	}
	plan.Fingerprint = hex.EncodeToString(fp.Sum(nil)[:16])
	return plan, nil
}

// order runs creates, updates and replaces in proposal order, then deletes
// in reverse, so what is built comes first and is taken down last.
func order(steps []resource.Step) []resource.Step {
	var out, deletes []resource.Step
	for i := range steps {
		if steps[i].Action == resource.ActionDelete {
			deletes = append(deletes, steps[i])
		} else {
			out = append(out, steps[i])
		}
	}
	slices.Reverse(deletes)
	return append(out, deletes...)
}

// FailedError reports a change whose apply failed, and what was done about
// it.
type FailedError struct {
	Change string
	Step   resource.Ref
	Err    error
	// Rollback is the change that put the applied steps back, if one ran.
	Rollback    string
	RollbackErr error
}

func (e *FailedError) Error() string {
	switch {
	case e.Rollback == "" && e.RollbackErr == nil:
		return fmt.Sprintf("%s failed at %s: %v", e.Change, e.Step, e.Err)
	case e.RollbackErr != nil:
		return fmt.Sprintf("%s failed at %s: %v. Rolling back failed too: %v. Check the servers, then undo or fix it by hand", e.Change, e.Step, e.Err, e.RollbackErr)
	default:
		return fmt.Sprintf("%s failed at %s: %v. It was rolled back in %s", e.Change, e.Step, e.Err, e.Rollback)
	}
}

func (e *FailedError) Unwrap() error { return e.Err }

// Apply carries out a planned change: it checks that nothing moved since
// the plan, commits the revisions, then runs the steps. If a step fails,
// the steps already done are rolled back in a change of their own, and
// the error is a *FailedError.
func (e *Engine) Apply(ctx context.Context, changeID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.apply(ctx, changeID)
}

func (e *Engine) apply(ctx context.Context, changeID string) error {
	var c store.Change
	var items []item
	err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if c, err = tx.GetChange(ctx, changeID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("there's no change %s", changeID)
			}
			return err
		}
		if c.State != store.ChangePlanned {
			return fmt.Errorf("%w: %s is %s", ErrNotPlanned, changeID, c.State)
		}
		items, err = e.current(ctx, tx, c)
		return err
	})
	if err != nil {
		return err
	}

	plan, err := e.plan(ctx, items)
	if err != nil {
		return err
	}
	for _, s := range plan.Servers {
		if s.Offline {
			return fmt.Errorf("%w: %s. Apply again once its agent reconnects", ErrOffline, s.Server)
		}
	}
	if plan.Fingerprint != c.Fingerprint {
		return fmt.Errorf("%w: plan %s again and review what changed", ErrPlanChanged, changeID)
	}

	// Desired state goes on record before anything runs, so agents
	// reconcile toward it even if the control plane stops halfway.
	now := e.now()
	err = e.db.Update(ctx, func(tx *store.Tx) error {
		if _, err := e.current(ctx, tx, c); err != nil {
			return err
		}
		for _, it := range items {
			if !it.changes() {
				continue
			}
			if _, err := tx.AddRevision(ctx, string(it.ref.Kind), it.ref.Server, it.ref.Name, it.encoded, changeID, now); err != nil {
				return err
			}
		}
		return tx.SetChangeState(ctx, changeID, store.ChangeApplying, "", now)
	})
	if err != nil {
		return err
	}

	specs := map[resource.Ref]proto.Message{}
	for _, it := range items {
		specs[it.ref] = it.spec
	}
	for _, s := range plan.Servers {
		agent, _ := e.agents.Agent(s.Server)
		for i := range s.Steps {
			step := &s.Steps[i]
			if err := agent.Apply(ctx, *step, specs[step.Ref]); err != nil {
				return e.fail(ctx, c, step.Ref, err)
			}
		}
	}
	return e.finish(ctx, c, store.ChangeApplied, "", "change.applied")
}

// current checks that every item still starts from the latest revision,
// and reads them as they stand.
func (e *Engine) current(ctx context.Context, tx *store.Tx, c store.Change) ([]item, error) {
	items := make([]item, 0, len(c.Items))
	for _, ci := range c.Items {
		ref := resource.Ref{Kind: resource.Kind(ci.Kind), Server: ci.Server, Name: ci.Name}
		it, err := e.load(ctx, tx, ref)
		if err != nil {
			return nil, err
		}
		if it.base != ci.BaseRevision {
			r, _ := tx.GetResource(ctx, ci.Kind, ci.Server, ci.Name)
			return nil, fmt.Errorf("%w: %s changed in %s after %s was planned. Plan it again", ErrStale, ref, r.ChangeID, c.ID)
		}
		it.encoded = ci.Spec
		if ci.Spec != nil {
			if it.spec, err = e.kinds.Decode(ref.Kind, ci.Spec); err != nil {
				return nil, err
			}
		}
		items = append(items, it)
	}
	return items, nil
}

// fail records a failed apply and rolls back what it changed.
func (e *Engine) fail(ctx context.Context, c store.Change, at resource.Ref, cause error) error {
	ferr := &FailedError{Change: c.ID, Step: at, Err: cause}
	if err := e.finish(ctx, c, store.ChangeFailed, cause.Error(), "change.failed"); err != nil {
		return errors.Join(ferr, err)
	}
	if c.Actor == SystemActor && c.Reverts != "" {
		// A rollback that fails isn't rolled back in turn: that would
		// re-apply what failed in the first place.
		return ferr
	}
	e.logger.WarnContext(ctx, "a change failed; rolling it back", slog.String("change", c.ID), slog.String("at", at.String()),
		slog.String("error", cause.Error()))
	edits, err := e.reverse(ctx, c.ID)
	if err != nil {
		ferr.RollbackErr = err
		return ferr
	}
	if len(edits) == 0 {
		return ferr
	}
	plan, err := e.propose(ctx, Proposal{Summary: "Roll back " + c.Summary, Actor: SystemActor, Edits: edits}, c.ID)
	if err != nil {
		ferr.RollbackErr = err
		return ferr
	}
	ferr.Rollback = plan.Change
	if err := e.apply(ctx, plan.Change); err != nil {
		ferr.RollbackErr = err
	}
	return ferr
}

func (e *Engine) finish(ctx context.Context, c store.Change, state, errText, event string) error {
	now := e.now()
	return e.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.SetChangeState(ctx, c.ID, state, errText, time.Time{}); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{ID: id.New(id.Event), At: now, Actor: c.Actor, Action: event, Subject: c.ID, Detail: c.Summary})
	})
}

// reverse returns the edits that put back what a change's revisions
// replaced, refusing any resource that has changed again since.
func (e *Engine) reverse(ctx context.Context, changeID string) ([]Edit, error) {
	var edits []Edit
	err := e.db.View(ctx, func(tx *store.Tx) error {
		revs, err := tx.RevisionsOf(ctx, changeID)
		if err != nil {
			return err
		}
		for i := range revs {
			rev := &revs[i]
			ref := resource.Ref{Kind: resource.Kind(rev.Kind), Server: rev.Server, Name: rev.Name}
			latest, err := tx.GetResource(ctx, rev.Kind, rev.Server, rev.Name)
			if err != nil {
				return err
			}
			if latest.Revision != rev.Revision {
				return fmt.Errorf("%w: %s changed again in %s. Undo that first, or edit %s directly", ErrCantUndo, ref, latest.ChangeID, rev.Name)
			}
			var before proto.Message
			if rev.Revision > 1 {
				prev, err := tx.GetRevision(ctx, rev.ID, rev.Revision-1)
				if err != nil {
					return err
				}
				if prev.Spec != nil {
					if before, err = e.kinds.Decode(ref.Kind, prev.Spec); err != nil {
						return err
					}
				}
			}
			edits = append(edits, Edit{Ref: ref, Spec: before})
		}
		return nil
	})
	return edits, err
}

// Undo plans putting back everything an applied change did. Apply the
// plan to carry it out.
func (e *Engine) Undo(ctx context.Context, changeID, actor string) (*Plan, error) {
	var c store.Change
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		c, err = tx.GetChange(ctx, changeID)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("there's no change %s", changeID)
		}
		return nil, err
	}
	if c.State != store.ChangeApplied {
		return nil, fmt.Errorf("%w: %s is %s, and only applied changes can be undone", ErrCantUndo, changeID, c.State)
	}
	if age := e.now().Sub(c.AppliedAt); age > UndoWindow {
		return nil, fmt.Errorf("%w: %s was applied %s ago, and changes can be undone for %s. Edit the resources directly instead",
			ErrCantUndo, changeID, days(age), days(UndoWindow))
	}
	edits, err := e.reverse(ctx, changeID)
	if err != nil {
		return nil, err
	}
	if len(edits) == 0 {
		return nil, fmt.Errorf("%w: %s made no revisions", ErrCantUndo, changeID)
	}
	return e.propose(ctx, Proposal{Summary: "Undo " + c.Summary, Actor: actor, Edits: edits}, changeID)
}

// Drift is a resource that doesn't match its latest revision.
type Drift struct {
	Ref resource.Ref
	// Revision is the latest revision, which the server should match.
	Revision int
	// Missing means the resource is gone from the server.
	Missing bool
	// Changes go from the latest revision to what the server has.
	Changes  []resource.Change
	Observed proto.Message
}

// Drift compares every resource a server should have with what it
// actually has.
func (e *Engine) Drift(ctx context.Context, server string) ([]Drift, error) {
	agent, online := e.agents.Agent(server)
	if !online {
		return nil, fmt.Errorf("%w: %s. Its drift is checked when its agent reconnects", ErrOffline, server)
	}
	var resources []store.Resource
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		resources, err = tx.ListResources(ctx, server)
		return err
	}); err != nil {
		return nil, err
	}
	refs := make([]resource.Ref, len(resources))
	for i, r := range resources {
		refs[i] = resource.Ref{Kind: resource.Kind(r.Kind), Server: r.Server, Name: r.Name}
	}
	observed, err := agent.Observe(ctx, refs)
	if err != nil {
		return nil, err
	}
	var out []Drift
	for i, r := range resources {
		desired, err := e.kinds.Decode(refs[i].Kind, r.Spec)
		if err != nil {
			return nil, err
		}
		d := Drift{Ref: refs[i], Revision: r.Revision, Observed: observed[i]}
		if observed[i] == nil {
			d.Missing = true
		} else {
			// Observed specs are normalized like desired ones, or noise looks
			// like drift.
			if err := e.kinds.Validate(refs[i].Kind, observed[i]); err != nil {
				return nil, fmt.Errorf("%s has something Dockwell can't read: %w", refs[i], err)
			}
			if d.Changes = resource.Diff(desired, observed[i]); len(d.Changes) == 0 {
				continue
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// Revert plans putting a drifted resource back to its latest revision.
func (e *Engine) Revert(ctx context.Context, d Drift, actor string) (*Plan, error) {
	var it item
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		it, err = e.load(ctx, tx, d.Ref)
		return err
	}); err != nil {
		return nil, err
	}
	summary := fmt.Sprintf("Revert %s to revision %d", d.Ref.Name, it.base)
	return e.propose(ctx, Proposal{Summary: summary, Actor: actor, Edits: []Edit{{Ref: d.Ref, Spec: it.current}}}, "")
}

// Adopt plans keeping what a server has for a drifted resource: a new
// revision that matches it, with nothing to do on the server.
func (e *Engine) Adopt(ctx context.Context, d Drift, actor string) (*Plan, error) {
	summary := fmt.Sprintf("Keep the changes made to %s outside Dockwell", d.Ref.Name)
	return e.propose(ctx, Proposal{Summary: summary, Actor: actor, Edits: []Edit{{Ref: d.Ref, Spec: d.Observed}}}, "")
}

func encodeOrNil(m proto.Message) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return resource.Encode(m)
}

// days writes a long duration the way the plan sheet does: "7 days".
func days(d time.Duration) string {
	n := int(d / (24 * time.Hour))
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}
