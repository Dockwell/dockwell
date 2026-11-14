// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
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
	// reached is what the backup says its volume reached, and one the one database
	// it holds alone, if it holds one.
	reached []string
	one     string
}

func (r *restores) JudgeRestore(_ context.Context, _ resource.Ref, source resource.RestoreSource) (resource.Decision, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done[source.Change] {
		return resource.Decision{}, false, nil
	}
	return resource.Decision{
		Action: resource.ActionReplace, Destroys: true, Downtime: time.Minute,
		Restore: &resource.RestorePlan{Backup: source.Backup, Undoes: source.Undoes, Method: resource.BackupCopy, Stops: []string{"web"}, Reached: r.reached, OneDatabase: r.one},
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
func withVolume(t *testing.T, b store.Backend) *restoreBench {
	t.Helper()
	e, engines, db := setupDocker(t, b, "srv_A")
	env := production(t, db)
	r := &restores{}
	agents := e.agents.(onDocker)             //nolint:forcetypeassert // setupDocker's
	local := agents["srv_A"].(resource.Local) //nolint:forcetypeassert // setupDocker's
	client := engines["srv_A"].Client()
	drivers := resource.NewRegistry(&volume.Driver{Runtime: &volume.Docker{Client: client, Kept: &volume.Kept{}, Paths: &container.Paths{Client: client}}, Restores: r})
	agents["srv_A"] = twoAgents{Local: local, volumes: resource.Local{Drivers: drivers}}
	ref := resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}
	return &restoreBench{e, r, agents, ref, env, db, engines["srv_A"]}
}

type restoreBench struct {
	e      *Engine
	r      *restores
	agents onDocker
	ref    resource.Ref
	env    string
	db     *store.DB
	engine *dockertest.Engine
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
		if desired[i].Restore != nil || desired[i].Backup != nil {
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
	if step.Restore != nil || step.BackupFirst != nil {
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
		x := withVolume(t, b)
		e, r, ref, env, db := x.e, x.r, x.ref, x.env, x.db
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

// A restore writes over all that's in a volume, so where the volume reaches what's as
// good as root it asks what any other way of putting something there asks: a save into
// /etc and a restore into /etc get the same answer. What reaches no further than its
// own data, which is nearly every volume, asks nothing more than it did.
func TestARestoreIntoWhatIsAsGoodAsRootTakesAPasskey(t *testing.T) {
	bound := func(device string) *resourcev1.Volume {
		return &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": device}}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		spec *resourcev1.Volume
		root bool
	}{
		"an ordinary volume":        {spec: &resourcev1.Volume{}},
		"a folder of the server's":  {spec: bound("/srv/nas")},
		"an NFS share":              {spec: &resourcev1.Volume{Options: map[string]string{"type": "nfs", "o": "addr=10.0.0.5", "device": ":/share"}}},
		"a bind of the system":      {spec: bound("/etc"), root: true},
		"a link to the system":      {spec: bound(link), root: true},
		"a plugin that mounts /etc": {spec: &resourcev1.Volume{Driver: "local-persist", Options: map[string]string{"mountpoint": "/etc"}}, root: true},
		"where Docker's socket is":  {spec: bound("/run"), root: true},
	} {
		t.Run(name, func(t *testing.T) {
			x := withVolume(t, store.SQLite)
			e, ref := x.e, x.ref
			apply(t, e, propose(t, e, "Make it", Edit{Ref: ref, Spec: tt.spec, Servers: []string{"srv_A"}}))

			plan, err := e.Restore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Server: "srv_A"}, restoreBy(), "")
			if err != nil {
				t.Fatal(err)
			}
			if !plan.Destructive() {
				t.Errorf("a restore doesn't say it destroys data: %+v", plan)
			}
			if got := plan.AsGoodAsRoot(); got != tt.root {
				t.Errorf("as good as root = %v, want %v: %+v", got, tt.root, plan.Resources)
			}
			says := func(p *Plan) bool {
				return slices.ContainsFunc(p.Notes, func(n Note) bool { return n.Reason == "new_code_as_root" })
			}
			if got := says(plan); got != tt.root {
				t.Errorf("says why it asks = %v, want %v: %+v", got, tt.root, plan.Notes)
			}
			apply(t, e, plan)

			// Undoing it writes over all that's there too.
			undo, err := e.Undo(t.Context(), plan.Change, restoreBy())
			if err != nil {
				t.Fatal(err)
			}
			if got := undo.AsGoodAsRoot(); got != tt.root {
				t.Errorf("the undo: as good as root = %v, want %v: %+v", got, tt.root, undo.Resources)
			}
			if got := says(undo); got != tt.root {
				t.Errorf("the undo says why it asks = %v, want %v: %+v", got, tt.root, undo.Notes)
			}
		})
	}
}

// A backup is tied to a volume by name alone, so one of a volume that reached the
// system can come back into an ordinary volume made since under its name, where
// whatever mounts it can read it. So the plan asks what the backup's volume would have
// asked, and says why. A volume that reaches that far itself already does.
func TestARestoreAsksWhatTheBackupsVolumeWould(t *testing.T) {
	for name, tt := range map[string]struct {
		spec     *resourcev1.Volume
		backupOf bool
		says     string
	}{
		"into an ordinary volume": {spec: &resourcev1.Volume{}, backupOf: true, says: "This backup was taken when data reached"},
		"into a bind of the system": {
			spec: &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": "/etc"}},
			says: "data reaches what's as good as root",
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := withVolume(t, store.SQLite)
			e, ref := x.e, x.ref
			x.r.reached = []string{"/etc"}
			apply(t, e, propose(t, e, "Make it", Edit{Ref: ref, Spec: tt.spec, Servers: []string{"srv_A"}}))
			plan, err := e.Restore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Server: "srv_A"}, restoreBy(), "")
			if err != nil {
				t.Fatal(err)
			}
			if !plan.AsGoodAsRoot() {
				t.Fatalf("it asks nothing more: %+v", plan.Resources)
			}
			var kinds []resource.AccessKind
			for _, a := range plan.Resources[0].HostAccess {
				kinds = append(kinds, a.Kind)
			}
			if got := slices.Contains(kinds, resource.AccessBackupOf); got != tt.backupOf {
				t.Errorf("it says %+v, want a backup of something: %v", plan.Resources[0].HostAccess, tt.backupOf)
			}
			if !slices.ContainsFunc(plan.Notes, func(n Note) bool {
				return n.Reason == "new_code_as_root" && strings.Contains(n.Message, tt.says)
			}) {
				t.Errorf("the plan says %+v, want %q", plan.Notes, tt.says)
			}
		})
	}
}

// A backup from before backups said what they reached asks what its volume's spec
// said then, as the control plane has it: a backup of a bind of /etc, restored into an
// ordinary volume of its name once the first has gone, asks as one that says so does.
func TestAnOlderBackupAsksWhatItsVolumeDidThen(t *testing.T) {
	x := withVolume(t, store.SQLite)
	e, ref, env, db := x.e, x.ref, x.env, x.db
	apply(t, e, propose(t, e, "The system", Edit{Ref: ref, Spec: &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": "/etc"}}, Servers: []string{"srv_A"}}))
	time.Sleep(5 * time.Millisecond)
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(volume.Kind), env, "data")
		if err != nil {
			return err
		}
		return tx.CreateBackup(t.Context(), store.Backup{
			ID: "bkp_old", EnvironmentID: env, Volume: "data", VolumeID: r.ID, ServerID: "srv_A", State: store.BackupDone, StartedAt: time.Now(),
		})
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	apply(t, e, propose(t, e, "Delete it", Edit{Ref: ref}))
	if err := x.engine.Client().RemoveVolume(t.Context(), volume.TargetOf(ref).Name); err != nil {
		t.Fatal(err)
	}
	apply(t, e, propose(t, e, "An ordinary one", Edit{Ref: ref, Spec: &resourcev1.Volume{}, Servers: []string{"srv_A"}}))

	plan, err := e.Restore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_old", Server: "srv_A"}, restoreBy(), "")
	if err != nil {
		t.Fatal(err)
	}
	want := resource.Access{Kind: resource.AccessBackupOf, Path: "/etc"}
	if !plan.AsGoodAsRoot() || !slices.Contains(plan.Resources[0].HostAccess, want) {
		t.Errorf("an older backup of the system asks %+v", plan.Resources)
	}
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
		x := withVolume(t, b)
		e, agents, ref, env := x.e, x.agents, x.ref, x.env
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
		x := withVolume(t, b)
		e, ref, env := x.e, x.ref, x.env
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
		x := withVolume(t, b)
		e, agents, ref, env := x.e, x.agents, x.ref, x.env
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
		x := withVolume(t, b)
		e, r, ref, env, db := x.e, x.r, x.ref, x.env, x.db
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
		x := withVolume(t, b)
		e, r, ref, env := x.e, x.r, x.ref, x.env
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
		x := withVolume(t, b)
		e, r, ref, env, db := x.e, x.r, x.ref, x.env, x.db
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

// backingUp is an agent that plans a step for a backup, as a server's does, and writes
// down what it was asked to do.
type backingUp struct {
	resource.Agent
	server     string
	unmeasured bool
	did        *[]string
}

func (a backingUp) Plan(ctx context.Context, desired []resource.Desired) ([]resource.Step, error) {
	plain := slices.Clone(desired)
	for i := range plain {
		plain[i].Backup = nil
	}
	steps, err := a.Agent.Plan(ctx, plain)
	if err != nil {
		return nil, err
	}
	for i := range desired {
		if asked := desired[i].Backup; asked != nil {
			steps[i].Action = resource.ActionUpdate
			steps[i].BackupFirst = &resource.BackupFirst{ID: asked.ID, Method: resource.BackupCopy, Unmeasured: a.unmeasured}
		}
	}
	return steps, nil
}

func (a backingUp) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if step.BackupFirst != nil {
		*a.did = append(*a.did, "back up on "+a.server)
		return nil
	}
	*a.did = append(*a.did, step.Ref.Name+" on "+a.server)
	return a.Agent.Apply(ctx, step, desired)
}

// An app on several servers has a backup on each before anything is replaced on any of
// them, so one that fails leaves them all as they were. And a volume nothing could
// measure says so once, however many servers have it.
func TestBackupsComeBeforeAnythingOnAnyServer(t *testing.T) {
	e, _, db := setupDocker(t, store.SQLite, "srv_A", "srv_B")
	env := production(t, db)
	agents := e.agents.(onDocker) //nolint:forcetypeassert // setupDocker's
	placed := func(version string) []Edit {
		web := uses(env, "srv_A", "srv_B")
		app := web.Spec.(*resourcev1.Service).GetApp() //nolint:forcetypeassert // uses's
		app.Env, app.Networks = map[string]string{"VERSION": version}, nil
		return []Edit{data(env, "srv_A", "srv_B"), web}
	}
	apply(t, e, propose(t, e, "Run web", placed("1")...))
	var did []string
	for _, server := range []string{"srv_A", "srv_B"} {
		agents[server] = backingUp{Agent: agents[server], server: server, unmeasured: true, did: &did}
	}
	edits := placed("2")
	edits[0].Backup = true
	plan := propose(t, e, "Move web on", edits...)
	unmeasured := 0
	for _, n := range plan.Notes {
		if n.Reason == "backup_unmeasured" {
			unmeasured++
			if !strings.Contains(n.Message, "data took too long to measure") {
				t.Errorf("the plan says %q", n.Message)
			}
		}
	}
	if unmeasured != 1 {
		t.Errorf("the plan says %d times that data is unmeasured: %+v", unmeasured, plan.Notes)
	}
	apply(t, e, plan)
	want := []string{"back up on srv_A", "back up on srv_B"}
	if len(did) < 3 || !slices.Equal(did[:2], want) {
		t.Errorf("did %v, want it to start with %v", did, want)
	}
}
