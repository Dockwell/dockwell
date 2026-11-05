// SPDX-License-Identifier: AGPL-3.0-only

package logs_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/logs"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// printMany writes lines of text to the test's container, a batch at a time.
func printMany(e *dockertest.Engine, text string, lines int) {
	batch := make([]string, 10_000)
	for i := range batch {
		batch[i] = text
	}
	for ; lines > 0; lines -= len(batch) {
		e.Print("app-1", docker.Stdout, batch[:min(lines, len(batch))]...)
	}
}

// A search holds what its bound says, whatever its lines' length. Short lines cost
// far more than their text, so a million one-letter matches once took 200 MB. This
// many are past any bound that counts text alone. make logs-load runs a million on
// the real agent build.
func TestASearchHoldsItsBoundWhateverItsLines(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	printMany(e, "x", 200_000)
	r := newReader(t, e)
	for _, call := range []*agentv1.LogsCall{
		{Resource: testRef, Query: "x", Tail: logsMaxTail},
		// As a stream that reads to the end asks when it carries on.
		{Resource: testRef, Query: "x", Tail: 0},
		// Levels, whose matches may be blank lines with no text at all.
		{Resource: testRef, Levels: []dockwellv1.LogLevel{dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED, dockwellv1.LogLevel_LOG_LEVEL_INFO}, Tail: 0},
	} {
		base := liveHeap()
		var peak atomic.Uint64
		var once sync.Once
		got := atomic.Uint64{}
		// Its matches go out after the read, so the first message with lines is
		// when all of them are still held.
		send := func(m *agentv1.SendLogsRequest) error {
			for _, c := range m.GetChunks() {
				got.Add(uint64(len(c.GetLines())))
				if len(c.GetLines()) > 0 {
					once.Do(func() { peak.Store(liveHeap()) })
				}
			}
			return nil
		}
		if err := r.Stream(t.Context(), call, send); err != nil {
			t.Fatal(err)
		}
		grew := float64(peak.Load()) - float64(base)
		t.Logf("tail %d held %.1f MB for one container, and sent %d lines", call.GetTail(), grew/(1<<20), got.Load())
		if grew > 8<<20 {
			t.Errorf("a search with tail %d held %.0f MB for one container", call.GetTail(), grew/(1<<20))
		}
	}
}

const logsMaxTail = 100_000

// A read of no lines reads nothing, whatever it would have filtered.
func TestAReadOfNoLinesReadsNothing(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "x")
	r := newReader(t, e)
	keep(t, r, e, "app-1", false)
	addContainer(t, e, "app-2", 2)
	e.Print("app-2", docker.Stdout, "x")
	c := once(t, r, &agentv1.LogsCall{Query: "x", Tail: -1, IncludeKept: true})
	if c.count() != 0 {
		t.Errorf("a read of no lines got %q", c.got())
	}
}

// A read to the end has nowhere to say older matches are there but the stream,
// as it has no pages.
func TestAReadToTheEndSaysWhatItLetGo(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	printMany(e, "match "+strings.Repeat("m", 1000), 3_000)
	c := once(t, newReader(t, e), &agentv1.LogsCall{Query: "match", Tail: logsMaxTail})
	total := uint64(c.count()) + c.dropped
	if c.count() == 0 || c.count() >= 3_000 {
		t.Fatalf("it sent %d of 3,000 lines of 1 KB", c.count())
	}
	if len(c.gaps) != 1 || c.gaps[0].GetReason() != dockwellv1.LogGapReason_LOG_GAP_REASON_MATCH_LIMIT || total != 3_000 {
		t.Errorf("it sent %d lines and said %v of what it let go", c.count(), c.gaps)
	}
	if g := c.gaps[0]; g.GetStartTime() == nil || g.GetEndTime() == nil {
		t.Errorf("the marker doesn't say where: %v", g)
	}
}

// A tail asked for is what was asked for, not what a limit took.
func TestATailAskedForIsntALimit(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	printMany(e, "match", 100)
	c := once(t, newReader(t, e), &agentv1.LogsCall{Query: "match", Tail: 10})
	if c.count() != 10 || len(c.gaps) != 0 {
		t.Errorf("a tail of 10 sent %d lines and %v", c.count(), c.gaps)
	}
}

// A search that stops at its most lines says so, and doesn't call that as far as
// the server keeps logs.
func TestASearchSaysWhenItStoppedAtItsLineLimit(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	printMany(e, "line", 1_100)
	r := newReader(t, e)
	r.SearchLines = 1_000
	c := once(t, r, &agentv1.LogsCall{Query: "nothing-matches", Tail: 5000})
	if len(c.scans) != 1 {
		t.Fatalf("scans = %v", c.scans)
	}
	if s := c.scans[0]; s.GetLines() != 1_000 || !s.GetCutShort() || s.GetTimedOut() {
		t.Errorf("of 1,100 lines it read %d, with cut_short %v and timed_out %v", s.GetLines(), s.GetCutShort(), s.GetTimedOut())
	}
}

func TestASearchThatReachesTheStartDoesntSayItStopped(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	printMany(e, "line", 1_000)
	r := newReader(t, e)
	r.SearchLines = 1_000
	c := once(t, r, &agentv1.LogsCall{Query: "nothing-matches", Tail: 5000})
	if s := c.scans[0]; s.GetLines() != 1_000 || s.GetCutShort() {
		t.Errorf("of 1,000 lines it read %d, with cut_short %v", s.GetLines(), s.GetCutShort())
	}
}

// A follow that outlives many deploys goes on reading each. Only one container runs
// at a time, so nothing is over any limit.
func TestAFollowOutlivesManyDeploys(t *testing.T) {
	e := dockertest.New(t)
	ctr := addContainer(t, e, "app-0", 0)
	r := newReader(t, e)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, r, &agentv1.LogsCall{Tail: -1}, c)
	waitFor(t, "caught up", c.isCaughtUp)
	for i := 1; i <= logs.MaxSourcesPerStream+2; i++ {
		if err := r.Client.RemoveContainer(t.Context(), ctr.ID, true); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("app-%d", i)
		ctr = addContainer(t, e, name, i)
		want := fmt.Sprintf("line from deploy %d", i)
		time.Sleep(50 * time.Millisecond)
		e.Print(name, docker.Stdout, want)
		waitFor(t, want, func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.byLine[want] != nil
		})
	}
}

// More containers than a stream reads at once get a word, rather than silence,
// and a turn once one stops.
func TestAStreamSaysWhichContainersWaitForRoom(t *testing.T) {
	e := dockertest.New(t)
	var names []string
	for i := range logs.MaxSourcesPerStream + 1 {
		name := fmt.Sprintf("app-%d", i)
		addContainer(t, e, name, i)
		names = append(names, name)
	}
	r := newReader(t, e)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, r, &agentv1.LogsCall{Tail: -1}, c)
	waiting := func() string {
		c.mu.Lock()
		defer c.mu.Unlock()
		for id, s := range c.sources {
			if s.GetLogError() != "" {
				return id
			}
		}
		return ""
	}
	waitFor(t, "a container to say it waits", func() bool { return waiting() != "" })
	id := waiting()
	c.mu.Lock()
	said := c.sources[id].GetLogError()
	name := c.sources[id].GetContainerName()
	c.mu.Unlock()
	if !strings.Contains(said, "at most 32 containers") {
		t.Errorf("it said %q", said)
	}
	for _, other := range names {
		if other != name {
			e.Stop(other)
			break
		}
	}
	waitFor(t, "it to have its turn", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.sources[id].GetLogError() == ""
	})
	e.Print(name, docker.Stdout, "its turn came")
	waitFor(t, "its line", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.byLine["its turn came"] != nil
	})
}

func quietContainer(e *dockertest.Engine) *dockwellv1.ResourceRef {
	e.AddContainer(docker.Container{
		Name: "/quiet-1",
		Config: docker.Config{Image: "app:1", Labels: map[string]string{
			docker.LabelProject: testProject, docker.LabelService: "quiet", docker.LabelNumber: "1",
		}},
		Created: time.Now().Add(-48 * time.Hour).UTC(),
		State:   docker.State{Status: "running", Running: true, StartedAt: time.Now().UTC()},
	})
	return &dockwellv1.ResourceRef{Kind: "service", Scope: "env_01TEST", Name: "quiet"}
}

// A container nobody trusts can't stop people reading another service, however many
// are following it.
func TestAQuietServiceLosesNothingToANoisyOne(t *testing.T) {
	for _, noisy := range []int{0, 2, 15} {
		t.Run(fmt.Sprint(noisy, " followers of a noisy service"), func(t *testing.T) {
			e := dockertest.New(t)
			addContainer(t, e, "app-1", 1)
			ref := quietContainer(e)
			r := newReader(t, e)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if noisy > 0 {
				flood := e.Flood("app-1", 200)
				defer flood.Stop()
				for range noisy {
					follow(ctx, r, &agentv1.LogsCall{Tail: -1}, newCollector())
				}
			}
			q := newCollector()
			follow(ctx, r, &agentv1.LogsCall{Resource: ref, Tail: -1}, q)
			waitFor(t, "caught up", q.isCaughtUp)
			time.Sleep(time.Second)
			const written = 300
			for i := range written {
				e.Print("quiet-1", docker.Stdout, fmt.Sprintf("quiet line %d %s", i, strings.Repeat("q", 200)))
				time.Sleep(10 * time.Millisecond)
			}
			waitFor(t, "every line", func() bool { return q.count() == written })
			if q.dropped != 0 {
				t.Errorf("a quiet service lost %d lines with %d followers of another", q.dropped, noisy)
			}
		})
	}
}

// What a marker says of why lines are missing is true: a service over a stream's own
// limit wrote too fast, and one over a share smaller than that was sharing.
func TestAMarkerSaysWhyLinesAreMissing(t *testing.T) {
	reason := func(t *testing.T, followers int) dockwellv1.LogGapReason {
		t.Helper()
		e := dockertest.New(t)
		addContainer(t, e, "app-1", 1)
		r := newReader(t, e)
		// Low enough that a flood is past them on any machine.
		r.LiveRate, r.TotalRate = 64<<10, 64<<10
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		flood := e.Flood("app-1", 400)
		defer flood.Stop()
		var last *collector
		for range followers {
			last = newCollector()
			follow(ctx, r, &agentv1.LogsCall{Tail: -1}, last)
		}
		waitFor(t, "a marker", func() bool {
			last.mu.Lock()
			defer last.mu.Unlock()
			return len(last.gaps) > 0
		})
		last.mu.Lock()
		defer last.mu.Unlock()
		return last.gaps[0].GetReason()
	}
	if got := reason(t, 1); got != dockwellv1.LogGapReason_LOG_GAP_REASON_TOO_FAST {
		t.Errorf("one follower of a flood was told %v", got)
	}
	if got := reason(t, 4); got != dockwellv1.LogGapReason_LOG_GAP_REASON_SHARED {
		t.Errorf("four followers of a flood were told %v", got)
	}
}

// What a server carries live stays within its limit however it is shared out.
func TestFollowersStayWithinTheServersLimit(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	ref := quietContainer(e)
	r := newReader(t, e)
	r.TotalRate = 1 << 20
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	flood := e.Flood("app-1", 400)
	defer flood.Stop()
	quiet := e.Flood("quiet-1", 400)
	defer quiet.Stop()
	cs := make([]*collector, 4)
	for i := range cs {
		cs[i] = newCollector()
		call := &agentv1.LogsCall{Tail: -1}
		if i%2 == 1 {
			call.Resource = ref
		}
		follow(ctx, r, call, cs[i])
	}
	total := func() uint64 {
		n := uint64(0)
		for _, c := range cs {
			c.mu.Lock()
			n += c.bytes
			c.mu.Unlock()
		}
		return n
	}
	time.Sleep(500 * time.Millisecond)
	before := total()
	time.Sleep(3 * time.Second)
	rate := float64(total()-before) / 3
	if rate > 1.3*(1<<20) {
		t.Errorf("four followers of two floods carried %.1f MB/s between them, with a limit of 1 MiB/s", rate/(1<<20))
	}
	if rate < 0.5*(1<<20) {
		t.Errorf("four followers of two floods carried only %.2f MB/s, with a limit of 1 MiB/s", rate/(1<<20))
	}
}

func cpuUsed() time.Duration {
	var u syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &u)
	return time.Duration(u.Utime.Nano() + u.Stime.Nano())
}

// Reading a flood to drop it or filter it costs CPU, however many read it: all a
// server's follows read at most the pace between them.
func TestFollowsAreHeldToThePace(t *testing.T) {
	const size, pace, seconds = 200, 8 << 20, 2
	for _, tc := range []struct {
		name      string
		followers int
		call      *agentv1.LogsCall
	}{
		{"one that drops", 1, &agentv1.LogsCall{Tail: -1}},
		{"one that filters", 1, &agentv1.LogsCall{Tail: -1, Query: "never-printed"}},
		{"sixteen that filter", 16, &agentv1.LogsCall{Tail: -1, Query: "never-printed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := dockertest.New(t)
			addContainer(t, e, "app-1", 1)
			r := newReader(t, e)
			// Only the pace limits it.
			r.LiveRate, r.TotalRate, r.ShedRate = 1, 1, pace
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			flood := e.Flood("app-1", size)
			defer flood.Stop()
			for range tc.followers {
				follow(ctx, r, &agentv1.LogsCall{Tail: -1, Query: tc.call.GetQuery()}, newCollector())
			}
			waitFor(t, "the followers", func() bool { return e.Followers() == tc.followers })
			time.Sleep(300 * time.Millisecond)
			l0, c0 := flood.Lines(), cpuUsed()
			time.Sleep(seconds * time.Second)
			rate := float64(flood.Lines()-l0) * size / seconds
			t.Logf("read %.1f MB/s, process CPU %s", rate/(1<<20), (cpuUsed()-c0)/seconds)
			// The fake engine writes each follower's copy, so what it counts is
			// what they read between them.
			if rate > 1.3*pace {
				t.Errorf("read %.1f MB/s, past the pace of %d MiB/s", rate/(1<<20), pace>>20)
			}
		})
	}
}

// A line that's filtered out doesn't slow a follow of what matches below the pace.
func TestPacingDoesntDelayAQuietFollow(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, newReader(t, e), &agentv1.LogsCall{Tail: -1, Query: "wanted"}, c)
	waitFor(t, "caught up", c.isCaughtUp)
	e.Print("app-1", docker.Stdout, "not this", "not this either")
	start := time.Now()
	e.Print("app-1", docker.Stdout, "wanted")
	waitFor(t, "the line", func() bool { return c.count() == 1 })
	if took := time.Since(start); took > time.Second {
		t.Errorf("a line took %s", took)
	}
}

// What Docker still has waits for a slow reader. Only what is live may drop.
func TestAFollowsHistoryWaitsForASlowReader(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	// More than what waits for the network holds, of 256 KiB.
	const lines = 10_000
	batch := make([]string, lines)
	for i := range batch {
		batch[i] = fmt.Sprintf("history line %06d %s", i, strings.Repeat("h", 100))
	}
	e.Print("app-1", docker.Stdout, batch...)
	time.Sleep(10 * time.Millisecond)
	r := newReader(t, e)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	var paused atomic.Bool
	send := func(m *agentv1.SendLogsRequest) error {
		// One pause, as a busy tab or a slow link makes.
		if paused.CompareAndSwap(false, true) {
			time.Sleep(600 * time.Millisecond)
		}
		return c.send(m)
	}
	go func() {
		_ = r.Stream(ctx, &agentv1.LogsCall{Resource: testRef, Follow: true, Tail: lines}, send)
	}()
	waitFor(t, "every line to be accounted for", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return uint64(len(c.lines))+c.dropped >= lines
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped != 0 {
		t.Errorf("a pause of 600 ms dropped %d of %d lines of history that Docker still has", c.dropped, lines)
	}
}

// One token can't take every place on a server, so another can read during an
// incident.
func TestOneReaderCantTakeEveryPlace(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	r := newReader(t, e)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	never := func(*agentv1.SendLogsRequest) error { <-ctx.Done(); return ctx.Err() }
	for range logs.MaxStreamsPerReader {
		go func() {
			_ = r.Stream(ctx, &agentv1.LogsCall{Resource: testRef, Follow: true, Tail: -1, Reader: "token:stuck"}, never)
		}()
	}
	waitFor(t, "the streams to start", func() bool { return e.Followers() == logs.MaxStreamsPerReader })
	err := r.Stream(ctx, &agentv1.LogsCall{Resource: testRef, Follow: true, Tail: -1, Reader: "token:stuck"}, never)
	if err == nil || !strings.Contains(err.Error(), "the most one reader can") {
		t.Errorf("one more of a reader's streams = %v", err)
	}
	other := &dockwellv1.ResourceRef{Kind: "service", Scope: "env_01TEST", Name: "other"}
	if err := r.Stream(ctx, &agentv1.LogsCall{Resource: other, Query: "error", Tail: 100, Reader: "user:mira"}, newCollector().send); err != nil {
		t.Errorf("another reader, with one reader's streams stuck = %v", err)
	}
}

// However many searches a script starts, someone can still read live.
func TestSearchesLeaveAPlaceForSomeoneReadingLive(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "a match")
	r := newReader(t, e)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release := make(chan struct{})
	var started atomic.Int32
	holding := func(*agentv1.SendLogsRequest) error {
		started.Add(1)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer close(release)
	refused := make(chan error, logs.MaxStreams)
	for i := range logs.MaxStreams {
		go func() {
			refused <- r.Stream(ctx, &agentv1.LogsCall{Resource: testRef, Query: "match", Tail: 10, Reader: fmt.Sprint("token:", i)}, holding)
		}()
	}
	// Four come back refused, and the rest hold on.
	for range 4 {
		select {
		case err := <-refused:
			if err == nil || !strings.Contains(err.Error(), "too many searches") {
				t.Errorf("a search past its places = %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no search was refused")
		}
	}
	c := newCollector()
	follow(ctx, r, &agentv1.LogsCall{Tail: 1, Reader: "user:mira"}, c)
	waitFor(t, "someone reading live", c.isCaughtUp)
}

// A reader that stops taking lines loses its place, and the server what it held.
func TestAStuckReaderLosesItsPlace(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "a line")
	r := newReader(t, e)
	r.StuckAfter = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stuck := func(*agentv1.SendLogsRequest) error { <-ctx.Done(); return ctx.Err() }
	done := make(chan error, 1)
	go func() {
		done <- r.Stream(ctx, &agentv1.LogsCall{Resource: testRef, Follow: true, Tail: 10}, stuck)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, logs.ErrStuck) {
			t.Errorf("a stuck reader's stream ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck reader kept its place")
	}
	waitFor(t, "Docker's follow to end", func() bool { return e.Followers() == 0 })
}

// A quiet stream isn't a stuck one, however long it waits.
func TestAQuietFollowKeepsItsPlace(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	r := newReader(t, e)
	r.StuckAfter = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	done := follow(ctx, r, &agentv1.LogsCall{Tail: -1}, c)
	waitFor(t, "caught up", c.isCaughtUp)
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("a quiet follow ended: %v", err)
	default:
	}
	e.Print("app-1", docker.Stdout, "still here")
	waitFor(t, "the line", func() bool { return c.count() == 1 })
}

// A kept log says when a limit cut it, as its reader would otherwise think it has
// all of what the container wrote.
func TestAKeptLogSaysWhenItsLineLimitCutIt(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	printMany(e, "short", 410_000)
	r := newReader(t, e)
	keep(t, r, e, "app-1", false)
	k := r.Archive.List(testProject, testService)
	if len(k) != 1 {
		t.Fatalf("kept = %v", k)
	}
	if k[0].Lines >= 410_000 || !k[0].CutShort {
		t.Errorf("kept %d of 410,000 lines, with cut_short %v", k[0].Lines, k[0].CutShort)
	}
}

func TestAKeptLogWithAllItsLinesDoesntSaySoOtherwise(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	printMany(e, "short", 400_000)
	r := newReader(t, e)
	keep(t, r, e, "app-1", false)
	if k := r.Archive.List(testProject, testService); k[0].Lines != 400_000 || k[0].CutShort {
		t.Errorf("kept %d of 400,000 lines, with cut_short %v", k[0].Lines, k[0].CutShort)
	}
}

// A container that writes more while it stops than the last copy looks back leaves a
// hole in the middle of its log, and a marker says where.
func TestAKeptLogMarksTheHoleItsLastCopyLeft(t *testing.T) {
	e := dockertest.New(t)
	ctr := addContainer(t, e, "app-1", 1)
	base := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	e.PrintAt("app-1", base, docker.Stdout, "before the stop")
	r := newReader(t, e)
	ctx := t.Context()
	k := r.Archive.Begin(ctx, e.Container("app-1"), false)
	for i := range 30_000 {
		e.PrintAt("app-1", base.Add(time.Duration(i+1)*time.Millisecond), docker.Stdout, fmt.Sprintf("while stopping %d", i))
	}
	e.Stop("app-1")
	k.Finish(ctx, e.Container("app-1"))
	r.Archive.WaitPruned()
	if err := r.Client.RemoveContainer(ctx, ctr.ID, true); err != nil {
		t.Fatal(err)
	}
	kept := r.Archive.List(testProject, testService)
	if len(kept) != 1 || !kept[0].CutShort || kept[0].HoleTo.IsZero() {
		t.Fatalf("kept = %+v", kept[0])
	}
	c := once(t, r, &agentv1.LogsCall{IncludeKept: true, Tail: 0})
	if len(c.gaps) != 1 || c.gaps[0].GetReason() != dockwellv1.LogGapReason_LOG_GAP_REASON_GONE {
		t.Fatalf("reading it back said %v", c.gaps)
	}
	g := c.gaps[0]
	if !g.GetStartTime().AsTime().Equal(base) || !g.GetEndTime().AsTime().After(base) {
		t.Errorf("the marker says from %v to %v", g.GetStartTime().AsTime(), g.GetEndTime().AsTime())
	}
}

// A kept log's reader hears what is wrong with it: the container has gone, so Docker
// has nothing to do with it, and no path on the server reaches them.
func TestAKeptLogThatCantBeReadSaysWhatWentWrong(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "kept line")
	r := newReader(t, e)
	id := keep(t, r, e, "app-1", false)
	seg := filepath.Join(r.Archive.Dir, testProject, testService, id, "000001.gz")
	said := func() string {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		c := newCollector()
		follow(ctx, r, &agentv1.LogsCall{IncludeKept: true, Tail: 5}, c)
		var msg string
		waitFor(t, "the source to say why", func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			msg = c.sources[id].GetLogError()
			return msg != ""
		})
		return msg
	}
	if err := os.WriteFile(seg, []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	msg := said()
	for _, wrong := range []string{"Docker can't", "docker inspect", r.Archive.Dir} {
		if strings.Contains(msg, wrong) {
			t.Errorf("a kept log that can't be read says %q", msg)
		}
	}
	if !strings.Contains(msg, "kept") {
		t.Errorf("a kept log that can't be read says %q", msg)
	}
	if err := os.Chmod(seg, 0); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() != 0 {
		if msg := said(); strings.Contains(msg, r.Archive.Dir) {
			t.Errorf("a kept log it can't open says %q", msg)
		}
	}
}

// Time counts by the clock lines are on: a server's own, however far the control
// plane's is from it.
func TestALengthOfTimeIsBackFromThisServersClock(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	now := time.Now().UTC()
	e.PrintAt("app-1", now.Add(-2*time.Hour), docker.Stdout, "too old")
	e.PrintAt("app-1", now.Add(-10*time.Minute), docker.Stdout, "in the last hour")
	c := once(t, newReader(t, e), &agentv1.LogsCall{Last: durationpb.New(time.Hour), Tail: 100})
	if got := c.got(); len(got) != 1 || got[0] != "in the last hour" {
		t.Errorf("the last hour read %q", got)
	}
}

// A next page holds the lines of the time the last one ended on that it hasn't got.
func TestAPageBoundaryKeepsLinesOfOneTime(t *testing.T) {
	e := dockertest.New(t)
	ctr := addContainer(t, e, "app-1", 1)
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for i := range 5 {
		e.PrintAt("app-1", at, docker.Stdout, fmt.Sprintf("line %d", i))
	}
	r := newReader(t, e)
	// A page that holds the newest two of those, as lines 3 and 4.
	c := once(t, r, &agentv1.LogsCall{
		Tail:   2,
		Before: []*agentv1.LogCursor{{SourceId: ctr.ID[:12], Time: timestamppb.New(at), Count: 2}},
	})
	if got := strings.Join(c.got(), ","); got != "line 1,line 2" {
		t.Errorf("the next page = %s", got)
	}
	// The same from what the server kept.
	id := keep(t, r, e, "app-1", false)
	k := once(t, r, &agentv1.LogsCall{
		Tail: 2, IncludeKept: true,
		Before: []*agentv1.LogCursor{{SourceId: id, Time: timestamppb.New(at), Count: 2}},
	})
	if got := strings.Join(k.got(), ","); got != "line 1,line 2" {
		t.Errorf("the next page of a kept log = %s", got)
	}
}

// A deleted service's kept logs can't be read by any name, a new service of the same
// name starts clean, and undoing the deletion brings them back.
func TestADeletedServicesLogsLeaveUnlessUndone(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "password=hunter2")
	r := newReader(t, e)
	keep(t, r, e, "app-1", false)
	if len(r.Archive.List(testProject, testService)) != 1 {
		t.Fatal("the log wasn't kept")
	}

	r.Archive.Forget(testProject, testService)
	if got := r.Archive.List(testProject, testService); len(got) != 0 {
		t.Errorf("a deleted service still lists %v", got)
	}
	for _, name := range []string{testService, ".deleted", "../.deleted/" + testProject + "/" + testService} {
		if got := r.Archive.List(testProject, name); len(got) != 0 {
			t.Errorf("the name %q reads what was deleted: %v", name, got)
		}
	}
	if got := r.Archive.List(".deleted", testProject); len(got) != 0 {
		t.Errorf("the project .deleted reads what was deleted: %v", got)
	}
	c := once(t, r, &agentv1.LogsCall{IncludeKept: true, Tail: 100})
	if c.count() != 0 {
		t.Errorf("reading a deleted service got %q", c.got())
	}

	// A new one of that name writes and keeps logs of its own.
	addContainer(t, e, "app-2", 1)
	e.Print("app-2", docker.Stdout, "the new one")
	keep(t, r, e, "app-2", false)
	c = once(t, r, &agentv1.LogsCall{IncludeKept: true, Tail: 100})
	if got := c.got(); len(got) != 1 || got[0] != "the new one" {
		t.Errorf("a new service of the name read %q", got)
	}

	// Deleted in turn, and undone, it brings back its own, not the older one's.
	r.Archive.Forget(testProject, testService)
	r.Archive.Restore(testProject, testService)
	c = once(t, r, &agentv1.LogsCall{IncludeKept: true, Tail: 100})
	if got := c.got(); len(got) != 1 || got[0] != "the new one" {
		t.Errorf("undoing the newest deletion read %q", got)
	}
}

func TestUndoingADeletionBringsItsLogsBack(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "from before")
	r := newReader(t, e)
	keep(t, r, e, "app-1", true)
	r.Archive.Forget(testProject, testService)
	r.Archive.Restore(testProject, testService)
	c := once(t, r, &agentv1.LogsCall{IncludeKept: true, Tail: 100})
	if got := c.got(); len(got) != 1 || got[0] != "from before" {
		t.Errorf("after undoing the deletion it read %q", got)
	}
}

// What a deletion hid goes 7 days after it, as undoing it can't come later.
func TestADeletedServicesLogsGoAfterSevenDays(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "from before")
	r := newReader(t, e)
	keep(t, r, e, "app-1", false)
	now := time.Now()
	r.Archive.Now = func() time.Time { return now }
	r.Archive.Forget(testProject, testService)
	hidden := filepath.Join(r.Archive.Dir, ".deleted", testProject, testService)
	entries := func() int {
		list, err := os.ReadDir(hidden)
		if err != nil {
			return 0
		}
		return len(list)
	}
	now = now.Add(logs.DeletedFor - time.Minute)
	if err := r.Archive.Prune(); err != nil {
		t.Fatal(err)
	}
	if entries() != 1 {
		t.Fatal("what a deletion hid left before its time")
	}
	now = now.Add(2 * time.Minute)
	if err := r.Archive.Prune(); err != nil {
		t.Fatal(err)
	}
	if entries() != 0 {
		t.Error("what a deletion hid stayed past its time")
	}
	r.Archive.Restore(testProject, testService)
	if got := r.Archive.List(testProject, testService); len(got) != 0 {
		t.Errorf("undoing too late brought back %v", got)
	}
}

// A server's first kept log is held to the same check as any other.
func TestTheFirstKeptLogChecksTheDisk(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "a line")
	dir := filepath.Join(t.TempDir(), "not", "there", "yet")
	asked := 0
	a := &logs.Archive{Dir: dir, Client: e.Client(), Free: func(string) (uint64, error) {
		asked++
		return 1 << 20, nil
	}}
	if k := a.Begin(t.Context(), e.Container("app-1"), false); k != nil {
		t.Error("it kept a log with 1 MiB free")
	}
	if asked != 1 {
		t.Errorf("it asked how full the disk is %d times", asked)
	}
	a.Free = nil
	if k := a.Begin(t.Context(), e.Container("app-1"), false); k == nil {
		t.Error("it can't read how full a disk is that was only just made a directory on")
	}
}

func TestADiskItCantReadKeepsNothing(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	a := &logs.Archive{Dir: t.TempDir(), Client: e.Client(), Free: func(string) (uint64, error) { return 0, errors.New("no") }}
	if k := a.Begin(t.Context(), e.Container("app-1"), false); k != nil {
		t.Error("it kept a log on a disk it can't read")
	}
}
