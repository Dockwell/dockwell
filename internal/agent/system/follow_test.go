// SPDX-License-Identifier: AGPL-3.0-only

package system_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/logs"
	"github.com/dockwell/dockwell/internal/agent/system"
	"github.com/dockwell/dockwell/internal/agent/system/systemtest"
)

// got collects what a follow is given, safely, and waits for it.
type got struct {
	mu      sync.Mutex
	entries []logs.JournalEntry
}

func (g *got) add(e logs.JournalEntry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.entries = append(g.entries, e)
}

func (g *got) messages() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, e := range g.entries {
		switch {
		case e.Gap:
			out = append(out, "(gap)")
		case e.Line != nil:
			out = append(out, e.Line.GetMessage())
		}
	}
	return out
}

func (g *got) wait(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Equal(g.messages(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the follow was given %q, want %q", g.messages(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func follows(s *systemtest.Stand) int {
	n := 0
	for _, c := range s.Calls() {
		if strings.HasPrefix(c, "journalctl") && strings.Contains(c, "--follow") {
			n++
		}
	}
	return n
}

// Every follow of a server shares one journalctl, which reads only while one
// follows; each is given what's narrowed to its unit, and systemd's own lines
// about that unit too.
func TestFollowsShareOneJournalctl(t *testing.T) {
	s := systemtest.New(t, systemtest.Unit{Props: systemtest.Running("nginx.service")})
	r := reader(s)
	ctx, cancel := context.WithCancel(t.Context())
	all, nginx := &got{}, &got{}
	var wg sync.WaitGroup
	for _, f := range []struct {
		unit string
		g    *got
	}{{"", all}, {"nginx.service", nginx}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.FollowJournal(ctx, f.unit, "", f.g.add)
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for follows(s) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	s.Append(t,
		systemtest.Entry("f1", "nginx.service", "40", "nginx: started"),
		`{"__CURSOR":"f2","__REALTIME_TIMESTAMP":"1762963200000000","_SYSTEMD_UNIT":"init.scope","_PID":"1","UNIT":"nginx.service","PRIORITY":"6","MESSAGE":"Started nginx.service."}`,
		systemtest.Entry("f3", "cron.service", "41", "cron: ran"),
	)
	all.wait(t, "nginx: started", "Started nginx.service.", "cron: ran")
	nginx.wait(t, "nginx: started", "Started nginx.service.")
	if n := follows(s); n != 1 {
		t.Errorf("two follows ran journalctl -f %d times, want once", n)
	}
	cancel()
	wg.Wait()
	// With nobody following, nothing runs: the next follow starts one anew.
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	later := &got{}
	go func() { _ = r.FollowJournal(ctx2, "", "", later.add) }()
	deadline = time.Now().Add(10 * time.Second)
	for follows(s) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	s.Append(t, systemtest.Entry("f4", "cron.service", "41", "cron: ran again"))
	later.wait(t, "cron: ran again")
}

// A follow that carries on after a cursor is given what came since first, at most
// CatchUpLines, then a gap, then what comes next, each line once.
func TestAFollowCarriesOnAfterItsCursor(t *testing.T) {
	was := system.CatchUpLines
	system.CatchUpLines = 2
	t.Cleanup(func() { system.CatchUpLines = was })
	s := systemtest.New(t, systemtest.Unit{Props: systemtest.Running("nginx.service")})
	s.After(t,
		systemtest.Entry("a1", "nginx.service", "40", "one"),
		systemtest.Entry("a2", "nginx.service", "40", "two"),
		systemtest.Entry("a3", "nginx.service", "40", "three"),
	)
	r := reader(s)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	g := &got{}
	go func() { _ = r.FollowJournal(ctx, "nginx.service", "a0", g.add) }()
	g.wait(t, "one", "two", "(gap)")
	if !slices.ContainsFunc(s.Calls(), func(c string) bool {
		return strings.Contains(c, "--after-cursor=a0") && strings.Contains(c, "--unit=nginx.service")
	}) {
		t.Errorf("journalctl ran %q", s.Calls())
	}
	if _, err := r.Journal(t.Context(), system.Ask{Before: "a\nb"}); err == nil {
		t.Error("a cursor with a newline was read")
	}
}

// A search reads newest first until it has its page or reaches its bound, and
// says how far back it looked; a time range goes to journalctl.
func TestASearchSaysHowFarItLooked(t *testing.T) {
	was := system.SearchLines
	system.SearchLines = 3
	t.Cleanup(func() { system.SearchLines = was })
	s := systemtest.New(t, systemtest.Unit{
		Props: systemtest.Running("nginx.service"),
		Journal: []string{
			`{"__CURSOR":"c5","__REALTIME_TIMESTAMP":"1762963205000000","_SYSTEMD_UNIT":"nginx.service","MESSAGE":"GET /"}`,
			`{"__CURSOR":"c4","__REALTIME_TIMESTAMP":"1762963204000000","_SYSTEMD_UNIT":"nginx.service","MESSAGE":"Timeout reading upstream"}`,
			`{"__CURSOR":"c3","__REALTIME_TIMESTAMP":"1762963203000000","_SYSTEMD_UNIT":"nginx.service","MESSAGE":"GET /a"}`,
			`{"__CURSOR":"c2","__REALTIME_TIMESTAMP":"1762963202000000","_SYSTEMD_UNIT":"nginx.service","MESSAGE":"timeout again"}`,
		},
	})
	r := reader(s)
	since := time.Unix(1762963000, 0)
	page, err := r.Journal(t.Context(), system.Ask{Unit: "nginx.service", Query: "TIMEOUT", Limit: 10, Since: since})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 1 || page.Lines[0].GetMessage() != "Timeout reading upstream" || !page.CutShort {
		t.Fatalf("the search found %v, cut short %t", page.Lines, page.CutShort)
	}
	if got := page.SearchedTo; !got.Equal(time.UnixMicro(1762963203000000)) {
		t.Errorf("it says it searched back to %s", got)
	}
	last := s.Calls()[len(s.Calls())-1]
	if !strings.Contains(last, "--since=@1762963000") || strings.Contains(last, "--lines=") {
		t.Errorf("the search ran %q", last)
	}
	// From where it stopped, the rest.
	more, err := r.Journal(t.Context(), system.Ask{Unit: "nginx.service", Query: "timeout", Limit: 10, Until: page.SearchedTo})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(more.Lines, func(l *dockwellv1.SystemLogLine) bool { return l.GetMessage() == "timeout again" }) {
		t.Errorf("the next search found %v", more.Lines)
	}
}
