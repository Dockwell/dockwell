// SPDX-License-Identifier: AGPL-3.0-only

// Package backup takes and restores backups of a server's volumes, on that
// server's disk. It reads a volume through a throwaway container that holds the
// volume and the agent's own binary, and a database through the database's own
// tools, and never touches what an app wrote from the host. See
// docs/adr/0033-backups-of-volumes-and-databases.md.
package backup

import (
	"cmp"
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
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// KeepFor is how long a server keeps what a restore replaced, as it keeps what
// any destructive step removes.
const KeepFor = volume.KeepFor

// Formats of what a backup is on disk.
const (
	formatArchive = "tar.gz"
	formatDump    = "sql.gz"
	// formatMongoDump is mongodump's archive, gzipped.
	formatMongoDump = "archive.gz"
)

// dumpFormats are what a database's own dump is kept as.
var dumpFormats = []string{formatDump, formatMongoDump}

// isDump says a backup is a database's own dump, which loads into a running one.
func isDump(format string) bool { return slices.Contains(dumpFormats, format) }

// formats are every format a backup is kept as.
var formats = append([]string{formatArchive}, dumpFormats...)

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
	// Reached is what its volume reached when it was taken that's as good as root.
	// A backup is tied to a volume by name alone, so what it holds can come back into
	// another of that name, and this is what says what it may hold.
	Reached []string `json:"reached,omitempty"`
}

// Runner takes and restores backups on this server.
type Runner struct {
	Client *docker.Client
	// Root is where backups live, the agent's backups directory.
	Root string
	// Paths follows server paths, to check where they lead before a container
	// starts again or a restore writes through one. Nil checks nothing.
	Paths *container.Paths
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
	// HTTP reaches a destination; http.DefaultClient unless set.
	HTTP *http.Client
	// Work is what a key takes for what this seals, for a test: a key takes over a
	// second under the race detector. Zero, as an agent has it, means seal.Iterations.
	Work int

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
	// Estimate is about how much a backup will take on the disk, in bytes, and zero
	// when that took too long to find out, which Unmeasured says. Free is what's free
	// there, and Reserve how much of that a backup leaves alone.
	Estimate, Free, Reserve int64
	Unmeasured              bool
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
	disk, err := r.disk()
	if err != nil {
		return Preview{}, err
	}
	p.Free, p.Reserve = disk.free, disk.reserve
	var known bool
	p.Estimate, known = r.estimate(ctx, Take{Ref: ref}, t)
	p.Unmeasured = !known
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
	v, err := r.Client.InspectVolume(ctx, t.Name)
	if err != nil {
		return nil, r.noVolume(take.Ref, err)
	}
	reached, err := r.reached(ctx, v)
	if err != nil {
		return nil, err
	}
	users, err := r.users(ctx, t)
	if err != nil {
		return nil, err
	}
	if err := r.roomFor(ctx, take, t); err != nil {
		return nil, err
	}
	m := &Manifest{ID: take.ID, Environment: take.Ref.Scope, Volume: take.Ref.Name, Taken: r.now().UTC(), Reached: reached}
	// Again once it's read, before the manifest says so: a link made while the bytes
	// were read is what they hold, and the backup holds whatever either says.
	reachedAfter := func() error {
		more, err := r.reached(ctx, v)
		for _, p := range more {
			if !slices.Contains(reached, p) {
				reached = append(reached, p)
			}
		}
		m.Reached = reached
		return err
	}
	if err := os.MkdirAll(r.dir(take.Ref), 0o700); err != nil {
		return nil, fmt.Errorf("can't make a place for the backup: %w", err)
	}
	if db := users.database(); db != nil && db.running {
		m.Method, m.Format, m.Database, m.Image = resource.BackupDump, db.kind.dumpFormat(), db.kind.name, db.image
		err = r.write(take.Ref, m, func(w io.Writer) (int64, error) {
			if m.Unpacked, err = r.dump(ctx, take.Ref, db, w); err != nil {
				return 0, err
			}
			return 0, reachedAfter()
		}, verifyDump(db.kind))
	} else {
		err = r.copyVolume(ctx, take, t, users, m, reachedAfter)
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// reached says what a volume reaches that's as good as root, where its path leads
// now, as a plan would have it.
func (r *Runner) reached(ctx context.Context, v *docker.Volume) ([]string, error) {
	var out []string
	access := (&volume.Driver{}).HostAccess(&resourcev1.Volume{Driver: v.Driver, Options: v.Options})
	for i := range access {
		a := access[i]
		// What a mount's device says, such as :/share on an NFS server, isn't a path here.
		if a.Kind == resource.AccessPath || a.Kind == resource.AccessDocker {
			leads, err := r.Paths.Follow(ctx, a.Path)
			if err != nil {
				return nil, err
			}
			a = a.Leading(leads[a.Path])
		}
		if a.AsGoodAsRoot() {
			out = append(out, cmp.Or(a.LeadsTo, a.Path))
		}
	}
	return out, nil
}

// copyVolume takes a copy of a volume's files, with what runs on it stopped if
// that was asked for. What stopped starts again as soon as the copy ends, and
// whatever became of it.
func (r *Runner) copyVolume(ctx context.Context, take Take, t volume.Target, users *users, m *Manifest, after func() error) (err error) {
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
		if err == nil {
			err = after()
		}
		// As soon as the copy ends: reading it back holds up nothing.
		return stats.Changed, errors.Join(err, startAgain())
	}, verifyArchive)
}

// verifier reads a backup back, and says whether it's whole.
type verifier func(io.Reader) error

// write takes a backup into a file beside where it belongs, reads it back, and
// only then gives it its name. A backup that fails leaves nothing.
func (r *Runner) write(ref resource.Ref, m *Manifest, take func(io.Writer) (int64, error), verify verifier) error {
	return r.writeChecked(ref, m, take, verify, "")
}

// writeChecked is write, for what has to hash to want before it takes a name.
func (r *Runner) writeChecked(ref resource.Ref, m *Manifest, take func(io.Writer) (int64, error), verify verifier, want string) (err error) {
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
	if want != "" && m.SHA256 != want {
		return fmt.Errorf("what came back hashes to %s, and %s was taken, so it isn't kept. Check what's in the storage", m.SHA256, want)
	}
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

// counter counts what's written, and looks at the disk every so often.
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

// Delete removes a backup from the disk, and with a key and a destination, from where
// it was sent. Going from one doesn't wait on the other: storage that refuses leaves
// the disk's copy gone, and says why in what it returns.
func (r *Runner) Delete(ctx context.Context, ref resource.Ref, id, key string, d *Destination) (copyError string, err error) {
	if strings.ContainsAny(id, "/\\") || id == "" || strings.HasPrefix(id, ".") {
		return "", fmt.Errorf("%q isn't a backup's ID", id)
	}
	dir := r.dir(ref)
	names := []string{id + ".json", id + ".partial"}
	for _, format := range formats {
		names = append(names, id+"."+format)
	}
	for _, name := range names {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("can't remove the backup: %w", err)
		}
	}
	if d != nil && key != "" {
		if err := r.deleteRemote(ctx, key, d); err != nil {
			return err.Error(), nil //nolint:nilerr // the server's copy is gone, and what kept the other is said, not failed on
		}
	}
	return "", nil
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

// startContainer starts a container this stopped, as long as its server paths lead
// where they did. Nothing plans this, so it's all that stands between a link made
// meanwhile and what Docker would follow.
func (r *Runner) startContainer(ctx context.Context, id string) error {
	c, err := r.Client.InspectContainer(ctx, id)
	if err != nil {
		return err
	}
	if err := r.Paths.LeadsAsMade(ctx, c, nil); err != nil {
		return err
	}
	return r.Client.StartContainer(ctx, id)
}

// start starts containers, and forgets them once all of them are.
func (r *Runner) start(ctx context.Context, run string, containers []*user) error {
	var down downList
	for _, c := range containers {
		resource.Progress(ctx, "Starting "+c.name)
		if err := r.startContainer(ctx, c.id); err != nil && !docker.IsNotFound(err) {
			down = append(down, stayedDown{user: c, err: err})
		}
	}
	r.mu.Lock()
	delete(r.running, run)
	r.mu.Unlock()
	// What a moved path keeps down stays down, kept from starting by itself, so a run
	// with nothing else down is done: trying it again would only try that again.
	if len(down) > 0 && !down.held() {
		return down
	}
	if err := r.noteStopped(func(l stoppedFile) { delete(l, run) }); err != nil {
		return err
	}
	if len(down) > 0 {
		return down
	}
	return nil
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
		var moved *container.PathMovedError
		if errors.As(c.err, &moved) {
			out = append(out, c.why())
			continue
		}
		out = append(out, fmt.Sprintf("%s, so it's down. Start it with docker start %s", c.why(), c.dockerName()))
	}
	return strings.Join(out, ". ")
}

// held reports that only moved paths keep these down.
func (d downList) held() bool {
	for _, c := range d {
		var moved *container.PathMovedError
		if !errors.As(c.err, &moved) {
			return false
		}
	}
	return true
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
	var errs []error
	for _, run := range slices.Sorted(maps.Keys(list)) {
		if list[run].Restore || active[run] {
			continue
		}
		containers := make([]*user, 0, len(list[run].Containers))
		for _, id := range list[run].Containers {
			containers = append(containers, &user{id: id, name: shortID(id)})
		}
		r.logger().Warn("something stopped containers for a backup or restore, and they didn't start again, so the agent starts them now", slog.Int("containers", len(containers)))
		err := r.start(ctx, run, containers)
		var down downList
		switch {
		case err == nil:
		case errors.As(err, &down) && down.held():
			// Each run on its own: what a moved path keeps down holds up no other run.
			r.logger().Warn("what a backup or restore stopped stays down, as a server path it mounts leads somewhere nobody approved", slog.String("run", run), slog.String("error", err.Error()))
		default:
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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

// Keep recovers from what a run that died left, as soon as Docker answers, and prunes
// every hour until ctx is done, whatever came of that.
func (r *Runner) Keep(ctx context.Context) {
	go r.keepPruned(ctx)
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
}

// keepPruned prunes every hour until ctx is done.
func (r *Runner) keepPruned(ctx context.Context) {
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
