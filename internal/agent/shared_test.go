// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Reads asked while one runs share it; once it's forgotten, the next starts a
// run of its own, which the forgotten one, ending, leaves under way.
func TestASharedReadForgottenLetsTheNextStartAgain(t *testing.T) {
	var s shared[int]
	var runs atomic.Int32
	release := map[int32]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}
	read := func(ctx context.Context) int {
		n := runs.Add(1)
		select {
		case <-release[n]:
		case <-ctx.Done():
		}
		return int(n)
	}
	ask := func() <-chan int {
		out := make(chan int, 1)
		go func() {
			v, err := s.read(t.Context(), time.Minute, read)
			if err != nil {
				t.Error(err)
			}
			out <- v
		}()
		return out
	}
	waitRuns := func(n int32) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for runs.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d runs, want %d", runs.Load(), n)
			}
			time.Sleep(time.Millisecond)
		}
	}

	first, beside := ask(), ask()
	waitRuns(1)
	s.forget()
	second := ask()
	waitRuns(2)
	close(release[1])
	if a, b := <-first, <-beside; a != 1 || b != 1 {
		t.Errorf("the reads asked before forgetting got %d and %d, want both the first run's", a, b)
	}
	// The first run, ending, leaves the second as the one under way, for the
	// next who asks to share.
	s.mu.Lock()
	still := s.reading
	s.mu.Unlock()
	if still == nil {
		t.Error("the forgotten run, ending, left no read under way, so the next would start a third")
	}
	close(release[2])
	if b := <-second; b != 2 {
		t.Errorf("the read asked after forgetting got %d, want the second run's", b)
	}
	if n := runs.Load(); n != 2 {
		t.Errorf("%d runs, want 2", n)
	}
}
