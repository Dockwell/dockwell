// SPDX-License-Identifier: AGPL-3.0-only

package logs_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

// A deploy makes its container a while before it starts it, as the old one stops
// first, and that can take seconds. A look in that time has read nothing of it.
// So one that then starts, says why it can't go on and is gone by the next look,
// as a failed deploy's is, still reads: this is what a follower is there to see.
func TestAFollowReadsAContainerItSawBeforeItStarted(t *testing.T) {
	for _, removed := range []bool{false, true} {
		name := "stopped"
		if removed {
			name = "kept"
		}
		t.Run(name, func(t *testing.T) {
			e := dockertest.New(t)
			ctr := made(t, e, "app-1")
			r := newReader(t, e)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := newCollector()
			follow(ctx, r, &agentv1.LogsCall{Tail: -1}, c)
			waitFor(t, "a first look, which finds it not started", c.isCaughtUp)
			if err := r.Client.StartContainer(t.Context(), ctr.ID); err != nil {
				t.Fatal(err)
			}
			e.Print("app-1", docker.Stdout, "can't reach the database")
			if removed {
				keep(t, r, e, "app-1", true)
			} else {
				e.Stop("app-1")
			}
			waitFor(t, "why it failed", func() bool { return slices.Contains(c.got(), "can't reach the database") })
		})
	}
}

// A look finds a container, and a rollback removes it before its log is asked
// for, so nothing was read of it. What the server kept of it is read instead.
func TestAFollowReadsAContainerThatGoesBeforeItsLogIsAskedFor(t *testing.T) {
	e := dockertest.New(t)
	r := newReader(t, e)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, r, &agentv1.LogsCall{Tail: -1}, c)
	waitFor(t, "a first look, which finds nothing", c.isCaughtUp)
	var once atomic.Bool
	e.Intercept(func(req *http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/logs") && once.CompareAndSwap(false, true) {
			keep(t, r, e, "app-1", true)
		}
		return nil
	})
	addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "can't reach the database")
	waitFor(t, "why it failed", func() bool { return slices.Contains(c.got(), "can't reach the database") })
	if !once.Load() {
		t.Error("its log was never asked for, so this tested nothing")
	}
}

// Docker answers 409 for a container being removed, whatever its logging driver.
// A follower that looks then reads what was kept once it has gone, and hears
// nothing of drivers, whether it came before the container went or as it did.
func TestAFollowReadsWhatWasKeptOfAContainerBeingRemoved(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "there as it went"
		if late {
			name = "there only once it had stopped"
		}
		t.Run(name, func(t *testing.T) {
			e := dockertest.New(t)
			r := newReader(t, e)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := newCollector()
			refused := func() int {
				n := 0
				for _, req := range e.Seen() {
					if strings.HasSuffix(req, "/logs") {
						n++
					}
				}
				return n
			}
			begin := func() { follow(ctx, r, &agentv1.LogsCall{Tail: 100}, c) }
			if !late {
				begin()
				waitFor(t, "a first look, which finds nothing", c.isCaughtUp)
			}
			ctr := addContainer(t, e, "app-1", 1)
			e.Print("app-1", docker.Stdout, "can't reach the database")
			k := r.Archive.Begin(t.Context(), e.Container("app-1"), true)
			e.Stop("app-1")
			k.Finish(t.Context(), e.Container("app-1"))
			r.Archive.WaitPruned()
			e.Removing("app-1")
			kept := refused()
			if late {
				time.Sleep(2 * time.Millisecond) // so it stopped before this began
				begin()
			}
			waitFor(t, "a look that Docker refuses", func() bool { return refused() > kept })
			if err := r.Client.RemoveContainer(t.Context(), ctr.ID, true); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "why it failed", func() bool { return slices.Contains(c.got(), "can't reach the database") })
			c.mu.Lock()
			defer c.mu.Unlock()
			if msg := c.sources[ctr.ID[:12]].GetLogError(); msg != "" {
				t.Errorf("the source says %q, and nothing is wrong with it", msg)
			}
		})
	}
}

// A dead container is refused for as long as it stays, so a reader hears why, and
// what to do. It isn't given up on: once it has gone, what was kept reads.
func TestADeadContainerSaysWhyItsLogDoesntShow(t *testing.T) {
	e := dockertest.New(t)
	ctr := addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "last words")
	r := newReader(t, e)
	r.RefusedLooks = 2
	k := r.Archive.Begin(t.Context(), e.Container("app-1"), true)
	e.Stop("app-1")
	k.Finish(t.Context(), e.Container("app-1"))
	r.Archive.WaitPruned()
	e.Dead("app-1")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, r, &agentv1.LogsCall{Tail: 100}, c)
	var said string
	waitFor(t, "the source to say why", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		said = c.sources[ctr.ID[:12]].GetLogError()
		return said != ""
	})
	if want := "docker rm -f " + ctr.ID[:12]; !strings.Contains(said, want) || strings.Contains(said, "driver") {
		t.Errorf("a dead container says %q, want how to remove it, and nothing of drivers", said)
	}
	if err := r.Client.RemoveContainer(t.Context(), ctr.ID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "what was kept", func() bool { return slices.Contains(c.got(), "last words") })
	waitFor(t, "the source to stop saying so", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.sources[ctr.ID[:12]].GetLogError() == ""
	})
}

// Docker going away partway through a stopped container's log is no end to it, or a
// read would pass for all there is, with no marker and no look at the rest.
func TestAReadThatBreaksOnAStoppedContainerReadsOn(t *testing.T) {
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	calls := map[string]*agentv1.LogsCall{
		"following":    {Resource: testRef, Follow: true, Tail: 100},
		"to the end":   {Resource: testRef, Tail: 100},
		"a search":     {Resource: testRef, Query: "line"},
		"up to a time": {Resource: testRef, Tail: 40, Until: timestamppb.New(time.Now().Add(time.Hour))},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			e := dockertest.New(t)
			addContainer(t, e, "app-1", 1)
			e.Print("app-1", docker.Stdout, lines...)
			e.Stop("app-1")
			e.BreakLog("app-1", 6)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := newCollector()
			done := make(chan error, 1)
			go func() { done <- newReader(t, e).Stream(ctx, call, c.send) }()
			waitFor(t, "all of it", func() bool { return c.count() >= len(lines) })
			if call.GetFollow() {
				time.Sleep(700 * time.Millisecond) // a further look, which has nothing more to read
				cancel()
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := c.got(); !slices.Equal(got, lines) {
				t.Errorf("read %d lines, want each of %d once, in order: %q", len(got), len(lines), got)
			}
		})
	}
}

// A follow with no place yet that broke before the end of its tail hasn't read that
// tail, however quiet it has been since: the next look still reads it.
func TestAFollowThatBrokeBeforeItsTailStillReadsIt(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "first", "second")
	e.Stop("app-1")
	e.BreakLog("app-1", 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, newReader(t, e), &agentv1.LogsCall{Tail: 10}, c)
	waitFor(t, "its tail", func() bool { return c.count() == 2 })
	if got := c.got(); !slices.Equal(got, []string{"first", "second"}) {
		t.Errorf("read %q, want its tail once", got)
	}
}

// A log that goes on breaking stops being tried, and a reader hears so, however it
// reads, rather than taking what came for all there is.
func TestALogThatKeepsBreakingSaysSo(t *testing.T) {
	for _, follows := range []bool{true, false} {
		name := "to the end"
		if follows {
			name = "following"
		}
		t.Run(name, func(t *testing.T) {
			e := dockertest.New(t)
			ctr := addContainer(t, e, "app-1", 1)
			e.Print("app-1", docker.Stdout, "first", "second")
			e.Stop("app-1")
			for range 12 {
				e.BreakLog("app-1", 0)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := newCollector()
			done := make(chan error, 1)
			go func() {
				done <- newReader(t, e).Stream(ctx, &agentv1.LogsCall{Resource: testRef, Tail: 100, Follow: follows}, c.send)
			}()
			waitFor(t, "the source to say why", func() bool {
				c.mu.Lock()
				defer c.mu.Unlock()
				return strings.Contains(c.sources[ctr.ID[:12]].GetLogError(), "docker logs "+ctr.ID[:12])
			})
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			reads := 0
			for _, req := range e.Seen() {
				if strings.HasSuffix(req, "/logs") {
					reads++
				}
			}
			if reads > 3 {
				t.Errorf("it read %d times, and it shouldn't go on trying", reads)
			}
		})
	}
}

// One that has no fix but to wait says so to a read to the end too, which has no
// later look to say it from.
func TestAReadToTheEndSaysWhatItCouldntRead(t *testing.T) {
	e := dockertest.New(t)
	ctr := e.AddContainer(docker.Container{
		Name: "/app-1",
		Config: docker.Config{Image: "app:1", Labels: map[string]string{
			docker.LabelProject: testProject, docker.LabelService: testService,
		}},
		HostConfig: docker.HostConfig{LogConfig: &docker.LogConfig{Type: "none"}},
	})
	c := newCollector()
	if err := newReader(t, e).Stream(t.Context(), &agentv1.LogsCall{Resource: testRef, Tail: 10}, c.send); err != nil {
		t.Fatal(err)
	}
	if msg := c.sources[ctr.ID[:12]].GetLogError(); !strings.Contains(msg, "logging driver") {
		t.Errorf("a read of a log Docker can't read says %q", msg)
	}
}

// A follow that starts with no lines, or with a tail that found none, has no place
// in a container's log. A restart between two looks still reads whatever that run
// said, from when the follow began, and nothing from before it.
func TestAFollowWithNoPlaceReadsARestart(t *testing.T) {
	for _, tail := range []int32{-1, 2} {
		for _, removed := range []bool{false, true} {
			name := fmt.Sprintf("tail %d", tail)
			if removed {
				name += ", then removed"
			}
			t.Run(name, func(t *testing.T) {
				e := dockertest.New(t)
				ctr := addContainer(t, e, "app-1", 1)
				if tail < 0 {
					e.Print("app-1", docker.Stdout, "from before it began")
				}
				r := newReader(t, e)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				c := newCollector()
				follow(ctx, r, &agentv1.LogsCall{Tail: tail}, c)
				waitFor(t, "caught up", c.isCaughtUp)
				waitFor(t, "it to be following", func() bool { return e.Followers() == 1 })
				e.Stop("app-1")
				waitFor(t, "the follow to end", func() bool { return e.Followers() == 0 })
				if err := r.Client.StartContainer(t.Context(), ctr.ID); err != nil {
					t.Fatal(err)
				}
				// More than the tail, so a tail asked for again wouldn't reach the first.
				e.Print("app-1", docker.Stdout, "one", "two", "three", "four")
				e.Stop("app-1")
				if removed {
					k := r.Archive.Begin(t.Context(), e.Container("app-1"), true)
					k.Finish(t.Context(), e.Container("app-1"))
					r.Archive.WaitPruned()
					if err := r.Client.RemoveContainer(t.Context(), ctr.ID, true); err != nil {
						t.Fatal(err)
					}
				}
				waitFor(t, "what that run said", func() bool { return slices.Contains(c.got(), "four") })
				time.Sleep(700 * time.Millisecond) // a further look, which has nothing more to read
				if got := c.got(); !slices.Equal(got, []string{"one", "two", "three", "four"}) {
					t.Errorf("read %q, want what the second run said, once", got)
				}
			})
		}
	}
}

// One that restarts and is gone between two looks reads what the last run said,
// and what it had read already, once.
func TestAFollowReadsARestartItDidNotSee(t *testing.T) {
	e := dockertest.New(t)
	ctr := addContainer(t, e, "app-1", 1)
	r := newReader(t, e)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, r, &agentv1.LogsCall{Tail: -1}, c)
	waitFor(t, "caught up", c.isCaughtUp)
	e.Print("app-1", docker.Stdout, "first run")
	waitFor(t, "the first run", func() bool { return slices.Contains(c.got(), "first run") })
	e.Stop("app-1")
	time.Sleep(50 * time.Millisecond)
	if err := r.Client.StartContainer(t.Context(), ctr.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // so its lines have times of their own
	e.Print("app-1", docker.Stdout, "second run")
	keep(t, r, e, "app-1", true)
	waitFor(t, "the second run", func() bool { return slices.Contains(c.got(), "second run") })
	time.Sleep(700 * time.Millisecond) // a further look, which has nothing more to read
	if got := c.got(); !slices.Equal(got, []string{"first run", "second run"}) {
		t.Errorf("read %q, want each line once", got)
	}
}

// A call names at most what a server reads for one, whatever the control plane said,
// so a crafted token costs a server nothing however many sources it names.
func TestACallNamingMoreThanAServerTakesIsRefused(t *testing.T) {
	at := timestamppb.New(time.Now())
	cursors := func(n int) []*agentv1.LogCursor {
		out := make([]*agentv1.LogCursor, n)
		for i := range out {
			out[i] = &agentv1.LogCursor{SourceId: fmt.Sprintf("%012x", i), Time: at, Count: 1}
		}
		return out
	}
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%012x", i)
		}
		return out
	}
	over := logs.MaxNamedSources + 1
	for name, call := range map[string]*agentv1.LogsCall{
		"after":      {After: cursors(over)},
		"before":     {Before: cursors(over)},
		"source_ids": {SourceIds: ids(over)},
		"levels":     {Levels: make([]dockwellv1.LogLevel, logs.MaxLevels+1)},
		"query":      {Query: strings.Repeat("x", 1<<10+1)},
		"bad after":  {After: []*agentv1.LogCursor{{SourceId: "../../etc", Time: at}}},
		"bad source": {SourceIds: []string{strings.Repeat("a", 65)}},
	} {
		t.Run(name, func(t *testing.T) {
			e := dockertest.New(t)
			addContainer(t, e, "app-1", 1)
			r := newReader(t, e)
			call.Resource, call.Follow = testRef, true
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := r.Stream(ctx, call, func(*agentv1.SendLogsRequest) error { return nil })
			if err == nil || !strings.Contains(err.Error(), "read nothing") {
				t.Errorf("it said %v", err)
			}
			if e.Followers() != 0 {
				t.Error("it read a log first")
			}
		})
	}
}

// As much as a call can name, it costs next to nothing, and ends at once with its
// reader: a marker for each made-up source, and no more.
func TestTheMostACallCanNameCostsLittle(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	r := newReader(t, e)
	at := timestamppb.New(time.Now().Add(-time.Hour))
	after := make([]*agentv1.LogCursor, logs.MaxNamedSources)
	for i := range after {
		after[i] = &agentv1.LogCursor{SourceId: fmt.Sprintf("%012x", i), Time: at, Count: 1}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	done := follow(ctx, r, &agentv1.LogsCall{Tail: -1, After: after, ResumeFrom: at}, c)
	waitFor(t, "a marker for each", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.gaps) == logs.MaxNamedSources
	})
	left := time.Now()
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("it ended with %v", err)
	}
	if took := time.Since(left); took > time.Second {
		t.Errorf("it took %s to stop after its reader left", took)
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

// A follow carries a burst from its first moment, as when it opens on a container that
// is just starting and prints why it failed.
func TestAFollowCarriesABurstFromItsFirstMoment(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, newReader(t, e), &agentv1.LogsCall{Tail: 100}, c)
	waitFor(t, "the follower", func() bool { return e.Followers() == 1 })
	long := strings.Repeat("x", 40_000)
	const burst = 6
	for i := range burst {
		e.Print("app-1", docker.Stdout, fmt.Sprintf("%d %s", i, long))
	}
	e.Print("app-1", docker.Stdout, "the line after")
	waitFor(t, "the line after", func() bool { return slices.Contains(c.got(), "the line after") })
	got := c.got()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(got) != burst+1 || len(c.gaps) != 0 {
		t.Fatalf("read %d of %d lines, and the markers said %v", len(got), burst+1, c.gaps)
	}
	for i, text := range got[:burst] {
		if len(text) != docker.MaxLogLine || !strings.HasPrefix(text, fmt.Sprintf("%d x", i)) {
			t.Errorf("line %d came as %d bytes, starting %.8q", i, len(text), text)
		}
	}
}

// A line costs what it carries, so one longer than any burst still comes, cut.
func TestALiveLineLongerThanABurstStillComes(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	follow(ctx, newReader(t, e), &agentv1.LogsCall{Tail: -1}, c)
	waitFor(t, "the follower", func() bool { return e.Followers() == 1 })
	time.Sleep(300 * time.Millisecond) // past any first burst, so this is the line's size alone
	const size = logs.LiveBytesPerSecond
	e.Print("app-1", docker.Stdout, strings.Repeat("x", size), "the line after")
	waitFor(t, "the line after", func() bool { return slices.Contains(c.got(), "the line after") })
	c.mu.Lock()
	defer c.mu.Unlock()
	line := c.byLine[strings.Repeat("x", docker.MaxLogLine)]
	if line == nil || int(line.GetCutBytes())+docker.MaxLogLine != size || len(c.gaps) != 0 {
		t.Errorf("a line of %d bytes came as %v with %d cut, and the markers said %v", size, line != nil, line.GetCutBytes(), c.gaps)
	}
}

// What is cut from a long line was read and goes nowhere, so a container writing long
// lines is read at the pace of what is dropped, however fast the wire may carry them.
func TestWhatALongLineCutsIsReadAtThePace(t *testing.T) {
	const (
		pace    = 8 << 20
		size    = 1 << 20
		seconds = 2
	)
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 1)
	r := newReader(t, e)
	r.LiveRate, r.TotalRate, r.ShedRate = 1<<40, 1<<40, pace
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	flood := e.Flood("app-1", size)
	defer flood.Stop()
	follow(ctx, r, &agentv1.LogsCall{Tail: -1}, newCollector())
	waitFor(t, "the follower", func() bool { return e.Followers() == 1 })
	time.Sleep(300 * time.Millisecond)
	before := flood.Lines()
	time.Sleep(seconds * time.Second)
	rate := float64(flood.Lines()-before) * size / seconds
	t.Logf("read %.1f MB/s", rate/(1<<20))
	if rate > 1.3*pace {
		t.Errorf("read %.1f MB/s, past the pace of %d MiB/s", rate/(1<<20), pace>>20)
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

// A power cut can leave a kept log with an empty record, as nothing syncs it. With no
// record it has no age and no size, so the 30 days and the budget would never reach
// it. It leaves, and what stands beside it doesn't.
func TestAKeptLogWithNoRecordLeaves(t *testing.T) {
	for name, damage := range map[string]func(record string) error{
		"missing": os.Remove,
		"empty":   func(record string) error { return os.WriteFile(record, nil, 0o600) },
		"cut off": func(record string) error { return os.WriteFile(record, []byte(`{"container_id":"ab`), 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			e := dockertest.New(t)
			r := newReader(t, e)
			addContainer(t, e, "app-1", 1)
			e.Print("app-1", docker.Stdout, "failed")
			fine := keep(t, r, e, "app-1", true)
			addContainer(t, e, "app-2", 2)
			e.Print("app-2", docker.Stdout, "replaced")
			broken := keep(t, r, e, "app-2", false)
			dir := filepath.Join(r.Archive.Dir, testProject, testService)
			if err := damage(filepath.Join(dir, broken, "kept.json")); err != nil {
				t.Fatal(err)
			}
			if err := r.Archive.Prune(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, broken)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a kept log with no record is still there: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, fine)); err != nil {
				t.Errorf("the kept log beside it went too: %v", err)
			}
		})
	}
}

// A record that can't be reached for now is no reason to lose what it describes.
func TestAKeptLogStaysThroughARecordItCantReachForNow(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads every file, so nothing here is out of reach")
	}
	e := dockertest.New(t)
	r := newReader(t, e)
	addContainer(t, e, "app-1", 1)
	e.Print("app-1", docker.Stdout, "failed")
	id := keep(t, r, e, "app-1", true)
	record := filepath.Join(r.Archive.Dir, testProject, testService, id, "kept.json")
	if err := os.Chmod(record, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.Archive.Prune(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(record, 0o600); err != nil {
		t.Fatalf("it went, record and all: %v", err)
	}
	if got := r.Archive.List(testProject, testService); len(got) != 1 {
		t.Errorf("the archive lists %d kept logs, want the one it had", len(got))
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

// What a deletion put out of reach isn't read, or listed, however it is asked for. A
// service made again keeps its name, its directory and its ID, and what it writes
// reads, as its revisions are later. A call that says nothing reads it all, as when the
// deletion was undone.
func TestAReadLeavesOutWhatADeletionPutOutOfReach(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 3)
	e.Print("app-1", docker.Stdout, "password=hunter2")
	r := newReader(t, e)
	keep(t, r, e, "app-1", false)
	addContainer(t, e, "app-2", 5)
	e.Print("app-2", docker.Stdout, "the new one")
	keep(t, r, e, "app-2", true)

	read := func(upTo int64) []string {
		t.Helper()
		got := once(t, r, &agentv1.LogsCall{IncludeKept: true, Tail: 100, ForgottenUpTo: upTo}).got()
		slices.Sort(got)
		return got
	}
	if got := read(4); !slices.Equal(got, []string{"the new one"}) {
		t.Errorf("past the deletion it read %q", got)
	}
	if got := read(5); len(got) != 0 {
		t.Errorf("with both out of reach it read %q", got)
	}
	if got := read(0); !slices.Equal(got, []string{"password=hunter2", "the new one"}) {
		t.Errorf("with nothing out of reach it read %q", got)
	}
	listed, err := r.Sources(t.Context(), &agentv1.LogSourcesCall{Resource: testRef, ForgottenUpTo: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].GetRevision() != 5 {
		t.Errorf("past the deletion it listed %v", listed)
	}
}

// A kept log with no revision, as a container Compose made has none, is taken to be
// from before a deletion, as nothing says it isn't.
func TestAKeptLogWithNoRevisionIsOutOfReachAfterADeletion(t *testing.T) {
	e := dockertest.New(t)
	ctr := addContainer(t, e, "app-1", 1)
	delete(ctr.Config.Labels, docker.OwnPrefix+"revision")
	e.Print("app-1", docker.Stdout, "from Compose")
	r := newReader(t, e)
	keep(t, r, e, "app-1", false)
	if got := once(t, r, &agentv1.LogsCall{IncludeKept: true, Tail: 100, ForgottenUpTo: 1}).got(); len(got) != 0 {
		t.Errorf("it read %q", got)
	}
	if got := once(t, r, &agentv1.LogsCall{IncludeKept: true, Tail: 100}).got(); len(got) != 1 {
		t.Errorf("with no deletion it read %q", got)
	}
}

// What a deletion put out of reach stays while it can be undone, and goes from the
// disk once the control plane says it can't, leaving what a later revision wrote.
func TestWhatADeletionPutOutOfReachGoesOnceItCantBeUndone(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "app-1", 3)
	e.Print("app-1", docker.Stdout, "from before")
	r := newReader(t, e)
	keep(t, r, e, "app-1", false)
	addContainer(t, e, "app-2", 5)
	e.Print("app-2", docker.Stdout, "the new one")
	keep(t, r, e, "app-2", true)
	revisions := func() []int64 {
		var out []int64
		for _, k := range r.Archive.List(testProject, testService) {
			out = append(out, k.Revision)
		}
		slices.Sort(out)
		return out
	}

	r.Forgotten([]*agentv1.ForgottenService{{Resource: testRef, UpToRevision: 4}})
	if got := revisions(); !slices.Equal(got, []int64{3, 5}) {
		t.Fatalf("while it can be undone, the server keeps revisions %v", got)
	}
	r.Forgotten([]*agentv1.ForgottenService{{Resource: testRef, UpToRevision: 4, Expired: true}})
	if got := revisions(); !slices.Equal(got, []int64{5}) {
		t.Errorf("once it can't be undone, the server keeps revisions %v", got)
	}
	// Told again, as it is each hour, it changes nothing.
	r.Forgotten([]*agentv1.ForgottenService{{Resource: testRef, UpToRevision: 4, Expired: true}})
	if got := revisions(); !slices.Equal(got, []int64{5}) {
		t.Errorf("told again, the server keeps revisions %v", got)
	}
}

// Running out of room, what a deletion put out of reach goes before what can be read.
func TestWhatADeletionPutOutOfReachGoesFirstWhenFull(t *testing.T) {
	e := dockertest.New(t)
	addContainer(t, e, "gone-1", 3)
	e.Print("gone-1", docker.Stdout, strings.Repeat("from before ", 2000))
	r := newReader(t, e)
	keep(t, r, e, "gone-1", true)
	addContainer(t, e, "gone-2", 5)
	e.Print("gone-2", docker.Stdout, strings.Repeat("the new one ", 2000))
	keep(t, r, e, "gone-2", true)
	r.Archive.WaitPruned()
	if got := r.Archive.List(testProject, testService); len(got) != 1 {
		t.Fatalf("one slot keeps %d", len(got))
	}
	addContainer(t, e, "gone-3", 6)
	e.Print("gone-3", docker.Stdout, strings.Repeat("replaced ", 2000))
	keep(t, r, e, "gone-3", false)
	r.Archive.WaitPruned()
	r.Forgotten([]*agentv1.ForgottenService{{Resource: testRef, UpToRevision: 5}})
	r.Archive.Budget = 1
	if err := r.Archive.Prune(); err != nil {
		t.Fatal(err)
	}
	left := r.Archive.List(testProject, testService)
	if len(left) != 1 || left[0].Revision != 6 {
		t.Errorf("over budget it kept %v, want the newest", left)
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
