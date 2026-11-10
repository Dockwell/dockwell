// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// What a server keeps of the containers a change removes, as Docker removes a
// container's log with it. See docs/adr/0025-logs-per-deploy.md.
const (
	// Budget is the most the archive takes of a server's disk.
	Budget = 256 << 20
	// segmentBytes is how much of a log, uncompressed, one file holds.
	segmentBytes = 4 << 20
	// maxSegments is how many of a container's newest files are kept, so
	// at most 32 MiB of its newest lines.
	maxSegments = 8
	// RetainFor is how long a kept log stays at most. Logs may hold secrets,
	// so they don't stay for ever just because the budget has room.
	RetainFor = 30 * 24 * time.Hour
	// minFreeBytes keeps the archive off a nearly full disk, which is what a
	// failed deploy may be about.
	minFreeBytes = 1 << 30
	// copyTail is how many of a container's newest lines it copies.
	copyTail = 400_000
	// copyWait bounds copying a log while the container still runs.
	copyWait = 10 * time.Second
	// catchUpTail is how many lines the copy looks back after a stop.
	catchUpTail = 20_000
)

// Kept is what the archive records of a removed container, beside its lines.
type Kept struct {
	ContainerID string    `json:"container_id"`
	Name        string    `json:"name"`
	Project     string    `json:"project"`
	Service     string    `json:"service"`
	Number      int       `json:"number"`
	Revision    int64     `json:"revision,omitempty"`
	Change      string    `json:"change,omitempty"`
	Image       string    `json:"image,omitempty"`
	Created     time.Time `json:"created"`
	Started     time.Time `json:"started,omitzero"`
	Finished    time.Time `json:"finished,omitzero"`
	ExitCode    int       `json:"exit_code,omitempty"`
	Failed      bool      `json:"failed,omitempty"`
	KeptAt      time.Time `json:"kept_at"`
	// CutShort says the server doesn't have all of what the container wrote.
	CutShort bool `json:"cut_short,omitempty"`
	// HoleFrom and HoleTo bound what's missing from the middle of a log, when
	// the container wrote more while it stopped than the last copy looks back.
	HoleFrom time.Time `json:"hole_from,omitzero"`
	HoleTo   time.Time `json:"hole_to,omitzero"`
	Lines    int64     `json:"lines"`
	// Disk is what its files take, compressed.
	Disk int64 `json:"disk"`
	// dir is where its files are.
	dir string
	// hidden says a deletion put it out of reach.
	hidden bool
}

// slot names what a kept log competes with to stay: a service keeps one for
// each slot. A rollout has two containers only while it moves, and one at rest, so
// this holds through one. Replicas, when they come, run several, and then this names
// the replica too, or what is kept is only one replica's.
func (k *Kept) slot() string {
	if k.Failed {
		return "failed"
	}
	return "replaced"
}

// wentBefore reports a container that had stopped before t, by the time it says
// it finished or, if it couldn't tell, when it was kept.
func (k *Kept) wentBefore(t time.Time) bool {
	at := k.Finished
	if at.IsZero() {
		at = k.KeptAt
	}
	return at.Before(t)
}

// Archive keeps the logs of containers a change removed, in a directory only
// its owner can read, as they may hold secrets. A failure in here never stops
// a change: it is logged, and the logs aren't kept.
type Archive struct {
	Dir    string
	Client *docker.Client
	Logger *slog.Logger
	// Budget is the most it takes of the disk; zero means the Budget constant.
	Budget int64
	// Free says how much of the disk is free; nil asks the system.
	Free func(dir string) (uint64, error)
	// Now is the time; nil means time.Now.
	Now func() time.Time
	// CopyWait bounds copying a log while its container still runs; zero means
	// the copyWait constant.
	CopyWait time.Duration

	mu sync.Mutex // held while what is kept is pruned, or told what is out of reach
	// forgotten is what the control plane last said a deletion put out of reach,
	// by service.
	forgotten map[string]*agentv1.ForgottenService

	pruning sync.WaitGroup
}

var _ container.LogKeeper = (*Archive)(nil)

func (a *Archive) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Archive) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

func (a *Archive) budget() int64 {
	if a.Budget > 0 {
		return a.Budget
	}
	return Budget
}

func (a *Archive) free() (uint64, error) {
	if a.Free != nil {
		return a.Free(a.Dir)
	}
	var s syscall.Statfs_t
	if err := syscall.Statfs(a.Dir, &s); err != nil {
		return 0, err
	}
	return s.Bavail * uint64(s.Bsize), nil //nolint:gosec // block sizes aren't negative
}

// safeName refuses a name that would leave the directory it goes in. Names
// here come from labels Dockwell set on what it owns, but a label can be
// written by anyone with Docker.
func safeName(name string) bool {
	return name != "" && !strings.HasPrefix(name, ".") && !strings.ContainsAny(name, `/\`) && !strings.ContainsRune(name, 0)
}

func (a *Archive) serviceDir(project, service string) (string, bool) {
	if !safeName(project) || !safeName(service) {
		return "", false
	}
	return filepath.Join(a.Dir, project, service), true
}

// keep is one container's log on its way into the archive.
type keep struct {
	a    *Archive
	meta Kept
	dir  string // its directory, under a temporary name until it is done
	seg  *segmentWriter
	// segments are the files written so far, oldest first.
	segments []string
	next     int
	// last is the last line copied: its time, and how many have that time,
	// for catching up from there.
	last      time.Time
	lastCount int
	// outOfTime says the first copy's time was up before it reached the end of
	// the log. Nothing is missing for that alone: the last copy carries on from
	// where it stopped, and says whether it got there.
	outOfTime bool
}

// Begin copies what the container's log holds, while it still runs, so the
// step's downtime doesn't wait for it. It returns what Finish takes in.
func (a *Archive) Begin(ctx context.Context, c *docker.Container, failed bool) container.LogKeep {
	k := &keep{a: a, meta: keptOf(c, failed)}
	dir, ok := a.serviceDir(k.meta.Project, k.meta.Service)
	if !ok {
		a.logger().Warn("won't keep a container's log, as its labels don't name a directory", slog.String("container", c.Name))
		return nil
	}
	k.dir = filepath.Join(dir, ".copying-"+k.meta.ContainerID)
	if err := a.makeRoom(k.dir, dir); err != nil {
		a.logger().Warn("can't keep a container's log", slog.String("container", c.Name), slog.String("error", err.Error()))
		return nil
	}
	// Asked now that the directory exists, as a server's first kept log has
	// nowhere to ask about until then.
	free, err := a.free()
	if err != nil {
		a.logger().Warn("can't tell how full the disk is, so the server doesn't keep this container's log", slog.String("container", c.Name), slog.String("error", err.Error()))
		_ = os.RemoveAll(k.dir)
		return nil
	}
	if free < minFreeBytes {
		a.logger().Warn("the disk is nearly full, so the server doesn't keep this container's log",
			slog.String("container", c.Name), slog.Uint64("free_bytes", free))
		_ = os.RemoveAll(k.dir)
		return nil
	}
	wait := a.CopyWait
	if wait == 0 {
		wait = copyWait
	}
	copyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)
	defer cancel()
	// One more than it keeps, to know whether there are more.
	got, _, err := k.copy(copyCtx, c, docker.LogOptions{Tail: copyTail + 1})
	if got > copyTail {
		k.meta.CutShort = true
	}
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded):
		k.outOfTime = true
	default:
		k.meta.CutShort = true
		a.logger().Warn("can't read all of a container's log to keep it", slog.String("container", c.Name), slog.String("error", err.Error()))
	}
	return k
}

// makeRoom makes a new copy's directory, empty and owner-only. It waits for a
// prune under way, which could otherwise remove the service's directory, empty,
// as this makes its way down to it. This runs while the container still runs,
// so that takes no downtime.
func (a *Archive) makeRoom(copying, dir string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.RemoveAll(copying); err != nil {
		return err
	}
	if err := os.MkdirAll(copying, 0o700); err != nil {
		return err
	}
	return a.chmodParents(dir)
}

// chmodParents keeps the whole tree owner-only, whatever the umask made it.
func (a *Archive) chmodParents(dir string) error {
	for p := dir; len(p) >= len(a.Dir); p = filepath.Dir(p) {
		if err := os.Chmod(p, 0o700); err != nil { //nolint:gosec // only its owner can use it, which it must to hold logs
			return err
		}
		if p == a.Dir {
			break
		}
	}
	return nil
}

func keptOf(c *docker.Container, failed bool) Kept {
	l := c.Config.Labels
	number, _ := strconv.Atoi(l[docker.LabelNumber])
	revision, _ := strconv.ParseInt(l[container.LabelRevision], 10, 64)
	return Kept{
		ContainerID: shortID(c.ID),
		Name:        container.PlainName(c.Name),
		Project:     l[docker.LabelProject],
		Service:     l[docker.LabelService],
		Number:      max(number, 1),
		Revision:    revision,
		Change:      l[container.LabelChange],
		Image:       c.Config.Image,
		Created:     c.Created,
		Started:     c.State.StartedAt,
		Failed:      failed,
	}
}

// shortID is what names a source: the first 12 digits of a container's ID.
func shortID(id string) string { return id[:min(12, len(id))] }

// copy reads the container's log into its files, from the last line copied. It
// says how many lines it read, and the time of the first new one.
func (k *keep) copy(ctx context.Context, c *docker.Container, o docker.LogOptions) (int, time.Time, error) {
	o.Since = k.last
	rd, err := k.a.Client.ContainerLogs(ctx, c.ID, c.Config.Tty, o)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer rd.Close()
	// Docker's since takes lines of exactly that time, and the copy before this
	// one has the first of them. Lines within one copy can come a little out of
	// order, or share a time, and all of them count.
	from, skip := k.last, k.lastCount
	got := 0
	var first time.Time
	for {
		e, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return got, first, k.closeSegment()
		}
		if err != nil {
			_ = k.closeSegment()
			return got, first, err
		}
		got++
		t, ok := docker.ParseLogTime(e.Time)
		if !ok {
			continue
		}
		if t.Before(from) {
			continue
		}
		if t.Equal(from) && skip > 0 {
			skip--
			continue
		}
		switch {
		case t.After(k.last):
			k.last, k.lastCount = t, 1
		case t.Equal(k.last):
			k.lastCount++
		}
		if first.IsZero() {
			first = t
		}
		if err := k.write(t, e); err != nil {
			return got, first, err
		}
	}
}

// write adds a line to the file under way, and starts another once it holds
// enough, dropping the oldest once there are too many: what is kept is the
// newest, which is what a reader of a failure needs.
func (k *keep) write(t time.Time, e *docker.LogEntry) error {
	if k.seg == nil {
		k.next++
		name := filepath.Join(k.dir, fmt.Sprintf("%06d.gz", k.next))
		seg, err := newSegment(name)
		if err != nil {
			return err
		}
		k.seg = seg
		k.segments = append(k.segments, name)
	}
	if err := k.seg.line(t, e); err != nil {
		return err
	}
	k.meta.Lines++
	if k.seg.bytes >= segmentBytes {
		if err := k.closeSegment(); err != nil {
			return err
		}
		if len(k.segments) > maxSegments {
			old := k.segments[0]
			k.segments = k.segments[1:]
			if err := os.Remove(old); err != nil {
				return err
			}
			k.meta.CutShort = true
		}
	}
	return nil
}

func (k *keep) closeSegment() error {
	if k.seg == nil {
		return nil
	}
	seg := k.seg
	k.seg = nil
	return seg.close()
}

// Finish takes in what the stopped container wrote since Begin, then moves its
// log into place. What the archive then has no room for goes on its own time, as
// this runs while the service is down.
func (k *keep) Finish(ctx context.Context, c *docker.Container) {
	a := k.a
	before := k.last
	// One more than it looks back, to know whether it reached the last copy.
	got, first, err := k.copy(ctx, c, docker.LogOptions{Tail: catchUpTail + 1})
	if got > catchUpTail {
		// It wrote more while it stopped than this looks back: the lines
		// between the two copies are gone, and a marker says where.
		k.meta.CutShort = true
		k.meta.HoleFrom, k.meta.HoleTo = before, first
	}
	if err != nil {
		k.meta.CutShort = true
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			a.logger().Warn("can't read a stopped container's last lines to keep them", slog.String("container", c.Name), slog.String("error", err.Error()))
		}
	}
	// A first copy that ran out of time never learned whether the log held more
	// than it copies. With more lines kept than that, older ones may be missing,
	// and with no more, this copy has just read all there were from where the
	// first one stopped.
	if k.outOfTime && k.meta.Lines > copyTail {
		k.meta.CutShort = true
	}
	_ = k.closeSegment()
	k.meta.Finished = c.State.FinishedAt
	k.meta.ExitCode = c.State.ExitCode
	if k.meta.Started.IsZero() {
		k.meta.Started = c.State.StartedAt
	}
	k.meta.KeptAt = a.now()
	if err := k.commit(); err != nil {
		a.logger().Warn("can't keep a container's log", slog.String("container", c.Name), slog.String("error", err.Error()))
		_ = os.RemoveAll(k.dir)
		return
	}
	a.pruning.Go(func() {
		if err := a.Prune(); err != nil {
			a.logger().Warn("can't prune the logs kept of removed containers", slog.String("error", err.Error()))
		}
	})
}

// WaitPruned waits for what Finish left pruning on its own time.
func (a *Archive) WaitPruned() { a.pruning.Wait() }

// commit writes the record and moves the log into place.
func (k *keep) commit() error {
	for _, name := range k.segments {
		info, err := os.Stat(name)
		if err != nil {
			return err
		}
		k.meta.Disk += info.Size()
	}
	b, err := json.Marshal(k.meta)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(k.dir, "kept.json"), b, 0o600); err != nil {
		return err
	}
	final := filepath.Join(filepath.Dir(k.dir), k.meta.ContainerID)
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	return os.Rename(k.dir, final)
}

// segmentWriter writes one compressed file of lines, as
// "<time> <o or e> <bytes cut> <text>\n".
type segmentWriter struct {
	f     *os.File
	gz    *gzip.Writer
	w     *bufio.Writer
	lines int
	bytes int
}

func newSegment(name string) (*segmentWriter, error) {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // a path of the archive's own
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &segmentWriter{f: f, gz: gz, w: bufio.NewWriterSize(gz, 64<<10)}, nil
}

func (s *segmentWriter) line(t time.Time, e *docker.LogEntry) error {
	var scratch [64]byte
	b := t.UTC().AppendFormat(scratch[:0], "2006-01-02T15:04:05.000000000Z")
	b = append(b, ' ')
	if e.Stream == docker.Stderr {
		b = append(b, 'e')
	} else {
		b = append(b, 'o')
	}
	b = append(b, ' ')
	b = strconv.AppendInt(b, int64(e.Cut), 10)
	b = append(b, ' ')
	if _, err := s.w.Write(b); err != nil {
		return err
	}
	if _, err := s.w.Write(e.Text); err != nil {
		return err
	}
	s.lines++
	s.bytes += len(e.Text) + len(b) + 1
	return s.w.WriteByte('\n')
}

func (s *segmentWriter) close() error {
	return errors.Join(s.w.Flush(), s.gz.Close(), s.f.Close())
}

// List returns what the archive keeps of a service, newest first.
func (a *Archive) List(project, service string) []*Kept {
	dir, ok := a.serviceDir(project, service)
	if !ok {
		return nil
	}
	return a.listIn(dir)
}

func (a *Archive) listIn(dir string) []*Kept {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []*Kept
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		k, err := readKept(filepath.Join(dir, e.Name()))
		if err != nil {
			a.logger().Warn("can't read a kept log's record, so it can't be read back", slog.String("dir", e.Name()), slog.String("error", err.Error()))
			continue
		}
		out = append(out, k)
	}
	slices.SortFunc(out, func(x, y *Kept) int { return y.KeptAt.Compare(x.KeptAt) })
	return out
}

// errBrokenRecord means a kept log's record is there, and can't be what commit wrote.
var errBrokenRecord = errors.New("its record is broken")

func readKept(dir string) (*Kept, error) {
	b, err := os.ReadFile(filepath.Join(dir, "kept.json")) //nolint:gosec // a path of the archive's own
	if err != nil {
		return nil, err
	}
	var k Kept
	if err := json.Unmarshal(b, &k); err != nil {
		return nil, fmt.Errorf("%w: %w", errBrokenRecord, err)
	}
	k.dir = dir
	return &k, nil
}

// Forgotten says which services' kept logs a deletion put out of reach, replacing what
// it last said. What can no longer be undone goes from the disk now, and what still can
// goes first if the archive runs out of room. What is read isn't decided here: each
// call says what is out of reach, as of when it was made.
func (a *Archive) Forgotten(services []*agentv1.ForgottenService) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forgotten = make(map[string]*agentv1.ForgottenService, len(services))
	for _, f := range services {
		project, service := docker.Project(f.GetResource().GetScope()), f.GetResource().GetName()
		dir, ok := a.serviceDir(project, service)
		if !ok {
			continue
		}
		a.forgotten[project+"/"+service] = f
		if !f.GetExpired() {
			continue
		}
		for _, k := range a.listIn(dir) {
			if k.Revision > f.GetUpToRevision() {
				continue
			}
			if err := os.RemoveAll(k.dir); err != nil {
				a.logger().Warn("can't remove what the server kept of a deleted service; it tries again when next told", slog.String("service", service), slog.String("error", err.Error()))
			}
		}
		_ = os.Remove(dir) // only if nothing is left
	}
}

// Prune applies the archive's limits. Each service keeps its newest failed and
// its newest replaced container on each server; nothing stays past RetainFor; and
// the whole stays within its budget, the oldest going first, what a deletion put out
// of reach before what was replaced, and that before what failed.
func (a *Archive) Prune() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var all []*Kept
	projects, err := os.ReadDir(a.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := a.now().Add(-RetainFor)
	for _, p := range projects {
		if !p.IsDir() || !safeName(p.Name()) {
			continue
		}
		services, err := os.ReadDir(filepath.Join(a.Dir, p.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, s := range services {
			if !s.IsDir() {
				continue
			}
			dir := filepath.Join(a.Dir, p.Name(), s.Name())
			upTo := a.forgotten[p.Name()+"/"+s.Name()].GetUpToRevision()
			seen := map[string]bool{}
			for _, k := range a.listIn(dir) {
				if seen[k.slot()] || k.KeptAt.Before(cutoff) {
					if err := os.RemoveAll(k.dir); err != nil {
						return err
					}
					continue
				}
				seen[k.slot()] = true
				k.hidden = k.Revision <= upTo && upTo > 0
				all = append(all, k)
			}
			a.removeAbandoned(dir)
			_ = os.Remove(dir) // only if nothing is left
		}
		_ = os.Remove(filepath.Join(a.Dir, p.Name()))
	}
	total := int64(0)
	for _, k := range all {
		total += k.Disk
	}
	// What goes first sorts first: what a deletion put out of reach, then replaced,
	// then failed, then oldest. The newest of all stays, whatever it takes, as
	// what was just kept.
	rank := func(k *Kept) int {
		switch {
		case k.hidden:
			return 0
		case !k.Failed:
			return 1
		}
		return 2
	}
	slices.SortFunc(all, func(x, y *Kept) int {
		if r := rank(x) - rank(y); r != 0 {
			return r
		}
		return x.KeptAt.Compare(y.KeptAt)
	})
	newest := time.Time{}
	for _, k := range all {
		if k.KeptAt.After(newest) {
			newest = k.KeptAt
		}
	}
	for _, k := range all {
		if total <= a.budget() {
			break
		}
		if k.KeptAt.Equal(newest) {
			continue
		}
		if err := os.RemoveAll(k.dir); err != nil {
			return err
		}
		total -= k.Disk
	}
	return nil
}

// removeAbandoned clears what can never be read back: copies a crash left behind,
// once they are old, and a kept log with no record it can read. With no record, it
// has no age and no size, so nothing else here would ever remove it. A record that
// can't be reached for now isn't that, and stays for the next prune.
func (a *Archive) removeAbandoned(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := filepath.Join(dir, e.Name())
		if strings.HasPrefix(e.Name(), ".copying-") {
			info, err := e.Info()
			if err == nil && a.now().Sub(info.ModTime()) > time.Hour {
				_ = os.RemoveAll(name)
			}
			continue
		}
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := readKept(name); errors.Is(err, fs.ErrNotExist) || errors.Is(err, errBrokenRecord) {
			if err := os.RemoveAll(name); err != nil {
				a.logger().Warn("can't remove a kept log with no record; the next prune tries again", slog.String("dir", e.Name()), slog.String("error", err.Error()))
			}
		}
	}
}

// KeepPruned prunes once at the start, and every hour until ctx is done, so a
// kept log leaves on time on a server with no deploys.
func (a *Archive) KeepPruned(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := a.Prune(); err != nil {
			a.logger().Warn("can't prune the logs kept of removed containers", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// keptLine is one line read back from the archive. Its slices are valid until
// the next.
type keptLine struct {
	time   time.Time
	stream docker.LogStream
	cut    int
	text   []byte
}

// read calls fn with each line of a kept log, in order, until fn says stop.
func (k *Kept) read(ctx context.Context, fn func(*keptLine) bool) error {
	names, err := filepath.Glob(filepath.Join(k.dir, "*.gz"))
	if err != nil {
		return err
	}
	slices.Sort(names)
	buf := make([]byte, 0, 64<<10)
	for _, name := range names {
		more, err := readSegment(ctx, name, buf, fn)
		if err != nil || !more {
			return err
		}
	}
	return nil
}

func readSegment(ctx context.Context, name string, buf []byte, fn func(*keptLine) bool) (bool, error) {
	f, err := os.Open(name) //nolint:gosec // a path of the archive's own
	if err != nil {
		return false, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return false, fmt.Errorf("can't read %s: %w", filepath.Base(name), err)
	}
	defer gz.Close()
	sc := bufio.NewScanner(gz)
	sc.Buffer(buf, docker.MaxLogLine+128)
	var l keptLine
	for n := 1; sc.Scan(); n++ {
		if n%1024 == 0 && ctx.Err() != nil {
			return false, ctx.Err()
		}
		if parseKeptLine(sc.Bytes(), &l) && !fn(&l) {
			return false, nil
		}
	}
	return true, sc.Err()
}

// parseKeptLine reads a line of the form a segmentWriter writes.
func parseKeptLine(line []byte, l *keptLine) bool {
	ts, rest, ok := bytes.Cut(line, []byte{' '})
	if !ok {
		return false
	}
	t, ok := docker.ParseLogTime(ts)
	if !ok {
		return false
	}
	which, rest, ok := bytes.Cut(rest, []byte{' '})
	if !ok || len(which) != 1 {
		return false
	}
	cut, text, ok := bytes.Cut(rest, []byte{' '})
	if !ok {
		return false
	}
	n, err := strconv.Atoi(string(cut))
	if err != nil {
		return false
	}
	l.time, l.cut, l.text = t, n, text
	l.stream = docker.Stdout
	if which[0] == 'e' {
		l.stream = docker.Stderr
	}
	return true
}
