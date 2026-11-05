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
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
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
	// ErrStopping means the control plane is stopping, so it starts no
	// change; apply it again once the control plane is back.
	ErrStopping = errors.New("the control plane is stopping")
	// ErrInterrupted ends a run the engine stopped partway, as the control
	// plane stopped. The change stays applying, and Resume finishes it
	// when the control plane starts again.
	ErrInterrupted = errors.New("the control plane stopped partway through the change")
)

// Surfaces a change can come from, as the store and the audit log record
// them.
const (
	ViaPanel     = store.SurfacePanel
	ViaAPI       = store.SurfaceAPI
	ViaCLI       = store.SurfaceCLI       // the dockwell CLI, with an API token
	ViaAutomatic = store.SurfaceAutomatic // Dockwell on its own, such as a rollback
)

// By is who does something to a change, and from where.
type By struct {
	Actor string
	Via   string // ViaPanel, ViaAPI, ViaCLI or ViaAutomatic
}

// system is Dockwell acting on its own.
var system = By{Actor: SystemActor, Via: ViaAutomatic}

// Sealer keeps the specs of kinds that may hold secrets encrypted at rest,
// so revisions and change items never hold one in the clear, and digests
// them for fingerprints with a key of its own (ADR 0022). secrets.Keeper
// is one.
type Sealer interface {
	// Seal encrypts a spec's encoding for where it will be stored, in tx,
	// so it opens there alone: a revision, or a change's item.
	Seal(ctx context.Context, tx *store.Tx, at store.SpecAt, encoded []byte) ([]byte, error)
	// Open decrypts what Seal made for where it is stored. A spec of a kind
	// that holds no secret comes back as it is, and one of a kind that may
	// hold a secret must be sealed.
	Open(ctx context.Context, tx *store.Tx, at store.SpecAt, stored []byte) ([]byte, error)
	// OpenAll is Open for many specs, in order, in a few reads however many
	// there are.
	OpenAll(ctx context.Context, tx *store.Tx, specs []store.StoredSpec) ([][]byte, error)
	// Digest is a keyed hash of a spec's encoding for one change's
	// fingerprint, which says nothing of it without the install's key, and
	// differs from change to change.
	Digest(changeID string, encoded []byte) []byte
}

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
	sealer Sealer
	logger *slog.Logger
	now    func() time.Time
	// mu makes commits take turns: checking again that nothing moved since
	// the plan, and committing the revisions, happen together. Planning
	// asks agents, so it happens outside mu, and so do the steps: a slow
	// server holds up only the changes that touch it (ADR 0009, ADR 0017).
	mu sync.Mutex

	// life guards stopping, and counts the runs under way: applies, and
	// Resume's. Stop and Wait wait for them.
	life     sync.Mutex
	stopping bool
	active   int
	idle     chan struct{} // closed while no run is under way
	// halted is done once Stop cancels the calls of the steps under way.
	halted context.Context
	halt   context.CancelFunc

	// pmu guards the progress of the changes running in this process, who
	// watches which change, which changes it holds, and whether to resume.
	pmu      sync.Mutex
	live     map[string]*Progress
	watchers map[string]map[chan Update]chan struct{} // each watcher's channel, and one closed when it is dropped
	// owned are the changes this process holds: being committed, resumed,
	// or running their steps.
	owned map[string]bool
	// busy names, for each resource an owned change touches, that change.
	// A change claims its resources before it plans, so two changes never
	// run steps on one resource at once.
	busy map[string]string
	// waiters are the owned changes Resume waits on: it skipped a change
	// because one of these held it or one of its resources, or a rollback
	// ended its first run waiting for a server that is back. Once one lets
	// go, Resume runs again. Only the change waited on wakes it, so a
	// change that lets go having done nothing can't wake Resume for
	// another, and Resume can't go round on its own. waking says the
	// engine's own Resume is running, and rerun that it goes round again.
	waiters       map[string]bool
	waking, rerun bool
}

// New returns an engine over db, using kinds to read and check specs,
// agents to reach servers, and sealer to keep the specs of kinds that may
// hold secrets encrypted. Projects and environments are always among the
// kinds.
func New(db *store.DB, kinds *resource.Registry, agents Agents, sealer Sealer, logger *slog.Logger) *Engine {
	idle := make(chan struct{})
	close(idle)
	halted, halt := context.WithCancel(context.Background())
	return &Engine{
		db: db, kinds: kinds.With(scope.Types()...), agents: agents, sealer: sealer, logger: logger, now: time.Now,
		idle: idle, halted: halted, halt: halt,
		live: map[string]*Progress{}, watchers: map[string]map[chan Update]chan struct{}{}, owned: map[string]bool{}, busy: map[string]string{},
		waiters: map[string]bool{},
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
	// ID is what a new resource is called, so another edit in the change can
	// live in it (NewProject). Empty gets it a new one. A resource that has
	// been made before keeps the one it has.
	ID string
	// Base is the revision Spec was made from, for an edit that read a spec,
	// changed it and proposes the rest as it read it. If that is no longer the
	// latest the edit is stale, as it would quietly undo what came between.
	// Zero is an edit that says the whole spec itself.
	Base int
	// Pull asks the server for the resource's image again as this applies, as a
	// deploy of the tag it already names does: the plan can't know what the tag
	// points at now, so a revision comes only if it finds a newer image.
	Pull bool
}

// Note is something a person should know before applying a plan that no step
// says, as it is about what stays as it is or what can be read. It never stops
// anyone applying it.
type Note struct {
	// Reason says what it is about, for a surface with words of its own.
	Reason string
	// Message says it, for people.
	Message string
	// Ref is the resource it is about.
	Ref resource.Ref
	// Names are what it is about, as the API's PlanNote says.
	Names []string `json:",omitempty"`
}

// Proposal is a set of edits someone wants to make.
type Proposal struct {
	Summary string // what it is for, in the actor's words
	Actor   string
	Via     string // where it comes from, such as ViaPanel
	Edits   []Edit
	Notes   []Note // what the plan should say beside its steps
	// RollsBackTo is the revision it puts back, for RollBackTo to record.
	RollsBackTo int
	// IfNothing refuses the proposal when its plan would do nothing, on the
	// record and on every server: for someone asking for what is already so.
	// That takes asking the servers, as a record can match what no server runs.
	IfNothing error
	// Unpinned is this proposal without the image digest it pins, to plan instead
	// if a server has no way to get that image.
	Unpinned *Proposal
}

// Place is where a plan's resources live, as far as anyone has a choice: Project
// is empty while there's one project, and Environment while that project has one
// environment (ADR 0014).
type Place struct {
	Project     string `json:",omitempty"`
	Environment string `json:",omitempty"`
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
	// plain is the fingerprint as an earlier version made it, hashing
	// every spec as it is, for a change it planned (fingerprintPlain).
	plain string
	// Notes are what a person should know before applying it. They aren't in
	// the fingerprint: they say nothing about what it does.
	Notes []Note `json:",omitempty"`
	// Places are where its resources live, once there's a choice. They aren't
	// in the fingerprint: its items say it already, by ID.
	Places []Place `json:",omitempty"`
	// RollsBackTo is the revision it puts back, for a plan RollBackTo made.
	RollsBackTo int `json:",omitempty"`
}

// How a change's fingerprint is made, as its record says (ADR 0022).
const (
	// fingerprintPlain hashed every spec as it is, before ADR 0022. A
	// change planned then is checked against plain.
	fingerprintPlain = 1
	// fingerprintKeyed digests the spec of a kind that may hold secrets
	// with a key derived from the install's key, bound to the change, and
	// says which items rotate their values.
	fingerprintKeyed = 2
)

// ResourcePlan is what a plan does to one resource as a whole.
type ResourcePlan struct {
	ID  string
	Ref resource.Ref
	// Action is what its steps on every server add up to (see
	// item.resourcePlan), or for a kind that runs on no server, what its
	// revision does: create, update or delete. None means nothing changes.
	Action resource.Action
	Base   int // the revision it starts from; 0 if new
	// Changes are the fields that differ from the latest revision.
	Changes []resource.Change
	// Servers is where it runs once applied, and Previous where it runs
	// now, by server ID.
	Servers, Previous []string
	// HostAccess is what the change lets it reach on its servers that it
	// couldn't before, from its specs (resource.HostAccesser). A plan with
	// any needs a person to apply it.
	HostAccess []resource.Access `json:",omitempty"`
}

// ServerPlan is the part of a plan on one server.
type ServerPlan struct {
	Server string
	Name   string // the server's name, for people
	// Offline means the server isn't connected, so the plan is based on
	// its last known desired state.
	Offline bool
	// Unplanned, in a rollback's plan, is why the server's agent couldn't
	// plan its part, though the server is connected. That part is planned
	// from its last known desired state too, and waits for Resume to plan
	// it against the server.
	Unplanned string `json:",omitempty"`
	// Removed means the server was removed from the fleet. Nothing runs
	// there any more, so the plan has no steps on it: what the change
	// takes off it is only recorded.
	Removed bool
	// Downtime adds up the steps' downtime, as they run one at a time.
	Downtime time.Duration
	// UntilHealthy adds up how long until each step's resource is healthy
	// again, at most, where a health check may take longer than the
	// downtime; zero when Downtime says it all.
	UntilHealthy time.Duration `json:",omitempty"`
	Steps        []resource.Step
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

// HostAccess reports a plan that lets a resource reach more of its server
// than it could, which a person approves (ADR 0021).
func (p *Plan) HostAccess() bool {
	return slices.ContainsFunc(p.Resources, func(r ResourcePlan) bool { return len(r.HostAccess) > 0 })
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
	// runs says the item's kind runs on servers: a host-level or
	// environment-level kind that isn't a valuer, such as a service.
	runs bool
	// sealed says its kind's specs may hold secrets, so the store keeps
	// them encrypted, and the fingerprint digests them with a key.
	sealed bool
	// rotates says the edit sets the value of a kind that holds values and
	// keeps them sealed, a secret, which plans as changed whatever it holds
	// (resource.DiffRotating). Setting one is a rotation: it makes a
	// revision and restarts what uses it, even when the value is the one
	// there, so no plan confirms a guess at it. Only what someone asks for
	// rotates, and what Dockwell puts back (undo and rollback). A workload's
	// own sensitive fields, such as a container's environment, compare as
	// any field does: edits carry whole specs, so applying the same thing
	// twice must change nothing (ADR 0022).
	rotates bool
	// pull says the change asks the server for its image again (Edit.Pull).
	pull bool
	// pulled are the servers a run of the change stopped partway had already
	// pulled on, as its revision's deployments say.
	pulled []string
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
	if it.rotates {
		return true // whatever the values are, and without comparing them
	}
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
	return e.propose(ctx, p, "", true)
}

// propose plans and records a change. With rotate, an edit that sets a
// secret's value rotates it (item.rotates).
func (e *Engine) propose(ctx context.Context, p Proposal, reverts string, rotate bool) (*Plan, error) {
	if len(p.Edits) == 0 {
		return nil, fault.New(ErrInvalid, "the change has no edits. Add one for each resource to change")
	}
	items, err := e.items(ctx, p.Edits, rotate)
	if err != nil {
		return nil, err
	}
	changeID := id.New(id.Change)
	plan, err := e.plan(ctx, changeID, items, false)
	if err != nil {
		return nil, err
	}
	if p.Unpinned != nil && plan.imageGone() {
		return e.propose(ctx, *p.Unpinned, reverts, rotate)
	}
	if p.IfNothing != nil && plan.Empty() {
		return nil, e.nothingToDo(ctx, items, p.IfNothing)
	}
	if plan.Places, err = e.places(ctx, items); err != nil {
		return nil, err
	}
	plan.Change, plan.Summary, plan.Reverts, plan.Notes, plan.RollsBackTo = changeID, p.Summary, reverts, p.Notes, p.RollsBackTo
	record, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	c := store.Change{
		ID: plan.Change, Summary: p.Summary, Actor: p.Actor, State: store.ChangePlanned, Reverts: reverts,
		Fingerprint: plan.Fingerprint, Plan: record, CreatedAt: e.now(), ProposedVia: p.Via, Servers: serversOf(plan),
	}
	err = e.db.Revise(ctx, func(tx *store.RevisionTx) error { return e.recordPlanned(ctx, tx, c, items) })
	if err != nil {
		return nil, err
	}
	return plan, nil
}

// imageGone reports a step whose pinned image a server has no way to get.
func (p *Plan) imageGone() bool {
	return slices.ContainsFunc(p.Servers, func(s ServerPlan) bool {
		return slices.ContainsFunc(s.Steps, func(st resource.Step) bool { return st.ImageGone })
	})
}

// nothingToDo says why a plan that does nothing was refused. A server that isn't
// connected was planned from what it last reported, so nothing says it still runs
// that, and it takes asking it again.
func (e *Engine) nothingToDo(ctx context.Context, items []item, refusal error) error {
	var offline []string
	for i := range items {
		for _, s := range items[i].targets() {
			if _, online := e.agents.Agent(s); !online && !slices.Contains(offline, s) {
				offline = append(offline, s)
			}
		}
	}
	if len(offline) == 0 {
		return refusal
	}
	names, err := e.serverNames(ctx, offline)
	if err != nil {
		return err
	}
	return fault.New(ErrOffline, "%s isn't connected, so Dockwell can't tell what runs there. Try again once it reconnects", names[offline[0]])
}

// places says where a plan's resources live, for each project and environment
// that has others beside it. What the change makes counts, as it will be there.
func (e *Engine) places(ctx context.Context, items []item) ([]Place, error) {
	var tree scope.Tree
	err := e.db.View(ctx, func(tx *store.Tx) (err error) {
		tree, err = scope.Load(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	type envAt struct{ project, name string }
	projects := map[string]string{} // by ID, to its name
	environments := map[string]envAt{}
	inProject := map[string]int{}
	for _, p := range tree.Projects {
		projects[p.ID] = p.Name
		for _, env := range p.Environments {
			environments[env.ID] = envAt{p.ID, env.Name}
			inProject[p.ID]++
		}
	}
	for i := range items {
		it := &items[i]
		if it.spec == nil {
			continue
		}
		switch it.ref.Kind {
		case scope.ProjectKind:
			projects[it.id] = it.ref.Name
		case scope.EnvironmentKind:
			if _, known := environments[it.id]; !known {
				environments[it.id] = envAt{it.ref.Scope, it.ref.Name}
				inProject[it.ref.Scope]++
			}
		}
	}
	var out []Place
	for i := range items {
		it := &items[i]
		env, ok := environments[it.ref.Scope]
		if !ok || it.level != resource.LevelEnvironment {
			continue
		}
		var place Place
		if len(projects) > 1 {
			place.Project = projects[env.project]
		}
		if inProject[env.project] > 1 {
			place.Environment = env.name
		}
		if place != (Place{}) && !slices.Contains(out, place) {
			out = append(out, place)
		}
	}
	return out, nil
}

// changeItems are what the change changeID asks of each item, as the store
// keeps it: sealed, for a kind that may hold secrets, for that item of that
// change.
func (e *Engine) changeItems(ctx context.Context, tx *store.Tx, changeID string, items []item) ([]store.ChangeItem, error) {
	out := make([]store.ChangeItem, len(items))
	for i := range items {
		it := &items[i]
		spec, err := e.stored(ctx, tx, it, store.InChange(it.id, string(it.ref.Kind), changeID, i))
		if err != nil {
			return nil, err
		}
		out[i] = store.ChangeItem{
			ResourceID: it.id, Kind: string(it.ref.Kind), Scope: it.ref.Scope, Name: it.ref.Name, BaseRevision: it.base, Spec: spec,
			Servers: it.servers, Rotates: it.rotates, Pull: it.pull,
		}
	}
	return out, nil
}

// stored is the item's spec as the store keeps it at at: sealed for there,
// for a kind that may hold secrets, and nil for a deletion.
func (e *Engine) stored(ctx context.Context, tx *store.Tx, it *item, at store.SpecAt) ([]byte, error) {
	if it.encoded == nil || !it.sealed {
		return it.encoded, nil
	}
	if e.sealer == nil {
		return nil, fmt.Errorf("can't store %s without the install's key, which keeps it encrypted", it.ref)
	}
	return e.sealer.Seal(ctx, tx, at, it.encoded)
}

// open reads the encoding of a spec stored at at: opened, if it is sealed.
func (e *Engine) open(ctx context.Context, tx *store.Tx, at store.SpecAt, stored []byte) ([]byte, error) {
	if stored == nil || e.sealer == nil {
		return stored, nil
	}
	return e.sealer.Open(ctx, tx, at, stored)
}

// decodeAll reads specs as stored, in order, in a few reads however many there
// are.
func (e *Engine) decodeAll(ctx context.Context, tx *store.Tx, specs []store.StoredSpec) ([]proto.Message, error) {
	plain := make([][]byte, len(specs))
	if e.sealer == nil {
		for i := range specs {
			plain[i] = specs[i].Spec
		}
	} else {
		var err error
		if plain, err = e.sealer.OpenAll(ctx, tx, specs); err != nil {
			return nil, fmt.Errorf("can't read the stored specs: %w", err)
		}
	}
	out := make([]proto.Message, len(specs))
	for i := range specs {
		var err error
		if out[i], err = e.kinds.Decode(resource.Kind(specs[i].At.Kind), plain[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// decode reads a spec stored at at.
func (e *Engine) decode(ctx context.Context, tx *store.Tx, at store.SpecAt, stored []byte) (proto.Message, error) {
	plain, err := e.open(ctx, tx, at, stored)
	if err != nil {
		return nil, fmt.Errorf("can't read the stored spec of %s %s: %w", at.Kind, at.ResourceID, err)
	}
	return e.kinds.Decode(resource.Kind(at.Kind), plain)
}

// recordPlanned puts a planned change on record, with its items, and
// audits it.
func (e *Engine) recordPlanned(ctx context.Context, tx *store.RevisionTx, c store.Change, items []item) error {
	c.FingerprintScheme = fingerprintKeyed
	var err error
	if c.Items, err = e.changeItems(ctx, tx.Tx, c.ID, items); err != nil {
		return err
	}
	if err := tx.CreateChange(ctx, c); err != nil {
		return err
	}
	return tx.RecordEvent(ctx, store.Event{
		ID: id.New(id.Event), At: c.CreatedAt, Actor: c.Actor, Action: "change.planned", Subject: c.ID, Detail: c.Summary, Surface: c.ProposedVia,
	})
}

// items reads where each edit's resource stands and what the edit wants of
// it, gives new resources their IDs, and checks the edits as a whole. With
// rotate, an edit that sets a secret's value rotates it.
func (e *Engine) items(ctx context.Context, edits []Edit, rotate bool) ([]item, error) {
	items := make([]item, 0, len(edits))
	seen := map[resource.Ref]bool{}
	err := e.db.View(ctx, func(tx *store.Tx) error {
		for _, ed := range edits {
			if seen[ed.Ref] {
				return fault.New(ErrInvalid, "the change edits %s twice. Combine the edits into one", ed.Ref)
			}
			seen[ed.Ref] = true
			it, err := e.load(ctx, tx, ed.Ref)
			if err != nil {
				return err
			}
			it.pull = ed.Pull
			if ed.Base != 0 && it.base != ed.Base {
				return fault.New(ErrStale, "%s changed while this was being planned, and planning from what was read would undo that. Try again, to plan it from the latest", ed.Ref)
			}
			if it.id == "" {
				it.id = cmp.Or(ed.ID, e.newID(ed.Ref.Kind))
			}
			if ed.Spec != nil {
				if err := e.checkName(ed.Ref); err != nil {
					return err
				}
				it.spec = proto.Clone(ed.Spec)
				if err := e.kinds.Validate(ed.Ref.Kind, it.spec); err != nil {
					return fault.New(ErrInvalid, "%s isn't valid: %w", ed.Ref, err)
				}
				if it.encoded, err = resource.Encode(it.spec); err != nil {
					return err
				}
				it.rotates = rotate && e.valuer(ed.Ref.Kind) && resource.SetsSensitive(it.spec)
			} else if it.current == nil {
				return fault.New(ErrInvalid, "there's no %s to delete. Check its kind, name and scope", ed.Ref)
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
		if items, err = e.withUsers(ctx, tx, items); err != nil {
			return err
		}
		if err := e.pinAll(ctx, tx, items); err != nil {
			return err
		}
		items = e.orderByReference(items)
		return e.check(ctx, tx, items)
	})
	return items, err
}

// load reads where a resource stands now.
func (e *Engine) load(ctx context.Context, tx *store.Tx, ref resource.Ref) (item, error) {
	t, err := e.kinds.Type(ref.Kind)
	if err != nil {
		return item{}, fault.New(ErrInvalid, "%w", err)
	}
	_, valuer := t.(resource.Valuer)
	it := item{ref: ref, level: t.Level(), runs: t.Level().RunsOnServers() && !valuer, sealed: resource.Sealed(t)}
	r, err := tx.GetResource(ctx, string(ref.Kind), ref.Scope, ref.Name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return it, nil
	case err != nil:
		return it, err
	}
	it.id, it.base, it.rev = r.ID, r.Revision, r.Revision
	if r.Spec != nil {
		if it.current, err = e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec); err != nil {
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
	case !it.runs || it.id == "" || revision == 0:
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
	case it.spec == nil || !it.runs:
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
			return nil, fault.New(ErrInvalid, "%sthere's no server to run %s on. Add a server first", removed, it.ref.Name)
		}
		names := make([]string, len(active))
		for i := range active {
			names[i] = active[i].Name
		}
		return nil, fault.New(ErrInvalid, "%sthere are %d servers, %s, so say which %s runs on", removed, len(active), human.List(names), it.ref.Name)
	}
	if len(want) == 0 {
		return nil, fault.New(ErrInvalid, "%s needs a server to run on. To stop it everywhere, delete it", it.ref.Name)
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
		return fault.New(ErrInvalid, "%s was removed from the fleet, so %s can't run there. Choose a server that is still in it", server, name)
	}
	return fault.New(ErrInvalid, "there's no server %s to run %s on. Choose one in the fleet", server, name)
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
		return fault.New(ErrInvalid, "a %s needs a name. Give it one", ref.Kind)
	}
	t, err := e.kinds.Type(ref.Kind)
	if err != nil {
		return err
	}
	if c, ok := t.(resource.NameChecker); ok {
		if err := c.CheckName(ref.Name); err != nil {
			return fault.New(ErrInvalid, "%w", err)
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
//     environment of a project, or the organization's last project;
//   - a resource naming one that isn't there, or isn't on its servers,
//     such as a service mounting a volume (checkReferences);
//   - a resource claiming what another holds, such as a domain and a path
//     (checkClaims);
//   - a resource of a kind that runs on one server placed on several.
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
		if err := e.checkNames(ctx, tx, items, it); err != nil {
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
			if err := e.checkOneServer(ctx, tx, it); err != nil {
				return err
			}
		}
		if it.ref.Kind == scope.ProjectKind && it.current == nil {
			envs, err := liveIn(ctx, tx, items, it.id, scope.EnvironmentKind)
			if err != nil {
				return err
			}
			if len(envs) == 0 {
				return fault.New(ErrInvalid, "project %s would have no environment. Make one in it in the same change, such as %s",
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
			return fault.New(ErrInvalid, "%s %s still holds %s. Delete them first, or in the same change", it.ref.Kind, it.ref.Name,
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
			return fault.New(ErrInvalid, "%s is the last %s in %s, which needs one. Make another first, or in the same change",
				it.ref.Name, it.ref.Kind, where)
		}
	}
	if err := e.checkClaims(ctx, tx, items); err != nil {
		return err
	}
	if err := e.checkReferences(ctx, tx, items); err != nil {
		return err
	}
	return e.checkAnswers(ctx, tx, items)
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
		return fault.New(ErrInvalid, "there's no %s %s for %s %s to live in", what, it.ref.Scope, it.ref.Kind, it.ref.Name)
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
		return fault.New(ErrInvalid, "this change deletes %s %s, so %s %s has nowhere to live. Keep %s, or %s",
			in.ref.Kind, in.ref.Name, it.ref.Kind, it.ref.Name, in.ref.Name, fix)
	}
	r, err := tx.GetResourceByID(ctx, it.ref.Scope)
	if errors.Is(err, store.ErrNotFound) || err == nil && (r.Kind != string(want) || r.Spec == nil) {
		return missing(string(want))
	}
	return err
}

// NewProject is the edits that bring a project to life with an environment,
// for a change that puts something in it at once, and the environment's ID
// for that something's scope. One made before, and deleted, continues its
// history, as it would through a change of its own (ADR 0014).
func (e *Engine) NewProject(ctx context.Context, tx *store.Tx, org, name, environment string) (edits []Edit, environmentID string, err error) {
	project := resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: name}
	projectID, err := e.knownID(ctx, tx, project)
	if err != nil {
		return nil, "", err
	}
	env := resource.Ref{Kind: scope.EnvironmentKind, Scope: projectID, Name: cmp.Or(environment, scope.DefaultEnvironment)}
	environmentID, err = e.knownID(ctx, tx, env)
	if err != nil {
		return nil, "", err
	}
	return []Edit{
		{Ref: project, Spec: &resourcev1.Project{}, ID: projectID},
		{Ref: env, Spec: &resourcev1.Environment{}, ID: environmentID},
	}, environmentID, nil
}

// knownID is the ID a resource has from before, or a new one.
func (e *Engine) knownID(ctx context.Context, tx *store.Tx, ref resource.Ref) (string, error) {
	r, err := tx.GetResource(ctx, string(ref.Kind), ref.Scope, ref.Name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return e.newID(ref.Kind), nil
	case err != nil:
		return "", err
	}
	return r.ID, nil
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
// same state give the same plan. They give the same fingerprint too, for
// the change changeID: the fingerprint leaves out the IDs given to new
// resources, which differ every time. A kind that may hold secrets is
// digested for that change alone, so two changes' fingerprints never say
// they set the same value.
//
// A rollback's plan never fails for an agent: one that can't plan now is
// planned from its server's last known desired state, like an offline
// one, and its steps wait for Resume to plan them against the server.
func (e *Engine) plan(ctx context.Context, changeID string, items []item, rollback bool) (*Plan, error) {
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
	// fp is the fingerprint, and plain the one an earlier version would
	// have made, which differ only in how they digest a spec.
	keyed, plain := sha256.New(), sha256.New()
	fp := io.MultiWriter(keyed, plain)
	for i := range items {
		it := &items[i]
		if it.changes() {
			plan.Revisions++
		}
		item := func(digest []byte) string {
			return fmt.Sprintf("item %s|%s|%d|%t|%x|%s", stable(it.id), ref(it.ref), it.base, it.spec != nil, digest, strings.Join(it.servers, " "))
		}
		sum := sha256.Sum256(it.encoded)
		_, _ = fmt.Fprintf(keyed, "%s|%t\n", item(e.digest(changeID, it)), it.rotates)
		_, _ = io.WriteString(plain, item(sum[:])+"\n")
	}
	actions := map[string][]resource.Action{} // each item's steps, on every server
	access := map[string][]resource.Access{}  // what each item gains on its servers
	var before, after map[resource.Ref]proto.Message
	var takes map[resource.Ref][]resource.Access
	if len(servers) > 0 {
		if before, after, err = e.named(ctx, items); err != nil {
			return nil, err
		}
		if takes, err = e.takeovers(ctx, items); err != nil {
			return nil, err
		}
	}
	for _, server := range servers {
		its := byServer[server]
		sp := ServerPlan{Server: server, Name: names[server]}
		var steps []resource.Step
		agent, online := e.agents.Agent(server)
		sp.Offline = !online
		if online {
			var err error
			if steps, err = e.planOn(ctx, agent, server, sp.Name, its); err != nil {
				if !rollback {
					return nil, err
				}
				e.logger.WarnContext(ctx, "a rollback is planned from a server's last known state, as its agent can't plan it now; it waits for Resume there",
					slog.String("server", sp.Name), slog.String("error", err.Error()))
				sp.Unplanned = couldntPlan(err)
			}
		}
		if sp.Offline || sp.Unplanned != "" {
			for _, it := range its {
				d, err := e.kinds.Driver(it.ref.Kind)
				if err != nil {
					return nil, err
				}
				step, err := resource.PlanStep(d, it.ref, it.currentOn(server), it.desiredOn(server))
				if err != nil {
					return nil, err
				}
				// Only the server can say what its tag names now, so a pull is a
				// step there whatever the specs say, and the change waits for it
				// as it does for any step there.
				if it.pull && it.desiredOn(server) != nil {
					if step.Action == resource.ActionNone {
						if step.Decision, err = d.Plan(it.currentOn(server), it.desiredOn(server), []resource.Change{{Path: "image"}}); err != nil {
							return nil, err
						}
					}
					step.Pull = step.Action != resource.ActionNone
				}
				steps = append(steps, step)
			}
		}
		ids := map[resource.Ref]string{}
		// What each item reaches on this server that it couldn't, from its
		// specs: the control plane decides it, whatever the agent says.
		gains := map[resource.Ref][]resource.Access{}
		for _, it := range its {
			ids[it.ref] = it.id
			gained := resource.NewAccess(e.reach(it.ref, it.currentOn(server), before), e.reach(it.ref, it.desiredOn(server), after))
			if it.desiredOn(server) != nil {
				gained = append(gained, takes[it.ref]...)
			}
			if len(gained) > 0 {
				gains[it.ref] = gained
				access[it.id] = append(access[it.id], resource.NewAccess(access[it.id], gained)...)
				fmt.Fprintf(fp, "access %s|%s|%v\n", server, ref(it.ref), gained)
			}
		}
		steps = order(steps)
		for i := range steps {
			s := &steps[i]
			if s.Action == resource.ActionNone {
				continue
			}
			gained := gains[s.Ref]
			// The panel's domain moving behind a proxy is the agent's to know,
			// as only it sees what holds the ports. A person approves it: a
			// token can't cut off what it can't approve (ADR 0016).
			if i := s.Ingress; i != nil && i.Change == resource.IngressStarts && i.PanelDomain != "" {
				a := resource.Access{Kind: resource.AccessPanelDomain, Path: i.PanelDomain, Detail: i.Proxy}
				gained = append(gained, a)
				access[ids[s.Ref]] = append(access[ids[s.Ref]], resource.NewAccess(access[ids[s.Ref]], []resource.Access{a})...)
			}
			s.HostAccess = gained
			sp.Steps = append(sp.Steps, *s)
			sp.Downtime += s.Downtime
			sp.UntilHealthy += max(s.Downtime, s.UntilHealthy)
			actions[ids[s.Ref]] = append(actions[ids[s.Ref]], s.Action)
			fmt.Fprintf(fp, "step %t|%s|%s|%s|%d|%d|%d|%t|%v|%d|%v|%t|%t\n", s.Pull, server, ref(s.Ref), s.Action, s.Effect, s.Downtime, len(s.Changes), s.Destroys, s.Warnings,
				s.UntilHealthy, s.Kept, s.Restores, s.HealthUnknown)
			for _, c := range s.Changes {
				fmt.Fprintf(fp, "  %s|%d|%s|%s|%t\n", c.Path, c.Op, c.From, c.To, c.Sensitive)
			}
			// What DNS says, s.Domain, stays out, as people often set it after.
			if i := s.Ingress; i != nil {
				fmt.Fprintf(fp, "  ingress %d|%s|%v|%s\n", i.Change, i.Proxy, i.Ports, i.PanelDomain)
			}
		}
		if sp.UntilHealthy == sp.Downtime {
			sp.UntilHealthy = 0
		}
		if len(sp.Steps) > 0 {
			plan.Servers = append(plan.Servers, sp)
		}
	}
	for _, s := range removed {
		plan.Servers = append(plan.Servers, ServerPlan{Server: s, Name: names[s], Removed: true})
		fmt.Fprintf(fp, "removed %s\n", s)
	}
	// A name that moves between a variable and a secret, either way, as a
	// switch does, its undo, or a rollback of it: the value may have been a
	// secret all along, so the plan shows it on neither side.
	masked := e.switchSides(items)
	for i := range items {
		rp := items[i].resourcePlan(actions[items[i].id])
		rp.HostAccess = access[items[i].id]
		if masked[items[i].ref] {
			for j := range rp.Changes {
				rp.Changes[j].Sensitive, rp.Changes[j].From, rp.Changes[j].To = true, "", ""
			}
		}
		plan.Resources = append(plan.Resources, rp)
	}
	plan.Fingerprint = hex.EncodeToString(keyed.Sum(nil)[:16])
	plan.plain = hex.EncodeToString(plain.Sum(nil)[:16])
	return plan, nil
}

// reach lists what a resource reaches on its server, from its spec: what
// its kind says of it, and what the resources it names reach, as a volume
// it mounts does, read-only where it uses them read-only. So mounting a
// volume that binds a server path reaches that path. named holds the specs
// of what it names, as they stand before or after the change.
func (e *Engine) reach(ref resource.Ref, spec proto.Message, named map[resource.Ref]proto.Message) []resource.Access {
	out := e.kinds.HostAccess(ref.Kind, spec)
	for _, use := range e.kinds.References(ref.Kind, spec) {
		if use.Apart {
			continue
		}
		for _, a := range e.kinds.HostAccess(use.Kind, named[resource.Ref{Kind: use.Kind, Scope: ref.Scope, Name: use.Name}]) {
			a.ReadOnly = a.ReadOnly || use.ReadOnly
			out = append(out, a)
		}
	}
	return out
}

// named reads what the items name (resource.Referrer), as it stands
// before the change, and as the change leaves it.
func (e *Engine) named(ctx context.Context, items []item) (before, after map[resource.Ref]proto.Message, err error) {
	before, after = map[resource.Ref]proto.Message{}, map[resource.Ref]proto.Message{}
	var wanted []resource.Ref
	for i := range items {
		it := &items[i]
		for _, spec := range []proto.Message{it.current, it.spec} {
			for _, use := range e.kinds.References(it.ref.Kind, spec) {
				if r := (resource.Ref{Kind: use.Kind, Scope: it.ref.Scope, Name: use.Name}); !slices.Contains(wanted, r) {
					wanted = append(wanted, r)
				}
			}
		}
	}
	if len(wanted) == 0 {
		return before, after, nil
	}
	err = e.db.View(ctx, func(tx *store.Tx) error {
		for _, r := range wanted {
			it, err := e.load(ctx, tx, r)
			if err != nil {
				return err
			}
			before[r], after[r] = it.current, it.current
		}
		return nil
	})
	for i := range items {
		if _, ok := after[items[i].ref]; ok {
			after[items[i].ref] = items[i].spec
		}
	}
	return before, after, err
}

// sealedKind reports a kind whose specs may hold secrets.
func (e *Engine) sealedKind(kind resource.Kind) bool {
	t, err := e.kinds.Type(kind)
	return err == nil && resource.Sealed(t)
}

// digest is what the fingerprint of the change changeID says of an item's
// spec: a hash, keyed for a kind that may hold secrets and bound to the
// change, so neither a fingerprint on record nor one compared with another
// change's can confirm a guess at one.
func (e *Engine) digest(changeID string, it *item) []byte {
	if it.sealed && e.sealer != nil {
		return e.sealer.Digest(changeID, it.encoded)
	}
	sum := sha256.Sum256(it.encoded)
	return sum[:]
}

// planOn asks a server's agent what the items would take there.
func (e *Engine) planOn(ctx context.Context, agent resource.Agent, server, name string, its []*item) ([]resource.Step, error) {
	desired := make([]resource.Desired, len(its))
	for i, it := range its {
		desired[i] = resource.Desired{Ref: it.ref, Spec: it.desiredOn(server), Pull: it.pull}
	}
	ctx, done := e.calling(ctx)
	defer done()
	steps, err := agent.Plan(ctx, desired)
	if err != nil {
		return nil, &AgentError{Server: name, Doing: "plan", Err: err}
	}
	if len(steps) != len(desired) {
		return nil, fmt.Errorf("can't plan on %s: its agent judged %d steps for %d resources", name, len(steps), len(desired))
	}
	return steps, nil
}

// resourcePlan says what the change does to the item as a whole, from its
// steps on every server:
//   - a replace on any server makes it a replace;
//   - deleting it, which takes it off every server it runs on, a delete;
//   - a new resource that every step creates, a create;
//   - anything else that changes it, its spec or where it runs, an update.
//
// A kind that runs on no server has no steps, and takes what its revision
// does. The plan's counts, which PlanDiff shows, follow from these.
func (it item) resourcePlan(steps []resource.Action) ResourcePlan {
	diff := resource.Diff
	if it.rotates {
		diff = resource.DiffRotating
	}
	rp := ResourcePlan{
		ID: it.id, Ref: it.ref, Base: it.base, Changes: diff(it.current, it.spec),
		Servers: slices.Clone(it.servers), Previous: slices.Clone(it.placed),
	}
	switch {
	case slices.Contains(steps, resource.ActionReplace):
		rp.Action = resource.ActionReplace
	case it.spec == nil:
		rp.Action = resource.ActionDelete
	case it.current == nil && !slices.ContainsFunc(steps, func(a resource.Action) bool { return a != resource.ActionCreate }):
		rp.Action = resource.ActionCreate
	case len(steps) > 0 || it.changes():
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
	return fmt.Sprintf("can't %s on %s: %s", e.Doing, e.Server, e.reason())
}

// reason is why the call failed, without the server's name when the
// agent's own failure gives it.
func (e *AgentError) reason() string {
	if se, ok := e.Err.(*resource.ServerError); ok && se.Server == e.Server { //nolint:errorlint // only the agent's own failure, unwrapped
		return se.Err.Error()
	}
	return e.Err.Error()
}

// couldntPlan says why an agent couldn't plan, for a step's message.
func couldntPlan(err error) string {
	if agent := (*AgentError)(nil); errors.As(err, &agent) {
		return agent.reason()
	}
	return err.Error()
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
	case errors.Is(e.RollbackErr, ErrInterrupted):
		return fmt.Sprintf("%s failed at %s: %v. It is being rolled back in %s, which finishes when the control plane starts again", e.Change, e.Step, e.Err, e.Rollback)
	case e.RollbackErr != nil:
		return fmt.Sprintf("%s failed at %s: %v. Rolling back failed too: %v. Check the servers, then undo or fix it by hand", e.Change, e.Step, e.Err, e.RollbackErr)
	default:
		return fmt.Sprintf("%s failed at %s: %v. It was rolled back in %s", e.Change, e.Step, e.Err, e.Rollback)
	}
}

func (e *FailedError) Unwrap() error { return e.Err }

// ErrPending means a rollback is on record but waits for a server to
// reconnect; Resume finishes it. Its message names the servers, as
// "hz-1 reconnects".
var ErrPending = errors.New("a server it touches reconnects")

// reconnects is ErrPending for the servers, by name.
func reconnects(names []string) error {
	if len(names) == 1 {
		return fault.New(ErrPending, "%s reconnects", names[0])
	}
	return fault.New(ErrPending, "%s reconnect", human.List(names))
}

// noSuchChange is ErrNotFound for a change.
func noSuchChange(changeID string) error {
	return fault.New(ErrNotFound, "there's no change %s. Check its ID, or list the changes to find it", changeID)
}

// notPlanned is ErrNotPlanned for applying a change in state: it says
// why the change can't be applied, and what to do instead.
func notPlanned(changeID, state string) error {
	switch state {
	case store.ChangeApplying:
		return fault.New(ErrNotPlanned, "%s is applying already, so it can't be applied again. Watch it to follow its steps", changeID)
	case store.ChangeApplied:
		return fault.New(ErrNotPlanned, "%s is applied already, so it can't be applied again. Undo it, or propose a new change", changeID)
	case store.ChangeDiscarded:
		return fault.New(ErrNotPlanned, "%s was discarded, so it can't be applied. Propose the change again", changeID)
	}
	return fault.New(ErrNotPlanned, "%s failed, so it can't be applied again. Propose the change again", changeID)
}

// IsRollback reports a change Dockwell made to roll another back. A
// rollback isn't refused for an offline server, and isn't rolled back in
// turn when it fails: that would re-apply what failed in the first place.
func IsRollback(c store.Change) bool { return c.Actor == SystemActor && c.Reverts != "" }

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
	if !e.enter() {
		return stopping(changeID)
	}
	defer e.exit()
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
	if !e.enter() {
		return stopping(changeID)
	}
	r, err := e.commit(ctx, changeID, by)
	if err != nil {
		e.exit()
		return err
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		defer e.exit()
		err := e.run(ctx, r)
		// A failure, a wait for a server and a stop are logged where they
		// happen.
		if ferr := (*FailedError)(nil); err != nil && !errors.As(err, &ferr) && !errors.Is(err, ErrPending) && !errors.Is(err, ErrInterrupted) {
			e.logger.ErrorContext(ctx, "a change stopped partway; Dockwell resumes it on its next start", slog.String("change", r.c.ID),
				slog.String("error", err.Error()))
		}
	}()
	return nil
}

func stopping(changeID string) error {
	return fault.New(ErrStopping, "the control plane is stopping, so %s wasn't applied. Apply it again once the control plane is back", changeID)
}

// Wait waits for the runs under way to end: the changes applying in the
// background, and Resume's.
func (e *Engine) Wait() {
	e.life.Lock()
	idle := e.idle
	e.life.Unlock()
	<-idle
}

// haltWait bounds how long Stop waits, once it has cancelled the steps
// under way, for their runs to record where they stopped.
const haltWait = 10 * time.Second

// Stop stops the engine, for the control plane to stop: from now on it
// starts no change, no Resume and no step. It lets the steps under way
// finish until ctx is done, then cancels them, and waits for every run to
// end, Resume's too. A change it stops partway stays applying, with what
// remains queued, and Resume finishes it when the control plane starts
// again. So does a change whose call fails because the control plane is
// stopping, such as when its agent's stream closes: it isn't failed, and
// isn't rolled back.
//
// Stop the API first, so no new change arrives, and close the agent
// listener after, so the steps under way can finish.
func (e *Engine) Stop(ctx context.Context) error {
	e.life.Lock()
	e.stopping = true
	idle, n := e.idle, e.active
	e.life.Unlock()
	if n > 0 {
		e.logger.InfoContext(ctx, "letting the changes under way finish their steps before stopping", slog.Int("runs", n))
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
	}
	e.logger.WarnContext(ctx, "cancelling the steps still running; their changes finish when the control plane starts again")
	e.halt()
	t := time.NewTimer(haltWait)
	defer t.Stop()
	select {
	case <-idle:
		return nil
	case <-t.C:
		e.life.Lock()
		n = e.active
		e.life.Unlock()
		return fmt.Errorf("%d runs of changes were still recording where they stopped %s after their steps were cancelled. "+
			"Their changes stay applying, and Dockwell finishes them when it starts again", n, haltWait)
	}
}

// enter counts a run starting, unless the engine is stopping.
func (e *Engine) enter() bool {
	e.life.Lock()
	defer e.life.Unlock()
	if e.stopping {
		return false
	}
	if e.active == 0 {
		e.idle = make(chan struct{})
	}
	e.active++
	return true
}

// exit counts a run ending.
func (e *Engine) exit() {
	e.life.Lock()
	defer e.life.Unlock()
	e.active--
	if e.active == 0 {
		close(e.idle)
	}
}

func (e *Engine) isStopping() bool {
	e.life.Lock()
	defer e.life.Unlock()
	return e.stopping
}

// calling gives an agent call a context that Stop cancels once the steps
// under way have had their time. done releases it.
func (e *Engine) calling(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	unhook := context.AfterFunc(e.halted, cancel)
	return ctx, func() { unhook(); cancel() }
}

// stoppedBy reports a call that failed because the engine is stopping:
// Stop cancelled it, or the agent's stream closed under it. Its change is
// interrupted, not failed.
func (e *Engine) stoppedBy(err error) bool {
	return e.isStopping() &&
		(errors.Is(err, context.Canceled) || errors.Is(err, resource.ErrLost) || errors.Is(err, resource.ErrNotConnected))
}

// committed is a change whose revisions are on record, ready to run.
type committed struct {
	c     store.Change
	items []item
	plan  *Plan
	// resumed is a run of what remains of a change left applying, planned
	// against every server it touches.
	resumed bool
}

// commit checks that a planned change still holds and commits its
// revisions, so desired state is on record before anything runs: agents
// then reconcile toward it even if the control plane stops halfway.
//
// It claims the change's resources before it plans, so a change that
// touches a resource another change is applying is refused as busy,
// whatever its plan would say now.
func (e *Engine) commit(ctx context.Context, changeID string, by By) (*committed, error) {
	// Holding the change before reading it means a run of it that ended
	// meanwhile, such as a retried apply's first try, has recorded how it
	// ended by the time it is read: the retry reads that the change isn't
	// planned any more, rather than finding its plan out of date.
	if !e.own(changeID) {
		return nil, fault.New(ErrBusy, "%s is being applied already. Watch it to follow its steps", changeID)
	}
	var c store.Change
	var items []item
	err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if c, err = tx.GetChange(ctx, changeID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return noSuchChange(changeID)
			}
			return err
		}
		if c.State != store.ChangePlanned {
			return notPlanned(changeID, c.State)
		}
		items, err = e.current(ctx, tx, c)
		return err
	})
	if err == nil {
		err = e.claim(changeID, "", items, false)
	}
	if err != nil {
		e.letGo(ctx, changeID)
		return nil, err
	}
	r, err := e.commitOwned(ctx, c, items, by, false)
	if err != nil {
		e.letGo(ctx, changeID)
	}
	return r, err
}

// rollBack plans and commits, in one, a change of Dockwell's own that puts
// back what a failed change did. Its intent is the desired state from
// before the failed change, so it commits whatever the servers say now:
// it isn't held to a fingerprint, and a server whose agent can't plan it
// is planned from its last known state, like an offline one. What it
// records as its plan is the plan that runs. So a rollback is never left
// planned: it is on record applying, or not at all.
func (e *Engine) rollBack(ctx context.Context, failed store.Change, edits []Edit) (*committed, error) {
	items, err := e.items(ctx, edits, true)
	if err != nil {
		return nil, err
	}
	c := store.Change{
		ID: id.New(id.Change), Summary: "Roll back " + failed.Summary, Actor: SystemActor, State: store.ChangePlanned, Reverts: failed.ID,
		CreatedAt: e.now(), ProposedVia: ViaAutomatic,
	}
	e.own(c.ID)
	// It takes over the failed change's resources, so nothing slips in
	// between.
	if err := e.claim(c.ID, failed.ID, items, false); err != nil {
		e.letGo(ctx, c.ID)
		return nil, err
	}
	r, err := e.commitOwned(ctx, c, items, system, true)
	if err != nil {
		e.letGo(ctx, c.ID)
	}
	return r, err
}

// expectRestores marks what a rollback must bring back, where its plan
// comes from a server's last known state, which can't say. What the failed
// change's steps took out of use and kept, the rollback restores, so when
// Resume reaches the server, a copy that is gone fails rather than making
// the resource empty.
func (e *Engine) expectRestores(ctx context.Context, c store.Change, plan *Plan) error {
	var failed store.Change
	err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		failed, err = tx.GetChange(ctx, c.Reverts)
		return err
	})
	if err != nil {
		return err
	}
	took, err := DecodePlan(failed.Plan)
	if err != nil {
		return err
	}
	for si := range plan.Servers {
		s := &plan.Servers[si]
		if !s.Offline && s.Unplanned == "" {
			continue
		}
		for i := range s.Steps {
			step := &s.Steps[i]
			was := took.step(s.Server, step.Ref)
			step.Restores = step.Restores || was != nil && was.Destroys && (was.Action == resource.ActionDelete || len(was.Kept) > 0)
		}
	}
	return nil
}

// step finds a plan's step for a resource on a server, or nil.
func (p *Plan) step(server string, ref resource.Ref) *resource.Step {
	for i := range p.Servers {
		if p.Servers[i].Server != server {
			continue
		}
		for j := range p.Servers[i].Steps {
			if p.Servers[i].Steps[j].Ref == ref {
				return &p.Servers[i].Steps[j]
			}
		}
	}
	return nil
}

// commitOwned plans a change this process owns, with its resources
// claimed, and commits it. A person's change must plan as it did when it
// was reviewed; a rollback isn't, and with create it is put on record here
// too, with the plan that runs.
func (e *Engine) commitOwned(ctx context.Context, c store.Change, items []item, by By, create bool) (*committed, error) {
	rollback := IsRollback(c)
	plan, err := e.plan(ctx, c.ID, items, rollback)
	if err != nil {
		return nil, err
	}
	if rollback {
		if err := e.expectRestores(ctx, c, plan); err != nil {
			return nil, err
		}
	}
	if create {
		plan.Change, plan.Summary, plan.Reverts = c.ID, c.Summary, c.Reverts
		if c.Plan, err = json.Marshal(plan); err != nil {
			return nil, err
		}
		c.Fingerprint, c.Servers = plan.Fingerprint, serversOf(plan)
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
	e.mu.Lock()
	err = e.db.Revise(ctx, func(tx *store.RevisionTx) error {
		if create {
			if err := e.recordPlanned(ctx, tx, c, items); err != nil {
				return err
			}
		}
		if !rollback {
			if err := holds(ctx, tx.Tx, c, plan); err != nil {
				return err
			}
		}
		// Nothing may have moved since the change was read, or for a
		// rollback since it was planned.
		if err := e.unmoved(ctx, tx.Tx, c, items); err != nil {
			return err
		}
		for i := range items {
			it := &items[i]
			if !it.changes() {
				continue
			}
			// Sealed for the revision it makes: the next after its base,
			// which unmoved found the latest in this transaction.
			number := it.base + 1
			spec, err := e.stored(ctx, tx.Tx, it, store.InRevision(it.id, string(it.ref.Kind), number))
			if err != nil {
				return err
			}
			rev, err := tx.AddRevision(ctx, store.NewRevision{
				Kind: string(it.ref.Kind), Scope: it.ref.Scope, Name: it.ref.Name, ID: it.id, Number: number,
				Spec: spec, Change: c.ID, Position: i, At: now, Uses: storeUses(e.usesOf(it, it.spec)),
			})
			if err != nil {
				return err
			}
			it.id, it.rev = rev.ID, rev.Revision
			if err := tx.PlaceRevision(ctx, rev.ID, rev.Revision, it.servers, now); err != nil {
				return err
			}
		}
		return tx.StartApplying(ctx, c.ID, by.Actor, by.Via, record, now)
	})
	e.mu.Unlock()
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Discarded, or applied by someone else, since it was read.
			return nil, fault.New(ErrNotPlanned, "%s was applied or discarded while it was being committed, so it wasn't applied again. Read it to see which", c.ID)
		}
		return nil, err
	}
	c.State, c.AppliedAt, c.AppliedBy, c.AppliedVia, c.Progress = store.ChangeApplying, now, by.Actor, by.Via, record
	e.track(c.ID, progress)
	e.published(c.ID, store.ChangeApplying)
	return &committed{c: c, items: items, plan: plan}, nil
}

// holds checks, as it commits, that a person's change still applies as it
// was reviewed. Its state comes first: a change applied or discarded since
// it was read, such as by a retried apply that lost the race, isn't
// planned any more, and its plan isn't to blame. Then every server must be
// there, and the plan must be the one reviewed.
func holds(ctx context.Context, tx *store.Tx, c store.Change, plan *Plan) error {
	now, err := tx.GetChange(ctx, c.ID)
	if err != nil {
		return err
	}
	if now.State != store.ChangePlanned {
		return notPlanned(c.ID, now.State)
	}
	for _, s := range plan.Servers {
		if s.Offline {
			return fault.New(ErrOffline, "%s isn't connected, so %s can't apply. Apply it again once %s's agent reconnects", s.Name, c.ID, s.Name)
		}
	}
	switch {
	case c.FingerprintScheme < fingerprintKeyed && plan.plain == c.Fingerprint:
		// Planned before the upgrade, and still the plan reviewed.
	case c.FingerprintScheme < fingerprintKeyed:
		return fault.New(ErrPlanChanged, "%s was planned by an earlier version of Dockwell, and its plan no longer matches what applying it would do now, "+
			"as the servers or how Dockwell plans changed since. Propose the change again and review the new plan", c.ID)
	case plan.Fingerprint != c.Fingerprint:
		return fault.New(ErrPlanChanged, "the servers changed since %s was planned, so its plan no longer matches them. "+
			"Propose the change again and review what changed", c.ID)
	}
	return nil
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
// connected and stays applying until Resume reaches them. A run the engine
// stops partway leaves its change applying, for Resume to finish.
func (e *Engine) run(ctx context.Context, r *committed) error {
	defer e.letGo(ctx, r.c.ID)
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
	digests := map[at]string{}

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

	// Steps are announced as they start, and as they end once the store
	// has how they ended, so a watch hears of no end the record lacks.
	//
	// stop records a failed step and skips the steps after it, then rolls
	// the change back. The skipped steps are recorded as the change fails,
	// and its event carries them.
	//
	// A step that failed leaving the server untouched leaves the revision it runs
	// reading as it did, unless this change made one it should be on by now.
	stop := func(qi int, err error, untouched bool) error {
		q := queue[qi]
		it := byRef[q.step.Ref]
		p, u := e.step(c.ID, q.index, StepFailed, err.Error())
		var recorded bool
		if untouched && it.rev == it.base {
			recorded = e.recordProgress(ctx, c.ID)
		} else {
			recorded = e.deployed(ctx, c.ID, it, q.server, err, p)
		}
		if recorded {
			e.announce(c.ID, u)
		}
		for _, rest := range queue[qi+1:] {
			e.step(c.ID, rest.index, StepSkipped, message(StepSkipped, rest.step.Action, rest.step.Ref, rest.name))
		}
		return e.fail(ctx, c, q.step.Ref, err)
	}
	// interrupt leaves the change applying, as the control plane stops,
	// with the step it was at queued again: Resume plans it against the
	// server when the control plane starts again, and finds it done or not.
	interrupt := func(qi int) error {
		q := queue[qi]
		_, u := e.step(c.ID, q.index, StepQueued, message(StepQueued, q.step.Action, q.step.Ref, q.name))
		if e.recordProgress(ctx, c.ID) {
			e.announce(c.ID, u)
		}
		e.logger.InfoContext(ctx, "stopped a change partway, as the control plane is stopping; it finishes when the control plane starts again",
			slog.String("change", c.ID))
		return ErrInterrupted
	}
	// A server planned from its last known state, because its agent wasn't
	// there or couldn't plan, runs none of those steps: Resume plans them
	// against the server once it can. A rollback waits for that; any other
	// change can't run without the server.
	planned, unplanned := map[string]bool{}, map[string]string{}
	for i := range plan.Servers {
		sp := &plan.Servers[i]
		planned[sp.Server] = !sp.Offline && sp.Unplanned == ""
		unplanned[sp.Server] = sp.Unplanned
	}
	pending := map[string]string{} // server ID → name
	var waits []*Update
	// wait leaves a step of a rollback waiting for its server: to
	// reconnect, or, where it is connected and its agent couldn't plan, to
	// plan it.
	wait := func(q queued, couldntPlan string) {
		pending[q.server] = q.name
		msg := message(StepWaiting, q.step.Action, q.step.Ref, q.name)
		if couldntPlan != "" {
			msg = fmt.Sprintf("Waiting for %s's agent, which couldn't plan this step: %s", q.name, couldntPlan)
		}
		_, u := e.step(c.ID, q.index, StepWaiting, msg)
		waits = append(waits, u)
	}
	done := map[at]bool{}
	for qi, q := range queue {
		if e.isStopping() {
			return interrupt(qi)
		}
		it := byRef[q.step.Ref]
		agent, online := e.agents.Agent(q.server)
		if !online || agent == nil || !planned[q.server] {
			if IsRollback(c) {
				var couldntPlan string
				if online {
					couldntPlan = unplanned[q.server]
				}
				wait(q, couldntPlan)
				continue
			}
			if e.isStopping() {
				return interrupt(qi)
			}
			return stop(qi, fault.New(ErrOffline, "%s lost its connection before this step, so the step didn't run", q.name), false)
		}
		running := message(StepRunning, q.step.Action, q.step.Ref, q.name)
		_, u := e.step(c.ID, q.index, StepRunning, running)
		e.announce(c.ID, u)
		callCtx, called := e.calling(ctx)
		index := q.index
		callCtx = resource.WithProgress(callCtx, func(m string) { e.progressed(c.ID, index, running+": "+m) })
		var told resource.Outcome
		callCtx = resource.WithOutcome(callCtx, func(o resource.Outcome) {
			told.Digest, told.Note, told.Untouched = cmp.Or(o.Digest, told.Digest), cmp.Or(o.Note, told.Note), told.Untouched || o.Untouched
			told.Did = cmp.Or(o.Did, told.Did)
			if o.Replacing {
				e.pulledNewer(ctx, c.ID, items, it, q.server, o)
			}
		})
		err := agent.Apply(callCtx, *q.step, it.desiredOn(q.server))
		called()
		switch {
		case err == nil:
		case e.stoppedBy(err):
			return interrupt(qi)
		case IsRollback(c) && (errors.Is(err, resource.ErrNotConnected) || errors.Is(err, resource.ErrLost)):
			// The agent went away: the call never reached it, or it may
			// have run. Either way a rollback waits for the server, as for
			// one that isn't connected, and Resume plans the step against
			// it again, to find it done or not.
			wait(q, "")
			continue
		default:
			return stop(qi, err, told.Untouched)
		}
		did := q.step.Action
		if told.Did != nil {
			did = *told.Did
		}
		p, u := e.finished(c.ID, q.index, cmp.Or(told.Note, message(StepDone, q.step.Action, q.step.Ref, q.name)), did)
		digests[at{q.server, it.ref}] = told.Digest
		if e.deployedImage(ctx, c.ID, it, q.server, nil, p, told.Digest) {
			e.announce(c.ID, u)
		}
		done[at{q.server, it.ref}] = true
	}
	// Where a revision needed no step, the server already has it. So does one a
	// pull found nothing newer on, where another server's pull made a revision
	// after that step had gone on record under the one before.
	for i := range items {
		it := &items[i]
		for _, s := range it.servers {
			_, waits := pending[s]
			if waits || slices.Contains(it.gone, s) {
				continue
			}
			if !done[at{s, it.ref}] || it.pull && it.rev != it.base {
				e.deployedImage(ctx, c.ID, it, s, nil, nil, digests[at{s, it.ref}])
			}
		}
	}
	if len(pending) > 0 {
		var names []string
		back := false
		for _, s := range slices.Sorted(maps.Keys(pending)) {
			names = append(names, pending[s])
			_, online := e.agents.Agent(s)
			back = back || online
		}
		e.logger.WarnContext(ctx, "a rollback waits for servers to reconnect", slog.String("change", c.ID), slog.Any("servers", names))
		if e.recordProgress(ctx, c.ID) {
			for _, u := range waits {
				e.announce(c.ID, u)
			}
		}
		// A server it waits for may be back already: it reconnected before
		// the rollback was on record for that connection's Resume to find,
		// or it was only planned from its last known state. Resume again
		// once this run lets go, so the rollback isn't left waiting for a
		// connection that already came. Only a first run asks. A resumed
		// one was planned against every server, so a step of it that waits
		// lost its server since, and the server's reconnect resumes it;
		// asking would only send Resume round again, getting nowhere.
		if back && !r.resumed {
			e.resumeAfter(c.ID)
		}
		return reconnects(names)
	}
	if err := e.finish(ctx, c, store.ChangeApplied, "", "change.applied", e.progress(c.ID)); err != nil {
		return err
	}
	e.published(c.ID, store.ChangeApplied)
	return nil
}

// pulledNewer puts a newer image a pull found on record, before anything is
// replaced with it: a revision of the same spec, with its digest, so what ran
// before stays what a rollback can go back to, a failed replace included. A pull
// that finds nothing newer never comes here, so it makes no revision. A record
// that can't be written is logged, not fatal: the step goes on either way.
func (e *Engine) pulledNewer(ctx context.Context, changeID string, items []item, it *item, server string, o resource.Outcome) {
	now := e.now()
	rev := it.rev
	e.mu.Lock()
	err := e.db.Revise(ctx, func(tx *store.RevisionTx) error {
		if o.Previous != "" {
			if err := tx.SetDeploymentDigest(ctx, it.id, it.base, server, o.Previous, true); err != nil {
				return err
			}
		}
		if rev == it.base {
			number := it.base + 1
			spec, err := e.stored(ctx, tx.Tx, it, store.InRevision(it.id, string(it.ref.Kind), number))
			if err != nil {
				return err
			}
			made, err := tx.AddRevision(ctx, store.NewRevision{
				Kind: string(it.ref.Kind), Scope: it.ref.Scope, Name: it.ref.Name, ID: it.id, Number: number,
				Spec: spec, Change: changeID, Position: slices.IndexFunc(items, func(o item) bool { return o.id == it.id }), At: now, Uses: storeUses(e.usesOf(it, it.spec)),
			})
			if err != nil {
				return err
			}
			if err := tx.PlaceRevision(ctx, made.ID, made.Revision, it.servers, now); err != nil {
				return err
			}
			rev = made.Revision
		}
		return tx.SetDeploymentDigest(ctx, it.id, rev, server, o.Digest, false)
	})
	e.mu.Unlock()
	if err != nil {
		e.logger.WarnContext(ctx, "can't record the newer image a pull found", slog.String("resource", it.ref.String()), slog.String("server", server),
			slog.String("error", err.Error()))
		return
	}
	it.rev = rev
}

// deployed records how a step went on a server: the revision is live
// there, or failed; or, where the step took it off the server, what it
// had there is superseded. It records the change's progress with it. A
// record that can't be written is logged, not fatal: the step itself
// happened, and Drift reads the servers, not this. It reports whether the
// record was written.
func (e *Engine) deployed(ctx context.Context, changeID string, it *item, server string, failed error, progress *Progress) bool {
	return e.deployedImage(ctx, changeID, it, server, failed, progress, "")
}

// deployedImage is deployed, with what image it ran, as its registry knows it.
func (e *Engine) deployedImage(ctx context.Context, changeID string, it *item, server string, failed error, progress *Progress, digest string) bool {
	now := e.now()
	err := e.db.Revise(ctx, func(tx *store.RevisionTx) error {
		if err := writeProgress(ctx, tx, changeID, progress); err != nil {
			return err
		}
		switch {
		case failed != nil && slices.Contains(it.servers, server):
			return tx.SetDeploymentState(ctx, store.Deployment{
				ResourceID: it.id, Revision: it.rev, Server: server, State: store.DeploymentFailed, Error: failed.Error(), UpdatedAt: now,
			})
		case failed != nil:
			return nil //nolint:nilerr // a failed step off a server leaves its deployment as it was
		case slices.Contains(it.servers, server):
			return tx.SetDeploymentState(ctx, store.Deployment{ResourceID: it.id, Revision: it.rev, Server: server, State: store.DeploymentLive, Digest: digest, UpdatedAt: now})
		default:
			return tx.Supersede(ctx, it.id, server, it.rev+1, now)
		}
	})
	if err != nil {
		e.logger.WarnContext(ctx, "can't record a deployment", slog.String("resource", it.ref.String()), slog.String("server", server),
			slog.String("error", err.Error()))
	}
	return err == nil
}

// recordProgress writes the live progress of a change running here,
// logging a failure: the steps happened either way. It reports whether the
// progress was written.
func (e *Engine) recordProgress(ctx context.Context, changeID string) bool {
	p := e.progress(changeID)
	if p == nil {
		return false
	}
	err := e.db.Revise(ctx, func(tx *store.RevisionTx) error { return writeProgress(ctx, tx, changeID, p) })
	if err != nil {
		e.logger.WarnContext(ctx, "can't record a change's progress", slog.String("change", changeID), slog.String("error", err.Error()))
	}
	return err == nil
}

// writeProgress records a change's progress in tx; nil leaves it as it is.
func writeProgress(ctx context.Context, tx *store.RevisionTx, changeID string, p *Progress) error {
	if p == nil {
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return tx.SetChangeProgress(ctx, changeID, b)
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
//
// No wakeup is lost: when Resume skips a change because it is running
// here, or because another change holds its resources, Resume runs again
// once that change lets go. Only that change wakes it, so Resume doesn't go
// round while it waits. A rollback whose first run ends waiting for a
// server that is connected by then wakes it too.
func (e *Engine) Resume(ctx context.Context) error {
	if !e.enter() {
		return nil // stopping: the next start resumes
	}
	defer e.exit()
	return e.resume(ctx)
}

func (e *Engine) resume(ctx context.Context) error {
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
		if e.isStopping() {
			break
		}
		r, err := e.prepareResume(ctx, changeID)
		if err == nil && r != nil {
			err = e.run(context.WithoutCancel(ctx), r)
		}
		if err != nil && !errors.Is(err, ErrPending) && !errors.Is(err, ErrInterrupted) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// prepareResume plans what remains of a change left applying, and holds
// it as running here. It returns nil when the change is held here already,
// or no longer applying, and ErrPending while a server it touches is
// offline or another change holds one of its resources.
func (e *Engine) prepareResume(ctx context.Context, changeID string) (*committed, error) {
	// Holding the change before reading it means a run of it that ended
	// meanwhile has recorded how it ended by the time it is read.
	if !e.ownOrResumeLater(changeID) {
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
		for i := range c.Items {
			ci := &c.Items[i]
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
			asked, err := e.open(ctx, tx, store.InChange(cmp.Or(ci.ResourceID, it.id), ci.Kind, c.ID, i), ci.Spec)
			if err != nil {
				return err
			}
			if !bytes.Equal(it.encoded, asked) || it.base == 0 {
				continue // a later change moved it on
			}
			if ci.Pull {
				moved, err := resumingPull(ctx, tx, c.ID, ci, &it)
				if err != nil {
					return err
				}
				if moved {
					continue
				}
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
		e.letGo(ctx, changeID)
		return nil, err
	}
	var offline []string
	for i := range items {
		for _, s := range items[i].targets() {
			if _, online := e.agents.Agent(s); !online && !slices.Contains(offline, s) {
				offline = append(offline, s)
			}
		}
	}
	if len(offline) > 0 {
		e.letGo(ctx, changeID)
		names, err := e.serverNames(ctx, offline)
		if err != nil {
			return nil, err
		}
		for i, s := range offline {
			offline[i] = names[s]
		}
		return nil, reconnects(offline)
	}
	if err := e.claim(changeID, "", items, true); err != nil {
		// The change holding them asks for Resume when it lets go.
		e.letGo(ctx, changeID)
		return nil, fault.New(ErrPending, "%w", err)
	}
	e.logger.InfoContext(ctx, "resuming a change left applying", slog.String("change", c.ID))
	plan, err := e.plan(ctx, c.ID, items, false)
	if err != nil {
		e.letGo(ctx, changeID)
		return nil, err
	}
	// Planning again holds to no fingerprint, so what was approved as a
	// restore stays one: if the copy is gone by now, the step fails, rather
	// than making the resource empty with nobody reviewing that.
	recorded, err := DecodePlan(c.Plan)
	if err != nil {
		e.letGo(ctx, changeID)
		return nil, err
	}
	for si := range plan.Servers {
		s := &plan.Servers[si]
		for i := range s.Steps {
			step := &s.Steps[i]
			if was := recorded.step(s.Server, step.Ref); was != nil && step.Action != resource.ActionDelete {
				step.Restores = step.Restores || was.Restores
			}
			for j := range items {
				step.Pulled = step.Pulled || step.Pull && items[j].ref == step.Ref && slices.Contains(items[j].pulled, s.Server)
			}
		}
	}
	before, err := decodeProgress(c.Progress)
	if err != nil {
		e.letGo(ctx, changeID)
		return nil, err
	}
	e.track(changeID, resumed(before, plan, items))
	e.published(changeID, store.ChangeApplying)
	return &committed{c: c, items: items, plan: plan, resumed: true}, nil
}

// resumingPull carries a change's pull over to the run that resumes it, as the
// specs alone say nothing is left to do. It reports a resource a later change has
// moved on since, which needs nothing more from this one. A revision this change
// made already is the one it goes on with, so going on makes no other, and where
// it records a digest, the server has pulled.
func resumingPull(ctx context.Context, tx *store.Tx, changeID string, ci *store.ChangeItem, it *item) (moved bool, err error) {
	r, err := tx.GetResource(ctx, ci.Kind, ci.Scope, ci.Name)
	if err != nil {
		return false, err
	}
	if r.ChangeID != changeID {
		it.pull = it.base == ci.BaseRevision
		return !it.pull, nil
	}
	it.pull, it.base = true, ci.BaseRevision
	deployments, err := tx.Deployments(ctx, it.id, it.rev)
	if err != nil {
		return false, err
	}
	for i := range deployments {
		if deployments[i].Digest != "" {
			it.pulled = append(it.pulled, deployments[i].Server)
		}
	}
	return false, nil
}

// current checks that every item still starts from the latest revision,
// and that its scope is still there, and reads them as they stand.
func (e *Engine) current(ctx context.Context, tx *store.Tx, c store.Change) ([]item, error) {
	items := make([]item, 0, len(c.Items))
	for i := range c.Items {
		ci := &c.Items[i]
		ref := resource.Ref{Kind: resource.Kind(ci.Kind), Scope: ci.Scope, Name: ci.Name}
		it, err := e.load(ctx, tx, ref)
		if err != nil {
			return nil, err
		}
		if it.base != ci.BaseRevision {
			r, _ := tx.GetResource(ctx, ci.Kind, ci.Scope, ci.Name)
			return nil, fault.New(ErrStale, "%s changed in %s after %s was planned, so its plan is out of date. Propose the change again", ref, r.ChangeID, c.ID)
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
		it.servers, it.rotates, it.pull = ci.Servers, ci.Rotates, ci.Pull
		// Opened as the item of the change it was sealed for, of the
		// resource the base check above makes the one there now.
		if it.encoded, err = e.open(ctx, tx, store.InChange(cmp.Or(ci.ResourceID, it.id), ci.Kind, c.ID, i), ci.Spec); err != nil {
			if c.FingerprintScheme < fingerprintKeyed {
				return nil, fault.New(ErrPlanChanged, "%s was planned by an earlier version of Dockwell, which kept what it asks of %s unencrypted, "+
					"so it can't apply. Propose the change again and review the new plan", c.ID, ref)
			}
			return nil, fmt.Errorf("can't read what %s asks of %s: %w", c.ID, ref, err)
		}
		if ci.Spec != nil {
			if it.spec, err = e.kinds.Decode(ref.Kind, it.encoded); err != nil {
				return nil, err
			}
		}
		items = append(items, it)
	}
	if err := e.check(ctx, tx, items); err != nil {
		return nil, fmt.Errorf("%w. %s can't apply as it was planned, so propose the change again", err, c.ID)
	}
	if err := e.stillPinned(ctx, tx, c, items); err != nil {
		return nil, err
	}
	return items, nil
}

// unmoved checks, as a change commits, that nothing moved since its items
// were read: each still starts from the latest revision, what it lives in
// and its names still hold, and what it uses is still pinned. It checks the
// items being committed, whatever kind of change they are: a person's,
// read from its record, or a rollback, planned and committed in one.
func (e *Engine) unmoved(ctx context.Context, tx *store.Tx, c store.Change, items []item) error {
	for i := range items {
		it := &items[i]
		r, err := tx.GetResource(ctx, string(it.ref.Kind), it.ref.Scope, it.ref.Name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if r.Revision != it.base {
			return fault.New(ErrStale, "%s changed in %s after %s was planned, so its plan is out of date. Propose the change again", it.ref, r.ChangeID, c.ID)
		}
	}
	if err := e.check(ctx, tx, items); err != nil {
		return fmt.Errorf("%w. %s can't apply as it was planned, so propose the change again", err, c.ID)
	}
	return e.stillPinned(ctx, tx, c, items)
}

// fail records a failed apply and rolls back what it changed. The
// rollback commits at once, so desired state goes back even when a server
// can't be reached yet. Watchers learn of the failure once the rollback is
// on record, so they can follow it, and only once the failure is: if it
// can't be recorded, the change stays applying on record, and Resume takes
// it up again.
func (e *Engine) fail(ctx context.Context, c store.Change, at resource.Ref, cause error) error {
	ferr := &FailedError{Change: c.ID, Step: at, Err: cause}
	if err := e.finish(ctx, c, store.ChangeFailed, cause.Error(), "change.failed", e.progress(c.ID)); err != nil {
		return errors.Join(ferr, err)
	}
	if IsRollback(c) {
		e.published(c.ID, store.ChangeFailed)
		return ferr
	}
	e.logger.WarnContext(ctx, "a change failed; rolling it back", slog.String("change", c.ID), slog.String("at", at.String()),
		slog.String("error", cause.Error()))
	edits, err := e.reverse(ctx, c.ID)
	if err != nil || len(edits) == 0 {
		ferr.RollbackErr = err
		e.published(c.ID, store.ChangeFailed)
		return ferr
	}
	r, err := e.rollBack(ctx, c, edits)
	if err != nil {
		ferr.RollbackErr = err
		e.published(c.ID, store.ChangeFailed)
		return ferr
	}
	ferr.Rollback = r.c.ID
	e.published(c.ID, store.ChangeFailed)
	if err := e.run(ctx, r); err != nil {
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
		if err := writeProgress(ctx, tx, c.ID, progress); err != nil {
			return err
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
			return noSuchChange(changeID)
		case err != nil:
			return err
		case c.State != store.ChangePlanned:
			return fault.New(ErrNotPlanned, "%s is %s, and only a planned change can be discarded", changeID, c.State)
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
				return fault.New(ErrCantUndo, "%s changed again in %s, so %s can't be undone whole. Undo %s first, or edit %s directly", ref, latest.ChangeID, changeID, latest.ChangeID, rev.Name)
			}
			ed := Edit{Ref: ref}
			if rev.Revision > 1 {
				prev, err := tx.GetRevision(ctx, rev.ID, rev.Revision-1)
				if err != nil {
					return err
				}
				if prev.Spec != nil {
					if ed.Spec, err = e.decode(ctx, tx, store.InRevision(rev.ID, rev.Kind, prev.Revision), prev.Spec); err != nil {
						return err
					}
					if ed.Spec, err = e.pinIfMoved(ctx, tx, rev, prev.Revision, ed.Spec); err != nil {
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

// pinIfMoved is spec, of an earlier revision, pinned to what that revision ran if
// its tag is the one rev names and rev ran something else: going back to the tag
// alone would run the image it's going back from.
func (e *Engine) pinIfMoved(ctx context.Context, tx *store.Tx, rev *store.Revision, earlier int, spec proto.Message) (proto.Message, error) {
	namer, ok := e.imageNamer(resource.Kind(rev.Kind))
	if !ok || rev.Spec == nil {
		return spec, nil
	}
	now, err := e.decode(ctx, tx, store.InRevision(rev.ID, rev.Kind, rev.Revision), rev.Spec)
	if err != nil {
		return nil, err
	}
	if resource.BareImage(namer.ImageOf(now)) != resource.BareImage(namer.ImageOf(spec)) {
		return spec, nil
	}
	ran, err := tx.Deployments(ctx, rev.ID, earlier)
	if err != nil {
		return nil, err
	}
	last, err := tx.Deployments(ctx, rev.ID, rev.Revision)
	if err != nil {
		return nil, err
	}
	digest, moved := oneDigest(ran), oneDigest(last)
	if digest == "" || moved == "" || digest == moved {
		return spec, nil
	}
	out := proto.Clone(spec)
	namer.SetImage(out, resource.PinnedImage(namer.ImageOf(spec), digest))
	return out, nil
}

// imageNamer returns a kind as an ImageNamer, if it is one.
func (e *Engine) imageNamer(kind resource.Kind) (resource.ImageNamer, bool) {
	t, err := e.kinds.Type(kind)
	if err != nil {
		return nil, false
	}
	n, ok := t.(resource.ImageNamer)
	return n, ok
}

// oneDigest is the digest every deployment that knows one ran, or nothing if they
// differ or none knows.
func oneDigest(deployments []store.Deployment) string {
	out := ""
	for i := range deployments {
		switch d := deployments[i].Digest; {
		case d == "":
		case out == "":
			out = d
		case d != out:
			return ""
		}
	}
	return out
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
		return nil, fault.New(ErrCantUndo, "before %s, %s ran on %s, which %s removed from the fleet, so there's nowhere to put it back. Edit %s directly instead",
			changeID, ref.Name, human.List(gone), wasOrWere(len(gone)), ref.Name)
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
			return nil, noSuchChange(changeID)
		}
		return nil, err
	}
	if c.State != store.ChangeApplied {
		return nil, fault.New(ErrCantUndo, "%s is %s, and only applied changes can be undone", changeID, c.State)
	}
	if age := e.now().Sub(c.AppliedAt); age > UndoWindow {
		return nil, fault.New(ErrCantUndo, "%s was applied %s ago, and changes can be undone for %s. Edit the resources directly instead",
			changeID, days(age), days(UndoWindow))
	}
	edits, err := e.reverse(ctx, changeID)
	if err != nil {
		return nil, err
	}
	if len(edits) == 0 {
		return nil, fault.New(ErrCantUndo, "%s made no revisions, so there's nothing to undo", changeID)
	}
	return e.propose(ctx, Proposal{Summary: "Undo " + c.Summary, Actor: by.Actor, Via: by.Via, Edits: edits}, changeID, true)
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
	names, err := e.serverNames(ctx, []string{server})
	if err != nil {
		return nil, err
	}
	name := names[server]
	agent, online := e.agents.Agent(server)
	if !online {
		return nil, fault.New(ErrOffline, "%s isn't connected, so its drift can't be checked. Check it again once its agent reconnects", name)
	}
	var resources []store.Resource
	var desired []proto.Message
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if resources, err = tx.ListPlaced(ctx, server); err != nil {
			return err
		}
		desired = make([]proto.Message, len(resources))
		for i, r := range resources {
			if desired[i], err = e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec); err != nil {
				return err
			}
		}
		return nil
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
		return nil, &AgentError{Server: name, Doing: "check drift", Err: err}
	}
	var out []Drift
	for i, r := range resources {
		// Agents normalize what they observe, so only real differences
		// are unequal.
		if resource.Equal(desired[i], observed[i]) {
			continue
		}
		d := Drift{ID: r.ID, Ref: refs[i], Revision: r.Revision, Observed: observed[i], Missing: observed[i] == nil}
		if !d.Missing {
			d.Changes = resource.Diff(desired[i], observed[i])
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
	// The latest revision's own values, which nobody chose now: no
	// rotation, only what the server needs to match it again.
	return e.propose(ctx, Proposal{Summary: summary, Actor: by.Actor, Via: by.Via, Edits: []Edit{{Ref: d.Ref, Spec: it.current}}}, "", false)
}

// Adopt plans keeping what a server has for a drifted resource: a new
// revision that matches it, with nothing to do on the server.
func (e *Engine) Adopt(ctx context.Context, d Drift, by By) (*Plan, error) {
	summary := fmt.Sprintf("Keep the changes made to %s outside Dockwell", d.Ref.Name)
	// What the server runs already, so nothing restarts.
	return e.propose(ctx, Proposal{Summary: summary, Actor: by.Actor, Via: by.Via, Edits: []Edit{{Ref: d.Ref, Spec: d.Observed}}}, "", false)
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
