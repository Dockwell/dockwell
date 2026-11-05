// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// rolling is an engine on a server whose agent runs real drivers, on a fake
// Docker Engine: what a real check failing does, from the change to what runs.
type rolling struct {
	engine  *Engine
	db      *store.DB
	docker  *dockertest.Engine
	runtime *container.Docker
	agents  *switchable
	env     string
	project string
	ref     resource.Ref
}

// switchable is the server's agent, which can go away.
type switchable struct {
	mu     sync.Mutex
	agent  resource.Agent
	online bool
}

func (s *switchable) Agent(id string) (resource.Agent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "srv_A" {
		return nil, false
	}
	return s.agent, s.online
}

func (s *switchable) set(online bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.online = online
}

func setupRolling(t *testing.T, b store.Backend) *rolling {
	t.Helper()
	db, _ := storetest.Open(t, b)
	if _, err := Setup(t.Context(), db, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.CreateServer(t.Context(), store.Server{ID: "srv_A", Name: "hz-1", CreatedAt: now, CertSerial: "A", CertNotAfter: now})
	}); err != nil {
		t.Fatal(err)
	}
	e := dockertest.New(t)
	e.Registry["good:1"] = dockertest.Image{Config: docker.Config{Cmd: []string{"app"}}}
	e.Registry["good:2"] = dockertest.Image{Config: docker.Config{Cmd: []string{"app"}}}
	e.Registry["good:3"] = dockertest.Image{Config: docker.Config{Cmd: []string{"app"}}}
	e.Registry["sick:1"] = dockertest.Image{Unhealthy: true, CheckSays: "curl: (22) The requested URL returned error: 503", Config: docker.Config{
		Cmd: []string{"app"}, Healthcheck: &docker.Healthcheck{Test: []string{"CMD", "curl", "-f", "http://localhost/healthz"}},
	}}
	e.Registry["crash:1"] = dockertest.Image{Exits: true, Config: docker.Config{Cmd: []string{"false"}}}
	e.Registry["db:1"] = dockertest.Image{Volumes: []string{"/data"}, Config: docker.Config{Cmd: []string{"db"}}}
	e.Registry["db:2"] = dockertest.Image{Volumes: []string{"/data"}, Config: docker.Config{Cmd: []string{"db"}}}
	e.Registry["dbsick:1"] = dockertest.Image{Exits: true, Volumes: []string{"/data"}, Config: docker.Config{Cmd: []string{"db"}}}
	client, kept := e.Client(), &volume.Kept{}
	rt := &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: time.Second, Deleted: kept}
	containers := container.Driver{Runtime: rt}
	kinds := resource.NewRegistry(&container.Driver{}, &service.Driver{}, &volume.Driver{}, &network.Driver{}, variable.Type{}, variable.SecretType{})
	keeper := secretstest.Keeper(db, kinds)
	agents := &switchable{online: true, agent: resource.Local{Drivers: resource.NewRegistry(&containers, &service.Driver{Containers: containers},
		&volume.Driver{Runtime: &volume.Docker{Client: client, Kept: kept}}, &network.Driver{Client: client}), Server: "srv_A", Values: keeper}}
	engine := New(db, kinds, agents, keeper, slog.New(slog.DiscardHandler))
	prod := production(t, db)
	return &rolling{
		engine: engine, db: db, docker: e, runtime: rt, agents: agents, env: prod, project: docker.Project(prod),
		ref: resource.Ref{Kind: service.Kind, Scope: prod, Name: "web"},
	}
}

func (r *rolling) edit(image string) Edit {
	c := &resourcev1.Container{Image: image}
	return Edit{Ref: r.ref, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: c}}}
}

func (r *rolling) instances() []string { return r.docker.Instances(r.project, "web") }

func (r *rolling) newest() *docker.Container { return r.docker.Newest(r.project, "web") }

// revisionsOf is the images of a service's revisions, in order.
func (r *rolling) revisions(t *testing.T) []string {
	t.Helper()
	var out []string
	err := r.db.View(t.Context(), func(tx *store.Tx) error {
		res, err := tx.GetResource(t.Context(), string(service.Kind), r.env, "web")
		if err != nil {
			return err
		}
		for n := 1; n <= res.Revision; n++ {
			rev, err := tx.GetRevision(t.Context(), res.ID, n)
			if err != nil {
				return err
			}
			spec, err := r.engine.decode(t.Context(), tx, store.SpecAt{Kind: string(service.Kind), ResourceID: res.ID, Revision: n}, rev.Spec)
			if err != nil {
				return err
			}
			out = append(out, spec.(*resourcev1.Service).GetApp().GetImage())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (r *rolling) deployments(t *testing.T) map[int]string {
	t.Helper()
	out := map[int]string{}
	err := r.db.View(t.Context(), func(tx *store.Tx) error {
		res, err := tx.GetResource(t.Context(), string(service.Kind), r.env, "web")
		if err != nil {
			return err
		}
		for n := 1; n <= res.Revision; n++ {
			deps, err := tx.Deployments(t.Context(), res.ID, n)
			if err != nil {
				return err
			}
			for i := range deps {
				out[n] = deps[i].State
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// changes is how many changes there are, of every state.
func (r *rolling) changes(t *testing.T) int {
	t.Helper()
	records, err := r.engine.Changes(t.Context(), store.ChangePage{}, 200)
	if err != nil {
		t.Fatal(err)
	}
	return len(records)
}

// A real failing check rolls a deploy back, on its own: the deploy stays failed
// on record, with what the check said, a new revision puts the old spec back, and
// what ran never stopped.
func TestAFailingCheckRollsADeployBack(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		apply(t, r.engine, propose(t, r.engine, "Run web", r.edit("good:1")))
		running := r.newest().ID

		p := propose(t, r.engine, "Deploy the sick one", r.edit("sick:1"))
		err := r.engine.Apply(t.Context(), p.Change)
		var ferr *FailedError
		if !errors.As(err, &ferr) || ferr.Rollback == "" || ferr.RollbackErr != nil {
			t.Fatalf("applying = %v, want it failed and rolled back", err)
		}
		failed := stored(t, r.db, p.Change)
		if failed.State != store.ChangeFailed {
			t.Errorf("the deploy is %s", failed.State)
		}
		// In plain words, which send nobody to a container the rollback takes away.
		// What the check said is in the rollback's reason, below.
		if !strings.Contains(failed.Error, "fails its health check") || strings.Contains(failed.Error, "docker inspect") {
			t.Errorf("the deploy's error is %s", failed.Error)
		}
		back, err := r.engine.Change(t.Context(), ferr.Rollback)
		if err != nil {
			t.Fatal(err)
		}
		if back.State != store.ChangeApplied || !IsRollback(back.Change) || back.Reverts != p.Change {
			t.Errorf("the rollback is %+v", back.Change)
		}
		if !strings.Contains(back.Plan.Because, "The requested URL returned error: 503") || back.Brief.Because != back.Plan.Because {
			t.Errorf("the rollback says it is because %q, in brief %q", back.Plan.Because, back.Brief.Because)
		}
		list, err := r.engine.Changes(t.Context(), store.ChangePage{}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(list, func(c *Record) bool { return c.ID == back.ID && strings.Contains(c.Brief.Because, "503") }) {
			t.Error("a list of changes doesn't say why the rollback happened")
		}

		// History as it happened: the failed revision stays, and the rollback's
		// comes after it.
		if got := r.revisions(t); !slices.Equal(got, []string{"good:1", "sick:1", "good:1"}) {
			t.Errorf("the revisions are %v", got)
		}
		if got := r.deployments(t); got[2] != store.DeploymentFailed || got[3] != store.DeploymentLive {
			t.Errorf("the deployments are %v", got)
		}
		// What ran is what runs, on its own, and it never stopped.
		if got := r.instances(); len(got) != 1 {
			t.Errorf("Docker has %v", got)
		}
		if c := r.newest(); c.ID != running || !c.State.Running {
			t.Errorf("what runs is %+v, want what ran before the deploy", c.State)
		}
		// Its step says so too: nothing stopped, and the data stays.
		step := back.Plan.step("srv_A", r.ref)
		if step == nil || step.Downtime != 0 || step.Rollout == nil || !step.Rollout.Rolling {
			t.Errorf("the rollback's step = %+v", step)
		}
	})
}

// A deploy whose rollout can't stop first and fails is rolled back the same way,
// with the data its image keeps where it was.
func TestAStopFirstDeployRollsBackWithItsData(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		apply(t, r.engine, propose(t, r.engine, "Run db", r.edit("db:1")))
		data := r.newest().Mounts[0].Name

		p := propose(t, r.engine, "Deploy a broken db", r.edit("dbsick:1"))
		if step := p.step("srv_A", r.ref); step == nil || step.Downtime == 0 || step.Rollout.Rolling {
			t.Errorf("its step = %+v", step)
		}
		if !saysDataStays(p, "/data") {
			t.Errorf("its plan's notes are %+v", p.Notes)
		}
		err := r.engine.Apply(t.Context(), p.Change)
		var ferr *FailedError
		if !errors.As(err, &ferr) || ferr.Rollback == "" || ferr.RollbackErr != nil {
			t.Fatalf("applying = %v", err)
		}
		c := r.newest()
		if c.Config.Image != "db:1" || !c.State.Running || c.Mounts[0].Name != data {
			t.Errorf("after rolling back it runs %s on %s, running %t; want db:1 on %s", c.Config.Image, c.Mounts[0].Name, c.State.Running, data)
		}
		back, err := r.engine.Change(t.Context(), ferr.Rollback)
		if err != nil {
			t.Fatal(err)
		}
		if !saysDataStays(back.Plan, "/data") {
			t.Errorf("the rollback's plan doesn't say the data stays: %+v", back.Plan.Notes)
		}
	})
}

// A rollback that fails too is on record, where a person sees it, with what each
// said. Nothing tries it again on its own, however often Resume runs, and what
// the service runs next is what a person says.
func TestARollbackThatFailsIsVisibleAndNeverLoops(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		apply(t, r.engine, propose(t, r.engine, "Run db", r.edit("db:1")))
		// What it was can't run any more either, as when a registry or a volume
		// went bad: the image now exits as soon as it starts.
		r.docker.SetExits("db:1", true)

		p := propose(t, r.engine, "Deploy a broken db", r.edit("dbsick:1"))
		// A loop wouldn't return at all, so it has a time to, and says so.
		applied := make(chan error, 1)
		go func() { applied <- r.engine.Apply(t.Context(), p.Change) }()
		var err error
		select {
		case err = <-applied:
		case <-time.After(30 * time.Second):
			t.Fatalf("a rollback that failed hasn't ended after 30 s, with %d changes so far: it loops", r.changes(t))
		}
		var ferr *FailedError
		if !errors.As(err, &ferr) || ferr.Rollback == "" || ferr.RollbackErr == nil {
			t.Fatalf("applying = %v, want the rollback to have failed too", err)
		}
		if msg := ferr.Error(); !strings.Contains(msg, "Rolling back failed too") || !strings.Contains(msg, "Check the servers") {
			t.Errorf("it says %q", msg)
		}
		if got := stored(t, r.db, p.Change); got.State != store.ChangeFailed {
			t.Errorf("the deploy is %s", got.State)
		}
		back, err := r.engine.Change(t.Context(), ferr.Rollback)
		if err != nil {
			t.Fatal(err)
		}
		if back.State != store.ChangeFailed || !IsRollback(back.Change) || !strings.Contains(back.Error, "exit code 1") {
			t.Errorf("the rollback reads %s, %q", back.State, back.Error)
		}
		if got := r.deployments(t); got[3] != store.DeploymentFailed {
			t.Errorf("the rollback's deployment is %q", got[3])
		}

		// No loop: nothing more comes of it, however often anyone resumes.
		count := r.changes(t)
		before := r.docker.Seen()
		for range 5 {
			if err := r.engine.Resume(t.Context()); err != nil {
				t.Fatalf("Resume = %v", err)
			}
			r.engine.Wait()
		}
		if got := r.changes(t); got != count {
			t.Errorf("Resume made %d more changes", got-count)
		}
		for _, req := range r.docker.Seen()[len(before):] {
			if !strings.HasPrefix(req, "GET") {
				t.Errorf("Resume did %s, with nothing left applying", req)
			}
		}

		// A person acts: the next change takes it from whatever is there.
		fixed := propose(t, r.engine, "Deploy one that works", r.edit("db:2"))
		apply(t, r.engine, fixed)
		if got := stored(t, r.db, fixed.Change); got.State != store.ChangeApplied {
			t.Errorf("the fix is %s: %s", got.State, got.Error)
		}
		if got := r.instances(); len(got) != 1 {
			t.Errorf("after the fix, Docker has %v", got)
		}
		if c := r.newest(); c.Config.Image != "db:2" || !c.State.Running {
			t.Errorf("after the fix it runs %s, running %t", c.Config.Image, c.State.Running)
		}
	})
}

// A restart is a change with a plan, and no revision: the spec is as it was, so
// there is nothing to undo, and a new container of it took over.
func TestARestartIsAChangeWithNoRevision(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		apply(t, r.engine, propose(t, r.engine, "Run web", r.edit("good:1")))
		before := r.newest().ID

		p, err := r.engine.Restart(t.Context(), r.ref, By{Actor: "user:usr_mira", Via: ViaPanel}, "")
		if err != nil {
			t.Fatal(err)
		}
		if p.Summary != "Restart web" || p.Revisions != 0 || len(p.Steps()) != 1 {
			t.Fatalf("its plan = %+v", p)
		}
		step := p.Steps()[0]
		if step.Action != resource.ActionReplace || step.Effect != resource.EffectRoll || step.Downtime != 0 || step.Restart != p.Change || len(step.Changes) != 0 {
			t.Errorf("its step = %+v", step)
		}
		if p.Counts().Replace != 1 {
			t.Errorf("its counts are %+v", p.Counts())
		}
		apply(t, r.engine, p)
		c := stored(t, r.db, p.Change)
		if c.State != store.ChangeApplied {
			t.Fatalf("the restart is %s: %s", c.State, c.Error)
		}
		if got := steps(t, c); len(got) != 1 || got[0] != StepDone {
			t.Errorf("its steps are %v", got)
		}
		progress, _ := decodeProgress(c.Progress)
		if progress.Steps[0].Message != "Restarted service web on hz-1" {
			t.Errorf("its step says %q", progress.Steps[0].Message)
		}
		if n := r.newest(); n.ID == before || !n.State.Running || n.Config.Labels[container.LabelRestart] != p.Change {
			t.Errorf("after the restart it is %+v, %v", n.State, n.Config.Labels)
		}
		if got := r.instances(); len(got) != 1 {
			t.Errorf("Docker has %v", got)
		}
		if got := r.revisions(t); !slices.Equal(got, []string{"good:1"}) {
			t.Errorf("the revisions are %v, want it to have made none", got)
		}
		rec, err := r.engine.Change(t.Context(), p.Change)
		if err != nil || !rec.Brief.Restart {
			t.Errorf("the change reads %+v, %v", rec, err)
		}
		if _, err := r.engine.Undo(t.Context(), p.Change, By{Actor: "user:usr_mira", Via: ViaPanel}); !errors.Is(err, ErrCantUndo) {
			t.Errorf("undoing a restart = %v, want it refused, as it has nothing to put back", err)
		}
		// Planned again, it is another restart, of its own.
		again, err := r.engine.Restart(t.Context(), r.ref, By{Actor: "user:usr_mira", Via: ViaPanel}, "Again")
		if err != nil || len(again.Steps()) != 1 || again.Summary != "Again" {
			t.Errorf("restarting again = %+v, %v", again, err)
		}
	})
}

// A restart that fails is a failed change. There is nothing to roll back, the
// revision's deployment stays as it was, and what ran goes on.
func TestARestartThatFailsChangesNothing(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		apply(t, r.engine, propose(t, r.engine, "Run web", r.edit("good:1")))
		ran := r.newest().ID
		r.docker.SetExits("good:1", true)
		p, err := r.engine.Restart(t.Context(), r.ref, By{Actor: "user:usr_mira", Via: ViaPanel}, "")
		if err != nil {
			t.Fatal(err)
		}
		err = r.engine.Apply(t.Context(), p.Change)
		var ferr *FailedError
		if !errors.As(err, &ferr) || ferr.Rollback != "" {
			t.Fatalf("applying = %v, want it failed, with no rollback", err)
		}
		if c := stored(t, r.db, p.Change); c.State != store.ChangeFailed || !strings.Contains(c.Error, "exit code 1") {
			t.Errorf("the restart reads %s, %q", c.State, c.Error)
		}
		if got := r.deployments(t); got[1] != store.DeploymentLive {
			t.Errorf("the revision's deployment is %q", got[1])
		}
		if got := r.instances(); len(got) != 1 || r.newest().ID != ran || !r.newest().State.Running {
			t.Errorf("what ran is %v, %+v", got, r.newest().State)
		}
	})
}

func TestAKindThatCantRestartIsRefused(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		_, err := r.engine.Restart(t.Context(), resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}, By{Actor: "user:usr_mira", Via: ViaPanel}, "")
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("restarting a container on its own = %v", err)
		}
		_, err = r.engine.Restart(t.Context(), r.ref, By{Actor: "user:usr_mira", Via: ViaPanel}, "")
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("restarting a service that isn't there = %v", err)
		}
	})
}

// stopAt stops the control plane at a step of a rollout, as cancelling the step
// under way once its grace ran out does.
func (r *rolling) stopAt(at string) {
	var once sync.Once
	r.runtime.Points = func(name string) {
		if name != at {
			return
		}
		once.Do(func() {
			gone, cancel := context.WithCancel(context.Background())
			cancel()
			go func() { _ = r.engine.Stop(gone) }()
			<-r.engine.halted.Done()
		})
	}
}

// replacement is one way a step replaces a service, with what makes it go that way.
type replacement struct {
	name     string
	points   []string
	from, to string
}

// eachStep goes through every point of both ways a service is replaced.
func eachStep() iter.Seq2[replacement, string] {
	return func(yield func(replacement, string) bool) {
		for _, way := range []replacement{
			{"rolling", container.Rolling, "good:1", "good:2"},
			{"stopping first", container.StopFirst, "db:1", "db:2"},
		} {
			for _, at := range way.points {
				if !yield(way, at) {
					return
				}
			}
		}
	}
}

// The control plane stopping at each step of a rollout leaves the change applying,
// neither failed nor rolled back. The next start finishes it, forward: one
// instance of the new spec.
func TestAControlPlaneStoppedAtEachStepFinishesTheRollout(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		for way, at := range eachStep() {
			t.Run(way.name+"/"+at, func(t *testing.T) {
				r := setupRolling(t, b)
				apply(t, r.engine, propose(t, r.engine, "Run web", r.edit(way.from)))
				p := propose(t, r.engine, "Deploy", r.edit(way.to))
				r.stopAt(at)
				if err := r.engine.Start(t.Context(), p.Change, By{Actor: "user:usr_mira", Via: ViaPanel}); err != nil {
					t.Fatal(err)
				}
				r.engine.Wait()
				r.runtime.Points = nil
				c := stored(t, r.db, p.Change)
				finished := at == way.points[len(way.points)-1]
				switch {
				case c.State == store.ChangeApplied && finished:
					return // nothing was left for the stop to cut off
				case c.State != store.ChangeApplying:
					t.Fatalf("stopped at %s, the change is %s: %s", at, c.State, c.Error)
				}
				if n := len(reverting(t, r.db, p.Change)); n != 0 {
					t.Fatalf("stopped at %s, it has %d rollbacks", at, n)
				}
				next := New(r.db, r.engine.Kinds(), r.agents, secretstest.Keeper(r.db, r.engine.Kinds()), slog.New(slog.DiscardHandler))
				if err := next.Resume(t.Context()); err != nil {
					t.Fatalf("Resume = %v", err)
				}
				next.Wait()
				c = stored(t, r.db, p.Change)
				if c.State != store.ChangeApplied {
					t.Fatalf("after the next start the change is %s: %s", c.State, c.Error)
				}
				if got := r.instances(); len(got) != 1 {
					t.Errorf("Docker has %v", got)
				}
				if n := r.newest(); n.Config.Image != way.to || !n.State.Running {
					t.Errorf("it runs %s, running %t", n.Config.Image, n.State.Running)
				}
				if got := r.revisions(t); !slices.Equal(got, []string{way.from, way.to}) {
					t.Errorf("the revisions are %v", got)
				}
			})
		}
	})
}

// lost is the agent's stream dropping as a step runs: the step's context ends,
// and the call fails as a stream's does.
type lost struct {
	resource.Agent
	cancel func()
}

func (l *lost) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	l.cancel = cancel
	err := l.Agent.Apply(ctx, step, desired)
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("%w: the stream ended", resource.ErrLost)
	}
	return err
}

// The agent's stream dropping at each step fails the change, as it may or may
// not have run, and its rollback commits at once and waits for the server. When
// the agent is back, it finishes: one instance of the old spec, and no orphan.
func TestAnAgentLostAtEachStepRollsBackWhenItReturns(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		for way, at := range eachStep() {
			t.Run(way.name+"/"+at, func(t *testing.T) {
				r := setupRolling(t, b)
				apply(t, r.engine, propose(t, r.engine, "Run web", r.edit(way.from)))
				p := propose(t, r.engine, "Deploy", r.edit(way.to))
				plain := r.agents.agent
				l := &lost{Agent: plain}
				r.agents.agent = l
				r.runtime.Points = func(name string) {
					if name == at {
						l.cancel()
						r.agents.set(false)
					}
				}
				err := r.engine.Apply(t.Context(), p.Change)
				r.runtime.Points = nil
				r.agents.agent = plain
				if at == way.points[len(way.points)-1] && err == nil {
					return // it ran to its end before the stream dropped
				}
				var ferr *FailedError
				if !errors.As(err, &ferr) || ferr.Rollback == "" || !errors.Is(ferr.RollbackErr, ErrPending) {
					t.Fatalf("lost at %s, applying = %v, want a rollback waiting for the server", at, err)
				}
				if c := stored(t, r.db, ferr.Rollback); c.State != store.ChangeApplying {
					t.Fatalf("the rollback is %s", c.State)
				}
				r.agents.set(true)
				if err := r.engine.Resume(t.Context()); err != nil {
					t.Fatalf("Resume = %v", err)
				}
				r.engine.Wait()
				if c := stored(t, r.db, ferr.Rollback); c.State != store.ChangeApplied {
					t.Fatalf("after the agent came back, the rollback is %s: %s", c.State, c.Error)
				}
				if got := r.instances(); len(got) != 1 {
					t.Errorf("Docker has %v", got)
				}
				if n := r.newest(); n.Config.Image != way.from || !n.State.Running {
					t.Errorf("it runs %s, running %t", n.Config.Image, n.State.Running)
				}
				if got := r.revisions(t); !slices.Equal(got, []string{way.from, way.to, way.from}) {
					t.Errorf("the revisions are %v", got)
				}
			})
		}
	})
}

// A restart the control plane's stop cuts off finishes on the next start, once.
func TestAStoppedRestartFinishesOnce(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		for way, at := range eachStep() {
			if at == container.PointForgotten {
				continue // the step has ended, so nothing is left for the next start
			}
			t.Run(way.name+"/"+at, func(t *testing.T) {
				r := setupRolling(t, b)
				apply(t, r.engine, propose(t, r.engine, "Run web", r.edit(way.from)))
				p, err := r.engine.Restart(t.Context(), r.ref, By{Actor: "user:usr_mira", Via: ViaPanel}, "")
				if err != nil {
					t.Fatal(err)
				}
				r.stopAt(at)
				if err := r.engine.Start(t.Context(), p.Change, By{Actor: "user:usr_mira", Via: ViaPanel}); err != nil {
					t.Fatal(err)
				}
				r.engine.Wait()
				r.runtime.Points = nil
				if c := stored(t, r.db, p.Change); c.State != store.ChangeApplying {
					t.Fatalf("stopped at %s, the restart is %s: %s", at, c.State, c.Error)
				}
				next := New(r.db, r.engine.Kinds(), r.agents, secretstest.Keeper(r.db, r.engine.Kinds()), slog.New(slog.DiscardHandler))
				if err := next.Resume(t.Context()); err != nil {
					t.Fatalf("Resume = %v", err)
				}
				next.Wait()
				if c := stored(t, r.db, p.Change); c.State != store.ChangeApplied {
					t.Fatalf("after the next start the restart is %s: %s", c.State, c.Error)
				}
				if got := r.instances(); len(got) != 1 {
					t.Errorf("Docker has %v", got)
				}
				if n := r.newest(); n.Config.Labels[container.LabelRestart] != p.Change || !n.State.Running {
					t.Errorf("it is %+v, %v", n.State, n.Config.Labels)
				}
				if got := r.revisions(t); !slices.Equal(got, []string{way.from}) {
					t.Errorf("the revisions are %v", got)
				}
				// Once more, the same change has nothing left to do.
				if p2, err := next.plan(t.Context(), p.Change, mustCurrent(t, next, p.Change), false); err != nil || len(p2.Steps()) != 0 {
					t.Errorf("planned again it has %+v, %v, want no steps", p2, err)
				}
			})
		}
	})
}

// Rolling back a rotation gives the secret a revision of its own with the
// old value (ADR 0022), and a service's spec pins that revision, so what ran
// doesn't read as what the rollback asks for, though it holds the same value.
// So it doesn't keep what ran: a new container takes its place, with the old
// value, and what ran serves until that one is up.
func TestRollingBackARotationMakesANewContainerWithTheOldValue(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		uses := func(image string) Edit {
			e := r.edit(image)
			e.Spec.(*resourcev1.Service).Secrets = map[string]string{"DB_PASSWORD": ""}
			return e
		}
		apply(t, r.engine, propose(t, r.engine, "Set up", secretEdit(r.env, "DB_PASSWORD", "the-old-value"), uses("good:1")))
		ran := r.newest()
		if ran == nil || !slices.Contains(ran.Config.Env, "DB_PASSWORD=the-old-value") {
			t.Fatalf("what ran = %+v, want the old value", ran)
		}

		p := propose(t, r.engine, "Rotate", secretEdit(r.env, "DB_PASSWORD", "the-new-value"), uses("crash:1"))
		var failed *FailedError
		if err := r.engine.Apply(t.Context(), p.Change); !errors.As(err, &failed) || failed.Rollback == "" {
			t.Fatalf("apply = %v, want it to fail and roll back", err)
		}

		now := r.newest()
		if now == nil || !now.State.Running {
			t.Fatalf("after the rollback, %+v, want it running", now)
		}
		if !slices.Contains(now.Config.Env, "DB_PASSWORD=the-old-value") {
			t.Errorf("after the rollback, the environment is %v, want the old value", now.Config.Env)
		}
		if now.ID == ran.ID {
			t.Errorf("the rollback kept what ran, %s, want a new container", ran.ID)
		}
		if got := r.instances(); len(got) != 1 {
			t.Errorf("after the rollback, %v, want one instance", got)
		}
	})
}

// A plan names the containers its step removes. One scaled by hand since it was
// reviewed changes neither its action nor its downtime, and it wasn't what the
// person agreed to take away.
func TestAPlansFingerprintCoversWhatTheRolloutRemoves(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		apply(t, r.engine, propose(t, r.engine, "Run web", r.edit("good:1")))
		p := propose(t, r.engine, "Change it", r.edit("good:2"))
		extra := *r.newest()
		extra.ID = ""
		extra.Name = "/" + docker.InstanceName(r.project, "web", 9)
		extra.Config.Labels = maps.Clone(extra.Config.Labels)
		extra.Config.Labels[docker.LabelNumber] = "9"
		r.docker.AddContainer(extra)
		if err := r.engine.Apply(t.Context(), p.Change); !errors.Is(err, ErrPlanChanged) {
			t.Errorf("applying a plan with a container more to remove = %v, want it refused", err)
		}
		if got := r.instances(); len(got) != 2 {
			t.Errorf("Docker has %v, want what it had", got)
		}
	})
}

// lostWhileStopping is the agent's stream dropping as Docker takes a stop, and
// carries on with it, whoever asked: moby's daemon/stop.go, "Cancelling the request
// should not cancel the stop".
func lostWhileStopping(r *rolling, l *lost, name string) func(*http.Request) error {
	var once sync.Once
	return func(req *http.Request) error {
		var cut bool
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/stop") {
			once.Do(func() {
				cut = true
				r.docker.Stop(name)
				l.cancel()
				r.agents.set(false)
				time.Sleep(50 * time.Millisecond)
			})
		}
		if cut {
			return errors.New("the client has gone")
		}
		return nil
	}
}

// A stop-first deploy whose agent is lost as Docker stops the old container. The
// rollback mustn't read as applied, with its revision live, and nothing running.
func TestAnAgentLostWhileAStopFirstDeployStopsEndsRunning(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		apply(t, r.engine, propose(t, r.engine, "Run db", r.edit("db:1")))
		first := strings.TrimPrefix(r.newest().Name, "/")
		p := propose(t, r.engine, "Deploy", r.edit("db:2"))
		plain := r.agents.agent
		l := &lost{Agent: plain}
		r.agents.agent = l
		r.docker.Intercept(lostWhileStopping(r, l, first))
		err := r.engine.Apply(t.Context(), p.Change)
		r.docker.Intercept(nil)
		r.agents.agent = plain
		var ferr *FailedError
		if !errors.As(err, &ferr) || ferr.Rollback == "" {
			t.Fatalf("applying = %v", err)
		}
		r.agents.set(true)
		if err := r.engine.Resume(t.Context()); err != nil {
			t.Fatalf("Resume = %v", err)
		}
		r.engine.Wait()
		back := stored(t, r.db, ferr.Rollback)
		n := r.newest()
		if back.State != store.ChangeApplied || !n.State.Running || n.Config.Image != "db:1" || len(r.instances()) != 1 {
			t.Errorf("the rollback is %s, %q; Docker has %v, newest running=%t on %s", back.State, back.Error, r.instances(), n.State.Running, n.Config.Image)
		}
	})
}

// Docker stops the old one, then can't remove it, with the agent fine throughout, so
// the rollback runs at once.
func TestARemoveThatFailsInAStopFirstDeployEndsRunning(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		apply(t, r.engine, propose(t, r.engine, "Run db", r.edit("db:1")))
		old := r.newest().ID
		p := propose(t, r.engine, "Deploy", r.edit("db:2"))
		failed := false
		r.docker.Intercept(func(req *http.Request) error {
			if req.Method == http.MethodDelete && strings.Contains(req.URL.Path, old) && !failed {
				failed = true
				return errors.New("removal of container " + old[:12] + " is already in progress")
			}
			return nil
		})
		err := r.engine.Apply(t.Context(), p.Change)
		r.docker.Intercept(nil)
		var ferr *FailedError
		if !errors.As(err, &ferr) || ferr.Rollback == "" {
			t.Fatalf("applying = %v", err)
		}
		back := stored(t, r.db, ferr.Rollback)
		n := r.newest()
		if back.State != store.ChangeApplied || !n.State.Running || n.Config.Image != "db:1" {
			t.Errorf("the rollback is %s; Docker has %v, newest running=%t on %s", back.State, r.instances(), n.State.Running, n.Config.Image)
		}
	})
}

// A restart that must stop first, whose agent is lost as the old one stops. Nothing
// follows a restart that fails, so it waits for the agent, and finishes, once.
func TestAnAgentLostWhileAStopFirstRestartStopsFinishes(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		apply(t, r.engine, propose(t, r.engine, "Run db", r.edit("db:1")))
		first := strings.TrimPrefix(r.newest().Name, "/")
		p, err := r.engine.Restart(t.Context(), r.ref, By{Actor: "user:usr_mira", Via: ViaPanel}, "")
		if err != nil {
			t.Fatal(err)
		}
		plain := r.agents.agent
		l := &lost{Agent: plain}
		r.agents.agent = l
		r.docker.Intercept(lostWhileStopping(r, l, first))
		err = r.engine.Apply(t.Context(), p.Change)
		r.docker.Intercept(nil)
		r.agents.agent = plain
		if c := stored(t, r.db, p.Change); c.State != store.ChangeApplying {
			t.Errorf("with its agent lost, the restart is %s, after %v: %q, and nothing would finish it", c.State, err, c.Error)
		}
		r.agents.set(true)
		if err := r.engine.Resume(t.Context()); err != nil {
			t.Fatalf("Resume = %v", err)
		}
		r.engine.Wait()
		c := stored(t, r.db, p.Change)
		n := r.newest()
		if c.State != store.ChangeApplied || !n.State.Running || n.Config.Labels[container.LabelRestart] != p.Change || len(r.instances()) != 1 {
			t.Errorf("the restart is %s, %q; Docker has %v, running=%t, by %q", c.State, c.Error, r.instances(), n.State.Running, n.Config.Labels[container.LabelRestart])
		}
	})
}

// Two containers writing to one place can corrupt a database, and only the person
// reading the plan can say whether this one's can take it, so a token can't choose it.
func TestStartFirstOverDataNeedsAPerson(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		apply(t, r.engine, propose(t, r.engine, "Run db", r.edit("db:1")))
		ed := r.edit("db:2")
		ed.Spec.(*resourcev1.Service).GetApp().Rollout = &resourcev1.Rollout{Order: resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST}
		p := propose(t, r.engine, "Roll over the data", ed)
		step := p.step("srv_A", r.ref)
		if !step.Rollout.Rolling || !step.Rollout.Chosen || !p.Destructive() {
			t.Errorf("rolling=%t chosen=%t destructive=%t", step.Rollout.Rolling, step.Rollout.Chosen, p.Destructive())
		}
	})
}

// With nothing shared, choosing to start first asks nothing of anyone.
func TestStartFirstOverNothingNeedsNoPerson(t *testing.T) {
	r := setupRolling(t, store.SQLite)
	apply(t, r.engine, propose(t, r.engine, "Run something plain", r.edit("good:1")))
	plain := r.edit("good:2")
	plain.Spec.(*resourcev1.Service).GetApp().Rollout = &resourcev1.Rollout{Order: resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST}
	if got := propose(t, r.engine, "Roll", plain); got.Destructive() {
		t.Error("start-first with nothing shared needs a person")
	}
}

// A change that failed says when it did. The time it began applying is all the
// change keeps, which a long deploy takes well away from that.
func TestAFailedChangeSaysWhenItFailed(t *testing.T) {
	r := setupRolling(t, store.SQLite)
	apply(t, r.engine, propose(t, r.engine, "Run web", r.edit("good:1")))
	p := propose(t, r.engine, "Deploy", r.edit("sick:1"))
	if err := r.engine.Apply(t.Context(), p.Change); err == nil {
		t.Fatal("it applied")
	}
	failed, err := r.engine.Change(t.Context(), p.Change)
	if err != nil {
		t.Fatal(err)
	}
	if ended := failed.EndedAt(); ended.IsZero() || !ended.After(failed.AppliedAt) {
		t.Errorf("it began at %s, and says it ended at %s", failed.AppliedAt, ended)
	}
	done, err := r.engine.Change(t.Context(), propose(t, r.engine, "Another", r.edit("good:2")).Change)
	if err != nil {
		t.Fatal(err)
	}
	if !done.EndedAt().IsZero() {
		t.Errorf("one that hasn't failed says it ended at %s", done.EndedAt())
	}
}

// mustCurrent reads a change's items as a Resume does, for planning again.
func mustCurrent(t *testing.T, e *Engine, changeID string) []item {
	t.Helper()
	var items []item
	err := e.db.View(t.Context(), func(tx *store.Tx) error {
		c, err := tx.GetChange(t.Context(), changeID)
		if err != nil {
			return err
		}
		for i := range c.Items {
			ci := &c.Items[i]
			ref := resource.Ref{Kind: resource.Kind(ci.Kind), Scope: ci.Scope, Name: ci.Name}
			it, err := e.load(t.Context(), tx, ref)
			if err != nil {
				return err
			}
			it.spec, it.encoded, it.servers = it.current, ci.Spec, it.placed
			if restarts(c, ref) {
				it.restart = c.ID
			}
			items = append(items, it)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// A plan's fingerprint covers how a step rolls, so a plan reviewed as having no
// downtime can't apply as one that has it.
func TestAPlansFingerprintCoversTheRollout(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		apply(t, r.engine, propose(t, r.engine, "Run web", r.edit("db:1")))
		first := propose(t, r.engine, "Change it", r.edit("db:2"))
		// What would have shared a volume now doesn't, so the plan moved on.
		r.docker.Edit(r.newest().ID, func(c *docker.Container) { c.Mounts = nil })
		err := r.engine.Apply(t.Context(), first.Change)
		if !errors.Is(err, ErrPlanChanged) {
			t.Errorf("applying a plan whose rollout has changed = %v, want it refused", err)
		}
	})
}

// A stop by hand isn't drift (ADR 0021), so nothing Dockwell does on its own starts
// that container, and a person's own request to run it does. What a rollout stopped
// isn't a stop by hand: its mark says so, and TestAnAgentLostWhileAStopFirstDeployStopsEndsRunning
// and its neighbours hold what becomes of that.
func TestWhatStartsAContainerStoppedByHand(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		stopped := func(t *testing.T) (*rolling, *docker.Container) {
			t.Helper()
			r := setupRolling(t, b)
			apply(t, r.engine, propose(t, r.engine, "Run web", r.edit("good:1")))
			r.docker.Stop(r.instances()[0])
			c := r.newest()
			if c.State.Running {
				t.Fatalf("it's running after a stop: %+v", c.State)
			}
			return r, c
		}
		other := func(r *rolling, image string) Edit {
			e := r.edit(image)
			e.Ref.Name = "api"
			return e
		}
		inPlace := func(r *rolling) Edit {
			e := r.edit("good:1")
			e.Spec.(*resourcev1.Service).GetApp().Restart = resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS
			return e
		}
		for name, act := range map[string]func(t *testing.T, r *rolling){
			"a plan with nothing to do": func(t *testing.T, r *rolling) {
				p := propose(t, r.engine, "Nothing", r.edit("good:1"))
				if len(p.Steps()) != 0 {
					t.Errorf("its steps are %s", shape(p))
				}
			},
			"a change to another service": func(t *testing.T, r *rolling) {
				apply(t, r.engine, propose(t, r.engine, "Run api", other(r, "good:2")))
			},
			"a resume": func(t *testing.T, r *rolling) {
				next := New(r.db, r.engine.Kinds(), r.agents, secretstest.Keeper(r.db, r.engine.Kinds()), slog.New(slog.DiscardHandler))
				if err := next.Resume(t.Context()); err != nil {
					t.Fatal(err)
				}
				next.Wait()
			},
			"its agent reconnecting": func(t *testing.T, r *rolling) {
				r.agents.set(false)
				r.agents.set(true)
				if err := r.engine.Resume(t.Context()); err != nil {
					t.Fatal(err)
				}
				r.engine.Wait()
			},
			"a change in place, which it skips verifying": func(t *testing.T, r *rolling) {
				p := propose(t, r.engine, "Restart it always", inPlace(r))
				if step := p.step("srv_A", r.ref); step == nil || step.Action != resource.ActionUpdate {
					t.Errorf("the step is %+v", step)
				}
				apply(t, r.engine, p)
			},
			"a rollback of another service's change, with this one in place": func(t *testing.T, r *rolling) {
				p := propose(t, r.engine, "Restart it always, and run api", inPlace(r), other(r, "crash:1"))
				var failed *FailedError
				if err := r.engine.Apply(t.Context(), p.Change); !errors.As(err, &failed) || failed.Rollback == "" {
					t.Fatalf("applying = %v, want it to roll back", err)
				}
				if c := stored(t, r.db, failed.Rollback); c.State != store.ChangeApplied {
					t.Errorf("the rollback ended %s: %s", c.State, c.Error)
				}
			},
		} {
			t.Run("leaves it stopped/"+name, func(t *testing.T) {
				r, before := stopped(t)
				act(t, r)
				now := r.docker.Container(strings.TrimPrefix(before.Name, "/"))
				if now == nil || now.ID != before.ID || now.State.Running {
					t.Errorf("after it, the container is %+v, and it was %s, stopped", now, before.ID)
				}
			})
		}
		says := func(t *testing.T, r *rolling, p *Plan) {
			t.Helper()
			step := p.step("srv_A", r.ref)
			if step == nil || step.Action != resource.ActionReplace || !slices.ContainsFunc(step.Changes, resource.IsStateChange) {
				t.Errorf("the plan should say it starts it, and is %+v", step)
			} else if step.Downtime != 0 {
				t.Errorf("nothing is up to take down, and the plan says %s of downtime", step.Downtime)
			}
		}
		for name, act := range map[string]func(t *testing.T, r *rolling){
			"a deploy of a new image": func(t *testing.T, r *rolling) {
				p := propose(t, r.engine, "Deploy", r.edit("good:2"))
				says(t, r, p)
				apply(t, r.engine, p)
			},
			"a deploy of the same tag": func(t *testing.T, r *rolling) {
				ed := r.edit("good:1")
				ed.Pull, ed.Run = true, true
				p := propose(t, r.engine, "Deploy again", ed)
				says(t, r, p)
				apply(t, r.engine, p)
			},
			"asking it to run": func(t *testing.T, r *rolling) {
				ed := r.edit("good:1")
				ed.Run = true
				p := propose(t, r.engine, "Run it", ed)
				says(t, r, p)
				apply(t, r.engine, p)
			},
			"a change that replaces it": func(t *testing.T, r *rolling) {
				ed := r.edit("good:1")
				ed.Spec.(*resourcev1.Service).GetApp().Env = map[string]string{"A": "1"}
				p := propose(t, r.engine, "Change it", ed)
				says(t, r, p)
				apply(t, r.engine, p)
			},
			"a restart": func(t *testing.T, r *rolling) {
				p, err := r.engine.Restart(t.Context(), r.ref, By{Actor: "user:usr_mira", Via: ViaPanel}, "")
				if err != nil {
					t.Fatal(err)
				}
				says(t, r, p)
				if err := r.engine.Apply(t.Context(), p.Change); err != nil {
					t.Fatal(err)
				}
			},
			"a rollback, with what failed still there": func(t *testing.T, r *rolling) {
				p := propose(t, r.engine, "Deploy a broken build", r.edit("crash:1"))
				var failed *FailedError
				if err := r.engine.Apply(t.Context(), p.Change); !errors.As(err, &failed) || failed.Rollback == "" {
					t.Fatalf("applying = %v, want it to roll back", err)
				}
			},
			"a rollback, where the deploy made nothing": func(t *testing.T, r *rolling) {
				p := propose(t, r.engine, "Deploy", r.edit("good:2"))
				var once sync.Once
				r.docker.Intercept(func(req *http.Request) error {
					var refused error
					if strings.HasSuffix(req.URL.Path, "/containers/create") {
						once.Do(func() { refused = errors.New("no room") })
					}
					return refused
				})
				defer r.docker.Intercept(nil)
				var failed *FailedError
				if err := r.engine.Apply(t.Context(), p.Change); !errors.As(err, &failed) || failed.Rollback == "" {
					t.Fatalf("applying = %v, want it to roll back", err)
				}
				if c := stored(t, r.db, failed.Rollback); c.State != store.ChangeApplied {
					t.Errorf("the rollback ended %s: %s", c.State, c.Error)
				}
				if p := DecodedPlan(t, r.db, failed.Rollback); p.step("srv_A", r.ref) == nil {
					t.Error("the rollback has no step for it, so it read as already so")
				}
			},
		} {
			t.Run("runs it/"+name, func(t *testing.T) {
				r, _ := stopped(t)
				act(t, r)
				if got := r.instances(); len(got) != 1 {
					t.Errorf("Docker has %v, want one", got)
				}
				if c := r.newest(); !c.State.Running {
					t.Errorf("after it, the container is %+v", c.State)
				}
			})
		}
		// It's verified as it starts, so what can't run doesn't read as live.
		t.Run("verifies it", func(t *testing.T) {
			r, _ := stopped(t)
			r.docker.SetExits("good:1", true)
			ed := r.edit("good:1")
			ed.Run = true
			p := propose(t, r.engine, "Run it", ed)
			if err := r.engine.Apply(t.Context(), p.Change); err == nil {
				t.Errorf("it can't stay up, and the change reads %s", stored(t, r.db, p.Change).State)
			}
		})
	})
}

// DecodedPlan reads the plan a change has on record.
func DecodedPlan(t *testing.T, db *store.DB, changeID string) *Plan {
	t.Helper()
	p, err := DecodePlan(stored(t, db, changeID).Plan)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Going back from a tag that moved pins the image it ran, as the tag alone would run
// what it's going back from. What ran reads as the tag, not the pin, so the rollback
// makes a new container of the old image, and ends with one, while what ran serves
// until that's up.
func TestARollbackPinnedToADigestMakesANewContainer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		apply(t, r.engine, propose(t, r.engine, "Run web", r.edit("good:1")))
		ran := r.newest()
		r.docker.Registry["good:1"] = dockertest.Image{Exits: true, Digest: "sha256:" + strings.Repeat("d", 64), Config: docker.Config{Cmd: []string{"app"}}}
		ed := r.edit("good:1")
		ed.Pull, ed.Run = true, true
		var failed *FailedError
		if err := r.engine.Apply(t.Context(), propose(t, r.engine, "Deploy again", ed).Change); !errors.As(err, &failed) || failed.Rollback == "" {
			t.Fatalf("applying = %v, want it to roll back", err)
		}
		if c := stored(t, r.db, failed.Rollback); c.State != store.ChangeApplied {
			t.Fatalf("the rollback ended %s: %s", c.State, c.Error)
		}
		now := r.newest()
		if got := r.instances(); len(got) != 1 {
			t.Errorf("Docker has %v, want one", got)
		}
		if now.ID == ran.ID {
			t.Errorf("the rollback kept what ran, %s, want a new container", ran.ID)
		}
		if now.Image != ran.Image || !now.State.Running || !strings.Contains(now.Config.Image, "@sha256:") {
			t.Errorf("it runs %s, image %s, running %t, and the one before ran image %s", now.Config.Image, now.Image, now.State.Running, ran.Image)
		}
	})
}

// saysDataStays reports a plan's saying that a rollback leaves what is at a place.
func saysDataStays(p *Plan, name string) bool {
	return slices.ContainsFunc(p.Notes, func(n Note) bool { return n.Reason == "volumes_stay" && slices.Contains(n.Names, name) })
}
