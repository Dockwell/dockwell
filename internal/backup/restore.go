// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// beforePrefix starts the name of what a restore replaced.
const beforePrefix = "before-"

// How far a restore got, kept beside its copy of what was there, so a restore
// that was cut short goes on from there, and never copies over that copy.
const (
	stageCopied    = "copied"    // what was there is safe
	stageReplacing = "replacing" // what's in the volume is neither what was there nor the backup
	stageReplaced  = "replaced"  // the volume holds what the backup does
	stageFinished  = "finished"  // what stopped started again
	stageUndone    = "undone"    // it failed, and what was there went back
)

// stages orders them, for a restore on its way. It isn't on its way once it's undone.
var stages = []string{"", stageCopied, stageReplacing, stageReplaced, stageFinished}

func reached(have, want string) bool {
	return slices.Index(stages, have) >= slices.Index(stages, want)
}

// state is what's kept of a restore: what it copied, and how far it got.
type state struct {
	Manifest
	Change string `json:"change"`
	Stage  string `json:"stage"`
	// Ended is when it finished or undid itself: an undo can come for 7 days after
	// that, however long it took.
	Ended time.Time `json:"ended,omitzero"`
}

func readState(path string) (*state, error) {
	b, err := os.ReadFile(path) //nolint:gosec // under the agent's own directory
	if err != nil {
		return nil, err
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("can't read %s: %w", filepath.Base(path), err)
	}
	return &s, nil
}

func (r *Runner) statePath(ref resource.Ref, change string) string {
	return filepath.Join(r.dir(ref), beforePrefix+change+".json")
}

func (r *Runner) saveState(ref resource.Ref, s *state, stage string) error {
	s.Stage = stage
	if stage == stageFinished || stage == stageUndone {
		s.Ended = r.now().UTC()
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(r.statePath(ref, s.Change), b)
}

// stageOf says how far a change's restore got.
func (r *Runner) stageOf(ref resource.Ref, change string) (*state, error) {
	s, err := readState(r.statePath(ref, change))
	if errors.Is(err, os.ErrNotExist) {
		return &state{Change: change}, nil
	}
	return s, err
}

// source finds what a restore brings back, and says so if it's gone.
func (r *Runner) source(ref resource.Ref, src resource.RestoreSource) (*Manifest, string, error) {
	dir := r.dir(ref)
	if src.Undoes != "" {
		s, err := readState(r.statePath(ref, src.Undoes))
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", errors.New("this server no longer has what that restore replaced, as it keeps that for 7 days. Restore a backup instead")
		}
		if err != nil {
			return nil, "", err
		}
		if s.Stage == stageUndone {
			return nil, "", errors.New("that restore failed and put back what was there itself, so there's nothing to undo")
		}
		if !reached(s.Stage, stageCopied) {
			return nil, "", errors.New("that restore never finished copying what it replaced, so there's nothing to put back. Restore a backup instead")
		}
		return &s.Manifest, filepath.Join(dir, beforePrefix+src.Undoes+"."+formatArchive), nil
	}
	m, err := readManifest(filepath.Join(dir, src.Backup+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("this server no longer has the backup %s. It may have been removed by hand. Choose another", src.Backup)
	}
	if err != nil {
		return nil, "", err
	}
	file := filepath.Join(dir, m.ID+"."+m.Format)
	if fi, err := os.Stat(file); err != nil || fi.Size() != m.Size {
		return nil, "", fmt.Errorf("the file of the backup %s is missing or a different size from when it was taken. Choose another", src.Backup)
	}
	return m, file, nil
}

// kindNamed finds a known database by the name a manifest has.
func kindNamed(name string) *databaseKind {
	for _, k := range databaseKinds {
		if k.name == name {
			return k
		}
	}
	return nil
}

// stopsFor says what a restore stops, and the database it loads a dump into.
// A dump needs the database running to load, and a client mid-write breaks a
// load, so it takes in the rest of the app on this server.
func (r *Runner) stopsFor(ctx context.Context, t volume.Target, u *users, m *Manifest) ([]*user, *database, error) {
	stops := u.running()
	if m.Format != formatDump {
		return stops, nil, nil
	}
	db := u.database()
	if db == nil || db.kind.name != m.Database {
		return nil, nil, fmt.Errorf("this backup is a dump of %s, which loads into a running %s, and nothing on this server runs one with %s as its data. "+
			"Deploy the service that uses it, then plan this again", m.Database, m.Database, t.Key)
	}
	others, err := r.Client.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + t.Project}})
	if err != nil {
		return nil, nil, err
	}
	for i := range others {
		c := &others[i]
		if c.State != "running" || slices.ContainsFunc(stops, func(s *user) bool { return s.id == c.ID }) {
			continue
		}
		full, err := r.Client.InspectContainer(ctx, c.ID)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		stops = append(stops, userOf(full))
	}
	return stops, db, nil
}

// estimate is about how long a restore takes what it stops out of service: time
// to stop and start, then copying what's there and bringing the backup back, at a
// speed a small server keeps. A dump loads slower than files unpack.
func estimate(m *Manifest) time.Duration {
	speed := int64(20 << 20)
	if m.Format == formatDump {
		speed = 4 << 20
	}
	return time.Minute + 2*time.Duration(m.Size/speed)*time.Second
}

// JudgeBackup plans a backup as a step of a change, and reports false if it's been
// taken already. It stops what runs on the volume for an exact copy, as what follows
// it in a change usually replaces that anyway.
func (r *Runner) JudgeBackup(ctx context.Context, ref resource.Ref, id string) (resource.Decision, bool, error) {
	if _, err := readManifest(filepath.Join(r.dir(ref), id+".json")); err == nil {
		return resource.Decision{}, false, nil
	}
	p, err := r.Preview(ctx, ref)
	if err != nil {
		return resource.Decision{}, false, err
	}
	dec := resource.Decision{Action: resource.ActionUpdate, BackupFirst: &resource.BackupFirst{ID: id, Method: p.Method, Database: p.Database, Estimate: p.Estimate, Unmeasured: p.Unmeasured}}
	if p.Method == resource.BackupCopyRunning {
		dec.Effect = resource.EffectRestart
		dec.BackupFirst.Method = resource.BackupCopy
		dec.BackupFirst.Stops = p.Running
		dec.Downtime = time.Minute + 2*time.Duration(sizeBucket(p.Estimate)/(20<<20))*time.Second
	}
	return dec, true, nil
}

// sizeBucket rounds a size up to a power of two, so a plan's downtime doesn't move
// between planning and applying because a file grew.
func sizeBucket(size int64) int64 {
	if size <= 0 {
		return 0
	}
	return 1 << bits.Len64(uint64(size))
}

// ApplyBackup takes a step's backup, and says what it took.
func (r *Runner) ApplyBackup(ctx context.Context, step resource.Step) error {
	first := step.BackupFirst
	m, err := readManifest(filepath.Join(r.dir(step.Ref), first.ID+".json"))
	if err != nil {
		if m, err = r.Take(ctx, Take{ID: first.ID, Ref: step.Ref, Stop: true}); err != nil {
			return err
		}
	}
	resource.Report(ctx, resource.Outcome{Backup: &resource.BackupTaken{ID: m.ID, Method: m.Method, Database: m.Database, SHA256: m.SHA256, Size: m.Size, Changed: m.Changed}})
	return nil
}

// JudgeRestore plans a restore, and reports false if the change did it already.
func (r *Runner) JudgeRestore(ctx context.Context, ref resource.Ref, src resource.RestoreSource) (resource.Decision, bool, error) {
	if s, err := r.stageOf(ref, src.Change); err != nil {
		return resource.Decision{}, false, err
	} else if s.Stage == stageFinished {
		return resource.Decision{}, false, nil
	}
	m, _, err := r.source(ref, src)
	if err != nil {
		return resource.Decision{}, false, err
	}
	t := volume.TargetOf(ref)
	if _, err := r.Client.InspectVolume(ctx, t.Name); err != nil {
		return resource.Decision{}, false, r.noVolume(ref, err)
	}
	u, err := r.users(ctx, t)
	if err != nil {
		return resource.Decision{}, false, err
	}
	stops, _, err := r.stopsFor(ctx, t, u, m)
	if err != nil {
		return resource.Decision{}, false, err
	}
	dec := resource.Decision{
		Action: resource.ActionReplace, Effect: resource.EffectRestart, Destroys: true,
		Restore: &resource.RestorePlan{
			Backup: src.Backup, Undoes: src.Undoes, Method: m.Method, Database: m.Database, Taken: m.Taken, Size: m.Size, Stops: namesOf(stops),
		},
	}
	// A dump loads into any release. Files open only under the one that wrote them.
	if db := u.database(); db != nil && m.Format == formatArchive && m.Image != "" && !db.kind.sameSeries(m.Image, db.image) {
		dec.Restore.WrittenBy, dec.Restore.RunsOn = m.Image, db.image
	}
	if len(stops) > 0 {
		dec.Downtime = estimate(m)
	}
	return dec, true, nil
}

// ApplyRestore replaces what's in a volume with what a backup holds (ADR 0033).
// What was there goes to a copy first, and stays for KeepFor. A restore that fails
// puts that back and starts what it stopped, as a failed deploy rolls back. One cut
// short does the same, and its change starts it over from that copy.
func (r *Runner) ApplyRestore(ctx context.Context, step resource.Step, src resource.RestoreSource) error {
	ref, t := step.Ref, volume.TargetOf(step.Ref)
	release, err := r.hold(t)
	if err != nil {
		return err
	}
	defer release()
	st, err := r.stageOf(ref, src.Change)
	if err != nil {
		return err
	}
	if st.Stage == stageFinished {
		return nil
	}
	if st.Stage == stageUndone {
		st.Stage = stageCopied
	}
	run := "restore-" + src.Change
	began, err := r.began(st, run)
	if err != nil {
		return err
	}
	// A run before this one stopped things or took a copy, and nothing else will put
	// that right: whatever stops this one has to.
	u, db := &users{}, (*database)(nil)
	stopping := func(err error) error {
		if !began {
			return err
		}
		return r.giveBack(ctx, ref, t, st, run, u, db, err)
	}
	m, file, err := r.source(ref, src)
	if err != nil {
		return stopping(err)
	}
	verify := verifyArchive
	if m.Format == formatDump {
		k := kindNamed(m.Database)
		if k == nil {
			return stopping(fmt.Errorf("this backup is a dump of %s, which this agent can't load. Update it to the control plane's version", m.Database))
		}
		verify = verifyDump(k)
	}
	// Before anything stops: a bad backup costs nothing.
	resource.Progress(ctx, "Checking the backup")
	if err := check(file, m, verify); err != nil {
		if began {
			return stopping(fmt.Errorf("the backup is damaged: %w. Choose another", err))
		}
		return fmt.Errorf("the backup is damaged, so nothing was changed or stopped: %w. Choose another", err)
	}
	if u, err = r.users(ctx, t); err != nil {
		u = &users{}
		return stopping(err)
	}
	stops, found, err := r.stopsFor(ctx, t, u, m)
	if err != nil {
		return stopping(err)
	}
	db = found
	// Once there's a copy it has room, and a restore that used up the room it wanted
	// has nothing to gain from being refused.
	if st.Stage == "" {
		if err := r.roomToRestore(ctx, ref, t, m, began); err != nil {
			return stopping(err)
		}
	}
	if err := r.adopt(run, src.Undoes); err != nil {
		return stopping(err)
	}
	if err := r.stop(ctx, run, stops, true); err != nil {
		return r.giveBack(ctx, ref, t, st, run, u, db, err)
	}
	if err := r.replace(ctx, ref, t, st, m, file, db); err != nil {
		return r.giveBack(ctx, ref, t, st, run, u, db, err)
	}
	if err := r.startAll(ctx, run, u, db); err != nil {
		var down downList
		if errors.As(err, &down) {
			err = errors.New(down.why())
		}
		return r.giveBack(ctx, ref, t, st, run, u, db, err)
	}
	return r.saveState(ref, st, stageFinished)
}

// began says an earlier run of this restore stopped something or took a copy.
func (r *Runner) began(st *state, run string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list, err := r.readStopped()
	if err != nil {
		return false, err
	}
	_, stopped := list[run]
	return stopped || st.Stage != "", nil
}

// adopt takes on what the restore an undo undoes left stopped, as nothing else will
// start it.
func (r *Runner) adopt(run, undoes string) error {
	if undoes == "" {
		return nil
	}
	from := "restore-" + undoes
	return r.noteStopped(func(l stoppedFile) {
		if left, ok := l[from]; ok {
			l[run] = stoppedRun{Containers: append(l[run].Containers, left.Containers...), Restore: true}
			delete(l, from)
		}
	})
}

// giveBack ends a restore that failed. While the volume is as it was, that only
// starts what stopped. Otherwise what was there goes back first, on a context of its
// own, as a volume mustn't be left half replaced however the call ended, and what
// stopped starts on that. If it can't go back, what stopped stays stopped, as an app
// mustn't start on half its data, and the error says how to get out of that.
func (r *Runner) giveBack(ctx context.Context, ref resource.Ref, t volume.Target, st *state, run string, u *users, db *database, failed error) error {
	if st.Stage == stageReplacing || st.Stage == stageReplaced {
		if err := r.putBack(ctx, ref, t, st, run, db); err != nil {
			return fmt.Errorf("%w. Putting back what was there failed too: %w. What stopped stays stopped, as what's in %s may not be whole. "+
				"A copy of what was there is at %s on this server. Run dockwell changes undo %s to try again",
				failed, err, ref.Name, r.beforeFile(ref, st.Change), st.Change)
		}
	}
	// Nothing waits for its change now: if what stopped won't start, a later start of
	// the agent tries again.
	if err := r.noteStopped(func(l stoppedFile) {
		if left, ok := l[run]; ok {
			left.Restore = false
			l[run] = left
		}
	}); err != nil {
		return errors.Join(failed, err)
	}
	if err := r.startAll(ctx, run, u, db); err != nil {
		if st.Stage == stageUndone {
			return fmt.Errorf("%w. What was there is back in %s, and %w", failed, ref.Name, err)
		}
		return fmt.Errorf("%w. %s still holds what it held, and %w", failed, ref.Name, err)
	}
	if st.Stage == stageUndone {
		return fmt.Errorf("%w. What was there is back in %s, and what stopped started again", failed, ref.Name)
	}
	return fmt.Errorf("%w. %s still holds what it held, and what stopped started again", failed, ref.Name)
}

// beforeFile is the copy a restore took of what was there.
func (r *Runner) beforeFile(ref resource.Ref, change string) string {
	return filepath.Join(r.dir(ref), beforePrefix+change+"."+formatArchive)
}

// putBack stops whatever a failed restore started, and unpacks the copy it took.
func (r *Runner) putBack(ctx context.Context, ref resource.Ref, t volume.Target, st *state, run string, db *database) error {
	ctx, cancel := resource.Finishing(ctx, putBackWait)
	defer cancel()
	resource.Progress(ctx, "Putting back what was there")
	r.mu.Lock()
	list, err := r.readStopped()
	r.mu.Unlock()
	if err != nil {
		return err
	}
	ids := slices.Clone(list[run].Containers)
	if db != nil && !slices.Contains(ids, db.id) {
		ids = append(ids, db.id)
	}
	for _, id := range ids {
		if err := r.Client.StopContainer(ctx, id, stopWait); err != nil && !docker.IsNotFound(err) {
			return fmt.Errorf("can't stop %s: %w", shortID(id), err)
		}
	}
	if _, err := r.unpack(ctx, t, r.beforeFile(ref, st.Change)); err != nil {
		return err
	}
	return r.saveState(ref, st, stageUndone)
}

// replace does the part of a restore that touches the data, from where st says.
func (r *Runner) replace(ctx context.Context, ref resource.Ref, t volume.Target, st *state, m *Manifest, file string, db *database) error {
	if !reached(st.Stage, stageCopied) {
		resource.Progress(ctx, "Copying what's in "+ref.Name+" now, which the server keeps for 7 days")
		st.Manifest = Manifest{
			ID: beforePrefix + st.Change, Environment: ref.Scope, Volume: ref.Name, Format: formatArchive, Method: resource.BackupCopy, Taken: r.now().UTC(),
		}
		if err := os.MkdirAll(r.dir(ref), 0o700); err != nil {
			return fmt.Errorf("can't make a place for what's there now: %w", err)
		}
		err := r.write(ref, &st.Manifest, func(w io.Writer) (int64, error) {
			stats, err := r.tool(ctx, t, "pack", docker.Streams{Stdout: w})
			st.Unpacked = stats.Bytes
			return stats.Changed, err
		}, verifyArchive)
		if err != nil {
			return fmt.Errorf("can't copy what's in %s now, so nothing was replaced: %w", ref.Name, err)
		}
		if err := r.saveState(ref, st, stageCopied); err != nil {
			return err
		}
	}
	if reached(st.Stage, stageReplaced) {
		return nil
	}
	var err error
	if m.Format == formatDump {
		// Emptying it can't be taken back, so that's on record first.
		if err := r.saveState(ref, st, stageReplacing); err != nil {
			return err
		}
		err = r.replaceWithDump(ctx, t, file, db)
	} else {
		var swapping bool
		swapping, err = r.unpack(ctx, t, file)
		if swapping && err != nil {
			err = errors.Join(err, r.saveState(ref, st, stageReplacing))
		}
	}
	if err != nil {
		return err
	}
	return r.saveState(ref, st, stageReplaced)
}

// unpack replaces what's in a volume with an archive, and says whether it had begun
// moving what's there out when it stopped: until then, the volume is as it was.
func (r *Runner) unpack(ctx context.Context, t volume.Target, file string) (swapping bool, err error) {
	f, err := os.Open(file) //nolint:gosec // under the agent's own directory
	if err != nil {
		return false, err
	}
	defer f.Close()
	stats, err := r.tool(ctx, t, "unpack", docker.Streams{Stdin: f})
	if err != nil {
		return stats.Swapping, fmt.Errorf("can't unpack the backup into the volume: %w", err)
	}
	return true, nil
}

// replaceWithDump starts the database anew, on an empty volume, and loads the dump
// into it. It stops at the first thing that fails, for giveBack to put back the copy.
func (r *Runner) replaceWithDump(ctx context.Context, t volume.Target, file string, db *database) error {
	f, err := os.Open(file) //nolint:gosec // under the agent's own directory
	if err != nil {
		return err
	}
	defer f.Close()
	resource.Progress(ctx, "Making a new "+db.kind.name+" to load the dump into")
	if _, err := r.tool(ctx, t, "empty", docker.Streams{}); err != nil {
		return err
	}
	if err := r.startContainer(ctx, db.id); err != nil {
		return fmt.Errorf("can't start %s: %w", db.name, err)
	}
	if err := r.awaitReady(ctx, db); err != nil {
		return err
	}
	resource.Progress(ctx, "Loading the dump")
	if err := r.loadDump(ctx, db, f); err != nil {
		return fmt.Errorf("the dump didn't load: %w", err)
	}
	return nil
}

const (
	// startWait bounds starting what a run stopped, however it ended.
	startWait = 5 * time.Minute
	// putBackWait bounds putting back what a dump that wouldn't load replaced.
	putBackWait = time.Hour
)

// startAgain starts what a run stopped, whatever became of its context.
func (r *Runner) startAgain(ctx context.Context, run string, containers []*user) error {
	ctx, cancel := resource.Finishing(ctx, startWait)
	defer cancel()
	return r.start(ctx, run, containers)
}

// startAll starts what a run stopped, the database first, whatever became of its
// context, and forgets it.
func (r *Runner) startAll(ctx context.Context, run string, u *users, db *database) error {
	ctx, cancel := resource.Finishing(ctx, startWait)
	defer cancel()
	r.mu.Lock()
	list, err := r.readStopped()
	r.mu.Unlock()
	if err != nil {
		return err
	}
	var containers []*user
	if db != nil {
		containers = append(containers, db.user)
	}
	for _, id := range list[run].Containers {
		if slices.ContainsFunc(containers, func(c *user) bool { return c.id == id }) {
			continue
		}
		c := &user{id: id, name: shortID(id)}
		if i := slices.IndexFunc(u.list, func(k *user) bool { return k.id == id }); i >= 0 {
			c = u.list[i]
		}
		containers = append(containers, c)
	}
	return r.start(ctx, run, containers)
}
