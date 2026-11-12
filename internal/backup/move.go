// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	engines "github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// A database moves to a newer release by a dump into a new volume, as the step that
// replaces its container does (ADR 0080). What else of its app runs on the server
// stops first, so nothing is written after the dump. The dump is a backup of the
// volume the data was on, which is only read, so it stays as it was for undo. A
// loader of the new release, on no network, starts on the new volume, loads the
// dump, starts again on what it wrote, and answers, before the container is
// replaced. Until then nothing changed but the stops, which end as the move does.

// Move is a database to move, as a service's step has it.
type Move struct {
	// Service is the service, whose scope is its environment.
	Service resource.Ref
	// Asked is what the change asks: the dump's ID, and the volumes.
	Asked resource.MoveAsked
	// Change is the change the step is of, which names what it stops.
	Change string
	// App is the container of the new release, with what its environment holds,
	// which the loader starts as: its image, environment, command and user.
	App *resourcev1.Container
}

func (m *Move) from() resource.Ref {
	return resource.Ref{Kind: volume.Kind, Scope: m.Service.Scope, Name: m.Asked.From}
}

func (m *Move) to() resource.Ref {
	return resource.Ref{Kind: volume.Kind, Scope: m.Service.Scope, Name: m.Asked.To}
}

// target is where the app has the new volume.
func (m *Move) target() string {
	for _, mt := range m.App.GetMounts() {
		if mt.GetVolume() == m.Asked.To && !mt.GetExternal() {
			return path.Clean(mt.GetTarget())
		}
	}
	return ""
}

// moveRun names what a move stops, in the list of what's stopped.
func moveRun(change string) string { return "move-" + change }

// JudgeMove says what moving a database does on this server, and about how long
// its app is down for it, or why it can't move: it has to be running, as a dump is
// what moves it.
func (r *Runner) JudgeMove(ctx context.Context, mv Move) (resource.MovePlan, time.Duration, error) {
	_, db, stops, err := r.moving(ctx, mv)
	if err != nil {
		return resource.MovePlan{}, 0, err
	}
	p, err := r.Preview(ctx, mv.from())
	if err != nil {
		return resource.MovePlan{}, 0, err
	}
	plan := resource.MovePlan{
		Backup: mv.Asked.Backup, Database: db.kind.name, From: mv.Asked.From, To: mv.Asked.To,
		Stops: namesOf(stops), Estimate: p.Estimate, Unmeasured: p.Unmeasured,
	}
	if e := engines.Recognise(mv.App.GetImage()); e != nil && e.MovesBySeries() {
		plan.FromSeries, plan.ToSeries = engines.Series(e, db.image), engines.Series(e, mv.App.GetImage())
	}
	return plan, moveEstimate(p.Estimate), nil
}

// moveEstimate is about how long a move keeps an app down: stopping and starting,
// the dump, and loading it, slower, at speeds a small server keeps.
func moveEstimate(size int64) time.Duration {
	size = sizeBucket(size)
	return time.Minute + time.Duration(size/(20<<20))*time.Second + 2*time.Duration(size/(4<<20))*time.Second
}

// moving reads what a move moves: the database on its volume, which has to be
// running, and what else stops, which is what else runs on that volume or in the
// app on this server.
func (r *Runner) moving(ctx context.Context, mv Move) (*users, *database, []*user, error) {
	from, to := volume.TargetOf(mv.from()), volume.TargetOf(mv.to())
	if _, err := r.Client.InspectVolume(ctx, from.Name); err != nil {
		return nil, nil, nil, r.noVolume(mv.from(), err)
	}
	if mv.target() == "" {
		return nil, nil, nil, fmt.Errorf("%s doesn't mount %s, which its data moves to", mv.Service.Name, to.Key)
	}
	u, err := r.users(ctx, from)
	if err != nil {
		return nil, nil, nil, err
	}
	db := u.database()
	switch {
	case db == nil:
		return nil, nil, nil, fmt.Errorf("what runs with %s as its data isn't a database Dockwell can dump: it dumps the official PostgreSQL, MySQL and MariaDB images, with their password in their environment. "+
			"So %s can't move to a new release by a dump, and its image stays as it is", from.Key, mv.Service.Name)
	case !db.running:
		return nil, nil, nil, fmt.Errorf("%s isn't running, and its data moves to the new release by a dump, which only a running %s can make. Start it, then plan this again",
			mv.Service.Name, db.kind.name)
	}
	stops := slices.DeleteFunc(u.running(), func(c *user) bool { return c.id == db.id })
	others, err := r.Client.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + from.Project}})
	if err != nil {
		return nil, nil, nil, err
	}
	for i := range others {
		c := &others[i]
		if _, helper := c.Labels[labelHelper]; helper || c.State != "running" || c.ID == db.id || slices.ContainsFunc(stops, func(s *user) bool { return s.id == c.ID }) {
			continue
		}
		full, err := r.Client.InspectContainer(ctx, c.ID)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, nil, nil, err
		}
		stops = append(stops, userOf(full))
	}
	slices.SortFunc(stops, func(a, b *user) int { return cmp.Compare(a.name, b.name) })
	return u, db, stops, nil
}

// MoveDatabase moves a database to a newer release, and calls replace for the step's
// own replace of its container, with its health check, once the new release has
// the data. What it stopped starts again however it ends. Before replace, a move
// that fails changed nothing, and its dump goes. After, the new release stops
// where it runs, so nothing writes to the new volume, and the rollback puts the old
// release back on the old one.
func (r *Runner) MoveDatabase(ctx context.Context, mv Move, replace func(context.Context) error) error {
	from := mv.from()
	u, db, stops, err := r.moving(ctx, mv)
	if err != nil {
		return err
	}
	run := moveRun(mv.Change)
	// started starts what stopped, whatever became of the call, and says so after
	// what failed.
	started := func(failed error) error {
		if err := r.startAll(ctx, run, u, nil); err != nil {
			var down downList
			if errors.As(err, &down) {
				err = errors.New(down.why())
			}
			return fmt.Errorf("%w. What stopped for the move didn't all start again: %w", failed, err)
		}
		return failed
	}
	runs := strings.TrimSpace(db.kind.name + " " + engines.Series(engines.Recognise(db.image), db.image))
	unchanged := func(failed error) error {
		r.forgetDump(ctx, from, mv.Asked.Backup)
		return started(fmt.Errorf("%w. Nothing changed: %s runs %s on %s as before", failed, mv.Service.Name, runs, mv.Asked.From))
	}
	if err := r.stop(ctx, run, stops, false); err != nil {
		return unchanged(err)
	}
	// A dump from a run cut short may be from before something wrote: this one's
	// is from after everything stopped.
	r.forgetDump(ctx, from, mv.Asked.Backup)
	resource.Stage(ctx, resource.PhaseBackingUp, "Dumping "+mv.Service.Name)
	m, err := r.Take(ctx, Take{ID: mv.Asked.Backup, Ref: from})
	switch {
	case err != nil:
		return unchanged(fmt.Errorf("can't dump %s: %w", mv.Service.Name, err))
	case m.Method != resource.BackupDump:
		return unchanged(fmt.Errorf("%s stopped before it could be dumped", mv.Service.Name))
	}
	if err := r.load(ctx, mv, m); err != nil {
		return unchanged(err)
	}
	if err := replace(ctx); err != nil {
		r.forgetDump(ctx, from, mv.Asked.Backup)
		if stopErr := r.stopOn(ctx, volume.TargetOf(mv.to())); stopErr != nil {
			err = fmt.Errorf("%w, and what runs on %s didn't stop: %w", err, mv.Asked.To, stopErr)
		}
		// What the replace said in plain words stays, with what it means for the data.
		after := fmt.Sprintf("%s still holds what %s had, and the rollback puts that back", mv.Asked.From, runs)
		plain, _ := resource.Explain(err)
		return started(&resource.Failure{Plain: strings.TrimRight(strings.TrimSpace(plain), ".") + ". " + after + ".", Err: fmt.Errorf("%w. %s", err, after)})
	}
	resource.Report(ctx, resource.Outcome{Backup: &resource.BackupTaken{ID: m.ID, Method: m.Method, Database: m.Database, SHA256: m.SHA256, Size: m.Size, Changed: m.Changed}})
	// The move is done, and a rollback would undo it for what Docker wouldn't start,
	// which the agent tries again as it next starts: the step says so instead.
	if err := r.startAll(ctx, run, u, nil); err != nil {
		var down downList
		if errors.As(err, &down) {
			err = errors.New(down.Error())
		}
		resource.Report(ctx, resource.Outcome{Note: fmt.Sprintf("%s moved to its new release on %s, but what stopped for the move didn't all start again: %v", mv.Service.Name, mv.Asked.To, err)})
	}
	return nil
}

// forgetDump removes a move's dump that's of no use: one from a run cut short, or
// one of a move that failed, whose data is where it was.
func (r *Runner) forgetDump(ctx context.Context, ref resource.Ref, id string) {
	if _, err := os.Stat(filepath.Join(r.dir(ref), id+".json")); errors.Is(err, os.ErrNotExist) {
		return
	}
	cleanup, cancel := resource.Finishing(ctx, r.helperCleanup())
	defer cancel()
	if _, err := r.Delete(cleanup, ref, id, "", nil); err != nil {
		r.logger().Warn("can't remove the dump of a move that didn't happen", "backup", id, "error", err.Error())
	}
}

// stopOn stops what runs on a volume, as a move that failed stops the new release.
func (r *Runner) stopOn(ctx context.Context, t volume.Target) error {
	ctx, cancel := resource.Finishing(ctx, startWait)
	defer cancel()
	u, err := r.users(ctx, t)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range u.running() {
		if err := r.Client.StopContainer(ctx, c.id, c.stopWait); err != nil && !docker.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("can't stop %s: %w", c.name, err))
		}
	}
	return errors.Join(errs...)
}

// load loads a dump into the new release on the new volume, in a loader of its
// own, then starts it again on what it wrote and asks it to answer.
func (r *Runner) load(ctx context.Context, mv Move, m *Manifest) error {
	to := volume.TargetOf(mv.to())
	release, err := r.hold(to)
	if err != nil {
		return err
	}
	defer release()
	if _, err := r.Client.InspectVolume(ctx, to.Name); err != nil {
		return r.noVolume(mv.to(), err)
	}
	k := kindNamed(m.Database)
	if k == nil {
		return fmt.Errorf("this agent can't load a dump of %s. Update it to the control plane's version", m.Database)
	}
	// What a run cut short loaded goes first.
	resource.Progress(ctx, "Making the new volume "+mv.Asked.To+" ready")
	if _, err := r.tool(ctx, to, "empty", docker.Streams{}); err != nil {
		return fmt.Errorf("can't empty %s for the move: %w", mv.Asked.To, err)
	}
	app := mv.App
	next := strings.TrimSpace(k.name + " " + engines.Series(engines.Recognise(app.GetImage()), app.GetImage()))
	env := make([]string, 0, len(app.GetEnv()))
	for _, k := range slices.Sorted(maps.Keys(app.GetEnv())) {
		env = append(env, k+"="+app.GetEnv()[k])
	}
	req := &docker.CreateRequest{
		Config: docker.Config{
			Image: app.GetImage(), Env: env, Entrypoint: app.GetEntrypoint(), Cmd: app.GetCommand(), User: app.GetUser(),
			Labels: map[string]string{labelHelper: "1"},
		},
		HostConfig: docker.HostConfig{
			NetworkMode: "none",
			Mounts:      []docker.Mount{{Type: "volume", Source: to.Name, Target: mv.target()}},
		},
	}
	id, err := r.Client.CreateContainer(ctx, "dockwell-move-"+randomHex(), req)
	if err != nil {
		return fmt.Errorf("docker can't make the %s to load the dump into: %w", next, err)
	}
	defer func() {
		cleanup, cancel := resource.Finishing(ctx, r.helperCleanup())
		defer cancel()
		if err := r.Client.RemoveContainer(cleanup, id, true); err != nil && !docker.IsNotFound(err) {
			r.logger().Warn("can't remove what loaded a move's dump; the agent removes it when it next starts", "container", id, "error", err.Error())
		}
	}()
	loader := &database{user: &user{id: id, name: mv.Service.Name, stopWait: stopWait}, kind: k, image: app.GetImage()}
	if k.portFlags {
		loader.port = portOf(app.GetCommand())
	}
	resource.Stage(ctx, resource.PhaseBackingUp, "Loading the dump into "+mv.Asked.To)
	if err := r.Client.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("docker can't start %s: %w", next, err)
	}
	if err := r.awaitReady(ctx, loader); err != nil {
		return fmt.Errorf("%s didn't start on %s: %w", next, mv.Asked.To, err)
	}
	f, err := os.Open(filepath.Join(r.dir(mv.from()), m.ID+"."+m.Format))
	if err != nil {
		return err
	}
	defer f.Close()
	resource.Progress(ctx, "Loading the dump")
	if err := r.loadDump(ctx, loader, f); err != nil {
		return fmt.Errorf("the dump didn't load into %s: %w", next, err)
	}
	// It has to start again on what it wrote, as the service's own container will.
	resource.Progress(ctx, "Checking "+next+" starts again on what it loaded")
	if err := r.Client.StopContainer(ctx, id, stopWait); err != nil {
		return fmt.Errorf("can't stop %s after the load: %w", next, err)
	}
	if err := r.Client.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("%s won't start again on what it loaded: %w", next, err)
	}
	if err := r.awaitReady(ctx, loader); err != nil {
		return fmt.Errorf("%s didn't start again on what it loaded: %w", next, err)
	}
	if err := r.Client.StopContainer(ctx, id, stopWait); err != nil {
		return fmt.Errorf("can't stop %s after the check: %w", next, err)
	}
	return nil
}
