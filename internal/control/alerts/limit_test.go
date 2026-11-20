// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Review M2: a person's places for runs are taken at once, so requests at the same
// moment can't each find one free; a place given back, for a run refused before it
// asked anything, frees one, and the wait is still from the oldest kept.
func TestARunsPlaceIsTakenAtOnce(t *testing.T) {
	t.Parallel()
	runs := &testers{seen: map[string][]time.Time{}}
	start := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	var granted atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if runs.allow("user:a", start) == 0 {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := granted.Load(); got != TestsPerMinute {
		t.Fatalf("%d requests at once took %d places, want %d", 20, got, TestsPerMinute)
	}

	// Later runs, then one of the first given back.
	runs = &testers{seen: map[string][]time.Time{}}
	for i := range TestsPerMinute {
		if wait := runs.allow("user:a", start.Add(time.Duration(i)*time.Second)); wait != 0 {
			t.Fatalf("run %d waits %v", i+1, wait)
		}
	}
	if wait := runs.allow("user:a", start.Add(10*time.Second)); wait != 50*time.Second {
		t.Fatalf("a seventh run waits %v, want 50s from the oldest", wait)
	}
	runs.release("user:a", start.Add(2*time.Second))
	if wait := runs.allow("user:a", start.Add(10*time.Second)); wait != 0 {
		t.Fatalf("after a place came back, a run waits %v", wait)
	}
	if wait := runs.allow("user:a", start.Add(11*time.Second)); wait != 49*time.Second {
		t.Fatalf("the next run waits %v, want 49s from the oldest still kept", wait)
	}
	if wait := runs.allow("user:b", start.Add(11*time.Second)); wait != 0 {
		t.Fatalf("another person waits %v", wait)
	}
}
