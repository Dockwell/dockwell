// SPDX-License-Identifier: AGPL-3.0-only

package logs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/logs"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

const (
	testProject = "env_01test"
	testService = "web"
)

var testRef = &dockwellv1.ResourceRef{Kind: "service", Scope: "env_01TEST", Name: testService}

// addContainer makes a running container of the test's service.
func addContainer(t *testing.T, e *dockertest.Engine, name string, revision int) *docker.Container {
	t.Helper()
	return e.AddContainer(docker.Container{
		Name: "/" + name,
		Config: docker.Config{Image: "app:1", Labels: map[string]string{
			docker.LabelProject: testProject, docker.LabelService: testService, docker.LabelNumber: "1",
			docker.OwnPrefix + "revision": strconv.Itoa(revision), docker.OwnPrefix + "change": "chg_" + strconv.Itoa(revision),
		}},
		Created: time.Now().Add(-48 * time.Hour).UTC(),
		State:   docker.State{Status: "running", Running: true, StartedAt: time.Now().UTC()},
	})
}

// collector gathers what a stream sends.
type collector struct {
	mu      sync.Mutex
	lines   []string
	byLine  map[string]*dockwellv1.LogLine
	sources map[string]*dockwellv1.LogSource
	gaps    []*dockwellv1.LogGap
	scans   []*agentv1.LogScan
	caught  bool
	dropped uint64
	bytes   uint64
}

func newCollector() *collector {
	return &collector{sources: map[string]*dockwellv1.LogSource{}, byLine: map[string]*dockwellv1.LogLine{}}
}

func (c *collector) send(m *agentv1.SendLogsRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range m.GetSources() {
		c.sources[s.GetId()] = s
	}
	c.scans = append(c.scans, m.GetScans()...)
	c.caught = c.caught || m.GetCaughtUp()
	for _, chunk := range m.GetChunks() {
		if g := chunk.GetGap(); g != nil {
			c.gaps = append(c.gaps, g)
			c.dropped += g.GetLines()
		}
		for _, l := range chunk.GetLines() {
			c.lines = append(c.lines, l.GetText())
			c.byLine[l.GetText()] = l
			c.bytes += uint64(len(l.GetText()))
		}
	}
	return nil
}

func (c *collector) got() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lines)
}

func (c *collector) isCaughtUp() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caught
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newReader(t *testing.T, e *dockertest.Engine) *logs.Reader {
	t.Helper()
	dir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &logs.Reader{
		Client:  e.Client(),
		Archive: &logs.Archive{Dir: dir, Client: e.Client(), Free: func(string) (uint64, error) { return 1 << 40, nil }},
	}
}

func follow(ctx context.Context, r *logs.Reader, call *agentv1.LogsCall, c *collector) chan error {
	if call.GetResource() == nil {
		call.Resource = testRef
	}
	call.Follow = true
	done := make(chan error, 1)
	go func() { done <- r.Stream(ctx, call, c.send) }()
	return done
}

func TestReadsATailThenFollows(t *testing.T) {
	e := dockertest.New(t)
	ctr := addContainer(t, e, "app-1", 3)
	for i := range 5 {
		e.Print(ctr.Name[1:], docker.Stdout, fmt.Sprintf("old %d", i))
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	done := follow(ctx, newReader(t, e), &agentv1.LogsCall{Tail: 2}, c)

	waitFor(t, "the tail", func() bool { return c.count() == 2 })
	if got := c.got(); got[0] != "old 3" || got[1] != "old 4" {
		t.Errorf("tail = %q, want the last two", got)
	}
	waitFor(t, "being caught up", c.isCaughtUp)

	before := time.Now()
	e.Print(ctr.Name[1:], docker.Stderr, "ERROR new line")
	waitFor(t, "the new line", func() bool { return c.count() == 3 })
	if took := time.Since(before); took > time.Second {
		t.Errorf("a new line took %s to arrive, want under a second", took)
	}
	line := c.byLine["ERROR new line"]
	if line.GetLevel() != dockwellv1.LogLevel_LOG_LEVEL_ERROR || line.GetStream() != dockwellv1.LogStream_LOG_STREAM_STDERR {
		t.Errorf("the line = %v, want an error on stderr", line)
	}
	src := c.sources[ctr.ID[:12]]
	if src == nil || src.GetRevision() != 3 || src.GetChangeId() != "chg_3" || src.GetReplica() != 1 || src.GetState() != dockwellv1.LogSourceState_LOG_SOURCE_STATE_RUNNING {
		t.Errorf("source = %v, want revision 3, change chg_3, replica 1, running", src)
	}

	// The reader goes away, and the work on Docker stops at once.
	if e.Followers() != 1 {
		t.Fatalf("followers = %d, want 1", e.Followers())
	}
	gone := time.Now()
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Stream = %v after its reader left, want nil", err)
	}
	waitFor(t, "Docker's follow to end", func() bool { return e.Followers() == 0 })
	if took := time.Since(gone); took > 500*time.Millisecond {
		t.Errorf("Docker's follow took %s to end after the reader left", took)
	}
}

func TestAnEmptyLogIsCaughtUp(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, newReader(t, e), &agentv1.LogsCall{Tail: 100}, c)
	waitFor(t, "being caught up with nothing to read", c.isCaughtUp)

	e2 := dockertest.New(t)
	c2 := newCollector()
	follow(ctx, newReader(t, e2), &agentv1.LogsCall{Tail: 100}, c2)
	waitFor(t, "being caught up with no containers", c2.isCaughtUp)
}

func TestFollowsANewContainerFromItsStart(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, newReader(t, e), &agentv1.LogsCall{Tail: 10}, c)
	waitFor(t, "being caught up", c.isCaughtUp)

	// A deploy: the old one goes, and the new one prints before anyone looks.
	e.Stop("app-1")
	next := addContainer(t, e, "app-2", 2)
	e.Print("app-2", docker.Stdout, "started", "ready")
	waitFor(t, "the new container's lines", func() bool { return c.count() == 2 })
	if got := c.got(); got[0] != "started" || got[1] != "ready" {
		t.Errorf("lines = %q, want them from the start", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if src := c.sources[next.ID[:12]]; src.GetRevision() != 2 {
		t.Errorf("the new container's source = %v, want revision 2", src)
	}
}

func TestFollowsAContainerThatRestarts(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "one")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, newReader(t, e), &agentv1.LogsCall{}, c)
	waitFor(t, "the first line", func() bool { return c.count() == 1 })
	e.Stop("app-1")
	time.Sleep(50 * time.Millisecond)
	e.Edit("app-1", func(ctr *docker.Container) { ctr.State = docker.State{Status: "running", Running: true} })
	e.Restart("app-1")
	e.Print("app-1", docker.Stdout, "two")
	waitFor(t, "the line after the restart", func() bool { return c.count() == 2 })
	if got := c.got(); got[1] != "two" {
		t.Errorf("lines = %q, want one then two, with nothing twice", got)
	}
}

// A very fast writer: everything it wrote is either delivered or counted, the
// stream never blocks on Docker, and memory stays flat.
func TestAFloodIsDroppedAndCountedExactly(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := newReader(t, e)
	// A limit any build's flood passes. Under -race with coverage, as CI runs it,
	// atomic counters in the loops over a kept line's bytes hold a stream that
	// drops nothing to about 1 MiB/s, under the real limit, so nothing would ever
	// drop. make logs-load measures the real limit, on a build without them.
	const rate = 64 << 10
	r.LiveRate = rate
	c := newCollector()
	done := follow(ctx, r, &agentv1.LogsCall{Tail: -1}, c)
	waitFor(t, "being caught up", c.isCaughtUp)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	flood := e.Flood("app-1", 200)
	time.Sleep(1500 * time.Millisecond)
	flood.Stop()
	written := flood.Lines()
	runtime.ReadMemStats(&after)

	// Everything written arrives, or is counted, once the last marker is due.
	waitFor(t, "every line to be accounted for", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return uint64(len(c.lines))+c.dropped == flood.Lines()
	})
	c.mu.Lock()
	kept, dropped := uint64(len(c.lines)), c.dropped
	gaps := len(c.gaps)
	c.mu.Unlock()
	t.Logf("wrote %d lines in 1.5 s: delivered %d, dropped %d, in %d markers; heap in use %.1f MB to %.1f MB",
		written, kept, dropped, gaps, float64(before.HeapInuse)/1e6, float64(after.HeapInuse)/1e6)
	if dropped == 0 || gaps == 0 {
		t.Errorf("dropped %d lines in %d markers, want some of each: the fake writes far past the limit", dropped, gaps)
	}
	limit := uint64(1.5*float64(rate))/232 + uint64(rate/4)/232 + 100
	if kept > limit {
		t.Errorf("delivered %d lines, past the %d the rate limit allows", kept, limit)
	}
	// Nothing past the limit sits in memory.
	if grew := int64(after.HeapInuse) - int64(before.HeapInuse); grew > 32<<20 {
		t.Errorf("the heap grew by %.1f MB during the flood", float64(grew)/1e6)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Stream = %v", err)
	}
}

// A reader that stops reading: the stream drops what it can't send, keeps
// reading Docker, and catches up with a marker when the reader returns.
func TestAStuckReaderDoesntBlockAndGetsAMarker(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := newReader(t, e)
	r.LiveRate, r.TotalRate = 1<<40, 1<<40 // only the reader limits it

	release := make(chan struct{})
	var mu sync.Mutex
	var blocked atomic.Bool
	lines, dropped := uint64(0), uint64(0)
	reasons := map[dockwellv1.LogGapReason]bool{}
	send := func(m *agentv1.SendLogsRequest) error { //nolint:unparam // what Stream takes
		mu.Lock()
		for _, chunk := range m.GetChunks() {
			lines += uint64(len(chunk.GetLines()))
			if g := chunk.GetGap(); g != nil {
				dropped += g.GetLines()
				reasons[g.GetReason()] = true
			}
		}
		mu.Unlock()
		if blocked.CompareAndSwap(false, true) {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- r.Stream(ctx, &agentv1.LogsCall{Resource: testRef, Follow: true, Tail: -1}, send)
	}()
	time.Sleep(100 * time.Millisecond)
	flood := e.Flood("app-1", 200)
	waitFor(t, "the reader to block", blocked.Load)
	// The stream goes on reading Docker while its reader is stuck.
	at := flood.Lines()
	waitFor(t, "Docker to go on being read", func() bool { return flood.Lines() > at+10_000 })
	close(release)
	time.Sleep(200 * time.Millisecond)
	flood.Stop()
	waitFor(t, "every line to be accounted for", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return lines+dropped == flood.Lines()
	})
	mu.Lock()
	defer mu.Unlock()
	if !reasons[dockwellv1.LogGapReason_LOG_GAP_REASON_READER_BEHIND] {
		t.Errorf("reasons = %v, want the reader behind", reasons)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Stream = %v", err)
	}
}

func TestResumesWithoutDuplicatesOrGaps(t *testing.T) {
	e := dockertest.New(t)
	ctr := addContainer(t, e, "app-1", 1)
	base := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	for i := range 6 {
		// Two lines share each time, which a place in a log has to tell apart.
		e.PrintAt("app-1", base.Add(time.Duration(i/2)*time.Millisecond), docker.Stdout, fmt.Sprintf("line %d", i))
	}
	r := newReader(t, e)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// It read lines 0 to 2 before it went away: the third line is the first
	// of the two at that time.
	c := newCollector()
	follow(ctx, r, &agentv1.LogsCall{
		After: []*agentv1.LogCursor{{SourceId: ctr.ID[:12], Time: timestamppb.New(base.Add(time.Millisecond)), Count: 1}},
	}, c)
	e.Print("app-1", docker.Stdout, "new")
	waitFor(t, "the lines after the cursor", func() bool { return c.count() == 4 })
	want := []string{"line 3", "line 4", "line 5", "new"}
	got := c.got()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resumed with %q, want %q", got, want)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.gaps) != 0 {
		t.Errorf("gaps = %v, want none", c.gaps)
	}
}

func TestSaysWhatRotationTookWhileAway(t *testing.T) {
	e := dockertest.New(t)
	ctr := addContainer(t, e, "app-1", 1)
	base := time.Now().Add(-time.Hour).UTC()
	for i := range 10 {
		e.PrintAt("app-1", base.Add(time.Duration(i)*time.Minute), docker.Stdout, fmt.Sprintf("line %d", i))
	}
	e.Rotate("app-1", 3)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, newReader(t, e), &agentv1.LogsCall{
		After: []*agentv1.LogCursor{{SourceId: ctr.ID[:12], Time: timestamppb.New(base.Add(2 * time.Minute)), Count: 1}},
	}, c)
	waitFor(t, "the lines Docker still has", func() bool { return c.count() == 3 })
	waitFor(t, "a marker for what it doesn't", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.gaps) == 1
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	g := c.gaps[0]
	if g.GetReason() != dockwellv1.LogGapReason_LOG_GAP_REASON_GONE || !g.GetStartTime().AsTime().Equal(base.Add(2*time.Minute)) || !g.GetEndTime().AsTime().Equal(base.Add(7*time.Minute)) {
		t.Errorf("gap = %v, want gone from minute 2 to minute 7", g)
	}
}

func TestSaysWhenAContainerIsGoneWhileAway(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, newReader(t, e), &agentv1.LogsCall{
		After: []*agentv1.LogCursor{{SourceId: "feedfacefeed", Time: timestamppb.New(time.Now().Add(-time.Minute)), Count: 1}},
	}, c)
	waitFor(t, "a marker for the container that went", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.gaps) == 1
	})
}

func TestCarriesOnFromAKeptContainer(t *testing.T) {
	e := dockertest.New(t)
	old := addContainer(t, e, "app-1", 1)
	base := time.Now().Add(-time.Hour).UTC()
	for i := range 4 {
		e.PrintAt("app-1", base.Add(time.Duration(i)*time.Second), docker.Stdout, fmt.Sprintf("old %d", i))
	}
	r := newReader(t, e)
	keep(t, r, e, "app-1", false)
	next := addContainer(t, e, "app-2", 2)
	e.Print("app-2", docker.Stdout, "new")
	_ = next

	// The stream read old 0 and old 1, then the server was away for the deploy.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, r, &agentv1.LogsCall{
		After:      []*agentv1.LogCursor{{SourceId: old.ID[:12], Time: timestamppb.New(base.Add(time.Second)), Count: 1}},
		ResumeFrom: timestamppb.New(base.Add(time.Second)),
	}, c)
	waitFor(t, "the rest of the old container, then the new one", func() bool { return c.count() == 3 })
	got := strings.Join(c.got(), ",")
	for _, line := range []string{"old 2", "old 3", "new"} {
		if !strings.Contains(got, line) {
			t.Errorf("got %s, want %s in it", got, line)
		}
	}
	if strings.Contains(got, "old 1") {
		t.Errorf("got %s, with a line sent before", got)
	}
}

func once(t *testing.T, r *logs.Reader, call *agentv1.LogsCall) *collector {
	t.Helper()
	call.Resource = testRef
	c := newCollector()
	if err := r.Stream(t.Context(), call, c.send); err != nil {
		t.Fatalf("Stream = %v", err)
	}
	return c
}

func TestReadsATimeRange(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for i := range 5 {
		e.PrintAt("app-1", base.Add(time.Duration(i)*time.Minute), docker.Stdout, fmt.Sprintf("line %d", i))
	}
	c := once(t, newReader(t, e), &agentv1.LogsCall{Since: timestamppb.New(base.Add(time.Minute)), Until: timestamppb.New(base.Add(3 * time.Minute))})
	if got := strings.Join(c.got(), ","); got != "line 1,line 2,line 3" {
		t.Errorf("range = %s, want lines 1 to 3", got)
	}
	if !c.isCaughtUp() {
		t.Error("a read that ended didn't say it was caught up")
	}
}

func TestPagesBackward(t *testing.T) {
	e := dockertest.New(t)
	ctr := addContainer(t, e, "app-1", 1)
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for i := range 6 {
		e.PrintAt("app-1", base.Add(time.Duration(i)*time.Second), docker.Stdout, fmt.Sprintf("line %d", i))
	}
	c := once(t, newReader(t, e), &agentv1.LogsCall{
		Tail:   2,
		Before: []*agentv1.LogCursor{{SourceId: ctr.ID[:12], Time: timestamppb.New(base.Add(4 * time.Second))}},
	})
	// A tail reads Docker's newest, then the page's end cuts them: a reader
	// that pages reads a full tail again, which the control plane asks for.
	got := strings.Join(c.got(), ",")
	if strings.Contains(got, "line 4") || strings.Contains(got, "line 5") {
		t.Errorf("page = %s, want only lines before the cursor", got)
	}
}

func TestSearchFindsLinesAnywhereWithinItsLimits(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "GET /orders 200", "POST /CART 500", "GET /cart 200", "ERROR the cart is on fire", "    at cart.go:1", "INFO fine")
	r := newReader(t, e)

	c := once(t, r, &agentv1.LogsCall{Query: "cart", Tail: 100})
	if got := strings.Join(c.got(), ","); got != "POST /CART 500,GET /cart 200,ERROR the cart is on fire,    at cart.go:1" {
		t.Errorf("search = %s", got)
	}
	if len(c.scans) != 1 || c.scans[0].GetLines() != 6 || c.scans[0].GetOldestTime() == nil {
		t.Errorf("scans = %v, want 6 lines read, with the oldest's time", c.scans)
	}

	// Levels: an indented line takes the level of the line it follows, and a
	// line with no level counts as info.
	c = once(t, r, &agentv1.LogsCall{Levels: []dockwellv1.LogLevel{dockwellv1.LogLevel_LOG_LEVEL_ERROR}, Tail: 100})
	if got := strings.Join(c.got(), ","); got != "ERROR the cart is on fire,    at cart.go:1" {
		t.Errorf("errors = %s", got)
	}
	c = once(t, r, &agentv1.LogsCall{Levels: []dockwellv1.LogLevel{dockwellv1.LogLevel_LOG_LEVEL_INFO}, Tail: 100})
	if got := strings.Join(c.got(), ","); got != "GET /orders 200,POST /CART 500,GET /cart 200,INFO fine" {
		t.Errorf("info = %s", got)
	}

	// A search keeps the newest matches.
	c = once(t, r, &agentv1.LogsCall{Query: "cart", Tail: 2})
	if got := strings.Join(c.got(), ","); got != "ERROR the cart is on fire,    at cart.go:1" {
		t.Errorf("newest matches = %s", got)
	}
}

func TestBinaryAndLongLinesStayValid(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	long := strings.Repeat("y", 40_000)
	e.Print("app-1", docker.Stdout, "bad \xff\xfe utf-8", "\x1b[31mred\x1b[0m", "nul\x00byte", long)
	c := once(t, newReader(t, e), &agentv1.LogsCall{})
	got := c.got()
	if len(got) != 4 {
		t.Fatalf("lines = %d, want 4", len(got))
	}
	if got[0] != "bad � utf-8" {
		t.Errorf("invalid UTF-8 = %q", got[0])
	}
	if got[1] != "\x1b[31mred\x1b[0m" {
		t.Errorf("escapes = %q, want them as printed, for a client to handle", got[1])
	}
	if len(got[3]) != docker.MaxLogLine || c.byLine[got[3]].GetCutBytes() != uint32(40_000-docker.MaxLogLine) {
		t.Errorf("a long line came as %d bytes, %d cut", len(got[3]), c.byLine[got[3]].GetCutBytes())
	}
}

func TestALogThatCantBeReadSaysSo(t *testing.T) {
	e := dockertest.New(t)
	ctr := e.AddContainer(docker.Container{
		Name: "/app-1",
		Config: docker.Config{Image: "app:1", Labels: map[string]string{
			docker.LabelProject: testProject, docker.LabelService: testService,
		}},
		HostConfig: docker.HostConfig{LogConfig: &docker.LogConfig{Type: "none"}},
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, newReader(t, e), &agentv1.LogsCall{}, c)
	waitFor(t, "the source to say why", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		s := c.sources[ctr.ID[:12]]
		return s != nil && strings.Contains(s.GetLogError(), "doesn't support reading") || s != nil && strings.Contains(s.GetLogError(), "does not support reading")
	})
}

func TestTooManyStreamsAreRefused(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	r := newReader(t, e)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for range logs.MaxStreams {
		follow(ctx, r, &agentv1.LogsCall{}, newCollector())
	}
	waitFor(t, "the streams to start", func() bool { return e.Followers() == logs.MaxStreams })
	err := r.Stream(ctx, &agentv1.LogsCall{Resource: testRef, Follow: true}, newCollector().send)
	if err == nil || !strings.Contains(err.Error(), "too many readers") {
		t.Errorf("one stream too many = %v", err)
	}
}

// keep puts a container's log in the archive and removes it, as a replace does.
func keep(t *testing.T, r *logs.Reader, e *dockertest.Engine, name string, failed bool) string {
	t.Helper()
	ctx := t.Context()
	ctr := e.Container(name)
	if ctr == nil {
		t.Fatalf("no container %s", name)
	}
	k := r.Archive.Begin(ctx, ctr, failed)
	e.Stop(name)
	k.Finish(ctx, e.Container(name))
	if err := r.Client.RemoveContainer(ctx, ctr.ID, true); err != nil {
		t.Fatal(err)
	}
	return ctr.ID[:12]
}

func TestAKeptLogOutlivesItsContainer(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 7)
	e.Print("app-1", docker.Stdout, "before the stop", "\x1b[31mERROR\x1b[0m boom")
	r := newReader(t, e)
	ctx := t.Context()
	ctr := e.Container("app-1")
	k := r.Archive.Begin(ctx, ctr, true)
	// What it writes between the first copy and the stop is kept too.
	e.Print("app-1", docker.Stderr, "last words")
	e.Stop("app-1")
	k.Finish(ctx, e.Container("app-1"))
	if err := r.Client.RemoveContainer(ctx, ctr.ID, true); err != nil {
		t.Fatal(err)
	}
	id := ctr.ID[:12]

	found, err := r.Sources(ctx, testRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].GetId() != id || found[0].GetState() != dockwellv1.LogSourceState_LOG_SOURCE_STATE_KEPT ||
		found[0].GetKept() != dockwellv1.LogKept_LOG_KEPT_FAILED || found[0].GetRevision() != 7 || found[0].GetChangeId() != "chg_7" {
		t.Fatalf("sources = %v, want the kept container, failed, of revision 7", found)
	}
	c := once(t, r, &agentv1.LogsCall{Revision: 7})
	if got := strings.Join(c.got(), ","); got != "before the stop,\x1b[31mERROR\x1b[0m boom,last words" {
		t.Errorf("kept lines = %q", got)
	}
	if l := c.byLine["last words"]; l.GetStream() != dockwellv1.LogStream_LOG_STREAM_STDERR {
		t.Errorf("stream = %v, want stderr", l.GetStream())
	}
	if l := c.byLine["\x1b[31mERROR\x1b[0m boom"]; l.GetLevel() != dockwellv1.LogLevel_LOG_LEVEL_ERROR {
		t.Errorf("level = %v, want error", l.GetLevel())
	}

	// Not unless asked: a live stream shows what runs now.
	c = once(t, r, &agentv1.LogsCall{})
	if len(c.got()) != 0 {
		t.Errorf("read %q of a kept log without asking", c.got())
	}
	// A tail of it reads its newest lines.
	c = once(t, r, &agentv1.LogsCall{Revision: 7, Tail: 1})
	if got := strings.Join(c.got(), ","); got != "last words" {
		t.Errorf("tail of a kept log = %q", got)
	}
	// A search goes through it too.
	c = once(t, r, &agentv1.LogsCall{IncludeKept: true, Query: "BOOM", Tail: 10})
	if got := c.got(); len(got) != 1 || !strings.Contains(got[0], "boom") {
		t.Errorf("search of a kept log = %q", got)
	}
	// Who can read it: only the owner.
	info, err := os.Stat(r.Archive.Dir + "/" + testProject + "/" + testService + "/" + id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("a kept log's directory is %v, want 0700", info.Mode().Perm())
	}
	files, _ := filepath.Glob(r.Archive.Dir + "/" + testProject + "/" + testService + "/" + id + "/*")
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v, want 0600", f, info.Mode().Perm())
		}
	}
}

func TestKeepsTheLastFailedAndTheLastReplaced(t *testing.T) {
	e := dockertest.New(t)
	r := newReader(t, e)
	now := time.Now()
	r.Archive.Now = func() time.Time { now = now.Add(time.Minute); return now }
	made := map[string]string{}
	for i, kind := range []string{"replaced", "failed", "replaced", "replaced", "failed", "replaced"} {
		name := fmt.Sprintf("app-%d", i)
		addContainer(t, e, name, i+1)
		e.Print(name, docker.Stdout, name)
		made[kind] = keep(t, r, e, name, kind == "failed")
	}
	found, err := r.Sources(t.Context(), testRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("kept %d, want the newest failed and the newest replaced", len(found))
	}
	for _, s := range found {
		want := made["replaced"]
		if s.GetKept() == dockwellv1.LogKept_LOG_KEPT_FAILED {
			want = made["failed"]
		}
		if s.GetId() != want {
			t.Errorf("kept %s for %v, want %s", s.GetId(), s.GetKept(), want)
		}
	}
}

func TestTheArchiveStaysWithinItsBudget(t *testing.T) {
	e := dockertest.New(t)
	r := newReader(t, e)
	now := time.Now()
	r.Archive.Now = func() time.Time { now = now.Add(time.Minute); return now }
	r.Archive.Budget = 12_000
	// Each service's lines don't compress, so each takes real room.
	noise := func(seed int) []string {
		out := make([]string, 200)
		for i := range out {
			out[i] = fmt.Sprintf("%d %x %x", seed*i, seed*i*7919, seed*i*104729)
		}
		return out
	}
	service := func(i int, failed bool) {
		name := fmt.Sprintf("svc%d-1", i)
		e.AddContainer(docker.Container{
			Name: "/" + name,
			Config: docker.Config{Image: "app:1", Labels: map[string]string{
				docker.LabelProject: testProject, docker.LabelService: fmt.Sprintf("svc%d", i), docker.LabelNumber: "1",
			}},
			State: docker.State{Status: "running", Running: true},
		})
		e.Print(name, docker.Stdout, noise(i+1)...)
		keep(t, r, e, name, failed)
	}
	// The first service failed; the others were replaced later, in order.
	service(0, true)
	for i := 1; i < 8; i++ {
		service(i, false)
	}
	total := int64(0)
	present := map[int]bool{}
	for i := range 8 {
		for _, k := range r.Archive.List(testProject, fmt.Sprintf("svc%d", i)) {
			total += k.Disk
			present[i] = true
		}
	}
	if total > r.Archive.Budget {
		t.Errorf("the archive holds %d bytes, past its budget of %d", total, r.Archive.Budget)
	}
	if !present[7] {
		t.Error("the newest log went, though it was just kept")
	}
	if !present[0] {
		t.Error("what failed went before what was only replaced")
	}
	if present[1] {
		t.Error("the oldest replaced log stayed, past the budget")
	}
}

func TestAKeptLogLeavesOnTime(t *testing.T) {
	e := dockertest.New(t)
	r := newReader(t, e)
	addContainer(t, e, "app-1", 1)
	keep(t, r, e, "app-1", true)
	r.Archive.Now = func() time.Time { return time.Now().Add(logs.RetainFor + time.Hour) }
	if err := r.Archive.Prune(); err != nil {
		t.Fatal(err)
	}
	if got := r.Archive.List(testProject, testService); len(got) != 0 {
		t.Errorf("kept %d past the time, want none", len(got))
	}
}

func TestAFullDiskKeepsNothing(t *testing.T) {
	e := dockertest.New(t)
	r := newReader(t, e)
	r.Archive.Free = func(string) (uint64, error) { return 100 << 20, nil }
	addContainer(t, e, "app-1", 1)
	if k := r.Archive.Begin(t.Context(), e.Container("app-1"), true); k != nil {
		t.Error("kept a log on a nearly full disk")
	}
}

func TestAKeptLogKeepsOnlyItsNewestLines(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a large log")
	}
	e := dockertest.New(t)
	r := newReader(t, e)
	addContainer(t, e, "app-1", 1)
	const lines = 40_000
	big := strings.Repeat("z", 1000)
	batch := make([]string, 0, lines)
	for i := range lines {
		batch = append(batch, fmt.Sprintf("%06d %s", i, big))
	}
	e.Print("app-1", docker.Stdout, batch...)
	keep(t, r, e, "app-1", false)
	got := r.Archive.List(testProject, testService)
	if len(got) != 1 || !got[0].CutShort {
		t.Fatalf("kept = %v, want one, cut short: it wrote 40 MB, past the 32 MiB kept", got)
	}
	c := once(t, r, &agentv1.LogsCall{IncludeKept: true, Tail: 1})
	if last := c.got(); len(last) != 1 || !strings.HasPrefix(last[0], fmt.Sprintf("%06d", lines-1)) {
		t.Errorf("the newest line kept = %.20q, want the last it wrote", last)
	}
	raw, _ := json.Marshal(got[0])
	t.Logf("%s", raw)
}
