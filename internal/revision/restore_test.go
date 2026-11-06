// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// restores stands in for what restores a volume on the server.
type restores struct {
	mu sync.Mutex
	// done are the changes that restored, as a server remembers them.
	done map[string]bool
	// ran counts what ApplyRestore carried out.
	ran []resource.RestoreSource
	// apply runs inside ApplyRestore; an error stands for the server's.
	apply func(context.Context) error
}

func (r *restores) JudgeRestore(_ context.Context, _ resource.Ref, source resource.RestoreSource) (resource.Decision, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done[source.Change] {
		return resource.Decision{}, false, nil
	}
	return resource.Decision{
		Action: resource.ActionReplace, Destroys: true, Downtime: time.Minute,
		Restore: &resource.RestorePlan{Backup: source.Backup, Undoes: source.Undoes, Method: resource.BackupCopy, Stops: []string{"web"}},
	}, true, nil
}

func (r *restores) ApplyRestore(ctx context.Context, _ resource.Step, source resource.RestoreSource) error {
	if r.apply != nil {
		if err := r.apply(ctx); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done == nil {
		r.done = map[string]bool{}
	}
	r.done[source.Change] = true
	r.ran = append(r.ran, source)
	return nil
}

// drop is an agent from before backups: the field that asks for a restore is one
// it has never heard of.
type drop struct {
	resource.Agent
}

func (d drop) Plan(ctx context.Context, desired []resource.Desired) ([]resource.Step, error) {
	for i := range desired {
		desired[i].Restore = nil
	}
	return d.Agent.Plan(ctx, desired)
}

// withVolume is an engine with one server that has a volume, and a service
// beside it.
func withVolume(t *testing.T, b store.Backend) (*Engine, *restores, onDocker, resource.Ref, string, *store.DB) {
	t.Helper()
	e, engines, db := setupDocker(t, b, "srv_A")
	env := production(t, db)
	r := &restores{}
	agents := e.agents.(onDocker)             //nolint:forcetypeassert // setupDocker's
	local := agents["srv_A"].(resource.Local) //nolint:forcetypeassert // setupDocker's
	client := engines["srv_A"].Client()
	drivers := resource.NewRegistry(&volume.Driver{Runtime: &volume.Docker{Client: client, Kept: &volume.Kept{}}, Restores: r})
	agents["srv_A"] = twoAgents{Local: local, volumes: resource.Local{Drivers: drivers}}
	ref := resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}
	return e, r, agents, ref, env, db
}

// twoAgents sends what's about volumes to a driver that restores, and the rest
// to the server's own.
type twoAgents struct {
	resource.Local
	volumes resource.Local
}

func (a twoAgents) Plan(ctx context.Context, desired []resource.Desired) ([]resource.Step, error) {
	steps := make([]resource.Step, len(desired))
	for i := range desired {
		l := a.Local
		if desired[i].Restore != nil {
			l = a.volumes
		}
		got, err := l.Plan(ctx, desired[i:i+1])
		if err != nil {
			return nil, err
		}
		steps[i] = got[0]
	}
	return steps, nil
}

func (a twoAgents) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if step.Restore != nil {
		return a.volumes.Apply(ctx, step, desired)
	}
	return a.Local.Apply(ctx, step, desired)
}

func withData(t *testing.T, e *Engine, env string) {
	t.Helper()
	apply(t, e, propose(t, e, "Run web", Edit{Ref: resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}, Spec: &resourcev1.Volume{}, Servers: []string{"srv_A"}}, uses2(env)))
}

// uses2 is a service that mounts data, with no network.
func uses2(env string) Edit {
	ed := uses(env, "srv_A")
	ed.Spec.(*resourcev1.Service).GetApp().Networks = nil //nolint:forcetypeassert // uses's
	return ed
}

func restoreBy() By { return By{Actor: "usr_test", Via: ViaPanel} }

// A restore changes no spec, so it makes no revision, as a restart doesn't. Yet it
// destroys data, so it reads as what it is, and it happens once.
func TestARestoreIsAPlanThatMakesNoRevision(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, r, _, ref, env, db := withVolume(t, b)
		withData(t, e, env)
		before := latest(t, db, ref)

		plan, err := e.Restore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Server: "srv_A"}, restoreBy(), "")
		if err != nil {
			t.Fatal(err)
		}
		if plan.Revisions != 0 || !plan.Destructive() || plan.Summary != "Restore data" {
			t.Errorf("plan = %+v", plan)
		}
		if step := plan.step("srv_A", ref); step == nil || step.Restore == nil || step.Restore.Backup != "bkp_1" {
			t.Fatalf("step = %+v", step)
		}
		apply(t, e, plan)
		if len(r.ran) != 1 || r.ran[0].Change != plan.Change || r.ran[0].Backup != "bkp_1" {
			t.Errorf("ran = %+v", r.ran)
		}
		if after := latest(t, db, ref); after != before {
			t.Errorf("the volume went from revision %d to %d", before, after)
		}
		c := stored(t, db, plan.Change)
		if c.State != store.ChangeApplied {
			t.Fatalf("the change is %s: %s", c.State, c.Error)
		}

		// Undoing it plans a restore of what it replaced, on that server.
		undo, err := e.Undo(t.Context(), plan.Change, restoreBy())
		if err != nil {
			t.Fatal(err)
		}
		if step := undo.step("srv_A", ref); step == nil || step.Restore == nil || step.Restore.Undoes != plan.Change {
			t.Fatalf("the undo's step = %+v", step)
		}
		if undo.Reverts != plan.Change || !undo.Destructive() {
			t.Errorf("undo = %+v", undo)
		}
		apply(t, e, undo)
		if len(r.ran) != 2 || r.ran[1].Undoes != plan.Change {
			t.Errorf("ran = %+v", r.ran)
		}
	})
}

func latest(t *testing.T, db *store.DB, ref resource.Ref) int {
	t.Helper()
	var rev int
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(ref.Kind), ref.Scope, ref.Name)
		rev = r.Revision
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rev
}

// What a server must say can't be guessed from its last known state.
func TestARestoreNeedsItsServer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, agents, ref, env, _ := withVolume(t, b)
		withData(t, e, env)
		delete(agents, "srv_A")
		_, err := e.Restore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Server: "srv_A"}, restoreBy(), "")
		if !errors.Is(err, ErrOffline) || !strings.Contains(err.Error(), "only its agent") {
			t.Errorf("Restore = %v", err)
		}
	})
}

// A volume that isn't on the server with the backup can't take it.
func TestARestoreGoesWhereTheBackupIs(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, _, ref, env, _ := withVolume(t, b)
		withData(t, e, env)
		_, err := e.Restore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Server: "srv_B"}, restoreBy(), "")
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "isn't on the server") {
			t.Errorf("Restore = %v", err)
		}
		gone := ref
		gone.Name = "gone"
		_, err = e.Restore(t.Context(), gone, resource.RestoreSource{Backup: "bkp_1", Server: "srv_A"}, restoreBy(), "")
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "make it again") {
			t.Errorf("Restore of a deleted volume = %v", err)
		}
	})
}

// An agent from before backups would plan nothing, and the change would read as
// done with nothing restored.
func TestAnAgentThatCantRestoreIsToldToUpdate(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, agents, ref, env, _ := withVolume(t, b)
		withData(t, e, env)
		agents["srv_A"] = drop{agents["srv_A"]}
		_, err := e.Restore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Server: "srv_A"}, restoreBy(), "")
		if err == nil || !strings.Contains(err.Error(), "Update it") {
			t.Errorf("Restore = %v", err)
		}
	})
}

// A restore that fails has nothing to roll back: it says what happened, and the
// volume stays where it was on record.
func TestARestoreThatFailsSaysSo(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, r, _, ref, env, db := withVolume(t, b)
		withData(t, e, env)
		r.apply = func(context.Context) error { return errors.New("the backup is damaged") }
		plan, err := e.Restore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Server: "srv_A"}, restoreBy(), "")
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Apply(t.Context(), plan.Change); err == nil {
			t.Fatal("it applied")
		}
		c := stored(t, db, plan.Change)
		if c.State != store.ChangeFailed || !strings.Contains(c.Error, "damaged") {
			t.Errorf("change = %s: %s", c.State, c.Error)
		}
		if got := reverting(t, db, plan.Change); len(got) != 0 {
			t.Errorf("a failed restore was rolled back by %v", got)
		}
	})
}

// A deploy can't start on data that's being replaced, nor a restore under a deploy.
func TestARestoreHoldsItsEnvironmentsServices(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, r, _, ref, env, _ := withVolume(t, b)
		withData(t, e, env)
		started, release := make(chan struct{}), make(chan struct{})
		r.apply = func(context.Context) error {
			close(started)
			<-release
			return nil
		}
		plan, err := e.Restore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Server: "srv_A"}, restoreBy(), "")
		if err != nil {
			t.Fatal(err)
		}
		edit := uses2(env)
		edit.Spec.(*resourcev1.Service).GetApp().Image = "nginx:1.28" //nolint:forcetypeassert // uses's
		deploy := propose(t, e, "Deploy", edit)

		done := make(chan error, 1)
		go func() { done <- e.Apply(context.WithoutCancel(t.Context()), plan.Change) }()
		<-started
		err = e.Apply(t.Context(), deploy.Change)
		close(release)
		if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), plan.Change) {
			t.Errorf("a deploy during a restore = %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("the restore: %v", err)
		}
	})
}

// The agent going away leaves a restore to resume, as a restart does: the server
// says how far it got, so it never runs twice.
func TestARestoreResumesWithoutRunningTwice(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, r, _, ref, env, db := withVolume(t, b)
		withData(t, e, env)
		plan, err := e.Restore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Server: "srv_A"}, restoreBy(), "")
		if err != nil {
			t.Fatal(err)
		}
		// It finished, and its answer was lost with the agent.
		r.apply = func(context.Context) error {
			r.done = map[string]bool{plan.Change: true}
			r.apply = nil
			return resource.ErrLost
		}
		if err := e.Apply(t.Context(), plan.Change); err == nil || !strings.Contains(err.Error(), "reconnects") {
			t.Fatalf("a restore that lost its agent waits for it: %v", err)
		}
		// It waits for the agent, or a wakeup has resumed it already.
		if c := stored(t, db, plan.Change); c.State != store.ChangeApplying && c.State != store.ChangeApplied {
			t.Fatalf("after the agent went, the change is %s: %s", c.State, c.Error)
		}
		if err := e.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		e.Wait()
		if c := stored(t, db, plan.Change); c.State != store.ChangeApplied {
			t.Errorf("the change is %s: %s", c.State, c.Error)
		}
	})
}
