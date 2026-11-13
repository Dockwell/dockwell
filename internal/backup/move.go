// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	engines "github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// A database moves to a newer release by a dump into a new volume, as the step that
// replaces its container does (ADR 0080). Everything that runs on its volume, or in
// its app on the server, stops first, the database too, so nothing can be written
// after the dump, from inside the server or out. The old release is started again
// in a container of its own, on no network, to be dumped; the dump is a backup of
// the volume the data was on, which is only read, so it stays as it was for undo.
// A loader of the new release, on no network, loads the dump into the new volume,
// which has to be new, then a second start brings what it loaded up to its own
// layout, and has to hold what the old release held, before the container is
// replaced. What stopped is noted as a restore's is, so a crash doesn't start it
// against either release: the change's resume goes on, or finishes.

// Move is a database to move, as a service's step has it.
type Move struct {
	// Service is the service, whose scope is its environment.
	Service resource.Ref
	// Asked is what the change asks: the dump's ID, the volumes and the change.
	Asked resource.MoveAsked
	// Change is the change the step is of, which names what it stops, where Asked
	// doesn't say.
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

func (m *Move) change() string { return cmp.Or(m.Asked.Change, m.Change) }

// target is where the app has the new volume.
func (m *Move) target() string {
	for _, mt := range m.App.GetMounts() {
		if mt.GetVolume() == m.Asked.To && !mt.GetExternal() {
			return path.Clean(mt.GetTarget())
		}
	}
	return ""
}

// release names the new release, such as PostgreSQL 17.
func (m *Move) release(k *databaseKind) string {
	return strings.TrimSpace(k.name + " " + engines.Series(engines.Recognise(m.App.GetImage()), m.App.GetImage()))
}

// moveRun names what a move stops, in the list of what's stopped.
func moveRun(change string) string { return "move-" + change }

// JudgeMove says what moving a database does on this server, and about how long
// its app is down for it, or why it can't move: it has to be running, as a dump is
// what moves it, with room for the dump and the new release's copy, and nothing
// in the way that a dump would lose, such as replication.
func (r *Runner) JudgeMove(ctx context.Context, mv Move) (resource.MovePlan, time.Duration, error) {
	u, db, stops, err := r.moving(ctx, mv)
	if err != nil {
		return resource.MovePlan{}, 0, err
	}
	// A resume finds the database stopped by this move: it was asked before.
	if db.running {
		if err := r.preflight(ctx, mv, db); err != nil {
			return resource.MovePlan{}, 0, err
		}
	}
	p, err := r.Preview(ctx, mv.from())
	if err != nil {
		return resource.MovePlan{}, 0, err
	}
	if err := r.roomToMove(ctx, mv, p.Estimate); err != nil {
		return resource.MovePlan{}, 0, err
	}
	plan := resource.MovePlan{
		Backup: mv.Asked.Backup, Database: db.kind.name, From: mv.Asked.From, To: mv.Asked.To,
		Stops: namesOf(slices.DeleteFunc(stops, func(c *user) bool { return c.id == db.id })), Estimate: p.Estimate, Unmeasured: p.Unmeasured,
	}
	// What else is on the old volume, which no service of Dockwell's is, as a plan
	// refuses those, stays stopped after the move: it would go on with the old data.
	for _, c := range u.list {
		if c.id != db.id {
			plan.StaysStopped = append(plan.StaysStopped, c.name)
		}
	}
	slices.Sort(plan.StaysStopped)
	if e := engines.Recognise(mv.App.GetImage()); e != nil && e.MovesBySeries() {
		plan.FromSeries, plan.ToSeries = engines.Series(e, db.image), engines.Series(e, mv.App.GetImage())
	}
	return plan, moveEstimate(p.Estimate), nil
}

// MoveFinishes says what's left of a move whose replace is done, as a run cut short
// after it leaves things: what it stopped, to start, and its dump, to report. A
// finishing step does both (FinishMove).
func (r *Runner) MoveFinishes(mv Move) (bool, error) {
	r.mu.Lock()
	list, err := r.readStopped()
	r.mu.Unlock()
	if err != nil {
		return false, err
	}
	if _, stopped := list[moveRun(mv.change())]; stopped {
		return true, nil
	}
	_, err = os.Stat(filepath.Join(r.dir(mv.from()), mv.Asked.Backup+".json"))
	return err == nil, nil
}

// FinishMove starts what a move stopped and reports its dump, for a move whose
// replace is done. Nothing on the old volume starts: that's the old release, or
// what would go on with its data, and only a step of the service starts the old
// release, as a rollback's does.
func (r *Runner) FinishMove(ctx context.Context, mv Move) error {
	if m, err := readManifest(filepath.Join(r.dir(mv.from()), mv.Asked.Backup+".json")); err == nil {
		resource.Report(ctx, resource.Outcome{Backup: &resource.BackupTaken{ID: m.ID, Method: m.Method, Database: m.Database, SHA256: m.SHA256, Size: m.Size, Changed: m.Changed}})
	}
	r.forgetMarker(mv)
	old, err := r.users(ctx, volume.TargetOf(mv.from()))
	if err != nil {
		return err
	}
	if err := r.noteStopped(func(l stoppedFile) {
		run, ok := l[moveRun(mv.change())]
		if !ok {
			return
		}
		run.Containers = slices.DeleteFunc(run.Containers, func(id string) bool {
			return slices.ContainsFunc(old.list, func(u *user) bool { return u.id == id })
		})
		l[moveRun(mv.change())] = run
	}); err != nil {
		return err
	}
	if err := r.startAll(ctx, moveRun(mv.change()), &users{}, nil); err != nil {
		var down downList
		if errors.As(err, &down) {
			err = errors.New(down.Error())
		}
		resource.Report(ctx, resource.Outcome{Note: fmt.Sprintf("%s moved to its new release on %s, but what stopped for the move didn't all start again: %v", mv.Service.Name, mv.Asked.To, err)})
	}
	return nil
}

// moveEstimate is about how long a move keeps an app down: stopping and starting,
// the dump, and loading it, slower, at speeds a small server keeps.
func moveEstimate(size int64) time.Duration {
	size = sizeBucket(size)
	return time.Minute + time.Duration(size/(20<<20))*time.Second + 2*time.Duration(size/(4<<20))*time.Second
}

// moving reads what a move moves: the database on its volume, which has to be
// running unless this move stopped it, and what stops, which is the database and
// what else runs on that volume or in the app on this server.
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
	r.mu.Lock()
	list, err := r.readStopped()
	r.mu.Unlock()
	if err != nil {
		return nil, nil, nil, err
	}
	noted := list[moveRun(mv.change())].Containers
	switch {
	case db == nil:
		return nil, nil, nil, fmt.Errorf("what runs with %s as its data isn't a database Dockwell can dump whole: it dumps the official PostgreSQL, MySQL and MariaDB images, with their password in their environment, "+
			"and a MariaDB older than 10.11 with a system-versioned table is copied as files, which a move can't use. So %s can't move to a new release by a dump, and its image stays as it is", from.Key, mv.Service.Name)
	case !db.running && !slices.Contains(noted, db.id):
		return nil, nil, nil, fmt.Errorf("%s isn't running, and its data moves to the new release by a dump, which only a running %s can make. Start it, then plan this again",
			mv.Service.Name, db.kind.name)
	}
	stops := u.running()
	others, err := r.Client.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + from.Project}})
	if err != nil {
		return nil, nil, nil, err
	}
	for i := range others {
		c := &others[i]
		if _, helper := c.Labels[labelHelper]; helper || c.State != "running" || slices.ContainsFunc(stops, func(s *user) bool { return s.id == c.ID }) {
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

// preflight asks the database what stands in the way of moving it by a dump, before
// anything stops, and refuses that, with why.
func (r *Runner) preflight(ctx context.Context, mv Move, db *database) error {
	if db.kind.preflight == nil {
		return nil
	}
	cmd := db.tool(db.kind.preflight)
	if db.kind.name == "MySQL" && atLeast(engines.Series(engines.Recognise(mv.App.GetImage()), mv.App.GetImage()), 8, 4) {
		cmd = append([]string{"env", "DW_NATIVE=1"}, cmd...)
	}
	out, errs := &tail{}, &tail{}
	code, err := r.Client.Exec(ctx, db.id, cmd, docker.Streams{Stdout: out, Stderr: errs})
	switch {
	case err != nil:
		return fmt.Errorf("can't ask %s what stands in the way of moving it: %w", mv.Service.Name, err)
	case code != 0:
		return fmt.Errorf("can't ask %s what stands in the way of moving it: %s. Check it's running, then plan this again", mv.Service.Name, scrubbed(errs.String()))
	}
	var noConnect, native, slots []string
	var replica, replicas bool
	for line := range strings.Lines(out.String()) {
		word, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch word {
		case "noconnect":
			noConnect = append(noConnect, rest)
		case "native":
			native = append(native, rest)
		case "replica":
			replica = true
		case "replicas", "subscription":
			replicas = true
		case "slot":
			slots = append(slots, rest)
		}
	}
	name, release := mv.Service.Name, mv.release(db.kind)
	switch {
	case replica:
		return fmt.Errorf("%s is a replica of another server, and a dump into a new release leaves that out, so its image stays as it is. Move the server it replicates first, or take it out of replication", name)
	case replicas || len(slots) > 0:
		return fmt.Errorf("%s has replicas or replication slots%s, which a dump into a new release leaves out, so they'd stop following it, and its image stays as it is. "+
			"Take them out of replication first", name, listed(slots))
	case len(noConnect) > 0:
		return fmt.Errorf("%s takes no connections to %s, so a dump would leave %s out, and its image stays as it is. "+
			"Let it take connections, with ALTER DATABASE ... ALLOW_CONNECTIONS true, or drop it, then plan this again", name, human.List(noConnect), plural(len(noConnect), "it", "them"))
	case len(native) > 0:
		return fmt.Errorf("%s signs %s in with mysql_native_password, which %s doesn't load unless told to, so they couldn't sign in after the move, and its image stays as it is. "+
			"Change them to caching_sha2_password first, with ALTER USER ... IDENTIFIED WITH caching_sha2_password", name, human.List(native), release)
	}
	return nil
}

func listed(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return " (" + human.List(names) + ")"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// atLeast says a release series, such as "8.4", is the given one or later.
func atLeast(series string, major, minor int) bool {
	a, b, _ := strings.Cut(series, ".")
	x, err := strconv.Atoi(a)
	if err != nil {
		return false
	}
	y, _ := strconv.Atoi(b)
	return x > major || x == major && y >= minor
}

// roomToMove refuses a move without room for its dump, where backups are kept, and
// for the new release's copy of the data, beside the volume: about as much as the
// volume holds now, and a margin. A disk that holds both is counted once, for both.
func (r *Runner) roomToMove(ctx context.Context, mv Move, dump int64) error {
	d, err := r.disk()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, measureWait)
	defer cancel()
	v, err := r.tool(ctx, volume.TargetOf(mv.from()), "measure", docker.Streams{})
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			r.logger().Warn("can't measure a volume", "volume", mv.Asked.From, "error", err.Error())
		}
		return nil
	}
	dump = max(dump, 0) + max(dump, 0)/margin
	copying := v.Bytes + v.Bytes/margin
	if v.Filesystem == d.filesystem {
		if d.free-dump-copying < d.reserve {
			return fmt.Errorf("the disk that holds %s and its backups has %s free, and moving it takes up to %s there, %s for its dump and %s for the new release's copy, leaving %s alone, so nothing was stopped. "+
				"Delete old backups or free some space, then try again", mv.Asked.From, human.Size(d.free), human.Size(dump+copying), human.Size(dump), human.Size(copying), human.Size(d.reserve))
		}
		return nil
	}
	if d.free-dump < d.reserve {
		return fmt.Errorf("the disk that holds backups has %s free, and %s's dump takes up to %s, leaving %s alone, so nothing was stopped. Delete old backups or free some space, then try again",
			human.Size(d.free), mv.Asked.From, human.Size(dump), human.Size(d.reserve))
	}
	if v.Free-copying < copying/margin {
		return fmt.Errorf("the disk that holds %s has %s free, and the new release's copy of its data takes up to %s, so nothing was stopped. Free some space on that disk, then try again",
			mv.Asked.From, human.Size(v.Free), human.Size(copying))
	}
	return nil
}

// MoveDatabase moves a database to a newer release, and calls replace for the step's
// own replace of its container, with its health check, once the new release has
// the data. Before replace, a move that fails changed nothing: what it stopped
// starts again, the new volume is emptied, and its dump goes. After, the new release
// stops where it runs, so nothing writes to the new volume, which is emptied too,
// and the rollback puts the old release back on the old volume.
func (r *Runner) MoveDatabase(ctx context.Context, mv Move, replace func(context.Context) error) error {
	from := mv.from()
	u, db, stops, err := r.moving(ctx, mv)
	if err != nil {
		return err
	}
	run := moveRun(mv.change())
	runs := strings.TrimSpace(db.kind.name + " " + engines.Series(engines.Recognise(db.image), db.image))
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
	loading := false
	unchanged := func(failed error) error {
		r.forgetDump(ctx, from, mv.Asked.Backup)
		if loading {
			failed = errors.Join(failed, r.emptyNew(ctx, mv))
		}
		return started(fmt.Errorf("%w. Nothing changed: %s runs %s on %s as before", failed, mv.Service.Name, runs, mv.Asked.From))
	}
	if db.running {
		if err := r.preflight(ctx, mv, db); err != nil {
			return err
		}
		p, err := r.Preview(ctx, from)
		if err != nil {
			return err
		}
		if err := r.roomToMove(ctx, mv, p.Estimate); err != nil {
			return err
		}
	}
	// Everything stops, the database too, and stays stopped through a crash, for
	// the change's resume to go on from.
	if err := r.stop(ctx, run, stops, true); err != nil {
		return unchanged(err)
	}
	// A dump from a run cut short goes: this one's is from after everything stopped.
	r.forgetDump(ctx, from, mv.Asked.Backup)
	resource.Stage(ctx, resource.PhaseBackingUp, "Dumping "+mv.Service.Name)
	m, before, err := r.dumpOld(ctx, mv, db)
	if err != nil {
		return unchanged(err)
	}
	loading = true
	if err := r.load(ctx, mv, m, before); err != nil {
		return unchanged(err)
	}
	if err := replace(ctx); err != nil {
		r.forgetDump(ctx, from, mv.Asked.Backup)
		if stopErr := r.stopOn(ctx, volume.TargetOf(mv.to())); stopErr != nil {
			err = fmt.Errorf("%w, and what runs on %s didn't stop: %w", err, mv.Asked.To, stopErr)
		} else if emptyErr := r.emptyNew(ctx, mv); emptyErr != nil {
			err = errors.Join(err, emptyErr)
		}
		// What the replace said in plain words stays, with what it means for the data.
		after := fmt.Sprintf("%s still holds what %s had, and the rollback puts that back", mv.Asked.From, runs)
		plain, _ := resource.Explain(err)
		return started(&resource.Failure{Plain: strings.TrimRight(strings.TrimSpace(plain), ".") + ". " + after + ".", Err: fmt.Errorf("%w. %s", err, after)})
	}
	return r.FinishMove(ctx, mv)
}

// dumpOld starts the old release in a container of its own, on no network, on the
// data as it was, and dumps it, saying what it held.
func (r *Runner) dumpOld(ctx context.Context, mv Move, db *database) (*Manifest, []string, error) {
	old, err := r.Client.InspectContainer(ctx, db.id)
	if err != nil {
		return nil, nil, fmt.Errorf("can't read %s's container: %w", mv.Service.Name, err)
	}
	if err := r.Paths.LeadsAsMade(ctx, old, nil); err != nil {
		return nil, nil, err
	}
	var mounts []docker.Mount
	for _, m := range old.Mounts {
		switch m.Type {
		case "volume":
			mounts = append(mounts, docker.Mount{Type: "volume", Source: m.Name, Target: m.Destination, ReadOnly: !m.RW})
		case "bind":
			mounts = append(mounts, docker.Mount{Type: "bind", Source: m.Source, Target: m.Destination, ReadOnly: !m.RW})
		}
	}
	// What the old release would run on its own while it's dumped, such as MySQL's
	// events, would land only on the old volume.
	cmd := slices.Clone(old.Config.Cmd)
	if db.kind.name == "MySQL" || db.kind.name == "MariaDB" {
		cmd = append(cmd, "--event-scheduler=OFF")
	}
	req := &docker.CreateRequest{
		Config: docker.Config{
			Image: old.Image, Env: old.Config.Env, Entrypoint: old.Config.Entrypoint, Cmd: cmd, User: old.Config.User,
			Labels: map[string]string{labelHelper: "1"},
		},
		HostConfig: docker.HostConfig{NetworkMode: "none", Mounts: mounts},
	}
	dumper, done, err := r.helperDatabase(ctx, db.kind, req, db.port, runs(db))
	if err != nil {
		return nil, nil, err
	}
	defer done()
	// The old release on its own data asks the same as the one that ran.
	if err := r.preflight(ctx, mv, dumper); err != nil {
		return nil, nil, err
	}
	before, err := r.census(ctx, dumper)
	if err != nil {
		return nil, nil, err
	}
	m, err := r.Take(ctx, Take{ID: mv.Asked.Backup, Ref: mv.from(), from: dumper})
	if err != nil {
		return nil, nil, fmt.Errorf("can't dump %s: %w", mv.Service.Name, err)
	}
	return m, before, nil
}

func runs(db *database) string {
	return strings.TrimSpace(db.kind.name + " " + engines.Series(engines.Recognise(db.image), db.image))
}

// helperDatabase makes and starts a database in a container of its own, a helper,
// and waits until it takes connections. done stops and removes it.
func (r *Runner) helperDatabase(ctx context.Context, k *databaseKind, req *docker.CreateRequest, port, name string) (*database, func(), error) {
	id, err := r.Client.CreateContainer(ctx, "dockwell-move-"+randomHex(), req)
	if err != nil {
		return nil, nil, fmt.Errorf("docker can't make the %s that moves the data: %w", name, err)
	}
	done := func() {
		cleanup, cancel := resource.Finishing(ctx, r.helperCleanup()+stopWait)
		defer cancel()
		if err := r.Client.StopContainer(cleanup, id, stopWait); err != nil && !docker.IsNotFound(err) {
			r.logger().Warn("can't stop what moved a database's data", "container", id, "error", err.Error())
		}
		if err := r.Client.RemoveContainer(cleanup, id, true); err != nil && !docker.IsNotFound(err) {
			r.logger().Warn("can't remove what moved a database's data; the agent removes it when it next starts", "container", id, "error", err.Error())
		}
	}
	db := &database{user: &user{id: id, name: name, stopWait: stopWait}, kind: k, image: req.Image, port: port}
	if err := r.Client.StartContainer(ctx, id); err != nil {
		done()
		return nil, nil, fmt.Errorf("docker can't start %s: %w", name, err)
	}
	if err := r.awaitReady(ctx, db); err != nil {
		done()
		return nil, nil, fmt.Errorf("%s didn't start: %w", name, err)
	}
	return db, done, nil
}

// census is what a database holds, as its kind counts it, sorted.
func (r *Runner) census(ctx context.Context, db *database) ([]string, error) {
	if db.kind.census == nil {
		return nil, nil
	}
	out, errs := &capture{}, &tail{}
	code, err := r.Client.Exec(ctx, db.id, db.tool(db.kind.census), docker.Streams{Stdout: out, Stderr: errs})
	switch {
	case err != nil:
		return nil, fmt.Errorf("can't ask %s what it holds: %w", db.kind.name, err)
	case code != 0:
		return nil, fmt.Errorf("can't ask %s what it holds: %s", db.kind.name, scrubbed(errs.String()))
	}
	var lines []string
	for line := range strings.Lines(out.String()) {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	slices.Sort(lines)
	return lines, nil
}

// capture keeps all of what's written, up to a bound a census keeps well under.
type capture struct{ buf []byte }

func (c *capture) Write(b []byte) (int, error) {
	if len(c.buf)+len(b) > 1<<20 {
		return 0, errors.New("it said more than a census does")
	}
	c.buf = append(c.buf, b...)
	return len(b), nil
}

func (c *capture) String() string { return string(c.buf) }

// differences says what the new release holds otherwise than the old one did, in a
// few of its census's lines.
func differences(before, after []string) string {
	var missing, more []string
	for _, l := range before {
		if !slices.Contains(after, l) {
			missing = append(missing, l)
		}
	}
	for _, l := range after {
		if !slices.Contains(before, l) {
			more = append(more, l)
		}
	}
	cut := func(l []string) []string { return l[:min(len(l), 3)] }
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "it lacks "+strings.Join(cut(missing), "; "))
	}
	if len(more) > 0 {
		parts = append(parts, "it has "+strings.Join(cut(more), "; "))
	}
	return strings.Join(parts, ", and ")
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

// moveMarker is where a move notes that it fills the new volume, so a run of the
// same move, and only that, empties it again.
func (r *Runner) moveMarker(mv Move) string {
	return filepath.Join(r.dir(mv.to()), "move-"+mv.change()+".json")
}

func (r *Runner) forgetMarker(mv Move) {
	if err := os.Remove(r.moveMarker(mv)); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.logger().Warn("can't forget a move's note on its new volume", "error", err.Error())
	}
}

// emptyNew empties the new volume of a move that didn't happen, rather than keep
// its copy for 7 days once the rollback deletes it.
func (r *Runner) emptyNew(ctx context.Context, mv Move) error {
	// Only what this move filled: one it refused to fill keeps what it holds.
	if _, err := os.Stat(r.moveMarker(mv)); err != nil {
		return nil
	}
	ctx, cancel := resource.Finishing(ctx, r.helperCleanup()+measureWait)
	defer cancel()
	if _, err := r.tool(ctx, volume.TargetOf(mv.to()), "empty", docker.Streams{}); err != nil {
		return fmt.Errorf("can't empty %s, which holds what the move loaded: %w", mv.Asked.To, err)
	}
	r.forgetMarker(mv)
	return nil
}

// load loads a dump into the new release on the new volume, in a loader of its
// own, brings it up to the new release's layout, and holds it to what the old
// release held. The new volume has to be one this move made, or this move filled
// before, with nothing else on it.
func (r *Runner) load(ctx context.Context, mv Move, m *Manifest, before []string) error {
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
	next := mv.release(k)
	if err := r.isNew(ctx, mv, to); err != nil {
		return err
	}
	// What a run cut short loaded goes first.
	resource.Progress(ctx, "Making the new volume "+mv.Asked.To+" ready")
	if err := os.MkdirAll(r.dir(mv.to()), 0o700); err != nil {
		return err
	}
	marker, err := json.Marshal(map[string]string{"change": mv.change(), "from": mv.Asked.From})
	if err != nil {
		return err
	}
	if err := writeFile(r.moveMarker(mv), marker); err != nil {
		return err
	}
	if _, err := r.tool(ctx, to, "empty", docker.Streams{}); err != nil {
		return fmt.Errorf("can't empty %s for the move: %w", mv.Asked.To, err)
	}
	app := mv.App
	env := make([]string, 0, len(app.GetEnv()))
	for _, k := range slices.Sorted(maps.Keys(app.GetEnv())) {
		env = append(env, k+"="+app.GetEnv()[k])
	}
	req := func(extra []string) *docker.CreateRequest {
		cmd := app.GetCommand()
		if len(extra) > 0 {
			cmd = append(slices.Clone(cmd), extra...)
			if len(app.GetCommand()) == 0 {
				// The image's own command, which a server's options follow.
				cmd = append([]string{k.serverCommand()}, extra...)
			}
		}
		return &docker.CreateRequest{
			Config: docker.Config{
				Image: app.GetImage(), Env: env, Entrypoint: app.GetEntrypoint(), Cmd: cmd, User: app.GetUser(),
				Labels: map[string]string{labelHelper: "1"},
			},
			HostConfig: docker.HostConfig{
				NetworkMode: "none",
				Mounts:      []docker.Mount{{Type: "volume", Source: to.Name, Target: mv.target()}},
			},
		}
	}
	port := ""
	if k.portFlags {
		port = portOf(app.GetCommand())
	}
	resource.Stage(ctx, resource.PhaseBackingUp, "Loading the dump into "+mv.Asked.To)
	loader, done, err := r.helperDatabase(ctx, k, req(nil), port, next)
	if err != nil {
		return fmt.Errorf("%w on %s", err, mv.Asked.To)
	}
	f, err := os.Open(filepath.Join(r.dir(mv.from()), m.ID+"."+m.Format))
	if err != nil {
		done()
		return err
	}
	defer f.Close()
	resource.Progress(ctx, "Loading the dump")
	if err := r.loadDump(ctx, loader, f); err != nil {
		done()
		return fmt.Errorf("the dump didn't load into %s: %w", next, err)
	}
	if k.upgrade != nil {
		resource.Progress(ctx, "Bringing what "+next+" loaded up to its own layout")
		errs := &tail{}
		code, err := r.Client.Exec(ctx, loader.id, loader.tool(k.upgrade), docker.Streams{Stderr: errs})
		if err == nil && code != 0 {
			err = fmt.Errorf("it exited with code %d: %s", code, scrubbed(errs.String()))
		}
		if err != nil {
			done()
			return fmt.Errorf("%s couldn't bring what it loaded up to its own layout: %w", next, err)
		}
	}
	done()
	// It has to start again on what it wrote, as the service's own container will,
	// and hold what the old release held.
	resource.Progress(ctx, "Checking "+next+" starts again on what it loaded")
	checker, done, err := r.helperDatabase(ctx, k, req(k.upgradeArgs), port, next)
	if err != nil {
		return fmt.Errorf("%s didn't start again on what it loaded: %w", next, err)
	}
	defer done()
	after, err := r.census(ctx, checker)
	if err != nil {
		return err
	}
	if !slices.Equal(before, after) {
		return fmt.Errorf("%s doesn't hold what %s held after the load: %s", next, mv.Asked.From, differences(before, after))
	}
	return nil
}

// isNew refuses filling a volume that something else has, or had: one a container
// other than a move's own mounts, or one with data in that no run of this move put
// there, as Docker takes on a volume of a name it has already.
func (r *Runner) isNew(ctx context.Context, mv Move, to volume.Target) error {
	u, err := r.users(ctx, to)
	if err != nil {
		return err
	}
	if len(u.list) > 0 {
		return fmt.Errorf("%s is mounted by %s, so Dockwell won't fill it with the move's copy. Nothing changed", mv.Asked.To, human.List(namesOf(u.list)))
	}
	if _, err := os.Stat(r.moveMarker(mv)); err == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, measureWait)
	defer cancel()
	v, err := r.tool(ctx, to, "measure", docker.Streams{})
	if err != nil {
		return fmt.Errorf("can't tell whether %s is empty, so Dockwell won't fill it: %w", mv.Asked.To, err)
	}
	if v.Bytes > 0 {
		return fmt.Errorf("%s already holds %s that this move didn't put there, so Dockwell won't empty it to fill it. Nothing changed", mv.Asked.To, human.Size(v.Bytes))
	}
	return nil
}

// serverCommand is what a kind's image runs as its server, which options follow.
func (k *databaseKind) serverCommand() string {
	switch k.name {
	case "PostgreSQL":
		return "postgres"
	case "MariaDB":
		return "mariadbd"
	case "MongoDB":
		return "mongod"
	}
	return "mysqld"
}
