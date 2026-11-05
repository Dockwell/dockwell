// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

const (
	shopImage   = "ghcr.io/acme/shop:1.4"
	shopBroken  = "ghcr.io/acme/shop:broken"
	recoverFile = "name: shop\nservices:\n  web:\n    image: " + shopImage + "\n"
)

// aFailedRollback leaves what a deploy of a broken image and its own rollback
// leave, when Docker starts refusing the request that ends in suffix from when the
// broken image's container is made.
func aFailedRollback(suffix string) func(t *testing.T, b *bench) (string, *dockwellv1.ServiceInfo) {
	return func(t *testing.T, b *bench) (string, *dockwellv1.ServiceInfo) {
		t.Helper()
		env := b.run(t, &dockwellv1.ImportComposeRequest{Compose: recoverFile})
		web := b.service(t, "shop", "web")
		name := docker.ContainerName(docker.Project(env), "web")
		b.e.Registry[shopBroken] = dockertest.Image{Exits: true, Config: docker.Config{Cmd: []string{"shop"}}}
		var fail atomic.Bool
		b.e.Intercept(func(r *http.Request) error {
			if fail.Load() && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, suffix) {
				return errors.New("no space left on device")
			}
			return nil
		})
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if c := b.e.Container(name); c != nil && c.Config.Image == shopBroken {
					fail.Store(true)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		got := applied(t, b.c, b.deploy(t, web.GetId(), shopBroken))
		if got.GetRolledBackBy() == "" {
			t.Fatalf("the deploy ended %v with no rollback: %s", got.GetState(), got.GetError())
		}
		var rollback *dockwellv1.Change
		waitFor(t, "the rollback to end", func() bool {
			rc, err := b.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: got.GetRolledBackBy()}))
			if err != nil {
				return false
			}
			rollback = rc.Msg.GetChange()
			return rollback.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLYING
		})
		if rollback.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED {
			t.Fatalf("the rollback ended %v", rollback.GetState())
		}
		b.e.Intercept(nil)
		return name, b.service(t, "shop", "web")
	}
}

// byHand leaves a service with a revision that matches its latest, running, with
// what someone then did to it.
func byHand(did func(t *testing.T, b *bench, name string)) func(t *testing.T, b *bench) (string, *dockwellv1.ServiceInfo) {
	return func(t *testing.T, b *bench) (string, *dockwellv1.ServiceInfo) {
		t.Helper()
		env := b.run(t, &dockwellv1.ImportComposeRequest{Compose: recoverFile})
		web := b.service(t, "shop", "web")
		name := docker.ContainerName(docker.Project(env), "web")
		b.done(t, b.deploy(t, web.GetId(), "ghcr.io/acme/shop:1.5"))
		b.done(t, b.deploy(t, web.GetId(), shopImage))
		did(t, b, name)
		return name, b.service(t, "shop", "web")
	}
}

// matching is an earlier revision the servers' record says matches the latest.
func (b *bench) matching(t *testing.T, id string) int64 {
	t.Helper()
	revs := b.revisions(t, id)
	for _, r := range revs[1:] {
		if r.GetMatchesLatest() {
			return r.GetNumber()
		}
	}
	t.Fatalf("no revision matches the latest: %v", revs)
	return 0
}

// Each way a service can be left not running has two ways back, as ADR 0026 says:
// deploying the tag it names again, and rolling back to a revision that matches.
// Each ends with it really running, and only then reads live.
func TestARollbackThatFailedCanBeRecovered(t *testing.T) {
	states := []struct {
		name string
		make func(t *testing.T, b *bench) (string, *dockwellv1.ServiceInfo)
		// plansState is what a plan says differs, for a container that's there.
		plansState string
	}{
		{name: "a rollback that failed making the container", make: aFailedRollback("/containers/create")},
		{name: "a rollback that failed starting it", make: aFailedRollback("/start"), plansState: "created"},
		{name: "a rollback that failed giving it its name", make: aFailedRollback("/rename")},
		{name: "a container removed by hand", make: byHand(func(t *testing.T, b *bench, name string) {
			t.Helper()
			if err := b.e.Client().RemoveContainer(t.Context(), b.e.Container(name).ID, true); err != nil {
				t.Fatal(err)
			}
		})},
		{name: "a container stopped by hand", plansState: "exited", make: byHand(func(t *testing.T, b *bench, name string) {
			t.Helper()
			if err := b.e.Client().StopContainer(t.Context(), b.e.Container(name).ID, time.Second); err != nil {
				t.Fatal(err)
			}
		})},
		{name: "a container restarting over and over", plansState: "restarting", make: byHand(func(t *testing.T, b *bench, name string) {
			t.Helper()
			b.e.Edit(name, func(c *docker.Container) {
				c.State = docker.State{Status: "restarting", Running: true, Restarting: true}
			})
		})},
		{name: "a container paused by hand", plansState: "paused", make: byHand(func(t *testing.T, b *bench, name string) {
			t.Helper()
			b.e.Edit(name, func(c *docker.Container) { c.State = docker.State{Status: "paused", Running: true, Paused: true} })
		})},
	}
	ways := []struct {
		name string
		plan func(t *testing.T, b *bench, id string) *dockwellv1.Change
	}{
		{"deploying its tag again", func(t *testing.T, b *bench, id string) *dockwellv1.Change {
			t.Helper()
			return b.deploy(t, id, shopImage)
		}},
		{"rolling back to what matches", func(t *testing.T, b *bench, id string) *dockwellv1.Change {
			t.Helper()
			back, err := b.s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: id, Revision: b.matching(t, id)}))
			if err != nil {
				t.Fatalf("rolling back = %v", err)
			}
			return back.Msg.GetChange()
		}},
	}
	for _, state := range states {
		for _, way := range ways {
			t.Run(state.name+", by "+way.name, func(t *testing.T) {
				b := newBench(t, store.SQLite)
				name, web := state.make(t, b)
				written := len(b.revisions(t, web.GetId()))
				if c := b.e.Container(name); c != nil && c.State.Running && !c.State.Restarting && !c.State.Paused {
					t.Fatalf("the scenario left it running: %+v", c.State)
				}
				plan := way.plan(t, b, web.GetId())
				if state.plansState != "" {
					steps := plan.GetPlan().GetServers()[0].GetSteps()
					if len(steps) != 1 || steps[0].GetDowntime() != nil || !saysState(steps[0], state.plansState) {
						t.Errorf("the plan's steps are %v", steps)
					}
				}
				got := b.done(t, plan)
				for _, s := range got.GetSteps() {
					if strings.Contains(s.GetMessage(), "stays as it is") {
						t.Errorf("the step says %q, and it wasn't running", s.GetMessage())
					}
				}
				c := b.e.Container(name)
				if c == nil || !c.State.Running || c.State.Restarting || c.State.Paused {
					t.Fatalf("after recovering, the container is %+v", c)
				}
				if image, _, _ := strings.Cut(c.Config.Image, "@"); image != shopImage {
					t.Errorf("after recovering, it runs %s", c.Config.Image)
				}
				// It comes back as the revision it was on, with what that really runs:
				// nothing newer was found, whatever the failure left there.
				revs := b.revisions(t, web.GetId())
				if len(revs) != written {
					t.Errorf("bringing it back wrote a revision: %d, from %d", len(revs), written)
				}
				img, err := b.e.Client().InspectImage(t.Context(), c.Image)
				if err != nil {
					t.Fatal(err)
				}
				if got := digestsOf(revs[0]); len(got) != 1 || got[0] != img.Digest(shopImage) {
					t.Errorf("the latest revision says it ran %v, and it runs %s", got, img.Digest(shopImage))
				}
				after := b.service(t, "shop", "web")
				for _, d := range after.GetDeployments() {
					if d.GetState() != dockwellv1.DeploymentState_DEPLOYMENT_STATE_LIVE {
						t.Errorf("after recovering, it reads %v on %s: %s", d.GetState(), d.GetServerName(), d.GetError())
					}
				}
			})
		}
	}
}

func saysState(step *dockwellv1.PlanStep, from string) bool {
	for _, c := range step.GetChanges() {
		if c.GetPath() == "state" && c.GetFrom() == from && c.GetTo() == "running" {
			return true
		}
	}
	return false
}

// A container that won't stay up isn't made to read live: starting it again is
// verified as any step is.
func TestRecoveringWhatWontStayUpFails(t *testing.T) {
	b := newBench(t, store.SQLite)
	name, web := runLatest(t, b)
	b.e.Stop(name)
	b.e.Registry[latest] = dockertest.Image{Digest: digestA, Exits: true, Config: docker.Config{Cmd: []string{"shop"}}}
	got := applied(t, b.c, b.deploy(t, web.GetId(), latest))
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(got.GetError(), "stopped right after it started") {
		t.Fatalf("the deploy ended %v: %s", got.GetState(), got.GetError())
	}
	for _, d := range b.revisions(t, web.GetId())[0].GetDeployments() {
		if d.GetState() == dockwellv1.DeploymentState_DEPLOYMENT_STATE_LIVE {
			t.Errorf("it reads live, and isn't running: %v", d)
		}
	}
}

// Nothing newer doesn't make it healthy: a step that reads done says it is.
func TestAPullWithNothingNewerStillVerifies(t *testing.T) {
	b := newBench(t, store.SQLite)
	name, web := runLatest(t, b)
	before := b.e.Container(name).ID
	b.e.Edit(name, func(c *docker.Container) { c.State.Health = &docker.Health{Status: "unhealthy", FailingStreak: 3} })
	got := applied(t, b.c, b.deploy(t, web.GetId(), latest))
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(got.GetError(), "is as asked, but it isn't healthy") ||
		!strings.Contains(got.GetError(), "fails its health check") {
		t.Fatalf("the deploy ended %v: %s", got.GetState(), got.GetError())
	}
	if b.e.Container(name).ID != before {
		t.Error("with nothing newer, the container was replaced")
	}
}

// Nothing is running to keep safe, so the image the server has will do, and the
// step says that's what happened.
func TestARemovedContainerComesBackWithItsRegistryDown(t *testing.T) {
	b := newBench(t, store.SQLite)
	name, web := runLatest(t, b)
	if err := b.e.Client().RemoveContainer(t.Context(), b.e.Container(name).ID, true); err != nil {
		t.Fatal(err)
	}
	delete(b.e.Registry, latest)
	got := b.done(t, b.deploy(t, web.GetId(), latest))
	c := b.e.Container(name)
	if c == nil || !c.State.Running {
		t.Fatalf("the image is on the server, and the container is %+v", c)
	}
	said := got.GetSteps()[0].GetMessage()
	if !strings.Contains(said, "Can't pull "+latest+" again") || !strings.Contains(said, "so web runs the image this server has") {
		t.Errorf("the step says %q", said)
	}
	if revs := b.revisions(t, web.GetId()); len(revs) != 1 || revs[0].GetDeployments()[0].GetState() != dockwellv1.DeploymentState_DEPLOYMENT_STATE_LIVE {
		t.Errorf("the revisions are %v", revs)
	}
}

// So does a stopped one, as nothing it could break is running.
func TestAStoppedContainerStartsWithItsRegistryDown(t *testing.T) {
	b := newBench(t, store.SQLite)
	name, web := runLatest(t, b)
	b.e.Stop(name)
	delete(b.e.Registry, latest)
	b.done(t, b.deploy(t, web.GetId(), latest))
	if c := b.e.Container(name); c == nil || !c.State.Running {
		t.Fatalf("the image is on the server, and the container is %+v", c)
	}
}

// With no image and no registry, it can't start, and says what's true: nothing
// runs to go on running.
func TestARemovedContainerWithNoImageSaysNothingRuns(t *testing.T) {
	b := newBench(t, store.SQLite)
	name, web := runLatest(t, b)
	if err := b.e.Client().RemoveContainer(t.Context(), b.e.Container(name).ID, true); err != nil {
		t.Fatal(err)
	}
	b.e.Forget(digestA)
	got := applied(t, b.c, b.deploy(t, web.GetId(), latest))
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED {
		t.Fatalf("the deploy ended %v", got.GetState())
	}
	for _, want := range []string{"can't pull " + latest, "web isn't running, and this server doesn't have that image", "Check that its registry answers"} {
		if !strings.Contains(got.GetError(), want) {
			t.Errorf("the error doesn't say %q: %s", want, got.GetError())
		}
	}
	if strings.Contains(got.GetError(), "goes on running") {
		t.Errorf("nothing runs, and the error says it does: %s", got.GetError())
	}
}

// A pin can't be changed in place, so leaving one replaces the container whatever
// the pull finds, and the plan and the step say so.
func TestLeavingAPinSaysItReplaces(t *testing.T) {
	b := newBench(t, store.SQLite)
	name, web := runLatest(t, b)
	b.done(t, b.deploy(t, web.GetId(), latest+"@"+digestA))
	before := b.e.Container(name).ID
	plan := b.deploy(t, web.GetId(), latest)
	note := noteOf(plan, "leaves_pin").GetMessage()
	if !strings.Contains(note, "replaces web with what that names now, in place of the digest it's pinned to") || strings.Contains(note, "only if") {
		t.Errorf("the plan says %q", note)
	}
	if noteOf(plan, "pulls_again") != nil {
		t.Errorf("the plan claims only if newer: %v", plan.GetPlan().GetNotes())
	}
	got := b.done(t, plan)
	if b.e.Container(name).ID == before {
		t.Fatal("the container still names the pin")
	}
	if said := got.GetSteps()[0].GetMessage(); strings.Contains(said, "stays as it is") || strings.Contains(said, "already the newest") {
		t.Errorf("it was replaced, and the step says %q", said)
	}
}

// What a step did is a fact on the step, server by server, rather than something
// to work out from what it says.
func TestAStepSaysWhatItDid(t *testing.T) {
	didOf := func(c *dockwellv1.Change) []dockwellv1.PlanAction {
		var out []dockwellv1.PlanAction
		for _, s := range c.GetSteps() {
			out = append(out, s.GetDid())
		}
		return out
	}
	none, replace, update := dockwellv1.PlanAction_PLAN_ACTION_NONE, dockwellv1.PlanAction_PLAN_ACTION_REPLACE, dockwellv1.PlanAction_PLAN_ACTION_UPDATE

	t.Run("a pull with nothing newer stayed", func(t *testing.T) {
		b := newBench(t, store.SQLite)
		name, web := runLatest(t, b)
		before := b.e.Container(name).ID
		got := b.done(t, b.deploy(t, web.GetId(), latest))
		if b.e.Container(name).ID != before || !slices.Equal(didOf(got), []dockwellv1.PlanAction{none}) {
			t.Errorf("replaced %t, did %v", b.e.Container(name).ID != before, didOf(got))
		}
		if a := got.GetSteps()[0].GetAction(); a != replace {
			t.Errorf("the step's action says %v, which is what was planned", a)
		}
	})

	t.Run("a pull with something newer replaced", func(t *testing.T) {
		b := newBench(t, store.SQLite)
		name, web := runLatest(t, b)
		before := b.e.Container(name).ID
		moveLatest(b.e, digestB)
		got := b.done(t, b.deploy(t, web.GetId(), latest))
		if b.e.Container(name).ID == before || !slices.Equal(didOf(got), []dockwellv1.PlanAction{replace}) {
			t.Errorf("replaced %t, did %v", b.e.Container(name).ID != before, didOf(got))
		}
	})

	// Run it, deploy another tag, roll back to r1, which pins what r1 ran, and deploy
	// the tag again. It hasn't moved, and the container still has to change.
	t.Run("leaving a pin with nothing newer replaced", func(t *testing.T) {
		b := newBench(t, store.SQLite)
		name, web := runLatest(t, b)
		b.e.Registry["ghcr.io/acme/shop:other"] = dockertest.Image{Digest: digestB, Config: docker.Config{Cmd: []string{"shop"}}}
		b.done(t, b.deploy(t, web.GetId(), "ghcr.io/acme/shop:other"))
		back, err := b.s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1}))
		if err != nil {
			t.Fatal(err)
		}
		b.done(t, back.Msg.GetChange())
		pinned := b.e.Container(name)
		if pinned.Config.Image != latest+"@"+digestA {
			t.Fatalf("the rollback runs %s", pinned.Config.Image)
		}

		plan := b.deploy(t, web.GetId(), latest)
		if noteOf(plan, "pulls_again") != nil || noteOf(plan, "leaves_pin") == nil {
			t.Errorf("the plan says %v", plan.GetPlan().GetNotes())
		}
		got := b.done(t, plan)
		if b.e.Container(name).ID == pinned.ID {
			t.Fatal("the container still names the pin")
		}
		if !slices.Equal(didOf(got), []dockwellv1.PlanAction{replace}) {
			t.Errorf("it was replaced, and the step did %v", didOf(got))
		}
		if said := got.GetSteps()[0].GetMessage(); strings.Contains(said, "stays as it is") || strings.Contains(said, "already the newest") {
			t.Errorf("it was replaced, and the step says %q", said)
		}
		if got.GetUndoDeadline() == nil {
			t.Error("it made a revision, and offers no undo")
		}
	})

	t.Run("a restart policy put right with nothing newer changed in place", func(t *testing.T) {
		b := newBench(t, store.SQLite)
		name, web := runLatest(t, b)
		before := b.e.Container(name).ID
		if err := b.e.Client().UpdateRestartPolicy(t.Context(), before, docker.RestartPolicy{Name: "always"}); err != nil {
			t.Fatal(err)
		}
		got := b.done(t, b.deploy(t, web.GetId(), latest))
		if b.e.Container(name).ID != before || !slices.Equal(didOf(got), []dockwellv1.PlanAction{update}) {
			t.Errorf("replaced %t, did %v", b.e.Container(name).ID != before, didOf(got))
		}
	})

	t.Run("a step that failed says nothing", func(t *testing.T) {
		b := newBench(t, store.SQLite)
		_, web := runLatest(t, b)
		delete(b.e.Registry, latest)
		got := applied(t, b.c, b.deploy(t, web.GetId(), latest))
		if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !slices.Equal(didOf(got), []dockwellv1.PlanAction{dockwellv1.PlanAction_PLAN_ACTION_UNSPECIFIED}) {
			t.Errorf("%v, did %v", got.GetState(), didOf(got))
		}
	})
}
