// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

func TestSearchingALineFindsItInAnyCase(t *testing.T) {
	for _, tc := range []struct {
		text, query string
		want        bool
	}{
		{"GET /Cart 200", "cart", true},
		{"GET /CART 200", "cart", true},
		{"nothing here", "cart", false},
		{"car", "cart", false},
		{"", "cart", false},
		{"anything", "", true},
		{"xxcart", "cart", true},
		{"cartxx", "cart", true},
		{"cCart", "cart", true},
		{"1234", "23", true},
		// What it falls back on when a start doesn't pan out.
		{"aaab", "aab", true},
		{"abababc", "ababc", true},
		{"ababab", "ababc", false},
		// A letter past ASCII matches as written, in the query as in the text.
		{"Été à Paris", "Été", true},
		{"ÉTÉ", "été", false},
		// A search written in capitals finds what any case holds.
		{"connection refused", "REFUSED", true},
	} {
		if got := newFoldSearch(tc.query).in([]byte(tc.text)); got != tc.want {
			t.Errorf("searching %q for %q = %v, want %v", tc.text, tc.query, got, tc.want)
		}
	}
}

// A query against the worst line for one takes about as long as one that fails at
// once: the time grows with the line, and not with how far each start gets. Compared,
// rather than timed, so a loaded machine or the race detector doesn't change the answer.
func TestSearchingALineIsLinear(t *testing.T) {
	took := func(search *foldSearch, text []byte) time.Duration {
		start := time.Now()
		for range 20 {
			if search.in(text) {
				t.Fatal("it found what isn't there")
			}
		}
		return time.Since(start)
	}
	text := bytes.Repeat([]byte{'a'}, 16<<10)
	easy := took(newFoldSearch("b"), text)
	if hard := took(newFoldSearch(strings.Repeat("a", 1<<10)+"b"), text); hard > 10*easy {
		t.Errorf("a query of 1 KiB took %s on a line of 16 KiB, and one of a letter %s", hard, easy)
	}
	// A query's first letter in the other case throughout, which looking for each
	// case apart reads over and over.
	mixed := bytes.Repeat([]byte("Ax"), 8<<10)
	if hard := took(newFoldSearch("ab"), mixed); hard > 10*took(newFoldSearch("z"), mixed) {
		t.Errorf("a query whose first letter is in the other case took %s", hard)
	}
}

func TestSearchingALineDoesntAllocate(t *testing.T) {
	search := newFoldSearch("needle")
	text := []byte("a haystack with a NEEDLE in it")
	if n := testing.AllocsPerRun(100, func() { search.in(text) }); n != 0 {
		t.Errorf("a search allocates %v times a line", n)
	}
}

// Pruning goes on its own time, so a stop's downtime doesn't wait for it.
func TestFinishingAKeptLogDoesntWaitForAPrune(t *testing.T) {
	e := dockertest.New(t)
	e.AddContainer(docker.Container{
		Name:    "/app-1",
		Config:  docker.Config{Image: "app:1", Labels: map[string]string{docker.LabelProject: "env_01test", docker.LabelService: "web", docker.LabelNumber: "1"}},
		Created: time.Now().UTC(),
		State:   docker.State{Status: "running", Running: true, StartedAt: time.Now().UTC()},
	})
	a := &Archive{Dir: t.TempDir(), Client: e.Client(), Free: func(string) (uint64, error) { return 1 << 40, nil }}
	k := a.Begin(t.Context(), e.Container("app-1"), false)
	e.Stop("app-1")
	a.mu.Lock() // as a prune holds it
	done := make(chan struct{})
	go func() {
		k.Finish(t.Context(), e.Container("app-1"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Finish waited for a prune")
	}
	a.mu.Unlock()
	a.WaitPruned()
}

// The names a label can give can't reach what a deletion set aside.
func TestSafeNames(t *testing.T) {
	for name, want := range map[string]bool{
		"web": true, "env_01abc": true, "": false, ".": false, "..": false, ".hidden": false, ".copying-abc": false, "a/b": false, `a\b`: false, "a\x00b": false,
	} {
		if got := safeName(name); got != want {
			t.Errorf("safeName(%q) = %v", name, got)
		}
	}
}

// A stream starts with a burst to spend, as what a container prints as it starts is
// often why someone is reading, and a change of its share doesn't give it another.
func TestABucketStartsWithItsBurst(t *testing.T) {
	b := newBucket()
	b.set(LiveBytesPerSecond)
	if !b.take(LiveBytesPerSecond / 4) {
		t.Error("a new stream can't send its first burst")
	}
	b.set(LiveBytesPerSecond)
	if b.take(LiveBytesPerSecond / 8) {
		t.Error("a change of share gave it another burst")
	}
}

// Gathering a message is linear in what it says, and with its reader gone it stops.
func TestGatheringIsLinearAndStopsWithItsReader(t *testing.T) {
	const n = 200_000
	full := func(ctx context.Context) *stream {
		s := &stream{ctx: ctx, call: &agentv1.LogsCall{}, open: map[string]*openChunk{}, gaps: make(map[string]*gap, n)}
		for i := range n {
			s.gaps[strconv.Itoa(i)] = &gap{told: &dockwellv1.LogGap{}}
		}
		return s
	}
	begin := time.Now()
	if m := full(t.Context()).gather(); len(m.GetChunks()) != n {
		t.Errorf("it gathered %d chunks", len(m.GetChunks()))
	}
	// A search through what has gathered for each would take minutes.
	if took := time.Since(begin); took > 30*time.Second {
		t.Errorf("gathering %d took %s", n, took)
	}
	ctx, cancel := context.WithCancel(t.Context())
	s := full(ctx)
	cancel()
	begin = time.Now()
	if m := s.gather(); len(m.GetChunks()) != 0 {
		t.Errorf("with its reader gone it gathered %d chunks", len(m.GetChunks()))
	}
	if took := time.Since(begin); took > time.Second {
		t.Errorf("with its reader gone it took %s", took)
	}
}
