// SPDX-License-Identifier: AGPL-3.0-only

// Package backup takes and restores backups of a server's volumes, on that
// server's disk. It reads a volume through a throwaway container that holds the
// volume and the agent's own binary, and a database through the database's own
// tools, and never touches what an app wrote from the host. See
// docs/adr/0033-backups-of-volumes-and-databases.md.
package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// KeepFor is how long a server keeps what a restore replaced, as it keeps what
// any destructive step removes.
const KeepFor = volume.KeepFor

// Formats of what a backup is on disk.
const (
	formatArchive = "tar.gz"
	formatDump    = "sql.gz"
)

// stopWait is how long what stops for a backup or a restore has to stop before
// Docker kills it. A database needs time to finish writing.
const stopWait = time.Minute

// Manifest is what's kept beside a backup, as <ID>.json, saying what it is, so
// it means something without the control plane.
type Manifest struct {
	ID          string `json:"id"`
	Environment string `json:"environment"`
	Volume      string `json:"volume"`
	Format      string `json:"format"`
	// Method is resource.BackupDump and so on.
	Method string `json:"method"`
	// Database names the database of a dump.
	Database string `json:"database,omitempty"`
	// Image is what wrote it, for a dump or a copy of one's files.
	Image string `json:"image,omitempty"`
	// Size is the size of the file, and SHA256 what it hashes to.
	Size   int64     `json:"size"`
	SHA256 string    `json:"sha256"`
	Taken  time.Time `json:"taken"`
	// Changed counts files that changed while they were read.
	Changed int64 `json:"changed,omitempty"`
	// Unpacked is how large it is once it's out of its file, for a restore to know
	// it fits.
	Unpacked int64 `json:"unpacked,omitempty"`
}

// Runner takes and restores backups on this server.
type Runner struct {
	Client *docker.Client
	// Root is where backups live, the agent's backups directory.
	Root string
	// Binary is what runs in the helper; /proc/self/exe unless set, which stays
	// right across an upgrade that replaces the file.
	Binary string
	// Now is the clock; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
	// CleanupWait bounds removing a helper, once its run has ended. Zero means a
	// minute.
	CleanupWait time.Duration
	// Disk stands in for the disk that holds backups, for a test.
	Disk func() disk

	mu      sync.Mutex
	working map[string]bool // volumes in Docker with a backup or restore under way
	// running are the runs under way that stopped something, which isn't for
	// Recover to start.
	running map[string]bool
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// hold takes a volume for one backup or restore, or says what's in the way.
func (r *Runner) hold(t volume.Target) (release func(), err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.working[t.Name] {
		return nil, fmt.Errorf("a backup or restore of %s is under way. Wait for it to end, then try again", t.Key)
	}
	if r.working == nil {
		r.working = map[string]bool{}
	}
	r.working[t.Name] = true
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.working, t.Name)
	}, nil
}

// dir is where a volume's backups live.
func (r *Runner) dir(ref resource.Ref) string {
	return filepath.Join(r.Root, ref.Scope, ref.Name)
}

// Preview is what backing up a volume would do.
type Preview struct {
	// Method is what it does: BackupDump, or BackupCopy when nothing runs on the
	// volume, or BackupCopyRunning when something does, unless it's asked to stop.
	Method string
	// Database names the database of a dump.
	Database string
	// Running names what runs on the volume now, as a plan does.
	Running []string
}

// Preview says what backing up a volume would do.
func (r *Runner) Preview(ctx context.Context, ref resource.Ref) (Preview, error) {
	t := volume.TargetOf(ref)
	if _, err := r.Client.InspectVolume(ctx, t.Name); err != nil {
		return Preview{}, r.noVolume(ref, err)
	}
	users, err := r.users(ctx, t)
	if err != nil {
		return Preview{}, err
	}
	p := Preview{Method: resource.BackupCopy}
	if db := users.database(); db != nil && db.running {
		p.Method, p.Database = resource.BackupDump, db.kind.name
	}
	running := users.running()
	p.Running = namesOf(running)
	if len(running) > 0 && p.Method == resource.BackupCopy {
		p.Method = resource.BackupCopyRunning
	}
	return p, nil
}

func (r *Runner) noVolume(ref resource.Ref, err error) error {
	if docker.IsNotFound(err) {
		return fmt.Errorf("docker has no volume %s on this server. Deploy the service that uses it first", ref.Name)
	}
	return err
}

// Take is a backup to take.
type Take struct {
	// ID is the backup's, which the control plane gave it.
	ID  string
	Ref resource.Ref
	// Stop stops what runs on the volume for an exact copy, and starts it again
	// after. A database's dump stops nothing.
	Stop bool
}

// Take takes a backup, and says what it is once it's whole on the disk. Nothing
// shows as a backup until it has been read back to its end.
func (r *Runner) Take(ctx context.Context, take Take) (*Manifest, error) {
	t := volume.TargetOf(take.Ref)
	release, err := r.hold(t)
	if err != nil {
		return nil, err
	}
	defer release()
	if _, err := r.Client.InspectVolume(ctx, t.Name); err != nil {
		return nil, r.noVolume(take.Ref, err)
	}
	users, err := r.users(ctx, t)
	if err != nil {
		return nil, err
	}
	if err := r.roomFor(ctx, take, t); err != nil {
		return nil, err
	}
	m := &Manifest{ID: take.ID, Environment: take.Ref.Scope, Volume: take.Ref.Name, Taken: r.now().UTC()}
	if err := os.MkdirAll(r.dir(take.Ref), 0o700); err != nil {
		return nil, fmt.Errorf("can't make a place for the backup: %w", err)
	}
	if db := users.database(); db != nil && db.running {
		m.Method, m.Format, m.Database, m.Image = resource.BackupDump, formatDump, db.kind.name, db.image
		err = r.write(take.Ref, m, func(w io.Writer) (int64, error) {
			m.Unpacked, err = r.dump(ctx, take.Ref, db, w)
			return 0, err
		}, verifyDump(db.kind))
	} else {
		err = r.copyVolume(ctx, take, t, users, m)
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// copyVolume takes a copy of a volume's files, with what runs on it stopped if
// that was asked for. What stopped starts again as soon as the copy ends, and
// whatever became of it.
func (r *Runner) copyVolume(ctx context.Context, take Take, t volume.Target, users *users, m *Manifest) (err error) {
	m.Format = formatArchive
	if db := users.database(); db != nil {
		m.Image = db.image
	}
	running := users.running()
	m.Method = resource.BackupCopy
	stopped := len(running) > 0 && take.Stop
	if len(running) > 0 && !take.Stop {
		m.Method = resource.BackupCopyRunning
	}
	startAgain := func() error {
		if !stopped {
			return nil
		}
		stopped = false
		return r.startAgain(ctx, take.ID, running)
	}
	defer func() { err = errors.Join(err, startAgain()) }()
	if stopped {
		if err := r.stop(ctx, take.ID, running, false); err != nil {
			return err
		}
	}
	return r.write(take.Ref, m, func(w io.Writer) (int64, error) {
		stats, err := r.tool(ctx, t, "pack", docker.Streams{Stdout: w})
		m.Unpacked = stats.Bytes
		// As soon as the copy ends: reading it back holds up nothing.
		return stats.Changed, errors.Join(err, startAgain())
	}, verifyArchive)
}

// verifier reads a backup back, and says whether it's whole.
type verifier func(io.Reader) error

// write takes a backup into a file beside where it belongs, reads it back, and
// only then gives it its name. A backup that fails leaves nothing.
func (r *Runner) write(ref resource.Ref, m *Manifest, take func(io.Writer) (int64, error), verify verifier) (err error) {
	dir := r.dir(ref)
	partial := filepath.Join(dir, m.ID+".partial")
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // under the agent's own directory, named by an ID
	if err != nil {
		return fmt.Errorf("can't write the backup: %w", err)
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(partial)
		}
	}()
	sum := sha256.New()
	counted := &counter{w: io.MultiWriter(f, sum), check: r.keepRoom}
	if m.Changed, err = take(counted); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("can't write the backup to the disk: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("can't write the backup to the disk: %w", err)
	}
	m.Size, m.SHA256 = counted.n, hex.EncodeToString(sum.Sum(nil))
	if err = check(partial, m, verify); err != nil {
		return fmt.Errorf("the backup didn't read back whole, so it isn't kept: %w. Try again, and check the server's disk if it happens again", err)
	}
	if err = writeManifest(filepath.Join(dir, m.ID+".json"), m); err != nil { //nolint:gocritic // the deferred check reads this err
		return err
	}
	if err = os.Rename(partial, filepath.Join(dir, m.ID+"."+m.Format)); err != nil {
		_ = os.Remove(filepath.Join(dir, m.ID+".json"))
		return fmt.Errorf("can't write the backup: %w", err)
	}
	return nil
}

// check reads a file through, against what its manifest says.
func check(path string, m *Manifest, verify verifier) error {
	f, err := os.Open(path) //nolint:gosec // under the agent's own directory
	if err != nil {
		return err
	}
	defer f.Close()
	sum := sha256.New()
	through := io.TeeReader(f, sum)
	if err := verify(through); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, through); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != m.SHA256 {
		return fmt.Errorf("it hashes to %s, and %s was written", got, m.SHA256)
	}
	return nil
}

type counter struct {
	w     io.Writer
	n     int64
	since int64
	check func() error
}

// checkEvery is how much is written between looks at the disk.
const checkEvery = 64 << 20

func (c *counter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	c.since += int64(n)
	if err == nil && c.since >= checkEvery && c.check != nil {
		c.since = 0
		err = c.check()
	}
	return n, err
}

func writeManifest(path string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, b)
}

// writeFile puts a small file in place whole.
func writeFile(path string, b []byte) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("can't write %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("can't write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func readManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path) //nolint:gosec // under the agent's own directory
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("can't read %s: %w", filepath.Base(path), err)
	}
	return &m, nil
}

// Delete removes a backup from the disk. One that's gone already is no error.
func (r *Runner) Delete(_ context.Context, ref resource.Ref, id string) error {
	if strings.ContainsAny(id, "/\\") || id == "" || strings.HasPrefix(id, ".") {
		return fmt.Errorf("%q isn't a backup's ID", id)
	}
	dir := r.dir(ref)
	for _, name := range []string{id + "." + formatArchive, id + "." + formatDump, id + ".json", id + ".partial"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("can't remove the backup: %w", err)
		}
	}
	return nil
}

// stoppedFile is what's stopped for a backup or restore under way, written down
// first: stopping what runs on a volume leaves it down if the agent dies before it
// starts it again.
type stoppedFile map[string]stoppedRun

// stoppedRun is what one run stopped.
type stoppedRun struct {
	Containers []string `json:"containers"`
	// Restore says a restore did, which goes on when its change resumes, so
	// nothing here starts them.
	Restore bool `json:"restore,omitempty"`
}

func (r *Runner) stoppedPath() string { return filepath.Join(r.Root, "stopped.json") }

// noteStopped changes the list of what's stopped, under r.mu.
func (r *Runner) noteStopped(change func(stoppedFile)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	list, err := r.readStopped()
	if err != nil {
		return err
	}
	change(list)
	if err := os.MkdirAll(r.Root, 0o700); err != nil {
		return fmt.Errorf("can't note what stops: %w", err)
	}
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return writeFile(r.stoppedPath(), b)
}

func (r *Runner) readStopped() (stoppedFile, error) {
	list := stoppedFile{}
	b, err := os.ReadFile(r.stoppedPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		return list, nil
	case err != nil:
		return nil, fmt.Errorf("can't read what a backup stopped: %w", err)
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("can't read what a backup stopped, in %s: %w. Start what's stopped by hand, then remove the file", r.stoppedPath(), err)
	}
	return list, nil
}

// stop stops containers, noting them first, beside what the run stopped before
// it died, if it did.
func (r *Runner) stop(ctx context.Context, run string, containers []*user, restore bool) error {
	r.mu.Lock()
	if r.running == nil {
		r.running = map[string]bool{}
	}
	r.running[run] = true
	r.mu.Unlock()
	err := r.noteStopped(func(l stoppedFile) {
		ids := l[run].Containers
		for _, c := range containers {
			if !slices.Contains(ids, c.id) {
				ids = append(ids, c.id)
			}
		}
		l[run] = stoppedRun{Containers: ids, Restore: restore}
	})
	if err != nil {
		return err
	}
	for _, c := range containers {
		resource.Progress(ctx, "Stopping "+c.name)
		if err := r.Client.StopContainer(ctx, c.id, c.stopWait); err != nil {
			return fmt.Errorf("can't stop %s: %w", c.name, err)
		}
	}
	return nil
}

// start starts containers, and forgets them once all of them are.
func (r *Runner) start(ctx context.Context, run string, containers []*user) error {
	var down downList
	for _, c := range containers {
		resource.Progress(ctx, "Starting "+c.name)
		if err := r.Client.StartContainer(ctx, c.id); err != nil && !docker.IsNotFound(err) {
			down = append(down, stayedDown{user: c, err: err})
		}
	}
	r.mu.Lock()
	delete(r.running, run)
	r.mu.Unlock()
	if len(down) > 0 {
		return down
	}
	return r.noteStopped(func(l stoppedFile) { delete(l, run) })
}

// stayedDown is something that wouldn't start again.
type stayedDown struct {
	*user
	err error
}

// downList is what wouldn't start again, and says how to start it: by the name
// Docker knows it by, which isn't a service's.
type downList []stayedDown

func (d downList) Error() string {
	out := make([]string, 0, len(d))
	for _, c := range d {
		out = append(out, fmt.Sprintf("%s, so it's down. Start it with docker start %s", c.why(), c.dockerName()))
	}
	return strings.Join(out, ". ")
}

// why says what went wrong, for what doesn't go on to say what to do about it.
func (d downList) why() string {
	out := make([]string, 0, len(d))
	for _, c := range d {
		out = append(out, c.why())
	}
	return strings.Join(out, ". ")
}

func (c stayedDown) why() string { return fmt.Sprintf("can't start %s: %v", c.name, c.err) }

// Recover clears what a run that died left: helpers and their images, unfinished
// files, and what it stopped, which starts again. A restore under way isn't started
// again here: its change resumes it, and what it stopped has to stay stopped till
// then, as does what a restore that couldn't put back what was there left stopped.
func (r *Runner) Recover(ctx context.Context) error {
	// Docker may only just be back, with a run started in the time that took.
	r.mu.Lock()
	idle := len(r.working) == 0
	r.mu.Unlock()
	if idle {
		if err := r.clearLeftovers(ctx); err != nil {
			return err
		}
	}
	r.mu.Lock()
	list, err := r.readStopped()
	active := maps.Clone(r.running)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	for _, run := range slices.Sorted(maps.Keys(list)) {
		if list[run].Restore || active[run] {
			continue
		}
		containers := make([]*user, 0, len(list[run].Containers))
		for _, id := range list[run].Containers {
			containers = append(containers, &user{id: id, name: shortID(id)})
		}
		r.logger().Warn("something stopped containers for a backup or restore, and they didn't start again, so the agent starts them now", slog.Int("containers", len(containers)))
		if err := r.start(ctx, run, containers); err != nil {
			return err
		}
	}
	return nil
}

// clearLeftovers removes the helpers, images and unfinished files of a run that died.
func (r *Runner) clearLeftovers(ctx context.Context) error {
	helpers, err := r.Client.ListContainers(ctx, map[string][]string{"label": {labelHelper}})
	if err != nil {
		return err
	}
	for i := range helpers {
		if err := r.Client.RemoveContainer(ctx, helpers[i].ID, true); err != nil && !docker.IsNotFound(err) {
			return err
		}
	}
	tags, err := r.Client.ImageTags(ctx, helperRepository)
	if err != nil {
		return err
	}
	for _, tag := range tags {
		if err := r.Client.RemoveImage(ctx, tag); err != nil {
			return err
		}
	}
	partials, err := filepath.Glob(filepath.Join(r.Root, "*", "*", "*.partial"))
	if err != nil {
		return err
	}
	for _, p := range partials {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func shortID(id string) string { return id[:min(len(id), 12)] }

// keepPast is how long what a restore replaced outlives its 7 days, as the control
// plane counts the undo window from a moment later than the agent does.
const keepPast = time.Hour

// Prune removes what restores replaced, once it's been kept for KeepFor from the
// end of the restore: until then, an undo that's offered still works.
func (r *Runner) Prune() error {
	found, err := filepath.Glob(filepath.Join(r.Root, "*", "*", beforePrefix+"*.json"))
	if err != nil {
		return err
	}
	for _, path := range found {
		st, err := readState(path)
		if err != nil {
			r.logger().Warn("can't read what a restore kept, so it stays", slog.String("path", path), slog.String("error", err.Error()))
			continue
		}
		since := st.Taken
		if st.Ended.After(since) {
			since = st.Ended
		}
		if r.now().Sub(since) < KeepFor+keepPast {
			continue
		}
		archive := strings.TrimSuffix(path, ".json") + "." + formatArchive
		if err := os.Remove(archive); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("can't remove what a restore kept: %w", err)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("can't remove what a restore kept: %w", err)
		}
		r.logger().Info("removed what a restore replaced, kept for 7 days", slog.String("path", archive))
	}
	return nil
}

// recoverEvery is how soon a Recover that couldn't reach Docker tries again.
const recoverEvery = time.Minute

// Keep recovers from what a run that died left, as soon as Docker answers, then
// prunes every hour until ctx is done.
func (r *Runner) Keep(ctx context.Context) {
	for ctx.Err() == nil {
		err := r.Recover(ctx)
		if err == nil {
			break
		}
		if !docker.IsUnavailable(err) && ctx.Err() == nil {
			r.logger().Warn("can't clear what a backup left yet; the agent tries again in a minute", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(recoverEvery):
		}
	}
	t := time.NewTicker(volume.PruneEvery)
	defer t.Stop()
	for {
		if err := r.Prune(); err != nil && ctx.Err() == nil {
			r.logger().Warn("can't remove what restores replaced; the agent tries again in an hour", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func randomHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
