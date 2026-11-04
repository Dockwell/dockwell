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
	// CutShort says the server has only the container's newest lines.
	CutShort bool  `json:"cut_short,omitempty"`
	Lines    int64 `json:"lines"`
	// Disk is what its files take, compressed.
	Disk int64 `json:"disk"`
	// dir is where its files are.
	dir string
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

	mu sync.Mutex // held while the directory changes
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
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && !strings.ContainsRune(name, 0)
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
	if free, err := a.free(); err == nil && free < minFreeBytes {
		a.logger().Warn("the disk is nearly full, so the server doesn't keep this container's log",
			slog.String("container", c.Name), slog.Uint64("free_bytes", free))
		return nil
	}
	k.dir = filepath.Join(dir, ".copying-"+k.meta.ContainerID)
	if err := os.RemoveAll(k.dir); err != nil {
		a.logger().Warn("can't keep a container's log", slog.String("container", c.Name), slog.String("error", err.Error()))
		return nil
	}
	if err := os.MkdirAll(k.dir, 0o700); err != nil {
		a.logger().Warn("can't keep a container's log", slog.String("container", c.Name), slog.String("error", err.Error()))
		return nil
	}
	if err := a.chmodParents(dir); err != nil {
		a.logger().Warn("can't keep a container's log", slog.String("container", c.Name), slog.String("error", err.Error()))
		return nil
	}
	copyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), copyWait)
	defer cancel()
	if err := k.copy(copyCtx, c, docker.LogOptions{Tail: copyTail}); err != nil {
		k.meta.CutShort = true
		if !errors.Is(err, context.DeadlineExceeded) {
			a.logger().Warn("can't read all of a container's log to keep it", slog.String("container", c.Name), slog.String("error", err.Error()))
		}
	}
	return k
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
		Name:        strings.TrimPrefix(c.Name, "/"),
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

// copy reads the container's log into its files, from the last line copied.
func (k *keep) copy(ctx context.Context, c *docker.Container, o docker.LogOptions) error {
	o.Since = k.last
	rd, err := k.a.Client.ContainerLogs(ctx, c.ID, c.Config.Tty, o)
	if err != nil {
		return err
	}
	defer rd.Close()
	sameTime := 0
	for {
		e, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return k.closeSegment()
		}
		if err != nil {
			_ = k.closeSegment()
			return err
		}
		t, ok := docker.ParseLogTime(e.Time)
		if !ok {
			continue
		}
		switch {
		case t.Before(k.last):
			continue
		case t.Equal(k.last):
			// Docker's since takes lines of exactly that time, which the
			// last copy may already have.
			sameTime++
			if sameTime <= k.lastCount {
				continue
			}
			k.lastCount++
		default:
			k.last, k.lastCount = t, 1
		}
		if err := k.write(t, e); err != nil {
			return err
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
// log into place, and prunes what the archive no longer has room for.
func (k *keep) Finish(ctx context.Context, c *docker.Container) {
	a := k.a
	if err := k.copy(ctx, c, docker.LogOptions{Tail: catchUpTail}); err != nil {
		k.meta.CutShort = true
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			a.logger().Warn("can't read a stopped container's last lines to keep them", slog.String("container", c.Name), slog.String("error", err.Error()))
		}
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
	if err := a.Prune(); err != nil {
		a.logger().Warn("can't prune the logs kept of removed containers", slog.String("error", err.Error()))
	}
}

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
	k.a.mu.Lock()
	defer k.a.mu.Unlock()
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

func readKept(dir string) (*Kept, error) {
	b, err := os.ReadFile(filepath.Join(dir, "kept.json")) //nolint:gosec // a path of the archive's own
	if err != nil {
		return nil, err
	}
	var k Kept
	if err := json.Unmarshal(b, &k); err != nil {
		return nil, err
	}
	k.dir = dir
	return &k, nil
}

// Prune applies the archive's limits. Each service keeps its newest failed and
// its newest replaced container on each server; nothing stays past RetainFor;
// and the whole stays within its budget, the oldest going first, what was
// replaced before what failed.
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
		if !p.IsDir() {
			continue
		}
		services, err := os.ReadDir(filepath.Join(a.Dir, p.Name()))
		if err != nil {
			return err
		}
		for _, s := range services {
			if !s.IsDir() {
				continue
			}
			dir := filepath.Join(a.Dir, p.Name(), s.Name())
			seenFailed, seenReplaced := false, false
			for _, k := range a.listIn(dir) {
				seen := &seenReplaced
				if k.Failed {
					seen = &seenFailed
				}
				if *seen || k.KeptAt.Before(cutoff) {
					if err := os.RemoveAll(k.dir); err != nil {
						return err
					}
					continue
				}
				*seen = true
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
	// What goes first sorts first: replaced before failed, then oldest. The
	// newest of all stays, whatever it takes, as what was just kept.
	slices.SortFunc(all, func(x, y *Kept) int {
		if x.Failed != y.Failed {
			if y.Failed {
				return -1
			}
			return 1
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

// removeAbandoned clears copies a crash left behind, once they are old.
func (a *Archive) removeAbandoned(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".copying-") {
			continue
		}
		info, err := e.Info()
		if err == nil && a.now().Sub(info.ModTime()) > time.Hour {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
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
