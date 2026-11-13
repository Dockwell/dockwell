// SPDX-License-Identifier: AGPL-3.0-only

package system

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/logs"
)

// Following the system log (ADR 0067): while anyone follows, one journalctl -f for
// the server, unfiltered, read once and handed to each follow, which narrows it; with
// nobody following, nothing runs and nothing is held. A follow that falls behind
// loses lines past what it may hold, and says how many. Without a journal, the end
// of /var/log/messages is read every second, through a rename and a copytruncate.

// What following holds and reads, at most.
const (
	// followQueue is the bytes of lines one follow holds that it hasn't taken yet.
	followQueue = 512 << 10
	// followBytesPerSecond is how fast the shared journalctl is read: past it, the
	// follow falls behind the journal rather than take a core to keep up with a
	// flood, as journald keeps it all to read back.
	followBytesPerSecond = 16 << 20
	// filePoll is how often the end of /var/log/messages is read.
	filePoll = time.Second
)

var _ logs.Journal = (*Reader)(nil)

// CatchUpLines is the most lines a follow that carries on after a cursor reads of
// what came since; past it, it says there's a gap. A test sets it lower.
var CatchUpLines = 1000

// journalHub is the one follow of the journal every follow of the server shares.
type journalHub struct {
	mu   sync.Mutex
	subs map[*follower]struct{}
	stop context.CancelFunc
	// done closes as the hub's read ends, which a new hub waits for.
	done chan struct{}
}

// follower is one follow's place on the hub: what it hasn't taken yet.
type follower struct {
	mu      sync.Mutex
	queue   []logs.JournalEntry
	bytes   int
	dropped uint64
	wake    chan struct{}
	// failed is why the hub's read ended, which a follow then says.
	failed error
}

// offer queues an entry for the follower, or counts it dropped past what it holds.
func (f *follower) offer(e logs.JournalEntry) {
	f.mu.Lock()
	if f.bytes+e.Size > followQueue {
		f.dropped++
	} else {
		f.queue = append(f.queue, e)
		f.bytes += e.Size
	}
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *follower) fail(err error) {
	f.mu.Lock()
	f.failed = err
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// take is what the follower holds, with how many it dropped since it last took.
func (f *follower) take() ([]logs.JournalEntry, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q, d := f.queue, f.dropped
	f.queue, f.bytes, f.dropped = nil, 0, 0
	return q, d, f.failed
}

// join gives a follow its place, and starts the shared read with the first.
func (r *Reader) join() *follower {
	r.hubMu.Lock()
	defer r.hubMu.Unlock()
	if r.hub == nil {
		r.hub = &journalHub{subs: map[*follower]struct{}{}}
	}
	h := r.hub
	f := &follower{wake: make(chan struct{}, 1)}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[f] = struct{}{}
	if len(h.subs) == 1 {
		// The read has its own context, which the last follow leaving ends.
		ctx, stop := context.WithCancel(context.Background())
		was := h.done
		h.stop, h.done = stop, make(chan struct{})
		go r.feed(ctx, h, was, h.done)
	}
	return f
}

// leave gives up a follow's place, and stops the shared read with the last.
func (r *Reader) leave(f *follower) {
	r.hubMu.Lock()
	h := r.hub
	r.hubMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, f)
	if len(h.subs) == 0 && h.stop != nil {
		h.stop()
		h.stop = nil
	}
}

// send hands an entry to every follow of the hub.
func (h *journalHub) send(e logs.JournalEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for f := range h.subs {
		f.offer(e)
	}
}

// failAll says to every follow why the read ended.
func (h *journalHub) failAll(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for f := range h.subs {
		f.fail(err)
	}
}

// feed reads the journal, or the messages file, for the hub until ctx ends. It
// waits for a read before it to end, so two never run.
func (r *Reader) feed(ctx context.Context, h *journalHub, before <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	if before != nil {
		<-before
	}
	var err error
	switch {
	case r.exists("run/systemd/journal"):
		err = r.feedJournal(ctx, h)
	case r.exists(filepath.Join("var", "log", "messages")):
		err = r.feedFile(ctx, h)
	default:
		err = errors.New("this server keeps no system log Dockwell reads, so there's nothing to follow")
	}
	if ctx.Err() == nil {
		if err == nil {
			err = errors.New("journalctl stopped following the journal. Open the system log again")
		}
		h.failAll(err)
	}
}

// pace keeps a reader to a rate in bytes a second, a stretch at a time.
type pace struct {
	rate  float64
	owed  int
	since time.Time
}

func (p *pace) wait(ctx context.Context, n int) {
	p.owed += n
	if p.owed < 256<<10 {
		return
	}
	now := time.Now()
	if !p.since.IsZero() {
		if wait := time.Duration(float64(p.owed)/p.rate*float64(time.Second)) - now.Sub(p.since); wait > 0 {
			t := time.NewTimer(min(wait, time.Second))
			select {
			case <-t.C:
			case <-ctx.Done():
			}
			t.Stop()
			now = time.Now()
		}
	}
	p.owed, p.since = 0, now
}

// feedJournal runs journalctl -f, read once for every follow.
func (r *Reader) feedJournal(ctx context.Context, h *journalHub) error {
	args := append(journalArgs(Ask{}), "--follow", "--lines=0")
	p := &pace{rate: followBytesPerSecond}
	_, res, err := r.Tools.Stream(ctx, journalctl, args, func(b []byte) bool {
		p.wait(ctx, len(b))
		if e, ok := journalEntry(b); ok {
			h.send(e)
		}
		return ctx.Err() == nil
	})
	if err != nil {
		return fmt.Errorf("can't follow the server's journal: %w", err)
	}
	if ctx.Err() == nil && res.Code > 0 {
		return fmt.Errorf("journalctl stopped following the journal: %s", strings.TrimSpace(res.Errs))
	}
	return nil
}

// journalEntry reads one entry of journalctl's JSON as a follow takes it.
func journalEntry(b []byte) (logs.JournalEntry, bool) {
	line, about, ok := parseJournal(b)
	if !ok {
		return logs.JournalEntry{}, false
	}
	return logs.JournalEntry{Line: line, About: about, Size: entrySize(line)}, true
}

// entrySize is what a line costs to send: its words, and room for the rest.
func entrySize(l *dockwellv1.SystemLogLine) int {
	return len(l.GetMessage()) + len(l.GetUnit()) + len(l.GetCursor()) + 32
}

// feedFile reads what's added to /var/log/messages every second, from its end
// when the follow begins: a file that shrank was copied and truncated, and one
// with another inode was rotated, and each is read from its start.
func (r *Reader) feedFile(ctx context.Context, h *journalHub) error {
	root := r.Root
	if root == "" {
		root = "/"
	}
	path := filepath.Join(root, "var", "log", "messages")
	var f *os.File
	var ino uint64
	var at int64
	open := func(fromEnd bool) error {
		if f != nil {
			_ = f.Close()
		}
		var err error
		f, err = os.Open(path) //nolint:gosec // G304: the server's own syslog file, below the stand's root in a test
		if err != nil {
			return err
		}
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			ino = st.Ino
		}
		at = 0
		if fromEnd {
			at = fi.Size()
		}
		return nil
	}
	if err := open(true); err != nil {
		return fmt.Errorf("can't follow the server's system log: %w", err)
	}
	defer func() { _ = f.Close() }()
	year := time.Now().Year()
	var rest []byte
	buf := make([]byte, 64<<10)
	tick := time.NewTicker(filePoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		if fi, err := os.Stat(path); err == nil {
			st, ok := fi.Sys().(*syscall.Stat_t)
			if ok && st.Ino != ino || fi.Size() < at {
				if err := open(false); err != nil {
					continue
				}
				rest = rest[:0]
			}
		}
		for {
			n, err := f.ReadAt(buf, at)
			at += int64(n)
			rest = append(rest, buf[:n]...)
			for {
				i := strings.IndexByte(string(rest), '\n')
				if i < 0 {
					break
				}
				if e, ok := messagesEntry(string(rest[:i]), year); ok {
					e.Cursor = fmt.Sprintf("file:%d", at-int64(len(rest))+int64(i))
					h.send(logs.JournalEntry{Line: e, Size: entrySize(e)})
				}
				rest = rest[i+1:]
			}
			if len(rest) > maxMessage {
				rest = rest[:0]
			}
			if n == 0 || errors.Is(err, io.EOF) || err != nil {
				break
			}
		}
	}
}

// FollowJournal gives each what a follow narrowed to unit reads (ADR 0067): what
// came after the cursor first, at most CatchUpLines and then a gap, then each new
// line as the shared read reads it, until ctx ends.
func (r *Reader) FollowJournal(ctx context.Context, unit, after string, each func(logs.JournalEntry)) error {
	if r.Unavailable != "" {
		return errors.New(r.Unavailable)
	}
	if unit != "" && !unitName().MatchString(unit) && !rcName().MatchString(unit) {
		return fmt.Errorf("%q isn't a system service's name", unit)
	}
	if strings.ContainsAny(after, "\n\x00") {
		return errors.New("that isn't a place in this server's system log")
	}
	f := r.join()
	defer r.leave(f)
	// What came since the cursor, read once the shared read holds what comes next,
	// so nothing falls between; a line both read is sent once.
	seen := map[string]bool{}
	if after != "" && r.exists("run/systemd/journal") {
		args := append(journalArgs(Ask{Unit: unit}), "--after-cursor="+after)
		read := 0
		_, _, err := r.Tools.Stream(ctx, journalctl, args, func(b []byte) bool {
			e, ok := journalEntry(b)
			if !ok {
				return true
			}
			if read == CatchUpLines {
				each(logs.JournalEntry{Gap: true})
				return false
			}
			read++
			seen[e.Line.GetCursor()] = true
			each(e)
			return true
		})
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("can't read what the system log has since: %w", err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-f.wake:
		}
		entries, dropped, failed := f.take()
		for _, e := range entries {
			if len(seen) > 0 && seen[e.Line.GetCursor()] {
				continue
			}
			if unit != "" && !aboutUnit(e, unit) {
				continue
			}
			e.Dropped, dropped = dropped, 0
			each(e)
		}
		if dropped > 0 {
			each(logs.JournalEntry{Dropped: dropped})
		}
		// Once the shared read is past the catch-up, nothing more can be twice.
		seen = nil
		if failed != nil {
			return failed
		}
	}
}

// aboutUnit reports a line that unit wrote, or that systemd wrote of it.
func aboutUnit(e logs.JournalEntry, unit string) bool {
	u := e.Line.GetUnit()
	return u == unit || e.About == unit || u+".service" == unit
}
