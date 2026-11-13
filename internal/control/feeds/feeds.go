// SPDX-License-Identifier: AGPL-3.0-only

// Package feeds shares one read of a server among every watch of it, as its
// processes and its system services are read: while someone watches, and once
// however many do (ADR 0041, ADR 0067).
package feeds

import (
	"context"
	"sync"
)

// Hub holds a feed for each server someone watches, and how many watches each
// session has open.
type Hub[T any] struct {
	// Max is how many watches one session may hold at once.
	Max int

	mu       sync.Mutex
	feeds    map[string]*Feed[T]
	watching map[string]int
	// found runs between finding a server's feed and joining it, for a test.
	found func()
}

// Feed is one server's latest read, or why there's none, for every watch of it.
type Feed[T any] struct {
	mu       sync.Mutex
	last     *T
	err      error
	watchers map[chan struct{}]struct{}
	stop     context.CancelFunc
	stopped  bool
	// again asks the feed's read to read now, rather than at its next time.
	again chan struct{}
}

// Watch is one watch of a server's feed.
type Watch[T any] struct {
	hub     *Hub[T]
	feed    *Feed[T]
	session string
	server  string
	told    chan struct{}
	sent    *T
}

// Join adds a watch of server's feed for session, and starts the feed with read
// when there's none, on a context of its own that ends once nobody watches. It
// finds the feed and joins it under one lock, so the last watch leaving can't
// stop the feed in between. It says false when session holds Max watches.
func (h *Hub[T]) Join(ctx context.Context, session, server string, read func(context.Context, *Feed[T])) (*Watch[T], bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.feeds == nil {
		h.feeds, h.watching = map[string]*Feed[T]{}, map[string]int{}
	}
	if h.watching[session] >= h.Max {
		return nil, false
	}
	h.watching[session]++
	f, ok := h.feeds[server]
	if !ok {
		reading, stop := context.WithCancel(context.WithoutCancel(ctx)) //nolint:gosec // the feed keeps stop, and its last watch leaving calls it
		f = &Feed[T]{watchers: map[chan struct{}]struct{}{}, stop: stop, again: make(chan struct{}, 1)}
		h.feeds[server] = f
		go read(reading, f)
	}
	if h.found != nil {
		h.found()
	}
	w := &Watch[T]{hub: h, feed: f, session: session, server: server, told: make(chan struct{}, 1)}
	f.mu.Lock()
	f.watchers[w.told] = struct{}{}
	if f.last != nil || f.err != nil {
		w.told <- struct{}{}
	}
	f.mu.Unlock()
	return w, true
}

// Next waits for a read newer than the last this watch had, and gives it, or the
// feed's error. It gives neither once ctx ends.
func (w *Watch[T]) Next(ctx context.Context) (*T, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		case <-w.told:
		}
		w.feed.mu.Lock()
		last, err := w.feed.last, w.feed.err
		w.feed.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if last == nil || last == w.sent {
			continue
		}
		w.sent = last
		return last, nil
	}
}

// Leave ends the watch, and the feed with its last watch.
func (w *Watch[T]) Leave() {
	h := w.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	h.watching[w.session]--
	if h.watching[w.session] == 0 {
		delete(h.watching, w.session)
	}
	f := w.feed
	f.mu.Lock()
	delete(f.watchers, w.told)
	last := len(f.watchers) == 0
	f.mu.Unlock()
	if last && h.feeds[w.server] == f {
		delete(h.feeds, w.server)
		f.end()
	}
}

// Tell keeps what a read found, or why it found nothing, and tells every watch.
func (f *Feed[T]) Tell(v *T, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last, f.err = v, err
	for w := range f.watchers {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

// Again asks server's feed, if anyone watches it, to read now: something moved
// that every watch should see at once, such as a unit an action restarted.
func (h *Hub[T]) Again(server string) {
	h.mu.Lock()
	f := h.feeds[server]
	h.mu.Unlock()
	if f == nil {
		return
	}
	select {
	case f.again <- struct{}{}:
	default:
	}
}

// Again is where a feed's read hears it's asked to read now.
func (f *Feed[T]) Again() <-chan struct{} { return f.again }

// Drop forgets a feed whose read failed, after Tell said why, so the next watch
// of its server starts anew.
func (h *Hub[T]) Drop(server string, f *Feed[T]) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.feeds[server] == f {
		delete(h.feeds, server)
	}
	f.end()
}

func (f *Feed[T]) end() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	f.stop()
}
