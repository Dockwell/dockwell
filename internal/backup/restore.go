// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	stageCopied   = "copied"   // what was there is safe
	stageReplaced = "replaced" // the volume holds what the backup does
	stageFinished = "finished" // what stopped started again
)

// stages orders them.
var stages = []string{"", stageCopied, stageReplaced, stageFinished}

func reached(have, want string) bool {
	return slices.Index(stages, have) >= slices.Index(stages, want)
}

// state is what's kept of a restore: what it copied, and how far it got.
type state struct {
	Manifest
	Change string `json:"change"`
	Stage  string `json:"stage"`
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
	if len(stops) > 0 {
		dec.Downtime = estimate(m)
	}
	return dec, true, nil
}

// ApplyRestore replaces what's in a volume with what a backup holds (ADR 0033).
// What was there goes to a copy first, and stays for KeepFor. A restore cut short
// goes on from where it got to, and what it stopped starts again however it ends.
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
	m, file, err := r.source(ref, src)
	if err != nil {
		return err
	}
	verify := verifyArchive
	if m.Format == formatDump {
		k := kindNamed(m.Database)
		if k == nil {
			return fmt.Errorf("this backup is a dump of %s, which this agent can't load. Update it to the control plane's version", m.Database)
		}
		verify = verifyDump(k)
	}
	// Before anything stops: a bad backup costs nothing.
	resource.Progress(ctx, "Checking the backup")
	if err := check(file, m, verify); err != nil {
		return fmt.Errorf("the backup is damaged, so nothing was changed or stopped: %w. Choose another", err)
	}
	u, err := r.users(ctx, t)
	if err != nil {
		return err
	}
	stops, db, err := r.stopsFor(ctx, t, u, m)
	if err != nil {
		return err
	}
	run := "restore-" + src.Change
	if err := r.stop(ctx, run, stops, true); err != nil {
		return errors.Join(err, r.startAll(ctx, run, u, db))
	}
	if err := r.replace(ctx, ref, t, st, m, file, db); err != nil {
		return errors.Join(err, r.startAll(ctx, run, u, db))
	}
	if err := r.startAll(ctx, run, u, db); err != nil {
		return fmt.Errorf("the volume is restored, and what stopped didn't all start: %w", err)
	}
	return r.saveState(ref, st, stageFinished)
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
		err = r.replaceWithDump(ctx, ref, t, st, file, db)
	} else {
		err = r.unpack(ctx, t, file)
	}
	if err != nil {
		return err
	}
	return r.saveState(ref, st, stageReplaced)
}

// unpack replaces what's in a volume with an archive.
func (r *Runner) unpack(ctx context.Context, t volume.Target, file string) error {
	f, err := os.Open(file) //nolint:gosec // under the agent's own directory
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := r.tool(ctx, t, "unpack", docker.Streams{Stdin: f}); err != nil {
		return fmt.Errorf("can't unpack the backup into the volume: %w", err)
	}
	return nil
}

// replaceWithDump starts the database anew, on an empty volume, and loads the
// dump into it. If that fails, it puts back the copy it took.
func (r *Runner) replaceWithDump(ctx context.Context, ref resource.Ref, t volume.Target, st *state, file string, db *database) error {
	f, err := os.Open(file) //nolint:gosec // under the agent's own directory
	if err != nil {
		return err
	}
	defer f.Close()
	resource.Progress(ctx, "Making a new "+db.kind.name+" to load the dump into")
	load := func() error {
		if _, err := r.tool(ctx, t, "empty", docker.Streams{}); err != nil {
			return err
		}
		if err := r.Client.StartContainer(ctx, db.id); err != nil {
			return fmt.Errorf("can't start %s: %w", db.name, err)
		}
		if err := r.awaitReady(ctx, db); err != nil {
			return err
		}
		resource.Progress(ctx, "Loading the dump")
		return r.loadDump(ctx, db, f)
	}
	failed := load()
	if failed == nil {
		return nil
	}
	if ctx.Err() != nil {
		// Out of time or told to stop, it's for a resume to go on with.
		return failed
	}
	resource.Progress(ctx, "Putting back what was there")
	back, cancel := resource.Finishing(ctx, putBackWait)
	defer cancel()
	if err := r.Client.StopContainer(back, db.id, db.stopWait); err != nil {
		return errors.Join(failed, fmt.Errorf("can't stop %s to put back what was there: %w", db.name, err))
	}
	before := filepath.Join(r.dir(ref), beforePrefix+st.Change+"."+formatArchive)
	if err := r.unpack(back, t, before); err != nil {
		return errors.Join(failed, fmt.Errorf("and putting back what was there failed too, so the volume may be empty. The copy of what was there is at %s: %w", before, err))
	}
	return fmt.Errorf("the dump didn't load: %w. The volume holds what it held before", failed)
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
