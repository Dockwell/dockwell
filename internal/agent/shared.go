// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"sync"
	"time"
)

// shared runs one read at a time, which everyone who asks while it runs
// shares, so two tabs open on a server run its tools once. The read is
// everyone's, so it doesn't end as the first who asked leaves; it ends at its
// own bound. A write to what it reads forgets it, so a read asked after the
// write starts again rather than share one that began before it.
type shared[T any] struct {
	mu      sync.Mutex
	reading *sharedRun[T]
}

type sharedRun[T any] struct {
	done chan struct{}
	out  T
	// joined is how many have asked for this run, the one who started it
	// included, under shared's lock: a test waits on it to know who shares it.
	joined int
}

// read joins the read under way, or starts one, and waits for what it says.
func (s *shared[T]) read(ctx context.Context, bound time.Duration, read func(context.Context) T) (T, error) {
	s.mu.Lock()
	r := s.reading
	if r == nil {
		r = &sharedRun[T]{done: make(chan struct{})}
		s.reading = r
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
		go func() {
			defer cancel()
			r.out = read(readCtx)
			s.mu.Lock()
			// Forgotten, a newer read may be under way by now, which this leaves.
			if s.reading == r {
				s.reading = nil
			}
			s.mu.Unlock()
			close(r.done)
		}()
	}
	r.joined++
	s.mu.Unlock()
	select {
	case <-r.done:
		return r.out, nil
	case <-ctx.Done():
		var zero T
		return zero, context.Cause(ctx)
	}
}

// forget has the next read start again rather than join the one under way, as
// after a write to what it reads, which that read may not show. Those already
// waiting on it keep it, and get what it says.
func (s *shared[T]) forget() {
	s.mu.Lock()
	s.reading = nil
	s.mu.Unlock()
}
