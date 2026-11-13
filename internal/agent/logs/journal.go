// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// A follow of the server's system log (ADR 0067) is a stream like a service's:
// it takes a place among the server's readers, and its share of what the server
// carries live, the journal counting as one more thing followed. What it reads
// comes from the one journalctl every follow of the server shares, which the
// Journal runs; the stream narrows it and holds it to its share, dropping past it
// and saying how many.

// Journal follows the server's system log for each follow of it.
type Journal interface {
	// FollowJournal gives each the entries a follow narrowed to unit reads: what
	// came after the line at after first, a bounded number, then each new one, until
	// ctx ends. It calls each from one goroutine.
	FollowJournal(ctx context.Context, unit, after string, each func(JournalEntry)) error
}

// JournalEntry is one line of the system log a follow is given.
type JournalEntry struct {
	Line *dockwellv1.SystemLogLine
	// About is the unit a line of systemd's own is about, as journald's UNIT
	// field says.
	About string
	// Size is what the line costs to send.
	Size int
	// Dropped is how many lines the follow fell behind by, just before this one.
	Dropped uint64
	// Gap says more came after the cursor than the follow carries on with. It
	// comes alone, with no line.
	Gap bool
}

// Search looks for a text in a line, in any case, as a search of logs does.
type Search struct{ f *foldSearch }

// NewSearch is a search for query; an empty one holds in every line.
func NewSearch(query string) Search { return Search{f: newFoldSearch(query)} }

// In reports text holding what the search looks for.
func (s Search) In(text []byte) bool { return s.f == nil || s.f.in(text) }

// Empty reports a search for nothing.
func (s Search) Empty() bool { return s.f == nil || s.f.empty() }

// journalFlushEvery is how long lines of a follow wait to go up together.
const journalFlushEvery = 100 * time.Millisecond

// journalBatch is how many bytes of lines go up in one message, at most.
const journalBatch = 128 << 10

// runJournal follows the system log for the stream, until its context ends.
func (s *stream) runJournal() error {
	if s.r.Journal == nil {
		return errors.New("this agent doesn't follow the system log")
	}
	f := s.call.GetSystem()
	level, search := f.GetLevel(), NewSearch(f.GetQuery())
	var (
		mu      sync.Mutex
		lines   []*dockwellv1.SystemLogLine
		size    int
		dropped uint64
		gap     bool
	)
	// flush sends what's gathered; a reader that's behind loses it, counted.
	flush := func() {
		mu.Lock()
		defer mu.Unlock()
		if len(lines) == 0 && dropped == 0 && !gap {
			return
		}
		m := &agentv1.SendLogsRequest{Stream: s.call.GetStream(), SystemLines: lines, SystemDropped: dropped, SystemGap: gap}
		select {
		case s.out <- m:
			dropped, gap = 0, false
		default:
			dropped += uint64(len(lines))
		}
		lines, size = nil, 0
	}
	// Its first message says it's open, as a service's does.
	s.out <- &agentv1.SendLogsRequest{Stream: s.call.GetStream(), Started: timestamppb.New(s.start)}
	ticker := time.NewTicker(journalFlushEvery)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				flush()
			}
		}
	}()
	err := s.r.Journal.FollowJournal(s.ctx, f.GetUnit(), f.GetAfter(), func(e JournalEntry) {
		mu.Lock()
		dropped += e.Dropped
		gap = gap || e.Gap
		l := e.Line
		take := l != nil &&
			(level == dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED || l.GetLevel() >= level) &&
			(search.Empty() || search.In([]byte(l.GetMessage())))
		switch {
		case !take:
		case !s.rate.take(e.Size):
			// Past its share of the server's rate: dropped here, and said.
			dropped++
		default:
			lines = append(lines, l)
			size += e.Size
		}
		full := size >= journalBatch
		mu.Unlock()
		if full {
			flush()
		}
	})
	flush()
	if s.ctx.Err() != nil {
		return nil
	}
	return err
}
