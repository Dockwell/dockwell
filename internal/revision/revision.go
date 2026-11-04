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

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
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
	// ErrNotFound means there's no such change.
	ErrNotFound = errors.New("there's no such change")
	// ErrBusy means another change is running steps on a resource the
	// change touches; apply it once that one ends.
	ErrBusy = errors.New("a resource in the change is being applied")
)

// Surfaces a change can come from, as the store and the audit log record
// them.
const (
	ViaPanel     = "panel"
	ViaAPI       = "api"
	ViaAutomatic = "automatic" // Dockwell on its own, such as a rollback
)

// By is who does something to a change, and from where.
type By struct {
	Actor string
	Via   string // ViaPanel, ViaAPI or ViaAutomatic
}

// system is Dockwell acting on its own.
var system = By{Actor: SystemActor, Via: ViaAutomatic}

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
	// One commit at a time keeps base revisions honest: checking that
	// nothing moved and committing the revisions happen together. The steps
	// then run outside it, so changes to different resources don't wait for
	// each other (ADR 0009).
	mu sync.Mutex
	// runs counts the changes running in the background, for Wait.
	runs sync.WaitGroup

	// pmu guards the progress of the changes running in this process, and
	// who watches which change.
	pmu      sync.Mutex
	live     map[string]*Progress
	watchers map[string]map[chan Update]struct{}
	// busy names, for each resource a running change touches, that change.
	// Only commits add to it, under mu, so two changes never run steps on
	// one resource at once.
	busy map[string]string
}

// New returns an engine over db, using kinds to read and check specs, and
// agents to reach servers. Projects and environments are always among the
// kinds.
func New(db *store.DB, kinds *resource.Registry, agents Agents, logger *slog.Logger) *Engine {
	return &Engine{
		db: db, kinds: kinds.With(scope.Types()...), agents: agents, logger: logger, now: time.Now,
		live: map[string]*Progress{}, watchers: map[string]map[chan Update]struct{}{}, busy: map[string]string{},
	}
}

// Kinds is the registry the engine reads and checks specs with.
func (e *Engine) Kinds() *resource.Registry { return e.kinds }

// Edit sets one resource's desired spec; a nil Spec deletes it.
type Edit struct {
	Ref  resource.Ref
	Spec proto.Message
	// Servers places a resource of an environment-level kind, such as a
	// service, by server ID. Nil keeps it where it runs now, on the
	// servers still in the fleet; a new one runs on the only server when
	// there is one, and must say where when there are several. A removed
	// server is never one of them. Other kinds ignore it: a host-level
	// resource is on its server, and projects and environments run on none.
	Servers []string
}

// Proposal is a set of edits someone wants to make.
type Proposal struct {
	Summary string // what it is for, in the actor's words
	Actor   string
	Via     string // where it comes from, such as ViaPanel
	Edits   []Edit
}

// Plan is what applying a change would do.
type Plan struct {
	Change  string // the planned change's ID, to apply or discard
	Summary string
	Reverts string // the change this undoes or rolls back, if any
	// Resources says what the change does to each resource as a whole, in
	// proposal order.
	Resources []ResourcePlan
	// Servers lists each server with something to do, in proposal order,
	// then each removed server the change takes resources off.
	Servers []ServerPlan
	// Revisions is how many resources get a new revision. A plan can have
	// revisions without steps, such as adopting drift or making an
	// environment.
	Revisions   int
	Fingerprint string
}

// ResourcePlan is what a plan does to one resource as a whole.
type ResourcePlan struct {
	ID  string
	Ref resource.Ref
	// Action is the most disruptive of its steps, or for a kind that runs
	// on no server, what its revision does: create, update or delete. None
	// means nothing changes.
	Action resource.Action
	Base   int // the revision it starts from; 0 if new
	// Changes are the fields that differ from the latest revision.
	Changes []resource.Change
	// Servers is where it runs once applied, and Previous where it runs
	// now, by server ID.
	Servers, Previous []string
}

// ServerPlan is the part of a plan on one server.
type ServerPlan struct {
	Server string
	Name   string // the server's name, for people
	// Offline means the server isn't connected, so the plan is based on
	// its last known desired state.
	Offline bool
	// Removed means the server was removed from the fleet. Nothing runs
	// there any more, so the plan has no steps on it: what the change
	// takes off it is only recorded.
	Removed bool
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

// Counts is how many resources a plan creates, changes, replaces and
// destroys. A replace is counted on its own, never among the changes.
type Counts struct {
	Create, Change, Replace, Destroy int
}

// Counts sums up the plan by resource.
func (p *Plan) Counts() Counts {
	var c Counts
	for i := range p.Resources {
		switch p.Resources[i].Action {
		case resource.ActionNone:
		case resource.ActionCreate:
			c.Create++
		case resource.ActionUpdate:
			c.Change++
		case resource.ActionReplace:
			c.Replace++
		case resource.ActionDelete:
			c.Destroy++
		}
	}
	return c
}

// Destructive reports a plan with a step that destroys data or may cut
// off access, which applying can't fully undo.
func (p *Plan) Destructive() bool {
	steps := p.Steps()
	for i := range steps {
		if steps[i].Destroys || len(steps[i].Warnings) > 0 {
			return true
		}
	}
	return false
}

// item is one edit with what it starts from.
type item struct {
	ref   resource.Ref
	level resource.Level
	// id names the resource. A new one gets its ID when the change is
	// planned, so another item in the change can live in it.
	id      string
	base    int           // the latest revision now; 0 if none
	current proto.Message // the latest spec; nil if none or deleted
	spec    proto.Message // what is wanted; nil deletes
	encoded []byte
	// placed is where the base revision is deployed, and servers where the
	// new one goes, by server ID and sorted. Both are empty for kinds that
	// run on no server.
	placed, servers []string
	// gone are the servers among those that were removed from the fleet.
	// The item takes no steps there.
	gone []string
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

// targets are the servers applying the item takes steps on: where it is
// and where it goes, less the servers that were removed.
func (it item) targets() []string {
	out := slices.Concat(it.placed, it.servers)
	slices.Sort(out)
	return slices.DeleteFunc(slices.Compact(out), func(s string) bool { return slices.Contains(it.gone, s) })
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
// apply. A new resource gets its ID now. A project the change brings to
// life gets its first environment in the same change: production, unless
// the change makes one in it.
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
			if it.id == "" {
				it.id = e.newID(ed.Ref.Kind)
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
		var err error
		if items, err = e.withFirstEnvironments(ctx, tx, items); err != nil {
			return err
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
		Fingerprint: plan.Fingerprint, Plan: record, CreatedAt: e.now(), ProposedVia: p.Via,
	}
	for i := range items {
		it := &items[i]
		c.Items = append(c.Items, store.ChangeItem{
			ResourceID: it.id, Kind: string(it.ref.Kind), Scope: it.ref.Scope, Name: it.ref.Name, BaseRevision: it.base, Spec: it.encoded,
			Servers: it.servers,
		})
	}
	err = e.db.Revise(ctx, func(tx *store.RevisionTx) error {
		if err := tx.CreateChange(ctx, c); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: c.CreatedAt, Actor: c.Actor, Action: "change.planned", Subject: c.ID, Detail: c.Summary, Surface: c.ProposedVia,
		})
	})
	if err != nil {
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
		if err := markGone(ctx, tx, &it); err != nil {
			return it, err
		}
	}
	return it, nil
}

// markGone notes which of the servers the item touches were removed from
// the fleet. Nothing runs on them any more, so the item takes no steps
// there.
func markGone(ctx context.Context, tx *store.Tx, it *item) error {
	it.gone = nil
	for _, s := range it.targets() {
		gone, err := serverGone(ctx, tx, s)
		if err != nil {
			return err
		}
		if gone {
			it.gone = append(it.gone, s)
		}
	}
	return nil
}

// serverGone reports a server that was removed from the fleet, or never
// was in it.
func serverGone(ctx context.Context, tx *store.Tx, server string) (bool, error) {
	s, err := tx.GetServer(ctx, server)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return true, nil
	case err != nil:
		return false, err
	}
	return s.RevokedAt != nil, nil
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
// or stays where it is, on the servers still in the fleet; a new one, or
// one whose servers were all removed, goes on the only server, and must be
// told where when there is a choice (PRD principle 4). Nothing is placed
// on a removed server.
func (e *Engine) place(ctx context.Context, tx *store.Tx, it item, want []string) ([]string, error) {
	switch {
	case it.spec == nil || !it.level.RunsOnServers():
		return nil, nil
	case it.level == resource.LevelServer:
		return []string{it.ref.Scope}, nil
	}
	// removed says why a service that ran only on removed servers is
	// placed like a new one.
	var removed string
	if want == nil && it.current != nil {
		kept := slices.DeleteFunc(slices.Clone(it.placed), func(s string) bool { return slices.Contains(it.gone, s) })
		if len(kept) > 0 {
			return kept, nil
		}
		removed = fmt.Sprintf("%s ran on %s, which %s removed from the fleet, and ", it.ref.Name, strings.Join(it.gone, ", "), wasOrWere(len(it.gone)))
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
			return nil, fmt.Errorf("%w: %sthere's no server to run %s on. Add a server first", ErrInvalid, removed, it.ref.Name)
		}
		names := make([]string, len(active))
		for i := range active {
			names[i] = active[i].Name
		}
		return nil, fmt.Errorf("%w: %sthere are %d servers, so say which %s runs on: %s", ErrInvalid, removed, len(active), it.ref.Name,
			strings.Join(names, ", "))
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("%w: %s needs a server to run on. To stop it everywhere, delete it", ErrInvalid, it.ref.Name)
	}
	out := slices.Clone(want)
	slices.Sort(out)
	out = slices.Compact(out)
	for _, s := range out {
		if !slices.ContainsFunc(active, func(a store.Server) bool { return a.ID == s }) {
			return nil, notInFleet(ctx, tx, s, it.ref.Name)
		}
	}
	return out, nil
}

// notInFleet explains why a resource can't be placed on a server.
func notInFleet(ctx context.Context, tx *store.Tx, server, name string) error {
	if _, err := tx.GetServer(ctx, server); err == nil {
		return fmt.Errorf("%w: %s was removed from the fleet, so %s can't run there. Choose a server that is still in it", ErrInvalid, server, name)
	}
	return fmt.Errorf("%w: there's no server %s to run %s on", ErrInvalid, server, name)
}

func wasOrWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
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
	if c, ok := t.(resource.NameChecker); ok {
		if err := c.CheckName(ref.Name); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	return nil
}

// check refuses a change that would leave the model broken once it
// applies. It looks at the change as a whole: what is stored, plus what the
// change makes and deletes. So it refuses
//   - an item whose scope isn't there, or goes in the same change;
//   - an item placed on a server that was removed;
//   - a project that would come to life with no environment;
//   - deleting a scope that would still hold something, or the last
//     environment of a project, or the organization's last project.
//
// It runs when proposing, and again in the transaction that applies, so a
// change planned before the world moved can't slip through.
func (e *Engine) check(ctx context.Context, tx *store.Tx, items []item) error {
	byID := make(map[string]*item, len(items))
	for i := range items {
		byID[items[i].id] = &items[i]
	}
	for i := range items {
		it := &items[i]
		if it.spec == nil {
			continue
		}
		if err := e.checkScope(ctx, tx, it, byID); err != nil {
			return err
		}
		if it.level == resource.LevelEnvironment {
			for _, s := range it.servers {
				gone, err := serverGone(ctx, tx, s)
				if err != nil {
					return err
				}
				if gone {
					return notInFleet(ctx, tx, s, it.ref.Name)
				}
			}
		}
		if it.ref.Kind == scope.ProjectKind && it.current == nil {
			envs, err := liveIn(ctx, tx, items, it.id, scope.EnvironmentKind)
			if err != nil {
				return err
			}
			if len(envs) == 0 {
				return fmt.Errorf("%w: project %s would have no environment. Make one in it in the same change, such as %s", ErrInvalid,
					it.ref.Name, scope.DefaultEnvironment)
			}
		}
	}
	for i := range items {
		it := &items[i]
		if it.spec != nil || it.current == nil || it.ref.Kind != scope.ProjectKind && it.ref.Kind != scope.EnvironmentKind {
			continue
		}
		// A scope goes only once what lives in it has gone, in this change
		// or an earlier one.
		left, err := liveIn(ctx, tx, items, it.id, "")
		if err != nil {
			return err
		}
		if len(left) > 0 {
			return fmt.Errorf("%w: %s %s still holds %s. Delete them first, or in the same change", ErrInvalid, it.ref.Kind, it.ref.Name,
				strings.Join(left, ", "))
		}
		// Its parent keeps a place for things to go, unless it goes too: a
		// project keeps an environment, and the organization a project.
		if parent, ok := byID[it.ref.Scope]; ok && parent.spec == nil {
			continue
		}
		siblings, err := liveIn(ctx, tx, items, it.ref.Scope, it.ref.Kind)
		if err != nil {
			return err
		}
		if len(siblings) == 0 {
			where := "the organization"
			if it.ref.Kind == scope.EnvironmentKind {
				project, err := tx.GetResourceByID(ctx, it.ref.Scope)
				if err != nil {
					return err
				}
				where = "project " + project.Name
			}
			return fmt.Errorf("%w: %s is the last %s in %s, which needs one. Make another first, or in the same change", ErrInvalid,
				it.ref.Name, it.ref.Kind, where)
		}
	}
	return nil
}

// liveIn names what lives in a scope once the change applies, of one kind
// or of every kind when kind is empty: what is stored there, less what the
// change deletes, plus what it makes.
func liveIn(ctx context.Context, tx *store.Tx, items []item, scopeID string, kind resource.Kind) ([]string, error) {
	stored, err := tx.ListResources(ctx, scopeID, string(kind))
	if err != nil {
		return nil, err
	}
	live := make(map[string]string, len(stored)) // ID → name
	for _, r := range stored {
		live[r.ID] = r.Name
	}
	for i := range items {
		it := &items[i]
		switch {
		case it.ref.Scope != scopeID || kind != "" && it.ref.Kind != kind:
		case it.spec == nil:
			delete(live, it.id)
		default:
			live[it.id] = it.ref.Name
		}
	}
	return slices.Sorted(maps.Values(live)), nil
}

// checkScope refuses an item whose scope isn't there once the change
// applies, or isn't the kind of thing its level lives in.
func (e *Engine) checkScope(ctx context.Context, tx *store.Tx, it *item, byID map[string]*item) error {
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
	// The change may make its scope, or delete it.
	if in, ok := byID[it.ref.Scope]; ok && in.ref.Kind == want {
		if in.spec != nil {
			return nil
		}
		fix := "delete " + it.ref.Name + " too"
		if it.current == nil {
			fix = "leave " + it.ref.Name + " out"
		}
		return fmt.Errorf("%w: this change deletes %s %s, so %s %s has nowhere to live. Keep %s, or %s", ErrInvalid,
			in.ref.Kind, in.ref.Name, it.ref.Kind, it.ref.Name, in.ref.Name, fix)
	}
	r, err := tx.GetResourceByID(ctx, it.ref.Scope)
	if errors.Is(err, store.ErrNotFound) || err == nil && (r.Kind != string(want) || r.Spec == nil) {
		return missing(string(want))
	}
	return err
}

// withFirstEnvironments gives each project the change brings to life an
// environment in the same change, so no project is ever without one:
// production, as the install's first project has (PRD principle 4), unless
// the change makes one in it. A project deleted before keeps its history,
// and so does its production.
func (e *Engine) withFirstEnvironments(ctx context.Context, tx *store.Tx, items []item) ([]item, error) {
	for i := range items {
		p := items[i]
		if p.ref.Kind != scope.ProjectKind || p.spec == nil || p.current != nil {
			continue
		}
		envs, err := liveIn(ctx, tx, items, p.id, scope.EnvironmentKind)
		if err != nil {
			return items, err
		}
		if len(envs) > 0 {
			continue
		}
		env, err := e.load(ctx, tx, resource.Ref{Kind: scope.EnvironmentKind, Scope: p.id, Name: scope.DefaultEnvironment})
		if err != nil {
			return items, err
		}
		if env.id == "" {
			env.id = e.newID(scope.EnvironmentKind)
		}
		env.spec = &resourcev1.Environment{}
		if env.encoded, err = resource.Encode(env.spec); err != nil {
			return items, err
		}
		items = append(items, env)
	}
	return items, nil
}

// plan asks each server's agent what the items would take there. A server
// that isn't connected is planned from its last known desired state, and
// one that was removed has no steps. Items that run on no server make
// revisions without steps.
//
// Planning changes nothing on any server, and the same items against the
// same state give the same plan and fingerprint: the fingerprint leaves
// out the IDs given to new resources, which differ every time.
func (e *Engine) plan(ctx context.Context, items []item) (*Plan, error) {
	var servers, removed []string
	byServer := map[string][]*item{}
	for i := range items {
		it := &items[i]
		for _, s := range it.targets() {
			if _, ok := byServer[s]; !ok {
				servers = append(servers, s)
			}
			byServer[s] = append(byServer[s], it)
		}
		for _, s := range it.gone {
			if !slices.Contains(removed, s) {
				removed = append(removed, s)
			}
		}
	}
	names, err := e.serverNames(ctx, slices.Concat(servers, removed))
	if err != nil {
		return nil, err
	}

	// A new resource's ID is given when it is planned, so planning again
	// gives another. The fingerprint names it by its place in the change.
	fresh := map[string]string{}
	for i := range items {
		if items[i].base == 0 {
			fresh[items[i].id] = fmt.Sprintf("new#%d", i)
		}
	}
	stable := func(s string) string {
		if v, ok := fresh[s]; ok {
			return v
		}
		return s
	}
	ref := func(r resource.Ref) string { return fmt.Sprintf("%s|%s|%s", r.Kind, stable(r.Scope), r.Name) }

	plan := &Plan{}
	fp := sha256.New()
	for i := range items {
		it := &items[i]
		if it.changes() {
			plan.Revisions++
		}
		sum := sha256.Sum256(it.encoded)
		fmt.Fprintf(fp, "item %s|%s|%d|%t|%x|%s\n", stable(it.id), ref(it.ref), it.base, it.spec != nil, sum, strings.Join(it.servers, " "))
	}
	actions := map[string]resource.Action{} // the most disruptive step of each item
	for _, server := range servers {
		its := byServer[server]
		sp := ServerPlan{Server: server, Name: names[server]}
		var steps []resource.Step
		agent, online := e.agents.Agent(server)
		if online {
			desired := make([]resource.Desired, len(its))
			for i, it := range its {
				desired[i] = resource.Desired{Ref: it.ref, Spec: it.desiredOn(server)}
			}
			var err error
			if steps, err = agent.Plan(ctx, desired); err != nil {
				return nil, &AgentError{Server: sp.Name, Doing: "plan", Err: err}
			}
			if len(steps) != len(desired) {
				return nil, fmt.Errorf("can't plan on %s: its agent judged %d steps for %d resources", sp.Name, len(steps), len(desired))
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
		ids := map[resource.Ref]string{}
		for _, it := range its {
			ids[it.ref] = it.id
		}
		steps = order(steps)
		for i := range steps {
			s := &steps[i]
			if s.Action == resource.ActionNone {
				continue
			}
			sp.Steps = append(sp.Steps, *s)
			sp.Downtime += s.Downtime
			if s.Action > actions[ids[s.Ref]] {
				actions[ids[s.Ref]] = s.Action
			}
			fmt.Fprintf(fp, "step %s|%s|%s|%d|%d|%d|%t|%v\n", server, ref(s.Ref), s.Action, s.Effect, s.Downtime, len(s.Changes), s.Destroys, s.Warnings)
			for _, c := range s.Changes {
				fmt.Fprintf(fp, "  %s|%d|%s|%s|%t\n", c.Path, c.Op, c.From, c.To, c.Sensitive)
			}
		}
		if len(sp.Steps) > 0 {
			plan.Servers = append(plan.Servers, sp)
		}
	}
	for _, s := range removed {
		plan.Servers = append(plan.Servers, ServerPlan{Server: s, Name: names[s], Removed: true})
		fmt.Fprintf(fp, "removed %s\n", s)
	}
	for i := range items {
		plan.Resources = append(plan.Resources, items[i].resourcePlan(actions[items[i].id]))
	}
	plan.Fingerprint = hex.EncodeToString(fp.Sum(nil)[:16])
	return plan, nil
}

// resourcePlan says what the change does to the item as a whole, given
// its most disruptive step.
func (it item) resourcePlan(step resource.Action) ResourcePlan {
	rp := ResourcePlan{
		ID: it.id, Ref: it.ref, Base: it.base, Changes: resource.Diff(it.current, it.spec),
		Servers: slices.Clone(it.servers), Previous: slices.Clone(it.placed),
	}
	switch {
	case it.spec == nil:
		rp.Action = resource.ActionDelete
	case it.current == nil:
		rp.Action = resource.ActionCreate
	case step == resource.ActionReplace:
		rp.Action = resource.ActionReplace
	case step != resource.ActionNone || it.changes():
		// Making it on a new server, or taking it off one, changes where
		// it runs: an update of the resource as a whole.
		rp.Action = resource.ActionUpdate
	}
	return rp
}

// serverNames reads the names of servers, removed ones included.
func (e *Engine) serverNames(ctx context.Context, servers []string) (map[string]string, error) {
	names := make(map[string]string, len(servers))
	if len(servers) == 0 {
		return names, nil
	}
	err := e.db.View(ctx, func(tx *store.Tx) error {
		for _, id := range servers {
			s, err := tx.GetServer(ctx, id)
			switch {
			case errors.Is(err, store.ErrNotFound):
				names[id] = id
			case err != nil:
				return err
			default:
				names[id] = s.Name
			}
		}
		return nil
	})
	return names, err
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

// AgentError is a server's agent failing a call, or refusing one, as for
// a kind it has no driver for. Its message is for people.
type AgentError struct {
	Server string // the server's name
	Doing  string // such as "plan"
	Err    error
}

func (e *AgentError) Error() string {
	return fmt.Sprintf("can't %s on %s: %v", e.Doing, e.Server, e.Err)
}

func (e *AgentError) Unwrap() error { return e.Err }

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

// Apply carries out a planned change on behalf of whoever proposed it: it
// checks that nothing moved since the plan, commits the revisions and
// places them on their servers, then runs the steps, and returns once they
// have run to an end. If a step fails, the steps already done are rolled
// back in a change of their own, and the error is a *FailedError.
func (e *Engine) Apply(ctx context.Context, changeID string) error {
	return e.ApplyBy(ctx, changeID, By{})
}

// ApplyBy is Apply on behalf of by. A zero By applies as the proposer.
func (e *Engine) ApplyBy(ctx context.Context, changeID string, by By) error {
	r, err := e.commit(ctx, changeID, by)
	if err != nil {
		return err
	}
	// From here the change is on record, so it runs to an end even if the
	// caller goes away: a change left half applied is worse than one that
	// finishes late.
	return e.run(context.WithoutCancel(ctx), r)
}

// Start is ApplyBy that returns once the revisions are committed and the
// change is applying, and runs its steps in the background. Follow them
// with Watch; Wait waits for every run to end.
func (e *Engine) Start(ctx context.Context, changeID string, by By) error {
	r, err := e.commit(ctx, changeID, by)
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	e.runs.Go(func() {
		err := e.run(ctx, r)
		// A failure and a wait for a server are logged where they happen.
		if ferr := (*FailedError)(nil); err != nil && !errors.As(err, &ferr) && !errors.Is(err, ErrPending) {
			e.logger.ErrorContext(ctx, "a change stopped partway; Dockwell resumes it on its next start", slog.String("change", r.c.ID),
				slog.String("error", err.Error()))
		}
	})
	return nil
}

// Wait waits for the changes running in the background to end.
func (e *Engine) Wait() { e.runs.Wait() }

// committed is a change whose revisions are on record, ready to run.
type committed struct {
	c     store.Change
	items []item
	plan  *Plan
}

// commit checks that a planned change still holds and commits its
// revisions, so desired state is on record before anything runs: agents
// then reconcile toward it even if the control plane stops halfway.
func (e *Engine) commit(ctx context.Context, changeID string, by By) (*committed, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var c store.Change
	var items []item
	err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if c, err = tx.GetChange(ctx, changeID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%w: there's no change %s", ErrNotFound, changeID)
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
		return nil, err
	}

	plan, err := e.plan(ctx, items)
	if err != nil {
		return nil, err
	}
	if !isRollback(c) {
		for _, s := range plan.Servers {
			if s.Offline {
				return nil, fmt.Errorf("%w: %s. Apply again once its agent reconnects", ErrOffline, s.Name)
			}
		}
	}
	if plan.Fingerprint != c.Fingerprint {
		return nil, fmt.Errorf("%w: propose the change again and review what changed", ErrPlanChanged)
	}
	if err := e.claim(changeID, items, false); err != nil {
		return nil, err
	}
	if by.Actor == "" {
		by = By{Actor: c.Actor, Via: c.ProposedVia}
	}
	progress := newProgress(plan, items)
	record, err := json.Marshal(progress)
	if err != nil {
		return nil, err
	}

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
				Kind: string(it.ref.Kind), Scope: it.ref.Scope, Name: it.ref.Name, ID: it.id,
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
		return tx.StartApplying(ctx, changeID, by.Actor, by.Via, record, now)
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Discarded, or applied by someone else, since it was read.
			return nil, fmt.Errorf("%w: %s was applied or discarded meanwhile", ErrNotPlanned, changeID)
		}
		return nil, err
	}
	c.State, c.AppliedAt, c.AppliedBy, c.AppliedVia, c.Progress = store.ChangeApplying, now, by.Actor, by.Via, record
	_ = e.claim(changeID, items, true)
	e.track(changeID, progress)
	e.published(changeID, store.ChangeApplying)
	return &committed{c: c, items: items, plan: plan}, nil
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
// goes is taken down last. Each step's deployment records how it went, and
// its progress what it is doing. A rollback skips servers that aren't
// connected and stays applying until Resume reaches them.
func (e *Engine) run(ctx context.Context, r *committed) error {
	defer e.untrack(r.c.ID)
	c, items, plan := r.c, r.items, r.plan
	type at struct {
		server string
		ref    resource.Ref
	}
	byRef := map[resource.Ref]*item{}
	for i := range items {
		byRef[items[i].ref] = &items[i]
	}
	progress := e.progress(c.ID)
	if progress == nil {
		progress = &Progress{}
	}
	type queued struct {
		server, name string
		step         *resource.Step
		index        int // in the change's progress
	}
	var builds, deletes []queued
	for si := range plan.Servers {
		s := &plan.Servers[si]
		for i := range s.Steps {
			q := queued{server: s.Server, name: s.Name, step: &s.Steps[i]}
			if it, ok := byRef[q.step.Ref]; ok {
				q.index = progress.index(s.Server, it.id)
			}
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
	queue := slices.Concat(builds, deletes)

	// A removed server takes no steps. What the change took off it is
	// superseded there at once; what it was to run there never got there.
	for i := range items {
		it := &items[i]
		for _, s := range it.gone {
			var failed error
			if slices.Contains(it.servers, s) {
				failed = fmt.Errorf("%s was removed from the fleet before %s reached it", s, it.ref.Name)
			}
			e.deployed(ctx, c.ID, it, s, failed, nil)
		}
	}

	// stop records a failed step, skips the steps after it, and rolls the
	// change back.
	stop := func(qi int, err error) error {
		q := queue[qi]
		e.deployed(ctx, c.ID, byRef[q.step.Ref], q.server, err, e.step(c.ID, q.index, StepFailed, err.Error()))
		for _, rest := range queue[qi+1:] {
			e.step(c.ID, rest.index, StepSkipped, message(StepSkipped, rest.step.Action, rest.step.Ref, rest.name))
		}
		return e.fail(ctx, c, q.step.Ref, err)
	}
	pending := map[string]bool{}
	done := map[at]bool{}
	for qi, q := range queue {
		it := byRef[q.step.Ref]
		agent, online := e.agents.Agent(q.server)
		if !online || agent == nil {
			if isRollback(c) {
				pending[q.server] = true
				e.step(c.ID, q.index, StepWaiting, message(StepWaiting, q.step.Action, q.step.Ref, q.name))
				continue
			}
			return stop(qi, fmt.Errorf("%w: %s lost its connection partway through", ErrOffline, q.name))
		}
		e.step(c.ID, q.index, StepRunning, message(StepRunning, q.step.Action, q.step.Ref, q.name))
		if err := agent.Apply(ctx, *q.step, it.desiredOn(q.server)); err != nil {
			return stop(qi, err)
		}
		e.deployed(ctx, c.ID, it, q.server, nil, e.step(c.ID, q.index, StepDone, message(StepDone, q.step.Action, q.step.Ref, q.name)))
		done[at{q.server, it.ref}] = true
	}
	// Where a revision needed no step, the server already has it.
	for i := range items {
		it := &items[i]
		for _, s := range it.servers {
			if !done[at{s, it.ref}] && !pending[s] && !slices.Contains(it.gone, s) {
				e.deployed(ctx, c.ID, it, s, nil, nil)
			}
		}
	}
	if len(pending) > 0 {
		waiting := slices.Sorted(maps.Keys(pending))
		e.logger.WarnContext(ctx, "a rollback waits for servers to reconnect", slog.String("change", c.ID), slog.Any("servers", waiting))
		if p := e.progress(c.ID); p != nil {
			e.recordProgress(ctx, c.ID, *p)
		}
		return fmt.Errorf("%w: %s", ErrPending, strings.Join(waiting, ", "))
	}
	if err := e.finish(ctx, c, store.ChangeApplied, "", "change.applied", e.progress(c.ID)); err != nil {
		return err
	}
	e.published(c.ID, store.ChangeApplied)
	return nil
}

// deployed records how a step went on a server: the revision is live
// there, or failed; or, where the step took it off the server, what it
// had there is superseded. It records the change's progress with it. A
// record that can't be written is logged, not fatal: the step itself
// happened, and Drift reads the servers, not this.
func (e *Engine) deployed(ctx context.Context, changeID string, it *item, server string, failed error, progress *Progress) {
	now := e.now()
	err := e.db.Revise(ctx, func(tx *store.RevisionTx) error {
		if progress != nil {
			b, err := json.Marshal(progress)
			if err != nil {
				return err
			}
			if err := tx.SetChangeProgress(ctx, changeID, b); err != nil {
				return err
			}
		}
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

// recordProgress writes a change's progress, logging a failure: the steps
// happened either way.
func (e *Engine) recordProgress(ctx context.Context, changeID string, p Progress) {
	b, err := json.Marshal(p)
	if err == nil {
		err = e.db.Revise(ctx, func(tx *store.RevisionTx) error { return tx.SetChangeProgress(ctx, changeID, b) })
	}
	if err != nil {
		e.logger.WarnContext(ctx, "can't record a change's progress", slog.String("change", changeID), slog.String("error", err.Error()))
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
// change has moved on. A change already running here is left to finish.
// Call it on start and whenever a server reconnects.
func (e *Engine) Resume(ctx context.Context) error {
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
		r, err := e.prepareResume(ctx, changeID)
		if err == nil && r != nil {
			err = e.run(context.WithoutCancel(ctx), r)
		}
		if err != nil && !errors.Is(err, ErrPending) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// prepareResume plans what remains of a change left applying, and tracks
// it as running here. It returns nil when the change is already running
// here, and ErrPending while a server it touches is offline.
func (e *Engine) prepareResume(ctx context.Context, changeID string) (*committed, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running(changeID) {
		return nil, nil
	}
	var c store.Change
	var items []item
	err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if c, err = tx.GetChange(ctx, changeID); err != nil {
			return err
		}
		if c.State != store.ChangeApplying {
			return nil // it finished meanwhile
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
			if err := markGone(ctx, tx, &it); err != nil {
				return err
			}
			items = append(items, it)
		}
		return nil
	})
	if err != nil || c.State != store.ChangeApplying {
		return nil, err
	}
	for i := range items {
		for _, s := range items[i].targets() {
			if _, online := e.agents.Agent(s); !online {
				return nil, fmt.Errorf("%w: %s", ErrPending, s)
			}
		}
	}
	if err := e.claim(changeID, items, false); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPending, err)
	}
	e.logger.InfoContext(ctx, "resuming a change left applying", slog.String("change", c.ID))
	plan, err := e.plan(ctx, items)
	if err != nil {
		return nil, err
	}
	before, err := decodeProgress(c.Progress)
	if err != nil {
		return nil, err
	}
	_ = e.claim(changeID, items, true)
	e.track(changeID, resumed(before, plan, items))
	e.published(changeID, store.ChangeApplying)
	return &committed{c: c, items: items, plan: plan}, nil
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
		if it.id == "" {
			// A new resource has the ID the change gave it, so what lives
			// in it is where the plan put it. A change planned before IDs
			// were given then gets one; its plan no longer matches, so it
			// is planned again before it applies.
			it.id = ci.ResourceID
			if it.id == "" {
				it.id = e.newID(ref.Kind)
			}
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
// can't be reached yet. Watchers learn of the failure once the rollback is
// on record, so they can follow it.
func (e *Engine) fail(ctx context.Context, c store.Change, at resource.Ref, cause error) error {
	ferr := &FailedError{Change: c.ID, Step: at, Err: cause}
	if err := e.finish(ctx, c, store.ChangeFailed, cause.Error(), "change.failed", e.progress(c.ID)); err != nil {
		e.published(c.ID, store.ChangeFailed)
		return errors.Join(ferr, err)
	}
	if isRollback(c) {
		e.published(c.ID, store.ChangeFailed)
		return ferr
	}
	e.logger.WarnContext(ctx, "a change failed; rolling it back", slog.String("change", c.ID), slog.String("at", at.String()),
		slog.String("error", cause.Error()))
	// Its steps are over: the rollback takes its resources on.
	e.release(c.ID)
	edits, err := e.reverse(ctx, c.ID)
	if err != nil {
		ferr.RollbackErr = err
		e.published(c.ID, store.ChangeFailed)
		return ferr
	}
	if len(edits) == 0 {
		e.published(c.ID, store.ChangeFailed)
		return ferr
	}
	plan, err := e.propose(ctx, Proposal{Summary: "Roll back " + c.Summary, Actor: SystemActor, Via: ViaAutomatic, Edits: edits}, c.ID)
	if err != nil {
		ferr.RollbackErr = err
		e.published(c.ID, store.ChangeFailed)
		return ferr
	}
	ferr.Rollback = plan.Change
	e.published(c.ID, store.ChangeFailed)
	if err := e.ApplyBy(ctx, plan.Change, system); err != nil {
		ferr.RollbackErr = err
	}
	return ferr
}

// finish records how a change ended, with its progress, and audits it as
// whoever applied it.
func (e *Engine) finish(ctx context.Context, c store.Change, state, errText, event string, progress *Progress) error {
	now := e.now()
	actor, via := c.Actor, c.ProposedVia
	if c.AppliedBy != "" {
		actor, via = c.AppliedBy, c.AppliedVia
	}
	return e.db.Revise(ctx, func(tx *store.RevisionTx) error {
		if err := tx.SetChangeState(ctx, c.ID, state, errText, time.Time{}); err != nil {
			return err
		}
		if progress != nil {
			b, err := json.Marshal(progress)
			if err != nil {
				return err
			}
			if err := tx.SetChangeProgress(ctx, c.ID, b); err != nil {
				return err
			}
		}
		return tx.RecordEvent(ctx, store.Event{ID: id.New(id.Event), At: now, Actor: actor, Action: event, Subject: c.ID, Detail: c.Summary, Surface: via})
	})
}

// Discard drops a planned change without applying it. It stays on record
// as discarded.
func (e *Engine) Discard(ctx context.Context, changeID string, by By) error {
	err := e.db.Revise(ctx, func(tx *store.RevisionTx) error {
		c, err := tx.GetChange(ctx, changeID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return fmt.Errorf("%w: there's no change %s", ErrNotFound, changeID)
		case err != nil:
			return err
		case c.State != store.ChangePlanned:
			return fmt.Errorf("%w: %s is %s, and only a planned change can be discarded", ErrNotPlanned, changeID, c.State)
		}
		if err := tx.SetChangeState(ctx, changeID, store.ChangeDiscarded, "", time.Time{}); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: e.now(), Actor: by.Actor, Action: "change.discarded", Subject: changeID, Detail: c.Summary, Surface: by.Via,
		})
	})
	if err != nil {
		return err
	}
	e.published(changeID, store.ChangeDiscarded)
	return nil
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
					if ed.Servers, err = inFleet(ctx, tx, ref, rev.ID, prev.Revision, changeID); err != nil {
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

// inFleet returns where a revision ran, on the servers still in the fleet:
// putting it back places nothing on a removed server. If every server it
// ran on was removed, there is nowhere to put it back.
func inFleet(ctx context.Context, tx *store.Tx, ref resource.Ref, resourceID string, revision int, changeID string) ([]string, error) {
	placed, err := tx.Placement(ctx, resourceID, revision)
	if err != nil {
		return nil, err
	}
	var out, gone []string
	for _, s := range placed {
		removed, err := serverGone(ctx, tx, s)
		switch {
		case err != nil:
			return nil, err
		case removed:
			gone = append(gone, s)
		default:
			out = append(out, s)
		}
	}
	if len(placed) > 0 && len(out) == 0 {
		return nil, fmt.Errorf("%w: before %s, %s ran on %s, which %s removed from the fleet, so there's nowhere to put it back. Edit %s directly instead",
			ErrCantUndo, changeID, ref.Name, strings.Join(gone, ", "), wasOrWere(len(gone)), ref.Name)
	}
	return out, nil
}

// Undo plans putting back everything an applied change did. Apply the
// plan to carry it out.
func (e *Engine) Undo(ctx context.Context, changeID string, by By) (*Plan, error) {
	var c store.Change
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		c, err = tx.GetChange(ctx, changeID)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: there's no change %s", ErrNotFound, changeID)
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
	return e.propose(ctx, Proposal{Summary: "Undo " + c.Summary, Actor: by.Actor, Via: by.Via, Edits: edits}, changeID)
}

// Drift is a resource that doesn't match its latest revision.
type Drift struct {
	ID  string // the resource's
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
	if len(refs) == 0 {
		return nil, nil
	}
	observed, err := agent.Observe(ctx, refs)
	if err != nil {
		return nil, &AgentError{Server: server, Doing: "check drift", Err: err}
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
		d := Drift{ID: r.ID, Ref: refs[i], Revision: r.Revision, Observed: observed[i], Missing: observed[i] == nil}
		if !d.Missing {
			d.Changes = resource.Diff(desired, observed[i])
		}
		out = append(out, d)
	}
	return out, nil
}

// Revert plans putting a drifted resource back to its latest revision.
func (e *Engine) Revert(ctx context.Context, d Drift, by By) (*Plan, error) {
	var it item
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		it, err = e.load(ctx, tx, d.Ref)
		return err
	}); err != nil {
		return nil, err
	}
	summary := fmt.Sprintf("Revert %s to revision %d", d.Ref.Name, it.base)
	return e.propose(ctx, Proposal{Summary: summary, Actor: by.Actor, Via: by.Via, Edits: []Edit{{Ref: d.Ref, Spec: it.current}}}, "")
}

// Adopt plans keeping what a server has for a drifted resource: a new
// revision that matches it, with nothing to do on the server.
func (e *Engine) Adopt(ctx context.Context, d Drift, by By) (*Plan, error) {
	summary := fmt.Sprintf("Keep the changes made to %s outside Dockwell", d.Ref.Name)
	return e.propose(ctx, Proposal{Summary: summary, Actor: by.Actor, Via: by.Via, Edits: []Edit{{Ref: d.Ref, Spec: d.Observed}}}, "")
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
