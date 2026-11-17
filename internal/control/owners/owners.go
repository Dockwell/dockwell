// SPDX-License-Identifier: AGPL-3.0-only

// Package owners makes catalog apps' owners: once an install's change applies and
// its service runs, it asks the app's agent to run the steps its app.owner holds,
// with the values they name for that call alone, and records what came of it as an
// observed fact. A made or an opened owner lifts its install's hold, and the runner
// releases what the hold kept, as a change of its own. See
// docs/adr/0105-a-catalog-apps-owner-is-made-at-install.md.
package owners

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/holds"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/ownerstep"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/appowner"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Agent is a server's agent, as making an owner asks it.
type Agent interface {
	MakeAppOwner(ctx context.Context, environment, service string, owner *resourcev1.OwnerRun, values map[string]string, checkFirst bool) (*agentv1.MakeAppOwnerResult, error)
	ReadAppOwner(ctx context.Context, environment, service string, owner *resourcev1.OwnerRun, values map[string]string, signups bool) (*agentv1.ReadAppOwnerResult, error)
}

// Every is how often the runner looks, and Poll how often it reads, while it's
// held, whether an owner that waits on its done is made.
const (
	Every = 10 * time.Second
	Poll  = 5 * time.Minute
)

// Keeper decodes a resource's spec, and opens a rotation's pending password.
type Keeper interface {
	holds.Decoder
	Open(ctx context.Context, tx *store.Tx, at store.SpecAt, stored []byte) ([]byte, error)
}

// Runner makes owners, sets their new passwords, and releases what their holds kept.
type Runner struct {
	DB     *store.DB
	Keeper Keeper
	Engine *revision.Engine
	Holds  *holds.Holds
	// Agent is the connected agent of a server, if any.
	Agent  func(server string) (Agent, bool)
	Logger *slog.Logger
	// Now is the clock, time.Now unless set.
	Now func() time.Time

	mu      sync.Mutex
	runs    Runs
	running map[string]bool
	// setUp are environments whose failed owner's app read set up, by done, while
	// a setup link was live.
	setUp map[string]bool
	wg    sync.WaitGroup
}

// forgetSetUp drops what was read of an environment's failed owner's app being set up.
func (r *Runner) forgetSetUp(environment string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.setUp, environment)
}

// SetUp says whether an environment's failed owner's app read set up, by its done,
// while a person set it up through its setup link (ADR 0105).
func (r *Runner) SetUp(environment string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.setUp[environment]
}

func (r *Runner) now() time.Time {
	r.mu.Lock()
	now := r.Now
	r.mu.Unlock()
	if now != nil {
		return now()
	}
	return time.Now()
}

// SetNow has the runner read the time from now, as a test does while it runs.
func (r *Runner) SetNow(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Now = now
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// Run looks every Every until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(Every)
	defer t.Stop()
	for {
		if err := r.Tick(ctx); err != nil && ctx.Err() == nil {
			r.logger().WarnContext(ctx, "can't make catalog apps' owners now; trying again", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			r.wg.Wait()
			return
		case <-t.C:
		}
	}
}

// job is an owner to make, or to read.
type job struct {
	id, environment, server string
	owner                   *resourcev1.OwnerRun
	values                  map[string]string
	checkFirst, readOnly    bool
	// setUp reads whether a failed owner's app reads set up, for its page, while a
	// person sets it up through its setup link: it records nothing (ADR 0105).
	setUp bool
	tries int
	// since is when what's observed of it last changed.
	since time.Time
	// rotation names a new password to set, by the entry's reset steps, and
	// pending is it, which values never hold: they hold the current one
	// (ADR 0105).
	rotation, pending string
}

// Runs reads what makes an environment's install's owner from its catalog entry at
// the release it runs (catalogapi.Handler.OwnerRun): the run, the plain values its
// steps name, and the secrets they name, by name.
type Runs interface {
	OwnerRun(ctx context.Context, environment string) (*resourcev1.OwnerRun, map[string]string, []string, error)
}

// ResetRuns reads what sets a new password for an environment's install's owner
// (catalogapi.Handler.OwnerResetRun), as Runs does.
type ResetRuns interface {
	OwnerResetRun(ctx context.Context, environment string) (*resourcev1.OwnerRun, map[string]string, []string, error)
}

// Unsets reads what an environment's install takes out of its owner's service once
// its owner is made (catalogapi.Handler.OwnerUnset): the service, and the names of
// its environment's variables.
type Unsets interface {
	OwnerUnset(ctx context.Context, environment string) (string, []string, error)
}

// SetRuns has the runner read what makes an owner from runs.
func (r *Runner) SetRuns(runs Runs) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = runs
}

// Tick makes what owners it can, records what's observed, and releases what the
// holds of installs no longer held kept. A make runs on its own, as waiting for an
// app to come up takes minutes.
func (r *Runner) Tick(ctx context.Context) error {
	var jobs []job
	var states []store.AppOwnerState
	var released []string
	err := r.DB.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			for _, e := range p.Environments {
				js, ss, err := r.look(ctx, tx, e.ID)
				if err != nil {
					return err
				}
				jobs, states = append(jobs, js...), append(states, ss...)
			}
		}
		js, err := r.rotations(ctx, tx)
		jobs = append(jobs, js...)
		return err
	})
	if err != nil {
		return err
	}
	if err := r.record(ctx, states...); err != nil {
		return err
	}
	for _, j := range jobs {
		ready, failed, err := r.ready(ctx, j)
		if err != nil {
			return err
		}
		if failed != nil {
			if err := r.record(ctx, *failed); err != nil {
				return err
			}
			continue
		}
		if ready != nil {
			r.start(ctx, *ready)
		}
	}
	// Whatever an owner made or opened lifted, released, as a change of Dockwell's.
	err = r.DB.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			for _, e := range p.Environments {
				ok, err := r.releasable(ctx, tx, e.ID)
				if err != nil {
					return err
				}
				if ok {
					released = append(released, e.ID)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, env := range released {
		if err := r.Release(ctx, env); err != nil {
			r.logger().WarnContext(ctx, "can't release a held install now; trying again", slog.String("environment", env), slog.String("error", err.Error()))
		}
	}
	return nil
}

// SetAgent has the runner ask fn for a server's agent, in place of Agent, as a test
// does with an agent of its own.
func (r *Runner) SetAgent(fn func(server string) (Agent, bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Agent = fn
}

func (r *Runner) agentFor(server string) (Agent, bool) {
	r.mu.Lock()
	fn := r.Agent
	r.mu.Unlock()
	if fn == nil {
		return nil, false
	}
	return fn(server)
}

// Wait waits for the makes the runner started to end, for tests.
func (r *Runner) Wait() { r.wg.Wait() }

// look finds what to do of an environment's owners: what to record, and what to make
// or read.
func (r *Runner) look(ctx context.Context, tx *store.Tx, environment string) ([]job, []store.AppOwnerState, error) {
	list, err := tx.ListResources(ctx, environment, string(appowner.Kind))
	if err != nil || len(list) == 0 {
		return nil, nil, err
	}
	ids := make([]string, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	seen, err := tx.AppOwnerStates(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	now := r.now()
	var jobs []job
	var record []store.AppOwnerState
	for i := range list {
		res := list[i]
		st, has := seen[res.ID]
		if has && store.OwnerFinal(st.State) {
			continue
		}
		if !has {
			st = store.AppOwnerState{ResourceID: res.ID, Environment: environment, State: store.OwnerWaiting, UpdatedAt: now}
		}
		// A person's open, once its change applied, ends it.
		// Any of the Opens planned, once applied (the re-check's N4).
		opened := false
		for _, change := range strings.Fields(st.Opening) {
			applied, err := changeApplied(ctx, tx, change)
			if err != nil {
				return nil, nil, err
			}
			opened = opened || applied
		}
		if opened {
			st.State, st.UpdatedAt = store.OwnerOpened, now
			record = append(record, st)
			continue
		}
		spec, err := r.Keeper.Decode(ctx, tx, store.InRevision(res.ID, res.Kind, res.Revision), res.Spec)
		if err != nil {
			return nil, nil, err
		}
		owner, ok := spec.(*resourcev1.AppOwner)
		if !ok {
			continue
		}
		switch {
		case owner.GetEmail() == "" && owner.GetUser() == "":
			if !has || st.State != store.OwnerWaitingEmail {
				st.State, st.UpdatedAt = store.OwnerWaitingEmail, now
				record = append(record, st)
			}
			continue
		case st.State == store.OwnerFailed:
			// It waits for a person: RetryCatalogOwner, or open it. While they set it
			// up through its setup link, its done is read for its page; once the link
			// ends, what was read goes (the review's L3).
			live, err := r.setupLive(ctx, tx, environment, now)
			if err != nil {
				return nil, nil, err
			}
			server := ""
			if live {
				if server, err = liveServer(ctx, tx, environment, owner.GetService()); err != nil {
					return nil, nil, err
				}
			}
			if server != "" {
				jobs = append(jobs, job{id: res.ID, environment: environment, server: server, setUp: true, tries: st.Tries, since: st.UpdatedAt})
			} else {
				r.forgetSetUp(environment)
			}
			continue
		}
		// It's not failed, so nothing it read of being set up holds.
		r.forgetSetUp(environment)
		if !has || st.State == store.OwnerWaitingEmail {
			st.State, st.UpdatedAt = store.OwnerWaiting, now
			record = append(record, st)
		}
		server, err := liveServer(ctx, tx, environment, owner.GetService())
		if err != nil {
			return nil, nil, err
		}
		if server == "" {
			continue
		}
		jobs = append(jobs, job{id: res.ID, environment: environment, server: server, checkFirst: st.Tries > 0, tries: st.Tries, since: st.UpdatedAt})
	}
	return jobs, record, nil
}

// changeApplied says whether a change applied.
func changeApplied(ctx context.Context, tx *store.Tx, change string) (bool, error) {
	c, err := tx.GetChange(ctx, change)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return c.State == store.ChangeApplied, nil
}

// liveServer is a server a service runs on now, empty while none does.
func liveServer(ctx context.Context, tx *store.Tx, environment, name string) (string, error) {
	r, err := tx.GetResource(ctx, string(service.Kind), environment, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	deployments, err := tx.LatestDeployments(ctx, []string{r.ID})
	if err != nil {
		return "", err
	}
	for i := range deployments[r.ID] {
		if d := &deployments[r.ID][i]; d.State == store.DeploymentLive {
			return d.Server, nil
		}
	}
	return "", nil
}

// ready reads what makes a job's owner, from its catalog entry at the release it
// runs, and the values its steps name, the secrets they name alone (the review's
// L3). An owner whose steps ran and that waits on its done is read, now and then,
// and its steps never run again. Where the steps can't be read, it's failed, saying
// why.
func (r *Runner) ready(ctx context.Context, j job) (*job, *store.AppOwnerState, error) {
	r.mu.Lock()
	runs := r.runs
	r.mu.Unlock()
	if runs == nil {
		return nil, nil, nil
	}
	if j.rotation != "" {
		return r.readyRotation(ctx, j, runs)
	}
	run, plain, secretNames, err := runs.OwnerRun(ctx, j.environment)
	if err != nil && j.setUp {
		// A read for the page says nothing it can't read.
		return nil, nil, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, &store.AppOwnerState{ResourceID: j.id, Environment: j.environment, State: store.OwnerFailed, Error: cut(err.Error()), Tries: j.tries, UpdatedAt: r.now()}, nil
	}
	j.readOnly = j.tries > 0 && run.GetMadeWhenDone() && !j.setUp
	if j.readOnly && r.now().Sub(j.since) < Poll {
		return nil, nil, nil
	}
	values, err := r.secrets(ctx, j.environment, plain, secretNames)
	if err != nil {
		return nil, nil, err
	}
	j.owner, j.values = run, values
	return &j, nil, nil
}

// secrets adds to plain the values of the secrets the owner's steps name.
func (r *Runner) secrets(ctx context.Context, environment string, plain map[string]string, secretNames []string) (map[string]string, error) {
	values := plain
	if values == nil {
		values = map[string]string{}
	}
	err := r.DB.View(ctx, func(tx *store.Tx) error {
		for _, name := range secretNames {
			res, err := tx.GetResource(ctx, string(variable.SecretKind), environment, name)
			if err != nil {
				return fmt.Errorf("can't read the secret %s the owner's steps name: %w", name, err)
			}
			spec, err := r.Keeper.Decode(ctx, tx, store.InRevision(res.ID, res.Kind, res.Revision), res.Spec)
			if err != nil {
				return err
			}
			s, _ := spec.(*resourcev1.Secret)
			values[name] = s.GetValue()
		}
		return nil
	})
	if err != nil {
		clear(values)
		return nil, err
	}
	return values, nil
}

// start runs a job on its own, once at a time for each owner.
func (r *Runner) start(ctx context.Context, j job) {
	r.mu.Lock()
	if r.running == nil {
		r.running = map[string]bool{}
	}
	if r.running[j.id] {
		r.mu.Unlock()
		clear(j.values)
		return
	}
	r.running[j.id] = true
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.running, j.id)
			r.mu.Unlock()
		}()
		defer clear(j.values)
		if j.rotation != "" {
			r.rotate(ctx, j)
			j.pending = ""
			return
		}
		if j.setUp {
			r.readSetUp(ctx, j)
			return
		}
		st := r.make(ctx, j)
		if err := r.record(context.WithoutCancel(ctx), st); err != nil {
			r.logger().WarnContext(ctx, "can't record what came of making an app's owner", slog.String("error", err.Error()))
			return
		}
		if st.State == store.OwnerMade {
			if err := r.Unset(context.WithoutCancel(ctx), j.environment); err != nil {
				r.logger().WarnContext(ctx, "can't take out what only making an app's owner needed", slog.String("environment", j.environment), slog.String("error", err.Error()))
			}
		}
	}()
}

// readSetUp reads a failed owner's done, for its page, and keeps what it read.
func (r *Runner) readSetUp(ctx context.Context, j job) {
	a, ok := r.agentFor(j.server)
	if !ok {
		return
	}
	got, err := a.ReadAppOwner(ctx, j.environment, j.owner.GetService(), j.owner, j.values, false)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.setUp == nil {
		r.setUp = map[string]bool{}
	}
	r.setUp[j.environment] = got.GetYes()
}

// setupLive says whether an environment has a held route whose setup link works now.
func (r *Runner) setupLive(ctx context.Context, tx *store.Tx, environment string, now time.Time) (bool, error) {
	list, err := tx.ListResources(ctx, environment, string(route.Kind))
	if err != nil {
		return false, err
	}
	for i := range list {
		res := list[i]
		if res.Spec == nil {
			continue
		}
		spec, err := r.Keeper.Decode(ctx, tx, store.InRevision(res.ID, res.Kind, res.Revision), res.Spec)
		if err != nil {
			return false, err
		}
		if rt, ok := spec.(*resourcev1.Route); ok && rt.GetHeld() && now.Before(time.Unix(rt.GetSetup().GetUntil(), 0)) {
			return true, nil
		}
	}
	return false, nil
}

// make asks the job's agent, and says what's observed.
func (r *Runner) make(ctx context.Context, j job) store.AppOwnerState {
	st := store.AppOwnerState{ResourceID: j.id, Environment: j.environment, State: store.OwnerWaiting, Tries: j.tries, UpdatedAt: r.now()}
	a, ok := r.agentFor(j.server)
	if !ok {
		return st
	}
	if j.readOnly {
		got, err := a.ReadAppOwner(ctx, j.environment, j.owner.GetService(), j.owner, j.values, false)
		if err == nil && got.GetYes() {
			st.State = store.OwnerMade
		}
		return st
	}
	st.Tries++
	got, err := a.MakeAppOwner(ctx, j.environment, j.owner.GetService(), j.owner, j.values, j.checkFirst)
	switch {
	case err != nil:
		// What the agent couldn't do is scrubbed here too (the review's L4).
		st.State, st.Error = store.OwnerFailed, ownerstep.Scrub([]byte(err.Error()), j.values, nil, j.owner)
	case got.GetMade():
		st.State = store.OwnerMade
	case got.GetWaiting():
		st.State = store.OwnerWaiting
	default:
		st.State = store.OwnerFailed
		st.Error = ownerstep.Scrub([]byte(fmt.Sprintf("Step %d: %s", got.GetStep(), got.GetSaid())), j.values, nil, j.owner)
	}
	return st
}

// cut keeps what a failure says to what's ever kept of it.
func cut(s string) string {
	if len(s) > 512 {
		s = strings.ToValidUTF8(s[:512], "")
	}
	return s
}

func (r *Runner) record(ctx context.Context, states ...store.AppOwnerState) error {
	if len(states) == 0 {
		return nil
	}
	return r.DB.Update(ctx, func(tx *store.Tx) error {
		for _, st := range states {
			before, err := tx.AppOwnerStates(ctx, []string{st.ResourceID})
			if err != nil {
				return err
			}
			wrote, err := tx.SetAppOwnerState(ctx, st)
			if err != nil {
				return err
			}
			// Activity says what came of it, once, as it turns made, failed or opened.
			if !wrote || before[st.ResourceID].State == st.State {
				continue
			}
			switch st.State {
			case store.OwnerMade:
				err = r.recordEvent(ctx, tx, ActionMade, st)
			case store.OwnerFailed:
				err = r.recordEvent(ctx, tx, ActionFailed, st)
			case store.OwnerOpened:
				err = r.recordEvent(ctx, tx, ActionOpened, st)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// What becomes of making an app's owner, on Activity (ADR 0105).
const (
	ActionMade   = "app_owner.made"
	ActionFailed = "app_owner.failed"
	ActionOpened = "app_owner.opened"
)

// Actions are the events the runner records.
var Actions = []string{ActionMade, ActionFailed, ActionOpened}

// Told is what an event says of an owner, in its detail: who, and how it stands,
// with what the app said scrubbed, and never a value its steps sent.
type Told struct {
	App   string `json:"app,omitempty"`
	Email string `json:"email,omitempty"`
	User  string `json:"user,omitempty"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	Tries int    `json:"tries,omitempty"`
}

// ReadTold reads what an event's detail says of an owner.
func ReadTold(detail string) (Told, bool) {
	var t Told
	err := json.Unmarshal([]byte(detail), &t)
	return t, err == nil && t.State != ""
}

// recordEvent puts what became of an owner on Activity, as Dockwell's own.
func (r *Runner) recordEvent(ctx context.Context, tx *store.Tx, action string, st store.AppOwnerState) error {
	told := Told{State: st.State, Error: st.Error, Tries: st.Tries}
	list, err := tx.ListResources(ctx, st.Environment, string(appowner.Kind))
	if err != nil {
		return err
	}
	for i := range list {
		if res := list[i]; res.ID == st.ResourceID && res.Spec != nil {
			spec, err := r.Keeper.Decode(ctx, tx, store.InRevision(res.ID, res.Kind, res.Revision), res.Spec)
			if err != nil {
				return err
			}
			if o, ok := spec.(*resourcev1.AppOwner); ok {
				told.App, told.Email, told.User = o.GetApp(), o.GetEmail(), o.GetUser()
			}
		}
	}
	detail, err := json.Marshal(told)
	if err != nil {
		return err
	}
	return tx.RecordEvent(ctx, store.Event{
		ID: id.New(id.Event), At: r.now(), Actor: revision.SystemActor, Action: action,
		Subject: st.ResourceID, Target: st.Environment, Detail: string(detail), Surface: store.SurfaceAutomatic,
	})
}

// releasable says whether an environment isn't held, and still has a route or a port
// its hold kept, where its owner is made or opened, or a person opened its install,
// as its pin says, so a route planned while it was held and applied after is
// released too (L10). A hold with neither, as one ADR 0084 started before ADR 0105,
// is a person's to open.
func (r *Runner) releasable(ctx context.Context, tx *store.Tx, environment string) (bool, error) {
	list, err := tx.ListResources(ctx, environment, string(appowner.Kind))
	if err != nil {
		return false, err
	}
	if len(list) == 0 {
		opened, err := r.Holds.Opened(ctx, tx, environment)
		if err != nil || !opened {
			return false, err
		}
		held, err := r.Holds.Held(ctx, tx, environment)
		if err != nil || held {
			return false, err
		}
		edits, err := r.releases(ctx, tx, environment)
		return len(edits) > 0, err
	}
	ids := make([]string, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	seen, err := tx.AppOwnerStates(ctx, ids)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if !store.OwnerFinal(seen[id].State) {
			return false, nil
		}
	}
	held, err := r.Holds.Held(ctx, tx, environment)
	if err != nil || held {
		return false, err
	}
	edits, err := r.releases(ctx, tx, environment)
	return len(edits) > 0, err
}

// releases are the edits that open what an environment's hold kept.
func (r *Runner) releases(ctx context.Context, tx *store.Tx, environment string) ([]revision.Edit, error) {
	keeper := r.Keeper
	var out []revision.Edit
	for _, kind := range []resource.Kind{route.Kind, service.Kind} {
		list, err := tx.ListResources(ctx, environment, string(kind))
		if err != nil {
			return nil, err
		}
		for i := range list {
			res := list[i]
			if res.Spec == nil {
				continue
			}
			spec, err := keeper.Decode(ctx, tx, store.InRevision(res.ID, res.Kind, res.Revision), res.Spec)
			if err != nil {
				return nil, err
			}
			var next proto.Message
			switch v := spec.(type) {
			case *resourcev1.Route:
				if v.GetHeld() {
					v.Held = false
					next = v
				}
			case *resourcev1.Service:
				opened := false
				for _, p := range v.GetApp().GetPorts() {
					opened = opened || p.GetHeld()
					p.Held = false
				}
				if opened {
					next = v
				}
			}
			if next != nil {
				out = append(out, revision.Edit{Ref: resource.Ref{Kind: kind, Scope: environment, Name: res.Name}, Spec: next, Base: res.Revision})
			}
		}
	}
	// A route to one of its ports on the server, held as it published there, from
	// any environment (the re-check's N1).
	stale, err := r.Holds.HeldHostPortRoutes(ctx, tx, environment)
	if err != nil {
		return nil, err
	}
	for _, h := range stale {
		if slices.ContainsFunc(out, func(ed revision.Edit) bool { return ed.Ref == h.Ref }) {
			continue
		}
		spec := proto.CloneOf(h.Spec)
		spec.Held = false
		out = append(out, revision.Edit{Ref: h.Ref, Spec: spec, Base: h.Revision})
	}
	return out, nil
}

// Unset takes out of an environment's owner's service what its entry says only
// making the owner needed, once it's made, as a change of Dockwell's applied at
// once: Vaultwarden's ADMIN_TOKEN, which turns its admin page off (ADR 0105). It
// changes nothing where the service sets none of it.
func (r *Runner) Unset(ctx context.Context, environment string) error {
	r.mu.Lock()
	runs := r.runs
	r.mu.Unlock()
	u, ok := runs.(Unsets)
	if !ok {
		return nil
	}
	name, names, err := u.OwnerUnset(ctx, environment)
	if err != nil || len(names) == 0 {
		return err
	}
	var edit *revision.Edit
	var project string
	err = r.DB.View(ctx, func(tx *store.Tx) error {
		res, err := tx.GetResource(ctx, string(service.Kind), environment, name)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil || res.Spec == nil {
			return err
		}
		spec, err := r.Keeper.Decode(ctx, tx, store.InRevision(res.ID, res.Kind, res.Revision), res.Spec)
		if err != nil {
			return err
		}
		svc, ok := spec.(*resourcev1.Service)
		if !ok {
			return nil
		}
		changed := false
		for _, n := range names {
			for _, m := range []map[string]string{svc.GetApp().GetEnv(), svc.GetVariables(), svc.GetSecrets()} {
				if _, ok := m[n]; ok {
					delete(m, n)
					changed = true
				}
			}
		}
		if changed {
			edit = &revision.Edit{Ref: resource.Ref{Kind: service.Kind, Scope: environment, Name: name}, Spec: svc, Base: res.Revision}
		}
		project, err = projectOf(ctx, tx, environment)
		return err
	})
	if err != nil || edit == nil {
		return err
	}
	by := revision.By{Actor: revision.SystemActor, Via: revision.ViaAutomatic}
	plan, err := r.Engine.Propose(ctx, revision.Proposal{
		Summary: "Take " + strings.Join(names, ", ") + " out of " + project + ": its owner exists", Actor: by.Actor, Via: by.Via,
		Edits: []revision.Edit{*edit},
		Notes: []revision.Note{{Reason: NoteOwnerUnset, Message: project + "'s owner exists, so " + strings.Join(names, ", ") + " goes from " + name + ", as only making the owner needed it", Names: names}},
	})
	if err != nil {
		return err
	}
	if err := r.Engine.Apply(ctx, plan.Change); err != nil {
		_ = r.Engine.Discard(context.WithoutCancel(ctx), plan.Change, by)
		return err
	}
	return nil
}

// NoteOwnerUnset is the note of the change that takes out what only making an
// owner needed.
const NoteOwnerUnset = "owner_unset"

// Release opens what an environment's hold kept, now that its owner is made or
// opened, or a person opened its install, as a change of Dockwell's, applied at once.
func (r *Runner) Release(ctx context.Context, environment string) error {
	var edits []revision.Edit
	var project string
	var owned bool
	err := r.DB.View(ctx, func(tx *store.Tx) error {
		held, err := r.Holds.Held(ctx, tx, environment)
		if err != nil || held {
			return err
		}
		list, err := tx.ListResources(ctx, environment, string(appowner.Kind))
		if err != nil {
			return err
		}
		owned = len(list) > 0
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			for _, e := range p.Environments {
				if e.ID == environment {
					project = p.Name
				}
			}
		}
		edits, err = r.releases(ctx, tx, environment)
		return err
	})
	if err != nil || len(edits) == 0 {
		return err
	}
	by := revision.By{Actor: revision.SystemActor, Via: revision.ViaAutomatic}
	summary, note := "Open "+project+": its owner exists", revision.Note{Reason: "owner_made", Message: project + "'s owner exists, so what its hold kept answers everyone again"}
	notes := []revision.Note{note}
	if !owned {
		summary, note = "Open "+project+": a person opened it", revision.Note{Reason: "install_opened", Message: "A person opened " + project + ", so what its hold kept after that answers everyone too"}
	}
	plan, err := r.Engine.Propose(ctx, revision.Proposal{
		Summary: summary, Actor: by.Actor, Via: by.Via, Edits: edits,
		Notes: notes,
	})
	if err != nil {
		return err
	}
	if err := r.Engine.Apply(ctx, plan.Change); err != nil {
		_ = r.Engine.Discard(context.WithoutCancel(ctx), plan.Change, by)
		return err
	}
	return nil
}

// Retry has a failed owner tried again at the next look, reading whether it's made
// first, and says what's observed now.
func (r *Runner) Retry(ctx context.Context, environment string) (store.AppOwnerState, error) {
	// A retry starts over: what was read of it being set up by hand goes.
	r.forgetSetUp(environment)
	var out store.AppOwnerState
	err := r.DB.Update(ctx, func(tx *store.Tx) error {
		list, err := tx.ListResources(ctx, environment, string(appowner.Kind))
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return fmt.Errorf("%w: Dockwell doesn't make this install's owner", store.ErrNotFound)
		}
		seen, err := tx.AppOwnerStates(ctx, []string{list[0].ID})
		if err != nil {
			return err
		}
		st, ok := seen[list[0].ID]
		if !ok {
			st = store.AppOwnerState{ResourceID: list[0].ID, Environment: environment, State: store.OwnerWaiting}
		}
		if st.State == store.OwnerFailed {
			st.State, st.Error, st.UpdatedAt = store.OwnerWaiting, "", r.now()
			if st.Tries == 0 {
				st.Tries = 1
			}
			if _, err := tx.SetAppOwnerState(ctx, st); err != nil {
				return err
			}
		}
		out = st
		return nil
	})
	return out, err
}
