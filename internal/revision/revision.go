// SPDX-License-Identifier: AGPL-3.0-only

// Package revision is the one path every change to managed state takes:
// propose edits, plan them against the servers, apply the plan, and undo
// it later. Applying writes immutable revisions and deploys them where they
// are placed; undo and rollback write new ones. It also finds drift, where
// a server no longer matches its latest revision. It is the only package
// that writes managed state (store.DB.Revise). See
// docs/adr/0009-resources-revisions-and-plans.md and
// docs/adr/0014-object-model-and-store.md.
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
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/scope"
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
// agents to reach servers. Projects and environments are always among the
// kinds.
func New(db *store.DB, kinds *resource.Registry, agents Agents, logger *slog.Logger) *Engine {
	return &Engine{db: db, kinds: kinds.With(scope.Types()...), agents: agents, logger: logger, now: time.Now}
}

// Edit sets one resource's desired spec; a nil Spec deletes it.
type Edit struct {
	Ref  resource.Ref
	Spec proto.Message
	// Servers places a resource of an environment-level kind, such as a
	// service, by server ID. Nil keeps it where it runs now; a new one
	// runs on the only server when there is one, and must say where when
	// there are several. Other kinds ignore it: a host-level resource is
	// on its server, and projects and environments run on none.
	Servers []string
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
	// revisions without steps, such as adopting drift or making an
	// environment.
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
	level   resource.Level
	id      string        // the resource's ID; empty until its first revision
	base    int           // the latest revision now; 0 if none
	current proto.Message // the latest spec; nil if none or deleted
	spec    proto.Message // what is wanted; nil deletes
	encoded []byte
	// placed is where the base revision is deployed, and servers where the
	// new one goes, by server ID and sorted. Both are empty for kinds that
	// run on no server.
	placed, servers []string
	// rev is the revision applying runs toward: the new one, once
	// committed, or base when the item makes none.
	rev int
}

// changes reports whether the item makes a new revision: its spec, or
// where it runs, differs from the latest revision.
func (it item) changes() bool {
	cur, err := encodeOrNil(it.current)
	return err != nil || it.base == 0 || !bytes.Equal(cur, it.encoded) || (it.current == nil) != (it.spec == nil) ||
		!slices.Equal(it.placed, it.servers)
}

// targets are the servers applying the item touches: where it is and
// where it goes.
func (it item) targets() []string {
	out := slices.Concat(it.placed, it.servers)
	slices.Sort(out)
	return slices.Compact(out)
}

// desiredOn is what the item wants on a server: its spec where it is
// placed, and nothing elsewhere.
func (it item) desiredOn(server string) proto.Message {
	if slices.Contains(it.servers, server) {
		return it.spec
	}
	return nil
}

// currentOn is what the latest revision has on a server.
func (it item) currentOn(server string) proto.Message {
	if slices.Contains(it.placed, server) {
		return it.current
	}
	return nil
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
				if err := e.checkName(ed.Ref); err != nil {
					return err
				}
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
			if it.servers, err = e.place(ctx, tx, it, ed.Servers); err != nil {
				return err
			}
			items = append(items, it)
		}
		return e.check(ctx, tx, items)
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
	for i := range items {
		it := &items[i]
		c.Items = append(c.Items, store.ChangeItem{
			Kind: string(it.ref.Kind), Scope: it.ref.Scope, Name: it.ref.Name, BaseRevision: it.base, Spec: it.encoded, Servers: it.servers,
		})
	}
	if err := e.db.Revise(ctx, func(tx *store.RevisionTx) error { return tx.CreateChange(ctx, c) }); err != nil {
		return nil, err
	}
	return plan, nil
}

// load reads where a resource stands now.
func (e *Engine) load(ctx context.Context, tx *store.Tx, ref resource.Ref) (item, error) {
	t, err := e.kinds.Type(ref.Kind)
	if err != nil {
		return item{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	it := item{ref: ref, level: t.Level()}
	r, err := tx.GetResource(ctx, string(ref.Kind), ref.Scope, ref.Name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return it, nil
	case err != nil:
		return it, err
	}
	it.id, it.base, it.rev = r.ID, r.Revision, r.Revision
	if r.Spec != nil {
		if it.current, err = e.kinds.Decode(ref.Kind, r.Spec); err != nil {
			return it, err
		}
		if it.placed, err = e.placement(ctx, tx, it, r.Revision); err != nil {
			return it, err
		}
	}
	return it, nil
}

// placement is where a revision of the item's resource is deployed.
func (e *Engine) placement(ctx context.Context, tx *store.Tx, it item, revision int) ([]string, error) {
	switch {
	case it.level == resource.LevelServer:
		return []string{it.ref.Scope}, nil
	case !it.level.RunsOnServers() || it.id == "" || revision == 0:
		return nil, nil
	}
	servers, err := tx.Placement(ctx, it.id, revision)
	slices.Sort(servers)
	return servers, err
}

// place decides where the item's new revision goes. A host-level resource
// is on its server. An environment-level one goes where it was asked to,
// or stays where it is; a new one goes on the only server, and must be
// told where when there is a choice (PRD principle 4).
func (e *Engine) place(ctx context.Context, tx *store.Tx, it item, want []string) ([]string, error) {
	switch {
	case it.spec == nil || !it.level.RunsOnServers():
		return nil, nil
	case it.level == resource.LevelServer:
		return []string{it.ref.Scope}, nil
	case want == nil && it.current != nil:
		return it.placed, nil
	}
	active, err := tx.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	if want == nil {
		switch len(active) {
		case 1:
			return []string{active[0].ID}, nil
		case 0:
			return nil, fmt.Errorf("%w: there's no server to run %s on. Add a server first", ErrInvalid, it.ref.Name)
		}
		names := make([]string, len(active))
		for i := range active {
			names[i] = active[i].Name
		}
		return nil, fmt.Errorf("%w: there are %d servers, so say which %s runs on: %s", ErrInvalid, len(active), it.ref.Name, strings.Join(names, ", "))
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("%w: %s needs a server to run on. To stop it everywhere, delete it", ErrInvalid, it.ref.Name)
	}
	out := slices.Clone(want)
	slices.Sort(out)
	out = slices.Compact(out)
	for _, s := range out {
		if !slices.ContainsFunc(active, func(a store.Server) bool { return a.ID == s }) {
			return nil, fmt.Errorf("%w: there's no server %s to run %s on", ErrInvalid, s, it.ref.Name)
		}
	}
	return out, nil
}

// checkName refuses a name its kind doesn't accept.
func (e *Engine) checkName(ref resource.Ref) error {
	if strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("%w: a %s needs a name", ErrInvalid, ref.Kind)
	}
	t, err := e.kinds.Type(ref.Kind)
	if err != nil {
		return err
	}
	if c, ok := t.(interface{ CheckName(string) error }); ok {
		if err := c.CheckName(ref.Name); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	return nil
}

// check refuses items whose scope isn't there, and deletions that would
// leave a scope's contents behind, or a project or organization with
// nowhere for things to go. It runs when proposing, and again in the
// transaction that applies, so a change planned before the world moved
// can't slip through.
func (e *Engine) check(ctx context.Context, tx *store.Tx, items []item) error {
	deleted := map[string]bool{} // resource IDs this change deletes
	for i := range items {
		if it := &items[i]; it.spec == nil && it.id != "" {
			deleted[it.id] = true
		}
	}
	for i := range items {
		it := &items[i]
		if it.spec != nil {
			if err := e.checkScope(ctx, tx, it); err != nil {
				return err
			}
			continue
		}
		if it.ref.Kind != scope.ProjectKind && it.ref.Kind != scope.EnvironmentKind {
			continue
		}
		// A scope goes only once what lives in it has gone, in this change
		// or an earlier one.
		inside, err := tx.ListResources(ctx, it.id, "")
		if err != nil {
			return err
		}
		var left []string
		for _, r := range inside {
			if !deleted[r.ID] {
				left = append(left, r.Name)
			}
		}
		if len(left) > 0 {
			return fmt.Errorf("%w: %s %s still holds %s. Delete them first, or in the same change", ErrInvalid, it.ref.Kind, it.ref.Name,
				strings.Join(left, ", "))
		}
		// Its parent keeps a place for things to go: a project keeps an
		// environment, and the organization a project.
		parent, err := tx.GetResourceByID(ctx, it.ref.Scope)
		parentGoes := err == nil && deleted[parent.ID]
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if parentGoes {
			continue
		}
		siblings, err := tx.ListResources(ctx, it.ref.Scope, string(it.ref.Kind))
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(siblings, func(r store.Resource) bool { return !deleted[r.ID] }) {
			where := "the organization"
			if parent.Name != "" {
				where = "project " + parent.Name
			}
			return fmt.Errorf("%w: %s is the last %s in %s, which needs one. Make another first", ErrInvalid, it.ref.Name, it.ref.Kind, where)
		}
	}
	return nil
}

// checkScope refuses an item whose scope isn't there, or isn't the kind
// of thing its level lives in.
func (e *Engine) checkScope(ctx context.Context, tx *store.Tx, it *item) error {
	missing := func(what string) error {
		return fmt.Errorf("%w: there's no %s %s for %s %s to live in", ErrInvalid, what, it.ref.Scope, it.ref.Kind, it.ref.Name)
	}
	var want resource.Kind
	switch it.level {
	case resource.LevelServer:
		s, err := tx.GetServer(ctx, it.ref.Scope)
		if errors.Is(err, store.ErrNotFound) || err == nil && s.RevokedAt != nil {
			return missing("server")
		}
		return err
	case resource.LevelOrganization:
		org, err := tx.Organization(ctx)
		if errors.Is(err, store.ErrNotFound) || err == nil && org.ID != it.ref.Scope {
			return missing("organization")
		}
		return err
	case resource.LevelEnvironment:
		want = scope.EnvironmentKind
	case resource.LevelProject:
		want = scope.ProjectKind
	}
	r, err := tx.GetResourceByID(ctx, it.ref.Scope)
	if errors.Is(err, store.ErrNotFound) || err == nil && (r.Kind != string(want) || r.Spec == nil) {
		return missing(string(want))
	}
	return err
}

// plan asks each server's agent what the items would take there. A server
// that isn't connected is planned from its last known desired state.
// Items that run on no server make revisions without steps.
func (e *Engine) plan(ctx context.Context, items []item) (*Plan, error) {
	var servers []string
	byServer := map[string][]*item{}
	for i := range items {
		it := &items[i]
		for _, s := range it.targets() {
			if _, ok := byServer[s]; !ok {
				servers = append(servers, s)
			}
			byServer[s] = append(byServer[s], it)
		}
	}

	plan := &Plan{}
	fp := sha256.New()
	for i := range items {
		it := &items[i]
		if it.changes() {
			plan.Revisions++
		}
		sum := sha256.Sum256(it.encoded)
		fmt.Fprintf(fp, "item %s|%s|%s|%d|%x|%s\n", it.ref.Kind, it.ref.Scope, it.ref.Name, it.base, sum, strings.Join(it.servers, " "))
	}
	for _, server := range servers {
		its := byServer[server]
		sp := ServerPlan{Server: server}
		var steps []resource.Step
		agent, online := e.agents.Agent(server)
		if online {
			desired := make([]resource.Desired, len(its))
			for i, it := range its {
				desired[i] = resource.Desired{Ref: it.ref, Spec: it.desiredOn(server)}
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
				step, err := resource.PlanStep(d, it.ref, it.currentOn(server), it.desiredOn(server))
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
			fmt.Fprintf(fp, "step %s|%s|%s|%d|%d|%d|%t\n", server, s.Ref, s.Action, s.Effect, s.Downtime, len(s.Changes), s.Destroys)
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
	// Rollback is the change that puts the applied steps back, if one was
	// made.
	Rollback    string
	RollbackErr error
}

func (e *FailedError) Error() string {
	switch {
	case e.Rollback == "" && e.RollbackErr == nil:
		return fmt.Sprintf("%s failed at %s: %v", e.Change, e.Step, e.Err)
	case errors.Is(e.RollbackErr, ErrPending):
		return fmt.Sprintf("%s failed at %s: %v. It is being rolled back in %s, which finishes when %v", e.Change, e.Step, e.Err, e.Rollback, e.RollbackErr)
	case e.RollbackErr != nil:
		return fmt.Sprintf("%s failed at %s: %v. Rolling back failed too: %v. Check the servers, then undo or fix it by hand", e.Change, e.Step, e.Err, e.RollbackErr)
	default:
		return fmt.Sprintf("%s failed at %s: %v. It was rolled back in %s", e.Change, e.Step, e.Err, e.Rollback)
	}
}

func (e *FailedError) Unwrap() error { return e.Err }

// ErrPending means a rollback is on record but waits for a server to
// reconnect; Resume finishes it.
var ErrPending = errors.New("a server it touches reconnects")

// isRollback reports a change Dockwell made to roll another back. A
// rollback isn't refused for an offline server, and isn't rolled back in
// turn when it fails: that would re-apply what failed in the first place.
func isRollback(c store.Change) bool { return c.Actor == SystemActor && c.Reverts != "" }

// Apply carries out a planned change: it checks that nothing moved since
// the plan, commits the revisions and places them on their servers, then
// runs the steps. If a step fails, the steps already done are rolled back
// in a change of their own, and the error is a *FailedError.
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
	if !isRollback(c) {
		for _, s := range plan.Servers {
			if s.Offline {
				return fmt.Errorf("%w: %s. Apply again once its agent reconnects", ErrOffline, s.Server)
			}
		}
	}
	if plan.Fingerprint != c.Fingerprint {
		return fmt.Errorf("%w: plan %s again and review what changed", ErrPlanChanged, changeID)
	}

	// Desired state goes on record before anything runs, so agents
	// reconcile toward it even if the control plane stops halfway.
	now := e.now()
	err = e.db.Revise(ctx, func(tx *store.RevisionTx) error {
		if _, err := e.current(ctx, tx.Tx, c); err != nil {
			return err
		}
		for i := range items {
			it := &items[i]
			if !it.changes() {
				continue
			}
			rev, err := tx.AddRevision(ctx, store.NewRevision{
				Kind: string(it.ref.Kind), Scope: it.ref.Scope, Name: it.ref.Name, ID: e.newID(it.ref.Kind),
				Spec: it.encoded, Change: changeID, Position: i, At: now,
			})
			if err != nil {
				return err
			}
			it.id, it.rev = rev.ID, rev.Revision
			if err := tx.PlaceRevision(ctx, rev.ID, rev.Revision, it.servers, now); err != nil {
				return err
			}
		}
		return tx.SetChangeState(ctx, changeID, store.ChangeApplying, "", now)
	})
	if err != nil {
		return err
	}
	// From here the change is on record, so it runs to an end even if the
	// caller goes away: a change left half applied is worse than one that
	// finishes late.
	return e.run(context.WithoutCancel(ctx), c, items, plan)
}

// newID names a new resource with its kind's prefix.
func (e *Engine) newID(kind resource.Kind) string {
	if t, err := e.kinds.Type(kind); err == nil {
		if p, ok := t.(resource.IDPrefixer); ok {
			return id.New(p.IDPrefix())
		}
	}
	return id.New(id.Resource)
}

// run carries out a committed change. Creates, updates and replaces run on
// every server before any delete, so what is built comes first and what
// goes is taken down last. Each step's deployment records how it went. A
// rollback skips servers that aren't connected and stays applying until
// Resume reaches them.
func (e *Engine) run(ctx context.Context, c store.Change, items []item, plan *Plan) error {
	type at struct {
		server string
		ref    resource.Ref
	}
	byRef := map[resource.Ref]*item{}
	for i := range items {
		byRef[items[i].ref] = &items[i]
	}
	type queued struct {
		server string
		step   *resource.Step
	}
	var builds, deletes []queued
	for si := range plan.Servers {
		s := &plan.Servers[si]
		for i := range s.Steps {
			q := queued{server: s.Server, step: &s.Steps[i]}
			if q.step.Action == resource.ActionDelete {
				deletes = append(deletes, q)
			} else {
				builds = append(builds, q)
			}
		}
	}
	// Each server's deletes are already last first; take the servers in
	// reverse too.
	slices.SortStableFunc(deletes, func(a, b queued) int {
		return slices.Index(serversOf(plan), b.server) - slices.Index(serversOf(plan), a.server)
	})

	pending := map[string]bool{}
	done := map[at]bool{}
	for _, q := range append(builds, deletes...) {
		it := byRef[q.step.Ref]
		agent, online := e.agents.Agent(q.server)
		if !online || agent == nil {
			if isRollback(c) {
				pending[q.server] = true
				continue
			}
			err := fmt.Errorf("%w: %s lost its connection partway through", ErrOffline, q.server)
			e.deployed(ctx, it, q.server, err)
			return e.fail(ctx, c, q.step.Ref, err)
		}
		if err := agent.Apply(ctx, *q.step, it.desiredOn(q.server)); err != nil {
			e.deployed(ctx, it, q.server, err)
			return e.fail(ctx, c, q.step.Ref, err)
		}
		e.deployed(ctx, it, q.server, nil)
		done[at{q.server, it.ref}] = true
	}
	// Where a revision needed no step, the server already has it.
	for i := range items {
		it := &items[i]
		for _, s := range it.servers {
			if !done[at{s, it.ref}] && !pending[s] {
				e.deployed(ctx, it, s, nil)
			}
		}
	}
	if len(pending) > 0 {
		waiting := slices.Sorted(maps.Keys(pending))
		e.logger.WarnContext(ctx, "a rollback waits for servers to reconnect", slog.String("change", c.ID), slog.Any("servers", waiting))
		return fmt.Errorf("%w: %s", ErrPending, strings.Join(waiting, ", "))
	}
	return e.finish(ctx, c, store.ChangeApplied, "", "change.applied")
}

// deployed records how a step went on a server: the revision is live
// there, or failed; or, where the step took it off the server, what it
// had there is superseded. A record that can't be written is logged, not
// fatal: the step itself happened, and Drift reads the servers, not this.
func (e *Engine) deployed(ctx context.Context, it *item, server string, failed error) {
	now := e.now()
	err := e.db.Revise(ctx, func(tx *store.RevisionTx) error {
		switch {
		case failed != nil && slices.Contains(it.servers, server):
			return tx.SetDeploymentState(ctx, store.Deployment{
				ResourceID: it.id, Revision: it.rev, Server: server, State: store.DeploymentFailed, Error: failed.Error(), UpdatedAt: now,
			})
		case failed != nil:
			return nil //nolint:nilerr // a failed step off a server leaves its deployment as it was
		case slices.Contains(it.servers, server):
			return tx.SetDeploymentState(ctx, store.Deployment{ResourceID: it.id, Revision: it.rev, Server: server, State: store.DeploymentLive, UpdatedAt: now})
		default:
			return tx.Supersede(ctx, it.id, server, it.rev+1, now)
		}
	})
	if err != nil {
		e.logger.WarnContext(ctx, "can't record a deployment", slog.String("resource", it.ref.String()), slog.String("server", server),
			slog.String("error", err.Error()))
	}
}

func serversOf(p *Plan) []string {
	out := make([]string, len(p.Servers))
	for i := range p.Servers {
		out[i] = p.Servers[i].Server
	}
	return out
}

// Resume finishes every change left applying: by a control plane that
// stopped partway, or by a rollback waiting for a server. Each resource is
// planned again against its servers, toward the revision the change
// committed, and the steps that remain run as in Apply. A change waits
// while any server it touches is offline, and skips resources a later
// change has moved on. Call it on start and whenever a server reconnects.
func (e *Engine) Resume(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ids []string
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		ids, err = tx.ChangesIn(ctx, store.ChangeApplying)
		return err
	}); err != nil {
		return err
	}
	var errs []error
	for _, changeID := range ids {
		if err := e.resume(ctx, changeID); err != nil && !errors.Is(err, ErrPending) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) resume(ctx context.Context, changeID string) error {
	var c store.Change
	var items []item
	err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if c, err = tx.GetChange(ctx, changeID); err != nil {
			return err
		}
		for _, ci := range c.Items {
			ref := resource.Ref{Kind: resource.Kind(ci.Kind), Scope: ci.Scope, Name: ci.Name}
			it, err := e.load(ctx, tx, ref)
			if err != nil {
				return err
			}
			if it.spec = it.current; it.current != nil {
				if it.encoded, err = resource.Encode(it.current); err != nil {
					return err
				}
			}
			if !bytes.Equal(it.encoded, ci.Spec) || it.base == 0 {
				continue // a later change moved it on
			}
			// It goes where the latest revision is placed, from wherever
			// the revision the change started from was, so the steps
			// that remain on servers it left still run.
			it.servers = it.placed
			if it.placed, err = e.placement(ctx, tx, it, ci.BaseRevision); err != nil {
				return err
			}
			items = append(items, it)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := range items {
		for _, s := range items[i].targets() {
			if _, online := e.agents.Agent(s); !online {
				return fmt.Errorf("%w: %s", ErrPending, s)
			}
		}
	}
	e.logger.InfoContext(ctx, "resuming a change left applying", slog.String("change", c.ID))
	plan, err := e.plan(ctx, items)
	if err != nil {
		return err
	}
	return e.run(context.WithoutCancel(ctx), c, items, plan)
}

// current checks that every item still starts from the latest revision,
// and that its scope is still there, and reads them as they stand.
func (e *Engine) current(ctx context.Context, tx *store.Tx, c store.Change) ([]item, error) {
	items := make([]item, 0, len(c.Items))
	for _, ci := range c.Items {
		ref := resource.Ref{Kind: resource.Kind(ci.Kind), Scope: ci.Scope, Name: ci.Name}
		it, err := e.load(ctx, tx, ref)
		if err != nil {
			return nil, err
		}
		if it.base != ci.BaseRevision {
			r, _ := tx.GetResource(ctx, ci.Kind, ci.Scope, ci.Name)
			return nil, fmt.Errorf("%w: %s changed in %s after %s was planned. Plan it again", ErrStale, ref, r.ChangeID, c.ID)
		}
		it.encoded, it.servers = ci.Spec, ci.Servers
		if ci.Spec != nil {
			if it.spec, err = e.kinds.Decode(ref.Kind, ci.Spec); err != nil {
				return nil, err
			}
		}
		items = append(items, it)
	}
	if err := e.check(ctx, tx, items); err != nil {
		return nil, fmt.Errorf("%w. Plan %s again", err, c.ID)
	}
	return items, nil
}

// fail records a failed apply and rolls back what it changed. The
// rollback commits at once, so desired state goes back even when a server
// can't be reached yet.
func (e *Engine) fail(ctx context.Context, c store.Change, at resource.Ref, cause error) error {
	ferr := &FailedError{Change: c.ID, Step: at, Err: cause}
	if err := e.finish(ctx, c, store.ChangeFailed, cause.Error(), "change.failed"); err != nil {
		return errors.Join(ferr, err)
	}
	if isRollback(c) {
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
	return e.db.Revise(ctx, func(tx *store.RevisionTx) error {
		if err := tx.SetChangeState(ctx, c.ID, state, errText, time.Time{}); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{ID: id.New(id.Event), At: now, Actor: c.Actor, Action: event, Subject: c.ID, Detail: c.Summary})
	})
}

// reverse returns the edits that put back what a change's revisions
// replaced, where they ran, refusing any resource that has changed again
// since.
func (e *Engine) reverse(ctx context.Context, changeID string) ([]Edit, error) {
	var edits []Edit
	err := e.db.View(ctx, func(tx *store.Tx) error {
		revs, err := tx.RevisionsOf(ctx, changeID)
		if err != nil {
			return err
		}
		for i := range revs {
			rev := &revs[i]
			ref := resource.Ref{Kind: resource.Kind(rev.Kind), Scope: rev.Scope, Name: rev.Name}
			latest, err := tx.GetResource(ctx, rev.Kind, rev.Scope, rev.Name)
			if err != nil {
				return err
			}
			if latest.Revision != rev.Revision {
				return fmt.Errorf("%w: %s changed again in %s. Undo that first, or edit %s directly", ErrCantUndo, ref, latest.ChangeID, rev.Name)
			}
			ed := Edit{Ref: ref}
			if rev.Revision > 1 {
				prev, err := tx.GetRevision(ctx, rev.ID, rev.Revision-1)
				if err != nil {
					return err
				}
				if prev.Spec != nil {
					if ed.Spec, err = e.kinds.Decode(ref.Kind, prev.Spec); err != nil {
						return err
					}
					if ed.Servers, err = tx.Placement(ctx, rev.ID, prev.Revision); err != nil {
						return err
					}
				}
			}
			edits = append(edits, ed)
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

// Drift compares every resource a server should have, its own and those
// deployed on it, with what it actually has.
func (e *Engine) Drift(ctx context.Context, server string) ([]Drift, error) {
	agent, online := e.agents.Agent(server)
	if !online {
		return nil, fmt.Errorf("%w: %s. Its drift is checked when its agent reconnects", ErrOffline, server)
	}
	var resources []store.Resource
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		resources, err = tx.ListPlaced(ctx, server)
		return err
	}); err != nil {
		return nil, err
	}
	refs := make([]resource.Ref, len(resources))
	for i, r := range resources {
		refs[i] = resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}
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
		// Agents normalize what they observe, so only real differences
		// are unequal.
		if resource.Equal(desired, observed[i]) {
			continue
		}
		d := Drift{Ref: refs[i], Revision: r.Revision, Observed: observed[i], Missing: observed[i] == nil}
		if !d.Missing {
			d.Changes = resource.Diff(desired, observed[i])
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

// days writes a long duration the way the plan sheet does: "7 days", or
// "7 days and 3 hours" when the hours matter.
func days(d time.Duration) string {
	n, h := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour)
	out := plural(n, "day")
	if h > 0 {
		out += " and " + plural(h, "hour")
	}
	return out
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
