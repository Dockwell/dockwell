// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

const sayFails = "curl: (22) 503"

// checked is an image with a check of its own, that keeps data, so what replaces a
// container of it stops that first.
func checked(unhealthy, starting bool) dockertest.Image {
	return dockertest.Image{Volumes: []string{"/data"}, Unhealthy: unhealthy, Starting: starting, CheckSays: sayFails, Config: docker.Config{
		Cmd: []string{"db"}, Healthcheck: &docker.Healthcheck{Test: []string{"CMD", "curl", "-f", "http://localhost/healthz"}},
	}}
}

// halt stops the control plane where this is called from, as a stop or a power cut
// does: what a step was doing goes no further, and the change stays applying.
func (r *rolling) halt() {
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	go func() { _ = r.engine.Stop(gone) }()
	<-r.engine.halted.Done()
}

// haltAtTurn halts the control plane the nth time a point is reached, once then has
// run: a rollback reaches the points of the deploy it follows, again.
func (r *rolling) haltAtTurn(at string, n int, then func()) {
	var turns atomic.Int32
	r.runtime.Points = func(name string) {
		if name == at && int(turns.Add(1)) == n {
			if then != nil {
				then()
			}
			r.halt()
		}
	}
}

// nextStart is the control plane coming back: an engine of its own, as nothing of the
// last is left, going on with what the record says.
func (r *rolling) nextStart(t *testing.T) error {
	t.Helper()
	r.runtime.Points = nil
	next := New(r.db, r.engine.Kinds(), r.agents, r.engine.sealer, r.engine.logger)
	err := next.Resume(t.Context())
	next.Wait()
	return err
}

// started starts a change as a person's click does, and waits for what comes of it.
func (r *rolling) started(t *testing.T, p *Plan) {
	t.Helper()
	if err := r.engine.Start(t.Context(), p.Change, By{Actor: "user:usr_mira", Via: ViaPanel}); err != nil {
		t.Fatal(err)
	}
	r.engine.Wait()
}

func (r *rolling) wants(t *testing.T, changeID, when, state, says string) store.Change {
	t.Helper()
	c := stored(t, r.db, changeID)
	if c.State != state {
		t.Fatalf("%s, the change is %s (%q), want %s", when, c.State, c.Error, state)
	}
	if !strings.Contains(c.Error, says) {
		t.Errorf("%s, it says %q, want %q", when, c.Error, says)
	}
	return c
}

// A stop-first deploy has taken the old container away before it waits for the new
// one's check. Cut short there, the new one runs and reads as its spec, and nothing is
// left over to say the step didn't end. So what the step was waiting for is still
// asked: it fails, and it rolls back as any failed deploy does, to what's healthy.
func TestADeployCutShortWhileItWaitsHasToPassItsCheck(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		for _, way := range []struct {
			name    string
			image   dockertest.Image
			says    string
			asPanel bool
		}{
			{name: "a check that fails", image: checked(true, false), says: "runs, but fails its health check"},
			{name: "a check that fails, asked to run as the panel does", image: checked(true, false), says: "runs, but fails its health check", asPanel: true},
			{name: "a check that never ends", image: checked(false, true), says: "didn't pass its health check within"},
		} {
			t.Run(way.name, func(t *testing.T) {
				r := setupRolling(t, b)
				r.docker.Registry["unwell:1"] = way.image
				apply(t, r.engine, propose(t, r.engine, "Run db", r.edit("db:1")))
				ed := r.edit("unwell:1")
				ed.Run = way.asPanel
				p := propose(t, r.engine, "Deploy", ed)
				if p.step("srv_A", r.ref).Rollout.Rolling {
					t.Fatal("the deploy rolls, so what it runs doesn't stop first")
				}
				r.stopAt(container.PointStarted)
				r.started(t, p)
				r.wants(t, p.Change, "stopped", store.ChangeApplying, "")
				if got := r.instances(); len(got) != 1 {
					t.Fatalf("stopped, Docker has %v, so something is left over to say it isn't done", got)
				}
				var failed *FailedError
				if err := r.nextStart(t); !errors.As(err, &failed) {
					t.Fatalf("after the next start, Resume = %v, want the deploy failed", err)
				}
				r.wants(t, p.Change, "after the next start", store.ChangeFailed, way.says)
				if back := reverting(t, r.db, p.Change); len(back) != 1 || back[0].State != store.ChangeApplied {
					t.Fatalf("its rollbacks are %+v", back)
				}
				n := r.newest()
				if n.Config.Image != "db:1" || !n.State.Running {
					t.Errorf("what's left runs %s, running %t", n.Config.Image, n.State.Running)
				}
				if got := r.instances(); len(got) != 1 {
					t.Errorf("Docker has %v", got)
				}
				if got := r.deployments(t); got[2] != store.DeploymentFailed || got[3] != store.DeploymentLive {
					t.Errorf("the deployments are %v, want the deploy failed and the rollback live", got)
				}
			})
		}
	})
}

// What was cut short and passes ends as any step does: nothing is made again, it says
// what the step did rather than that nothing was needed, and its image goes on record,
// which a rollback to it needs.
func TestADeployCutShortThatPassesItsCheckEndsAsAnyStepDoes(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		r.docker.Registry["well:1"] = checked(false, false)
		apply(t, r.engine, propose(t, r.engine, "Run db", r.edit("db:1")))
		p := propose(t, r.engine, "Deploy", r.edit("well:1"))
		r.stopAt(container.PointStarted)
		r.started(t, p)
		id := r.newest().ID
		if err := r.nextStart(t); err != nil {
			t.Fatalf("Resume = %v", err)
		}
		c := r.wants(t, p.Change, "after the next start", store.ChangeApplied, "")
		if n := r.newest(); n.ID != id || !n.State.Running {
			t.Errorf("the container went from %s to %s, running %t", id, n.ID, n.State.Running)
		}
		progress, err := decodeProgress(c.Progress)
		if err != nil || len(progress.Steps) != 1 {
			t.Fatalf("the progress is %+v, %v", progress, err)
		}
		st := progress.Steps[0]
		if st.State != StepDone || st.Did == nil || *st.Did != resource.ActionReplace || !strings.HasPrefix(st.Message, "Replaced ") {
			t.Errorf("the step reads %+v, want it to say it replaced what ran", st)
		}
		var digest, state string
		if err := r.db.View(t.Context(), func(tx *store.Tx) error {
			res, err := tx.GetResource(t.Context(), string(service.Kind), r.env, "web")
			if err != nil {
				return err
			}
			deps, err := tx.Deployments(t.Context(), res.ID, res.Revision)
			if err != nil {
				return err
			}
			digest, state = deps[0].Digest, deps[0].State
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if state != store.DeploymentLive || digest == "" {
			t.Errorf("its deployment is %s, with the digest %q", state, digest)
		}
	})
}

// A restart is the same, and has no revision to go back to, so it says what that
// leaves. It goes the same whether the control plane stopped, or the agent's stream
// dropped, which needs nothing else to have gone wrong.
func TestARestartCutShortWhileItWaitsHasToPassItsCheck(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		for _, way := range []string{"the control plane stops", "the agent's stream drops"} {
			t.Run(way, func(t *testing.T) {
				r := setupRolling(t, b)
				r.docker.Registry["well:1"] = checked(false, false)
				apply(t, r.engine, propose(t, r.engine, "Run db", r.edit("well:1")))
				p, err := r.engine.Restart(t.Context(), r.ref, By{Actor: "user:usr_mira", Via: ViaPanel}, "")
				if err != nil {
					t.Fatal(err)
				}
				sick := func() { r.docker.SetHealth(strings.TrimPrefix(r.newest().Name, "/"), "unhealthy", sayFails) }
				if way == "the control plane stops" {
					r.haltAtTurn(container.PointStarted, 1, sick)
					r.started(t, p)
				} else {
					plain := r.agents.agent
					l := &lost{Agent: plain}
					r.agents.agent = l
					var once sync.Once
					r.runtime.Points = func(name string) {
						if name == container.PointStarted {
							once.Do(func() {
								sick()
								l.cancel()
								r.agents.set(false)
							})
						}
					}
					if err := r.engine.Apply(t.Context(), p.Change); !errors.Is(err, ErrPending) {
						t.Fatalf("applying = %v, want it waiting for the server", err)
					}
					r.agents.agent = plain
					r.agents.set(true)
				}
				r.wants(t, p.Change, "cut short", store.ChangeApplying, "")
				_ = r.nextStart(t)
				r.wants(t, p.Change, "after", store.ChangeFailed, "runs, but fails its health check")
				if back := reverting(t, r.db, p.Change); len(back) != 0 {
					t.Errorf("a restart has no revision to go back to, and has %+v", back)
				}
				if got := r.deployments(t); got[1] != store.DeploymentLive {
					t.Errorf("the deployments are %v: a restart that failed leaves its revision where it was", got)
				}
			})
		}
	})
}

// A first deploy has nothing to go back to either: what failed goes, as it does when
// it isn't cut short.
func TestAFirstDeployCutShortWhileItWaitsHasToPassItsCheck(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		r.docker.Registry["unwell:1"] = checked(true, false)
		p := propose(t, r.engine, "Run it", r.edit("unwell:1"))
		r.stopAt(container.PointStarted)
		r.started(t, p)
		r.wants(t, p.Change, "stopped", store.ChangeApplying, "")
		_ = r.nextStart(t)
		r.wants(t, p.Change, "after the next start", store.ChangeFailed, "runs, but fails its health check")
		if got := r.instances(); len(got) != 0 {
			t.Errorf("Docker has %v, and nothing of what failed should be left", got)
		}
	})
}

// A rollback cut short is held to the same, and what's asked of it is what's true: it
// reads as having failed, and it doesn't go round again.
func TestARollbackCutShortWhileItWaitsHasToPassItsCheck(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		r.docker.Registry["well:1"] = checked(false, false)
		r.docker.Registry["unwell:1"] = checked(true, false)
		apply(t, r.engine, propose(t, r.engine, "Run db", r.edit("well:1")))
		p := propose(t, r.engine, "Deploy", r.edit("unwell:1"))
		// The deploy starts its container first, then the rollback starts one.
		r.haltAtTurn(container.PointStarted, 2, func() {
			r.docker.SetHealth(strings.TrimPrefix(r.newest().Name, "/"), "unhealthy", sayFails)
		})
		var failed *FailedError
		if err := r.engine.Apply(t.Context(), p.Change); !errors.As(err, &failed) || failed.Rollback == "" {
			t.Fatalf("applying = %v, want a failed deploy with a rollback under way", err)
		}
		r.wants(t, failed.Rollback, "stopped", store.ChangeApplying, "")
		count := r.changes(t)
		_ = r.nextStart(t)
		r.wants(t, failed.Rollback, "after the next start", store.ChangeFailed, "runs, but fails its health check")
		if got := r.changes(t); got != count {
			t.Errorf("there were %d changes, and now %d: a rollback isn't rolled back", count, got)
		}
	})
}

// A step that ended on record has had its check, and isn't asked again for how things
// have gone since, which is what the health of a service is for. Only the one cut
// short is, so a change doesn't fail for what it had finished with.
func TestAStepThatEndedIsntCheckedAgain(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		r := setupRolling(t, b)
		r.docker.Registry["well:1"] = checked(false, false)
		r.docker.Registry["well:2"] = checked(false, false)
		api := resource.Ref{Kind: service.Kind, Scope: r.env, Name: "api"}
		spec := func(image string) *resourcev1.Service {
			return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: image}}}
		}
		apply(t, r.engine, propose(t, r.engine, "Run both", r.edit("well:1"), Edit{Ref: api, Spec: spec("well:1")}))
		p := propose(t, r.engine, "Deploy both", r.edit("well:2"), Edit{Ref: api, Spec: spec("well:2")})
		r.haltAtTurn(container.PointStarted, 2, nil)
		r.started(t, p)
		c := r.wants(t, p.Change, "stopped", store.ChangeApplying, "")
		progress, err := decodeProgress(c.Progress)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(progress.Steps, func(s StepProgress) bool { return s.State == StepDone })
		if i < 0 {
			t.Fatalf("no step has ended: %+v", progress.Steps)
		}
		ended := r.docker.Newest(r.project, progress.Steps[i].Ref.Name)
		r.docker.SetHealth(strings.TrimPrefix(ended.Name, "/"), "unhealthy", sayFails)
		if err := r.nextStart(t); err != nil {
			t.Fatalf("Resume = %v", err)
		}
		r.wants(t, p.Change, "after the next start", store.ChangeApplied, "")
	})
}

// A change in place cut short has its check too, on what runs. What someone stopped
// stays as they left it, with nothing to ask of it, as it is when it isn't cut short.
func TestAChangeInPlaceCutShortHasItsCheckIfItRuns(t *testing.T) {
	for _, way := range []struct {
		name  string
		then  func(r *rolling, name string)
		state string
		says  string
	}{
		{"what someone stopped stays stopped", func(r *rolling, name string) { r.docker.Stop(name) }, store.ChangeApplied, ""},
		{"what runs and fails its check fails", func(r *rolling, name string) { r.docker.SetHealth(name, "unhealthy", sayFails) }, store.ChangeFailed, "runs, but fails its health check"},
		{"what runs and passes ends", func(*rolling, string) {}, store.ChangeApplied, ""},
	} {
		t.Run(way.name, func(t *testing.T) {
			r := setupRolling(t, store.SQLite)
			r.docker.Registry["well:1"] = checked(false, false)
			apply(t, r.engine, propose(t, r.engine, "Run db", r.edit("well:1")))
			name := strings.TrimPrefix(r.newest().Name, "/")
			stops := way.state == store.ChangeApplied && way.says == "" && strings.Contains(way.name, "stopped")
			if stops {
				way.then(r, name)
			}
			ed := r.edit("well:1")
			ed.Spec.(*resourcev1.Service).GetApp().Restart = resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS
			p := propose(t, r.engine, "Restart it always", ed)
			if got := p.step("srv_A", r.ref); got == nil || got.Action != resource.ActionUpdate {
				t.Fatalf("the step is %+v, want a change in place", got)
			}
			id := r.newest().ID
			var once sync.Once
			r.docker.Intercept(func(req *http.Request) error {
				if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/update") {
					once.Do(r.halt)
				}
				return nil
			})
			r.started(t, p)
			r.docker.Intercept(nil)
			r.wants(t, p.Change, "stopped", store.ChangeApplying, "")
			if !stops {
				way.then(r, name)
			}
			_ = r.nextStart(t)
			r.wants(t, p.Change, "after the next start", way.state, way.says)
			if stops {
				if n := r.newest(); n.ID != id || n.State.Running {
					t.Errorf("what someone stopped is now %s, running %t", n.ID, n.State.Running)
				}
			}
		})
	}
}

// A delete has nothing to pass, so one cut short once what it deletes has gone asks
// nothing more of the server.
func TestADeleteCutShortHasNoCheckToPass(t *testing.T) {
	r := setupRolling(t, store.SQLite)
	apply(t, r.engine, propose(t, r.engine, "Run web", r.edit("good:1")))
	p := propose(t, r.engine, "Delete web", Edit{Ref: r.ref})
	var once sync.Once
	r.docker.Intercept(func(req *http.Request) error {
		if req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/containers/") {
			once.Do(r.halt)
		}
		return nil
	})
	r.started(t, p)
	r.docker.Intercept(nil)
	r.wants(t, p.Change, "stopped", store.ChangeApplying, "")
	if n := r.newest(); n != nil {
		r.docker.Remove(strings.TrimPrefix(n.Name, "/"))
	}
	plain := r.agents.agent
	var asked atomic.Int32
	r.agents.agent = hooked{Agent: plain, before: func(resource.Step) { asked.Add(1) }}
	defer func() { r.agents.agent = plain }()
	if err := r.nextStart(t); err != nil {
		t.Fatalf("Resume = %v", err)
	}
	r.wants(t, p.Change, "after the next start", store.ChangeApplied, "")
	if got := asked.Load(); got != 0 {
		t.Errorf("the agent was asked %d times to apply a step with nothing left", got)
	}
}

// writers counts, at each start, how many containers run over a volume with that one.
type writers struct {
	mu   sync.Mutex
	most int
}

func (w *writers) watch(r *rolling, volume string) {
	r.docker.Intercept(func(req *http.Request) error {
		if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/start") {
			return nil
		}
		n := 1
		for _, name := range r.instances() {
			c := r.docker.Container(name)
			if c == nil || !c.State.Running {
				continue
			}
			if slices.ContainsFunc(c.HostConfig.Mounts, func(m docker.Mount) bool { return strings.HasSuffix(m.Source, volume) }) {
				n++
			}
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		w.most = max(w.most, n)
		return nil
	})
}

func ordered(ed Edit, order resourcev1.RolloutOrder) Edit {
	ed.Spec.(*resourcev1.Service).GetApp().Rollout = &resourcev1.Rollout{Order: order}
	return ed
}

func mounted(ed Edit, name string) Edit {
	ed.Spec.(*resourcev1.Service).GetApp().Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: name}, Target: "/data"}}
	return ed
}

func (r *rolling) volumeEdit(name string, options map[string]string) Edit {
	return Edit{Ref: resource.Ref{Kind: volume.Kind, Scope: r.env, Name: name}, Spec: &resourcev1.Volume{Options: options}}
}

// A rollback runs before anyone reads its plan. So a token that chose start-first when
// nothing was shared yet, then deployed something sick with stop-first, which needs no
// person, can't get two containers over one volume out of the rollback: it stops first
// there, whatever the order it goes back to says, and through a stop and a resume.
func TestARollbackStopsFirstOverSharedDataWhateverOrderItGoesBackTo(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		for _, cut := range []bool{false, true} {
			name := "in one run"
			if cut {
				name = "cut short before it stops anything"
			}
			t.Run(name, func(t *testing.T) {
				r := setupRolling(t, b)
				first := propose(t, r.engine, "Run web", r.volumeEdit("data", nil),
					ordered(mounted(r.edit("good:1"), "data"), resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST))
				if first.Destructive() {
					t.Fatal("the first deploy needs a person, so this way round is shut")
				}
				apply(t, r.engine, first)
				deploy := propose(t, r.engine, "Deploy", ordered(mounted(r.edit("sick:1"), "data"), resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST))
				if deploy.Destructive() {
					t.Fatal("the deploy needs a person, so this way round is shut")
				}
				var w writers
				w.watch(r, "data")
				if cut {
					// The deploy's own step goes by this point first.
					r.haltAtTurn(container.PointSwapReady, 2, nil)
				}
				var failed *FailedError
				if err := r.engine.Apply(t.Context(), deploy.Change); !errors.As(err, &failed) || failed.Rollback == "" {
					t.Fatalf("applying = %v", err)
				}
				if cut {
					r.wants(t, failed.Rollback, "stopped", store.ChangeApplying, "")
					if err := r.nextStart(t); err != nil {
						t.Fatalf("Resume = %v", err)
					}
				}
				r.wants(t, failed.Rollback, "after", store.ChangeApplied, "")
				if w.most > 1 {
					t.Errorf("%d containers ran over one volume, and nobody approved that", w.most)
				}
				plan := DecodedPlan(t, r.db, failed.Rollback)
				if step := plan.step("srv_A", r.ref); step == nil || step.Rollout == nil || step.Rollout.Rolling || plan.Destructive() {
					t.Errorf("the rollback's plan says %+v, destructive %t, want it stopping first", step, plan.Destructive())
				}
				if n := r.newest(); n.Config.Image != "good:1" || !n.State.Running {
					t.Errorf("it runs %s, running %t", n.Config.Image, n.State.Running)
				}
				if got := r.instances(); len(got) != 1 {
					t.Errorf("Docker has %v", got)
				}
			})
		}
	})
}

// With nothing shared, a rollback goes back the way its spec says.
func TestARollbackOverNothingSharedStartsFirstAsItsOrderSays(t *testing.T) {
	r := setupRolling(t, store.SQLite)
	apply(t, r.engine, propose(t, r.engine, "Run web", ordered(r.edit("good:1"), resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST)))
	deploy := propose(t, r.engine, "Deploy", ordered(r.edit("sick:1"), resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST))
	var failed *FailedError
	if err := r.engine.Apply(t.Context(), deploy.Change); !errors.As(err, &failed) || failed.Rollback == "" {
		t.Fatalf("applying = %v", err)
	}
	plan := DecodedPlan(t, r.db, failed.Rollback)
	if step := plan.step("srv_A", r.ref); step == nil || step.Rollout == nil || !step.Rollout.Rolling {
		t.Errorf("the rollback's step is %+v, want it starting first", step)
	}
}

// A volume the same change makes isn't there for a plan to read, so a plan to start
// first over nothing shared may turn out to share what the old one has. Nobody approved
// that. The step stops there and says to plan again, which is what asks a person, in a
// run that isn't cut short, and in one that is, when planning again could read it.
func TestStartFirstStopsAtTwoWritersItsPlanCouldntRead(t *testing.T) {
	bound := map[string]string{"type": "none", "o": "bind", "device": "/srv/db"}
	for _, way := range []string{"in one run", "cut short once the volume is made"} {
		t.Run(way, func(t *testing.T) {
			r := setupRolling(t, store.SQLite)
			apply(t, r.engine, propose(t, r.engine, "Run web", r.volumeEdit("a", bound), mounted(r.edit("good:1"), "a")))
			kept := r.newest().ID
			p := propose(t, r.engine, "Move to b", r.volumeEdit("b", bound),
				ordered(mounted(r.edit("good:2"), "b"), resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST))
			if step := p.step("srv_A", r.ref); !step.Rollout.Rolling || step.Rollout.Chosen {
				t.Fatalf("the plan says %+v, which reads what it shares, and this needs one that can't", *step.Rollout)
			}
			if way == "in one run" {
				r.started(t, p)
			} else {
				var made atomic.Bool
				var once sync.Once
				r.docker.Intercept(func(req *http.Request) error {
					switch {
					case made.Load():
						once.Do(r.halt)
					case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/volumes/create"):
						made.Store(true)
					}
					return nil
				})
				r.started(t, p)
				r.docker.Intercept(nil)
				r.wants(t, p.Change, "stopped", store.ChangeApplying, "")
				_ = r.nextStart(t)
			}
			r.wants(t, p.Change, "after", store.ChangeFailed, "Plan the change again")
			c := stored(t, r.db, p.Change)
			if !strings.Contains(c.Error, "/srv/db") {
				t.Errorf("it says %q, and should name what's shared", c.Error)
			}
			if n := r.newest(); n.ID != kept || !n.State.Running {
				t.Errorf("what ran went from %s to %s, running %t: nothing should have been stopped", kept, n.ID, n.State.Running)
			}
			if got := r.instances(); len(got) != 1 {
				t.Errorf("Docker has %v", got)
			}
		})
	}
}

// What a person approved stays what they approved: a plan that said two writers rolls.
func TestStartFirstOverDataRollsOnceAPersonApprovesIt(t *testing.T) {
	r := setupRolling(t, store.SQLite)
	apply(t, r.engine, propose(t, r.engine, "Run db", r.edit("db:1")))
	p := propose(t, r.engine, "Roll over the data", ordered(r.edit("db:2"), resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST))
	if !p.step("srv_A", r.ref).Rollout.TwoWriters() {
		t.Fatalf("the plan says %+v", *p.step("srv_A", r.ref).Rollout)
	}
	if err := r.engine.Apply(t.Context(), p.Change); err != nil {
		t.Fatalf("applying what was approved = %v", err)
	}
	if n := r.newest(); n.Config.Image != "db:2" || !n.State.Running {
		t.Errorf("it runs %s, running %t", n.Config.Image, n.State.Running)
	}
}
