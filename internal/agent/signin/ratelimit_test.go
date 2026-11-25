// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRateLimiterPerRouteAndGlobalEvictionAndDrop(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	rl := &RateLimiter{}

	// Single-route burst & 429 behavior.
	for i := range 3 {
		if ok, retry := rl.Allow("route-a", "203.0.113.1", 60, 2, now); !ok || retry != 0 {
			t.Fatalf("req %d = (%v, %d), want (true, 0)", i+1, ok, retry)
		}
	}
	if ok, retry := rl.Allow("route-a", "203.0.113.1", 60, 2, now); ok || retry < 1 {
		t.Fatalf("4th req = (%v, %d), want (false, >=1)", ok, retry)
	}

	// Per-route cap evicts oldest idle entry without exceeding maxRateVisitors.
	for i := range maxRateVisitors + 200 {
		now = now.Add(time.Millisecond)
		ip := fmt.Sprintf("198.51.%d.%d", i/256, i%256)
		rl.Allow("route-a", ip, 600, 5, now)
	}
	if got := len(rl.routes["route-a"].visitors); got != maxRateVisitors {
		t.Fatalf("per-route buckets = %d, want %d", got, maxRateVisitors)
	}
	if got := rl.Count(); got != maxRateVisitors {
		t.Fatalf("Count = %d, want %d", got, maxRateVisitors)
	}

	// Global cap across many routes never exceeds maxTotalRateBuckets and evicts oldest idle globally.
	for r := range 6 {
		routeID := fmt.Sprintf("route-flood-%d", r)
		for i := range 3000 {
			now = now.Add(time.Millisecond)
			ip := fmt.Sprintf("192.0.%d.%d", i/256, i%256)
			rl.Allow(routeID, ip, 600, 5, now)
		}
	}
	if got := rl.Count(); got != maxTotalRateBuckets {
		t.Fatalf("Count after multi-route flood = %d, want cap %d", got, maxTotalRateBuckets)
	}
	// Route A's entries were the oldest idle, so global eviction must have evicted all of them.
	if _, stillPresent := rl.routes["route-a"]; stillPresent {
		t.Fatalf("expected oldest idle route-a to be evicted under global cap")
	}

	// Drop frees every bucket of a route immediately.
	beforeDrop := rl.Count()
	route5Count := len(rl.routes["route-flood-5"].visitors)
	if route5Count == 0 {
		t.Fatal("expected route-flood-5 to hold buckets before Drop")
	}
	rl.Drop("route-flood-5")
	if got := rl.Count(); got != beforeDrop-route5Count {
		t.Fatalf("Count after Drop = %d, want %d", got, beforeDrop-route5Count)
	}
	if _, ok := rl.routes["route-flood-5"]; ok {
		t.Fatal("expected route-flood-5 map entry to be removed by Drop")
	}
}

// TestRateLimiterFloodOverUnixSocketBoundsMemory exercises M4 under a multi-route IP
// flood across the Unix socket path that Caddy uses, verifying bounded buckets and low
// heap growth.
func TestRateLimiterFloodOverUnixSocketBoundsMemory(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "signin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	rl := &RateLimiter{}
	srv := &Server{RateLimits: rl}
	go func() { _ = Serve(ln, srv, 10*time.Second, slog.New(slog.DiscardHandler)) }()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 16,
		},
		Timeout: 5 * time.Second,
	}
	defer client.CloseIdleConnections()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	// Flood 10 rate-limited routes with 2,500 distinct IPs each (25,000 unique IPs total).
	const routes = 10
	const ipsPerRoute = 2500
	for r := range routes {
		routeID := fmt.Sprintf("dockwell.env.site%d", r)
		url := fmt.Sprintf("http://signin%s%s?rpm=120&burst=20", RateLimitPrefix, routeID)
		for i := range ipsPerRoute {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Dockwell-Client", fmt.Sprintf("203.%d.%d.%d", r, i/256, i%256))
			res, err := client.Do(req)
			if err != nil {
				t.Fatalf("flood request r=%d i=%d failed: %v", r, i, err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("unexpected status %d on first hit of unique IP", res.StatusCode)
			}
		}
	}

	if got := rl.Count(); got != maxTotalRateBuckets {
		t.Fatalf("Count after 25k flood = %d, want %d", got, maxTotalRateBuckets)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	var growthMB float64
	if after.HeapAlloc > before.HeapAlloc {
		growthMB = float64(after.HeapAlloc-before.HeapAlloc) / (1024 * 1024)
	}
	t.Logf("M4 flood: 10 routes x 2500 IPs = 25000 requests; Count=%d; HeapAlloc=%.2f MiB (delta=%.2f MiB)",
		rl.Count(), float64(after.HeapAlloc)/(1024*1024), growthMB)
	if growthMB > 6.0 {
		t.Fatalf("rate-limiter flood heap growth %.2f MiB exceeded 6 MiB budget", growthMB)
	}

	// Dropping all routes frees all buckets back to 0.
	for r := range routes {
		rl.Drop(fmt.Sprintf("dockwell.env.site%d", r))
	}
	if got := rl.Count(); got != 0 {
		t.Fatalf("Count after dropping all routes = %d, want 0", got)
	}
}
