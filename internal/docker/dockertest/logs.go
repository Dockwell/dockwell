// SPDX-License-Identifier: AGPL-3.0-only

package dockertest

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
)

// containerLog is what a container printed, as Docker keeps it.
type containerLog struct {
	mu    sync.Mutex
	lines []loggedLine
	// wake is closed, and made again, whenever something is written or the
	// container stops, for whoever follows.
	wake  chan struct{}
	ended bool
	flood *Flood
}

type loggedLine struct {
	at     time.Time
	stream docker.LogStream
	text   string
}

func (l *containerLog) wakeLocked() {
	if l.wake != nil {
		close(l.wake)
	}
	l.wake = nil
}

func (l *containerLog) write(s docker.LogStream, lines ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, text := range lines {
		l.lines = append(l.lines, loggedLine{at: time.Now().UTC(), stream: s, text: text})
	}
	if len(lines) > 0 {
		l.ended = false
		l.wakeLocked()
	}
}

// start marks the container running again, so a new follow waits.
func (l *containerLog) start() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ended = false
}

// end marks the container stopped, which ends every follow.
func (l *containerLog) end() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ended = true
	l.wakeLocked()
}

// waiting returns the lines from index on, and otherwise what to wait for,
// and whether the log has ended.
func (l *containerLog) waiting(from int) ([]loggedLine, <-chan struct{}, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if from < len(l.lines) {
		return l.lines[from:], nil, false
	}
	if l.ended {
		return nil, nil, true
	}
	if l.wake == nil {
		l.wake = make(chan struct{})
	}
	return nil, l.wake, false
}

// Flood makes a container write as fast as whoever follows it can read: each
// line a follower reads is a line it wrote, so what it wrote and what arrived
// can be compared exactly. Stop it to go back to normal.
type Flood struct {
	size    int
	stopped atomic.Bool
	lines   atomic.Uint64
	bytes   atomic.Uint64
}

// Stop ends the flood.
func (f *Flood) Stop() { f.stopped.Store(true) }

// Lines is how many lines it has written.
func (f *Flood) Lines() uint64 { return f.lines.Load() }

// Bytes is how many bytes those lines held.
func (f *Flood) Bytes() uint64 { return f.bytes.Load() }

// Flood starts a container writing lines of size bytes as fast as they can
// be read, once someone follows it.
func (e *Engine) Flood(name string, size int) *Flood {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.byNameOrID(name)
	if c == nil {
		panic("dockertest: no container " + name)
	}
	f := &Flood{size: size}
	c.log.mu.Lock()
	defer c.log.mu.Unlock()
	c.log.flood = f
	c.log.wakeLocked()
	return f
}

// Print makes a container write lines to an output, now.
func (e *Engine) Print(name string, s docker.LogStream, lines ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.byNameOrID(name)
	if c == nil {
		panic("dockertest: no container " + name)
	}
	c.log.write(s, lines...)
}

// PrintAt makes a container's log hold a line from another time, as its log
// does once it has run for a while.
func (e *Engine) PrintAt(name string, at time.Time, s docker.LogStream, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.byNameOrID(name)
	if c == nil {
		panic("dockertest: no container " + name)
	}
	c.log.mu.Lock()
	defer c.log.mu.Unlock()
	c.log.lines = append(c.log.lines, loggedLine{at: at.UTC(), stream: s, text: text})
	c.log.wakeLocked()
}

// Rotate drops a container's oldest lines, keeping keep, as Docker's rotation
// does.
func (e *Engine) Rotate(name string, keep int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.byNameOrID(name)
	if c == nil {
		panic("dockertest: no container " + name)
	}
	c.log.mu.Lock()
	defer c.log.mu.Unlock()
	if n := len(c.log.lines) - keep; n > 0 {
		c.log.lines = append([]loggedLine(nil), c.log.lines[n:]...)
	}
}

// Followers is how many readers follow a container's log now. A reader that
// went away stops counting at once, as Docker sees its connection close.
func (e *Engine) Followers() int { return int(e.followers.Load()) }

// partBytes is where the engine cuts a long line into parts.
const partBytes = 16 << 10

var logsPath = regexp.MustCompile(`^(?:/v[0-9.]+)?/containers/([^/]+)/logs$`)

// serveLogs answers a request for a container's log, if this is one, as the
// engine does: multiplexed frames for a container without a tty, a time in
// front of each line, and a follow that ends when the container stops.
func (e *Engine) serveLogs(w http.ResponseWriter, r *http.Request) bool {
	m := logsPath.FindStringSubmatch(r.URL.Path)
	if m == nil || r.Method != http.MethodGet {
		return false
	}
	e.mu.Lock()
	e.Requests = append(e.Requests, r.Method+" "+r.URL.Path)
	c, err := e.findContainer(m[1])
	refused := c != nil && (c.State.Status == "removing" || c.State.Status == "dead")
	e.mu.Unlock()
	switch {
	case err != nil:
		fail(w, http.StatusBadRequest, "%v", err)
		return true
	case c == nil:
		fail(w, http.StatusNotFound, "No such container: %s", m[1])
		return true
	case refused:
		fail(w, http.StatusConflict, "can not get logs from container which is dead or marked for removal")
		return true
	}
	q := r.URL.Query()
	if lc := c.HostConfig.LogConfig; lc != nil && lc.Type == "none" {
		fail(w, http.StatusNotImplemented, "configured logging driver does not support reading")
		return true
	}
	since, until := logBound(q.Get("since")), logBound(q.Get("until"))
	tail := -1
	if t := q.Get("tail"); t != "" && t != "all" {
		tail, _ = strconv.Atoi(t)
	}
	follow := q.Get("follow") == "1" || q.Get("follow") == "true"

	w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flusher.Flush()
	out := bufio.NewWriterSize(w, 64<<10)
	tty := c.Config.Tty
	frame := func(s docker.LogStream, payload string) error {
		if tty {
			_, err := out.WriteString(strings.ReplaceAll(payload, "\n", "\r\n"))
			return err
		}
		var hdr [8]byte
		hdr[0] = byte(s)
		binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload))) //nolint:gosec // a test's lines are short
		if _, err := out.Write(hdr[:]); err != nil {
			return err
		}
		_, err := out.WriteString(payload)
		return err
	}
	// As the engine does, a line of partBytes or more goes in parts, each with the
	// time of the first, and only the last with a newline, which may be all it holds.
	send := func(s docker.LogStream, at time.Time, text string) error {
		stamp := at.Format("2006-01-02T15:04:05.000000000Z") + " "
		for len(text) >= partBytes {
			if err := frame(s, stamp+text[:partBytes]); err != nil {
				return err
			}
			text = text[partBytes:]
		}
		return frame(s, stamp+text+"\n")
	}

	lines, _, _ := c.log.waiting(0)
	next := len(lines)
	if tail >= 0 && tail < len(lines) {
		lines = lines[len(lines)-tail:]
	}
	for _, l := range lines {
		if (!since.IsZero() && l.at.Before(since)) || (!until.IsZero() && l.at.After(until)) {
			continue
		}
		if err := send(l.stream, l.at, l.text); err != nil {
			return true
		}
	}
	_ = out.Flush()
	flusher.Flush()
	if !follow {
		return true
	}
	e.followers.Add(1)
	defer e.followers.Add(-1)
	for {
		if f := c.log.flooding(); f != nil {
			n := int(f.lines.Load()) //nolint:gosec // a test's count fits
			text := strings.Repeat("x", max(f.size-10, 0))
			for !f.stopped.Load() && r.Context().Err() == nil {
				n++
				if err := send(docker.Stdout, time.Now().UTC(), fmt.Sprintf("%09d %s", n, text)); err != nil {
					return true
				}
				f.lines.Add(1)
				f.bytes.Add(uint64(f.size)) //nolint:gosec // a test's sizes are small
			}
			_ = out.Flush()
			flusher.Flush()
			if r.Context().Err() != nil {
				return true
			}
			c.log.clearFlood(f)
		}
		more, wake, ended := c.log.waiting(next)
		switch {
		case ended:
			return true
		case wake != nil:
			select {
			case <-wake:
			case <-r.Context().Done():
				return true
			}
			continue
		}
		next += len(more)
		for _, l := range more {
			if !until.IsZero() && l.at.After(until) {
				return true
			}
			if err := send(l.stream, l.at, l.text); err != nil {
				return true
			}
		}
		_ = out.Flush()
		flusher.Flush()
		if r.Context().Err() != nil {
			return true
		}
	}
}

func (l *containerLog) flooding() *Flood {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flood
}

func (l *containerLog) clearFlood(f *Flood) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.flood == f {
		l.flood = nil
	}
}

// logBound reads a time as the engine takes it in since and until: seconds,
// and maybe nanoseconds.
func logBound(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	secs, nanos, _ := strings.Cut(s, ".")
	sec, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Time{}
	}
	nano := int64(0)
	if nanos != "" {
		nano, _ = strconv.ParseInt((nanos + "000000000")[:9], 10, 64)
	}
	return time.Unix(sec, nano).UTC()
}

// Removing marks a stopped container as Docker does while it removes one: it is
// still listed, and its log is refused until it has gone.
func (e *Engine) Removing(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.byNameOrID(name); c != nil {
		c.State.Status = "removing"
	}
}

// Dead marks a container as Docker does when it couldn't remove one: it is still
// listed, and its log is refused for as long as it stays.
func (e *Engine) Dead(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.byNameOrID(name); c != nil {
		c.State.Status = "dead"
	}
}

// Restart marks a container that was stopped as running again, as Docker's
// restart policy would: the same container, with the same log.
func (e *Engine) Restart(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.byNameOrID(name)
	if c == nil {
		panic("dockertest: no container " + name)
	}
	c.State = docker.State{Status: "running", Running: true, StartedAt: time.Now().UTC()}
	c.log.start()
}
