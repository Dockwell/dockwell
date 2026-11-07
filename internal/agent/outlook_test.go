// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dockwell/dockwell/internal/agent/collect"
)

// A link that flaps opens a stream every 30 seconds, and each says what's known. Only
// the schedule asks anyone again.
func TestOnlyTheScheduleLooksAgain(t *testing.T) {
	var asked atomic.Int32
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		http.NotFound(w, nil)
	}))
	defer metadata.Close()
	cfg := Config{Collector: &collect.Collector{
		Root:      fstest.MapFS{},
		Metadata:  metadata.URL,
		STUN:      []string{},
		Addresses: func() ([]net.Addr, error) { return nil, nil },
	}}
	var o outlook
	first, wait := o.current(t.Context(), cfg)
	if first == nil || asked.Load() == 0 || wait <= 0 || wait > networkEvery {
		t.Fatalf("a first look got %v after %d requests, and waits %v", first, asked.Load(), wait)
	}
	made := asked.Load()
	for range 5 {
		again, wait := o.current(t.Context(), cfg)
		if again != first || wait <= 0 {
			t.Errorf("a new stream got %v, and waits %v", again, wait)
		}
	}
	if asked.Load() != made {
		t.Errorf("new streams asked %d times more", asked.Load()-made)
	}
	o.at = time.Now().Add(-networkEvery)
	o.current(t.Context(), cfg)
	if asked.Load() == made {
		t.Error("with a look due, it didn't look")
	}
}

// A server must not lose what it found to a stream that ended as it looked.
func TestALookCutShortIsntKept(t *testing.T) {
	cfg := Config{Collector: &collect.Collector{
		Root:      fstest.MapFS{},
		Metadata:  collect.ClosedMetadata,
		STUN:      []string{},
		Addresses: func() ([]net.Addr, error) { return nil, nil },
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var o outlook
	o.current(ctx, cfg)
	if o.last != nil {
		t.Errorf("it kept %v", o.last)
	}
}
