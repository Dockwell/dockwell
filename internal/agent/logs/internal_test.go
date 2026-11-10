// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// A source says what a person knows a container by, and that it's the service's one
// replica, whatever a rollout has it under for now, or numbered.
func TestASourceSaysWhatAContainerIsKnownBy(t *testing.T) {
	for _, name := range []string{"/env_a-web-3", "/stopping_0123456789ab_env_a-web-3", "/0123456789ab_env_a-web-3"} {
		c := &docker.Container{ID: "0123456789abcdef", Name: name, Config: docker.Config{Labels: map[string]string{
			docker.LabelProject: "env_a", docker.LabelService: "web", docker.LabelNumber: "3",
		}}}
		if s := dockerSource(c); s.GetContainerName() != "env_a-web-3" || s.GetReplica() != 1 {
			t.Errorf("a running %s reads as %q, replica %d", name, s.GetContainerName(), s.GetReplica())
		}
		k := keptOf(c, false)
		if s := keptSource(&k); s.GetContainerName() != "env_a-web-3" || s.GetReplica() != 1 {
			t.Errorf("a kept %s reads as %q, replica %d", name, s.GetContainerName(), s.GetReplica())
		}
	}
}

// A follow says when a container it reads has gone with nothing kept of it, as a full
// disk leaves it. Otherwise a reader goes on counting it as running, for as long as it
// reads.
func TestAFollowSaysWhatHasGoneWithNothingKept(t *testing.T) {
	e := dockertest.New(t)
	project := docker.Project("env_a")
	labels := map[string]string{docker.LabelProject: project, docker.LabelService: "web", docker.LabelNumber: "1"}
	name := docker.InstanceName(project, "web", 1)
	e.AddContainer(docker.Container{
		Name: "/" + name, Created: time.Now().UTC(), Config: docker.Config{Image: "app", Labels: labels},
		State: docker.State{Status: "running", Running: true, StartedAt: time.Now().UTC()},
	})
	var said []dockwellv1.LogSourceState
	var mu sync.Mutex
	r := &Reader{Client: e.Client()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.Stream(ctx, &agentv1.LogsCall{Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: "env_a", Name: "web"}, Follow: true}, func(m *agentv1.SendLogsRequest) error {
			mu.Lock()
			defer mu.Unlock()
			for _, s := range m.GetSources() {
				said = append(said, s.GetState())
			}
			return nil
		})
	}()
	waitUntil := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s, with %v", what, said)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitUntil("it to say it runs", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(said, dockwellv1.LogSourceState_LOG_SOURCE_STATE_RUNNING)
	})
	if err := e.Client().RemoveContainer(t.Context(), e.Container(name).ID, true); err != nil {
		t.Fatal(err)
	}
	waitUntil("it to say it has gone", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(said, dockwellv1.LogSourceState_LOG_SOURCE_STATE_GONE)
	})
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Stream = %v after its reader left", err)
	}
}

// A first copy that ran out of time partway never learned whether the log held more
// than it copies. When the two copies together hold more lines than that, older ones
// may be missing, and the kept log says so, though the last copy reached the first.
func TestAKeptLogWhoseFirstCopyRanOutOfTimeSaysSoPastItsLineLimit(t *testing.T) {
	e := dockertest.New(t)
	c := e.AddContainer(docker.Container{
		Name: "/app-1", Created: time.Now().UTC(),
		Config: docker.Config{Image: "app", Labels: map[string]string{docker.LabelProject: "env_01test", docker.LabelService: "web", docker.LabelNumber: "1"}},
		State:  docker.State{Status: "running", Running: true, StartedAt: time.Now().UTC()},
	})
	batch := slices.Repeat([]string{"short"}, 10_000)
	for range 41 {
		e.Print("app-1", docker.Stdout, batch...)
	}
	dir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &Archive{Dir: dir, Client: e.Client(), Free: func(string) (uint64, error) { return 1 << 40, nil }, CopyWait: -1}
	ctx := t.Context()
	k, ok := a.Begin(ctx, c, false).(*keep)
	if !ok || !k.outOfTime || k.meta.CutShort {
		t.Fatalf("a first copy with no time: %+v", k)
	}
	// Its time being up partway, without a clock: the same read of the newest lines,
	// which the engine ends 10,001 lines before the last.
	e.BreakLog("app-1", copyTail-10_000)
	if _, _, err := k.copy(ctx, c, docker.LogOptions{Tail: copyTail + 1}); err == nil {
		t.Fatal("the read didn't stop partway")
	}
	e.Stop("app-1")
	k.Finish(ctx, e.Container("app-1"))
	a.WaitPruned()
	kept := a.List("env_01test", "web")
	if len(kept) != 1 {
		t.Fatalf("kept = %v", kept)
	}
	if got := kept[0]; got.Lines != copyTail+1 || !got.CutShort || !got.HoleTo.IsZero() {
		t.Errorf("kept %d of 410,000 lines, with cut_short %v and a hole to %v", got.Lines, got.CutShort, got.HoleTo)
	}
}
