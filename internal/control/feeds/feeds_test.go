// SPDX-License-Identifier: AGPL-3.0-only

package feeds

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type list struct{ n int }

// Every watch of a server shares one read, a session holds Max at most, the read
// ends with the last watch, and a failed read reaches every watch.
func TestWatchesOfAServerShareOneRead(t *testing.T) {
	h := &Hub[list]{Max: 2}
	var reads atomic.Int32
	ended := make(chan struct{})
	read := func(ctx context.Context, f *Feed[list]) {
		reads.Add(1)
		f.Tell(&list{n: 1}, nil)
		<-ctx.Done()
		close(ended)
	}
	a, ok := h.Join(t.Context(), "ses_1", "web-1", read)
	if !ok {
		t.Fatal("a first watch was refused")
	}
	b, ok := h.Join(t.Context(), "ses_1", "web-1", read)
	if !ok {
		t.Fatal("a second watch was refused")
	}
	if _, ok := h.Join(t.Context(), "ses_1", "web-1", read); ok {
		t.Error("a session's third watch, past Max, was taken")
	}
	for _, w := range []*Watch[list]{a, b} {
		if got, err := w.Next(t.Context()); err != nil || got.n != 1 {
			t.Errorf("a watch read %v, %v", got, err)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("two watches of one server read it %d times", n)
	}
	a.Leave()
	select {
	case <-ended:
		t.Fatal("the read ended while a watch was left")
	case <-time.After(50 * time.Millisecond):
	}
	b.Leave()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the read went on after the last watch left")
	}

	failed := errors.New("web-1 can't list them now")
	c, _ := h.Join(t.Context(), "ses_1", "web-1", func(_ context.Context, f *Feed[list]) {
		f.Tell(nil, failed)
		h.Drop("web-1", f)
	})
	if _, err := c.Next(t.Context()); !errors.Is(err, failed) {
		t.Errorf("a failed read reads as %v", err)
	}
	c.Leave()
}

// A watch that joins as the last one leaves joins a feed that goes on, not one
// that stopped: finding the feed and joining it is one step.
func TestAWatchJoiningAsTheLastLeavesKeepsTheFeed(t *testing.T) {
	h := &Hub[list]{Max: 8}
	read := func(ctx context.Context, f *Feed[list]) {
		f.Tell(&list{n: 1}, nil)
		<-ctx.Done()
	}
	b, _ := h.Join(t.Context(), "ses_b", "web-1", read)
	left := make(chan struct{})
	h.found = func() {
		h.found = nil
		go func() {
			b.Leave()
			close(left)
		}()
		// Give the leave every chance to land between finding and joining.
		select {
		case <-left:
		case <-time.After(200 * time.Millisecond):
		}
	}
	a, _ := h.Join(t.Context(), "ses_a", "web-1", read)
	<-left
	a.feed.mu.Lock()
	stopped := a.feed.stopped
	a.feed.mu.Unlock()
	if stopped {
		t.Fatal("a watch joined a feed the last watch had stopped, so nothing new would reach it")
	}
	a.feed.Tell(&list{n: 2}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		got, err := a.Next(ctx)
		if err != nil || got == nil {
			t.Fatalf("the watch heard %v, %v, want the second read", got, err)
		}
		if got.n == 2 {
			break
		}
	}
	a.Leave()
}
