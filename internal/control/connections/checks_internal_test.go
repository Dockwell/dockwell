// SPDX-License-Identifier: AGPL-3.0-only

package connections

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/probe"
)

// A connection is checked when it hasn't been at its revision, and then no more
// often than every 6 hours; a provider that turns checks away for its rate is asked
// half as often each time, down to once a day, and as often again once it answers;
// one no server could check waits as its retry says (#799).
func TestAConnectionIsCheckedOnItsSchedule(t *testing.T) {
	t.Parallel()
	c := newChecker(&Service{})
	at := time.Unix(1_800_000_000, 0)
	p := &plan{revision: 3}
	if !c.due(p, at) {
		t.Error("a connection never checked isn't due")
	}
	p.checked, p.tried = true, at
	for _, after := range []time.Duration{0, time.Minute, 5*time.Hour + 59*time.Minute} {
		if c.due(p, at.Add(after)) {
			t.Errorf("checked again %v after, before its 6 hours", after)
		}
	}
	if !c.due(p, at.Add(6*time.Hour)) {
		t.Error("not due 6 hours after")
	}
	p.limited = 1
	if c.due(p, at.Add(6*time.Hour)) || !c.due(p, at.Add(12*time.Hour)) {
		t.Error("turned away once, it isn't asked again at 12 hours")
	}
	p.limited = 2
	if c.due(p, at.Add(23*time.Hour)) || !c.due(p, at.Add(24*time.Hour)) {
		t.Error("turned away twice, it isn't asked again at 24 hours")
	}
	p.retry = at.Add(2 * time.Minute)
	if c.due(p, at.Add(time.Minute)) || !c.due(p, at.Add(2*time.Minute)) {
		t.Error("one no server could check isn't tried when its retry says")
	}
}

// A provider's limit reads as one: S3's 429 and SlowDown, a mail server's 421 or a
// 4.7 code. A refused key or a passed check doesn't. A scheduled read that can't
// confirm the keys isn't a refusal, as reads lock nothing.
func TestAProvidersLimitIsKnown(t *testing.T) {
	t.Parallel()
	for _, r := range []*dockwellv1.CheckConnectionResponse{
		{Status: 429},
		{Code: "SlowDown", Status: 503},
		{Code: "TooManyRequests"},
		{Code: "RateLimitExceeded", Status: 429},
		{Code: "421 4.7.0"},
		{Code: "450 4.7.1"},
	} {
		if !Limited(r) {
			t.Errorf("%v isn't a limit", r)
		}
	}
	for _, r := range []*dockwellv1.CheckConnectionResponse{
		{Code: "SignatureDoesNotMatch", Status: 403},
		{Code: "535 5.7.8"},
		{Code: "550 5.1.1"},
		{Code: "refused"},
		{Passed: true, Status: 429},
	} {
		if Limited(r) {
			t.Errorf("%v reads as a limit", r)
		}
	}
	for _, r := range []*dockwellv1.CheckConnectionResponse{
		{Code: "SignatureDoesNotMatch", Status: 403},
		{Code: "InvalidAccessKeyId", Status: 403},
		{Code: "Unauthorized", Status: 401},
		{Code: "AccessDenied", Status: 403, Step: "write"},
		{Code: "535 5.7.8"},
		{Code: "530 5.7.0"},
	} {
		if !TurnedAway(r) {
			t.Errorf("%v doesn't put the next check off", r)
		}
	}
	for _, r := range []*dockwellv1.CheckConnectionResponse{
		{Code: "timeout"},
		{Code: "NoSuchBucket", Status: 404},
		{Code: "AccessDenied", Status: 403, Step: "list"},
		{Code: CodeAgentUpdate, Step: "list"},
		{Code: "550 5.1.1"},
		{Passed: true},
	} {
		if TurnedAway(r) {
			t.Errorf("%v puts the next check off", r)
		}
	}
}

// What's kept is each server's last check of the connection's revision, by its ID,
// newest first; a person's check adds to it, and one of an older revision isn't
// shown as the newer's. A provider's limit on a person's check moves the schedule.
func TestAConnectionsChecksAreKeptByServerAndRevision(t *testing.T) {
	t.Parallel()
	c := newChecker(&Service{})
	at := time.Unix(1_800_000_000, 0)
	check := func(server string, after time.Duration, passed bool) *dockwellv1.CheckConnectionResponse {
		return &dockwellv1.CheckConnectionResponse{Passed: passed, Server: server, Time: timestamppb.New(at.Add(after))}
	}
	v2 := Saved{ID: "cnx_1", Name: "uploads", Revision: 2}
	c.record(v2, "srv_1", check("web-1", 0, true))
	c.record(v2, "srv_2", check("web-2", time.Minute, false))
	c.record(Saved{ID: "cnx_1", Name: "uploads", Revision: 1}, "srv_3", check("web-3", 2*time.Minute, true))
	c.record(Saved{Name: "uploads"}, "srv_4", check("web-4", 0, true)) // as typed: never kept
	got := c.Of("cnx_1", 2)
	if len(got) != 2 || got[0].GetServer() != "web-2" || got[1].GetServer() != "web-1" {
		t.Fatalf("revision 2's checks are %v, want web-2's then web-1's", got)
	}
	got[0].Server = "changed"
	if c.Of("cnx_1", 2)[0].GetServer() != "web-2" {
		t.Error("what Of returns is what's kept, not a copy")
	}
	c.record(v2, "srv_1", check("web-1", 3*time.Minute, false))
	if got := c.Of("cnx_1", 2); len(got) != 2 || got[0].GetServer() != "web-1" || got[0].GetPassed() {
		t.Errorf("after web-1's second check: %v", got)
	}
	c.plans = map[string]*plan{"cnx_1": {revision: 2, checked: true, tried: at}}
	c.record(v2, "srv_1", &dockwellv1.CheckConnectionResponse{Code: "SlowDown", Status: 503, Time: timestamppb.New(at)})
	if c.plans["cnx_1"].limited != 1 {
		t.Error("a person's check the provider turned away doesn't put the schedule off")
	}
}

// fake is a checker over connections and servers a test says, whose probe answers
// as answer does and remembers what it ran, as "name kind server".
type fake struct {
	*Checker
	mu      sync.Mutex
	at      time.Time
	listed  []Saved
	unread  []string
	targets map[string][]Target
	answer  func(conn Saved, at Target, full bool) (*dockwellv1.CheckConnectionResponse, error)
	ran     []string
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{Checker: newChecker(&Service{}), at: time.Unix(1_800_000_000, 0), targets: map[string][]Target{}}
	f.Now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.at
	}
	f.Checker.listed = func(context.Context) ([]Saved, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Clone(f.listed), nil
	}
	f.saved = func(context.Context) ([]Saved, []string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var opened []Saved
		for _, s := range f.listed {
			if !slices.Contains(f.unread, s.Name) {
				opened = append(opened, s)
			}
		}
		return opened, slices.Clone(f.unread), nil
	}
	f.Checker.targets = func(_ context.Context, name string) []Target {
		f.mu.Lock()
		defer f.mu.Unlock()
		if at, ok := f.targets[name]; ok {
			return slices.Clone(at)
		}
		return []Target{{Server: "srv_1"}}
	}
	f.answer = func(_ Saved, at Target, _ bool) (*dockwellv1.CheckConnectionResponse, error) {
		return &dockwellv1.CheckConnectionResponse{Passed: true, Server: at.Server}, nil
	}
	f.probe = func(_ context.Context, conn Saved, at Target, full bool) (*dockwellv1.CheckConnectionResponse, error) {
		kind := "read"
		if full {
			kind = "full"
		}
		f.mu.Lock()
		f.ran = append(f.ran, conn.Name+" "+kind+" "+at.Server)
		answer := f.answer
		f.mu.Unlock()
		got, err := answer(conn, at, full)
		if got != nil {
			got.Time = timestamppb.New(f.Now())
		}
		return got, err
	}
	return f
}

// look runs one look, waits for its checks, and says what they ran.
func (f *fake) look() []string {
	f.mu.Lock()
	f.ran = nil
	f.mu.Unlock()
	f.Check(context.Background())
	f.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.ran)
	slices.Sort(out)
	return out
}

func (f *fake) set(do func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	do()
}

// The first look after a start reads each connection, as nothing says which changed
// while it was stopped; one saved or changed since gets Check's full check, which
// writes; every 6 hours after, a read again, which writes nothing (ADR 0099, #799).
func TestOnlyASavedOrChangedConnectionGetsTheFullCheck(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.set(func() { f.listed = []Saved{{ID: "cnx_u", Name: "uploads", Revision: 1}} })
	if got := f.look(); !slices.Equal(got, []string{"uploads read srv_1"}) {
		t.Errorf("after a start: %v", got)
	}
	f.set(func() {
		f.listed = []Saved{{ID: "cnx_u", Name: "uploads", Revision: 2}, {ID: "cnx_m", Name: "mail", Revision: 1}}
		f.at = f.at.Add(time.Minute)
	})
	if got := f.look(); !slices.Equal(got, []string{"mail full srv_1", "uploads full srv_1"}) {
		t.Errorf("changed and saved since: %v", got)
	}
	f.set(func() { f.at = f.at.Add(6 * time.Hour) })
	if got := f.look(); !slices.Equal(got, []string{"mail read srv_1", "uploads read srv_1"}) {
		t.Errorf("on their schedule: %v", got)
	}
}

// A full check no server could run, as an agent busy with a person's Check, isn't
// done: it stays owed and is tried at the next look, then less often while it can't
// run; and an edit before the first check ran gets the full check too (#799's
// review, M1, the reviewer's probe as a test).
func TestAFullCheckNoServerRanStaysOwed(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.set(func() { f.listed = []Saved{{ID: "cnx_o", Name: "old", Revision: 1}} })
	f.look() // after a start
	busy := errors.New(probe.ErrBusy.Error())
	f.set(func() {
		f.listed = append(f.listed, Saved{ID: "cnx_n", Name: "new", Revision: 1})
		f.at = f.at.Add(time.Minute)
		f.answer = func(conn Saved, at Target, _ bool) (*dockwellv1.CheckConnectionResponse, error) {
			if conn.Name == "new" {
				return nil, busy
			}
			return &dockwellv1.CheckConnectionResponse{Passed: true, Server: at.Server}, nil
		}
	})
	if got := f.look(); !slices.Equal(got, []string{"new full srv_1"}) {
		t.Fatalf("saved since: %v", got)
	}
	// Tried again at the next look, the full check still; then 2 minutes on.
	f.set(func() { f.at = f.at.Add(time.Minute) })
	if got := f.look(); !slices.Equal(got, []string{"new full srv_1"}) {
		t.Errorf("a minute after it couldn't run: %v", got)
	}
	f.set(func() { f.at = f.at.Add(time.Minute) })
	if got := f.look(); len(got) != 0 {
		t.Errorf("a minute after it couldn't run twice: %v, want it to wait 2", got)
	}
	f.set(func() {
		f.at = f.at.Add(time.Minute)
		f.answer = func(_ Saved, at Target, _ bool) (*dockwellv1.CheckConnectionResponse, error) {
			return &dockwellv1.CheckConnectionResponse{Passed: true, Server: at.Server}, nil
		}
	})
	if got := f.look(); !slices.Equal(got, []string{"new full srv_1"}) {
		t.Errorf("once the agent is free: %v", got)
	}
	f.set(func() { f.at = f.at.Add(6 * time.Hour) })
	if got := f.look(); !slices.Equal(got, []string{"new read srv_1", "old read srv_1"}) {
		t.Errorf("6 hours on: %v", got)
	}

	// No server at the first look after a start, then an edit: the full check.
	g := newFake(t)
	g.set(func() {
		g.listed = []Saved{{ID: "cnx_e", Name: "edited", Revision: 1}}
		g.targets["edited"] = nil
	})
	if got := g.look(); len(got) != 0 {
		t.Fatalf("with no server: %v", got)
	}
	g.set(func() {
		g.listed = []Saved{{ID: "cnx_e", Name: "edited", Revision: 2}}
		delete(g.targets, "edited")
		g.at = g.at.Add(time.Minute)
	})
	if got := g.look(); !slices.Equal(got, []string{"edited full srv_1"}) {
		t.Errorf("edited before its first check ran: %v, want the full check", got)
	}
}

// A connection has one check at a time, and each server one check of the schedule
// at a time, so the agent's other place is a person's (#799's review, M4): with 4
// connections on 2 servers, 2 run at once, one on each. A look while they run starts
// none again. One a provider turns away stops there, its other servers too.
func TestConnectionChecksTakeTurns(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	names := []string{"a", "b", "c", "d"}
	f.set(func() {
		for _, n := range names {
			f.listed = append(f.listed, Saved{ID: "cnx_" + n, Name: n, Revision: 1})
			f.targets[n] = []Target{{Server: "srv_1"}, {Server: "srv_2"}}
		}
	})
	release := make(chan struct{})
	var mu sync.Mutex
	on := map[string]int{}
	most := map[string]int{}
	calls := map[string]int{}
	f.set(func() {
		f.answer = func(conn Saved, at Target, _ bool) (*dockwellv1.CheckConnectionResponse, error) {
			mu.Lock()
			on[at.Server]++
			most[at.Server] = max(most[at.Server], on[at.Server])
			calls[conn.Name]++
			mu.Unlock()
			<-release
			mu.Lock()
			on[at.Server]--
			mu.Unlock()
			if conn.Name == "d" {
				return &dockwellv1.CheckConnectionResponse{Status: 429, Server: at.Server}, nil
			}
			return &dockwellv1.CheckConnectionResponse{Passed: true, Server: at.Server}, nil
		}
	})
	f.Check(t.Context())
	f.Check(t.Context()) // while they run: starts nothing again
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		n := on["srv_1"] + on["srv_2"]
		mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no check started")
		}
	}
	time.Sleep(20 * time.Millisecond) // a second on one server would start here
	close(release)
	f.Wait()
	mu.Lock()
	defer mu.Unlock()
	if most["srv_1"] > 1 || most["srv_2"] > 1 {
		t.Errorf("at most %v checks ran at once on a server, want 1", most)
	}
	for _, n := range names[:3] {
		if calls[n] != 2 {
			t.Errorf("%s was checked %d times, want once from each of its 2 servers", n, calls[n])
		}
	}
	if calls["d"] != 1 {
		t.Errorf("turned away for its rate, d was still checked %d times", calls["d"])
	}
	f.mu.Lock()
	p := *f.plans["cnx_d"]
	f.mu.Unlock()
	if p.limited != 1 || p.busy {
		t.Errorf("d's plan is %+v, want once turned away, not running", p)
	}
}

// Each server's last result stays until that server answers again or stops being
// one the connection is checked from: one whose agent can't run the next check
// keeps its failure, rather than leave the other's pass alone (#799's review, L3).
func TestEachServersLastCheckStaysUntilItAnswers(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.set(func() {
		f.listed = []Saved{{ID: "cnx_u", Name: "uploads", Revision: 1}}
		f.targets["uploads"] = []Target{{Server: "srv_a"}, {Server: "srv_b"}}
		f.answer = func(_ Saved, at Target, _ bool) (*dockwellv1.CheckConnectionResponse, error) {
			return &dockwellv1.CheckConnectionResponse{Passed: at.Server == "srv_b", Code: "timeout", Server: at.Server}, nil
		}
	})
	f.look()
	f.set(func() {
		f.at = f.at.Add(6 * time.Hour)
		f.answer = func(_ Saved, at Target, _ bool) (*dockwellv1.CheckConnectionResponse, error) {
			if at.Server == "srv_a" {
				return nil, errors.New("the agent on srv_a isn't connected")
			}
			return &dockwellv1.CheckConnectionResponse{Passed: true, Server: at.Server}, nil
		}
	})
	f.look()
	got := f.Of("cnx_u", 1)
	if len(got) != 2 || got[0].GetServer() != "srv_b" || got[1].GetServer() != "srv_a" || got[1].GetPassed() {
		t.Errorf("after srv_a couldn't check: %v, want srv_b's new pass and srv_a's failure still", got)
	}
	// srv_a no longer runs a linked app: its result goes.
	f.set(func() {
		f.at = f.at.Add(6 * time.Hour)
		f.targets["uploads"] = []Target{{Server: "srv_b"}}
	})
	f.look()
	if got := f.Of("cnx_u", 1); len(got) != 1 || got[0].GetServer() != "srv_b" {
		t.Errorf("once srv_a is no longer one: %v", got)
	}
}

// One connection the control plane can't open leaves the rest checked, and is said
// once, not each minute (#799's review, L1).
func TestAnUnreadableConnectionLeavesTheRestChecked(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	var said []string
	f.set(func() {
		f.listed = []Saved{{ID: "cnx_a", Name: "a", Revision: 1}, {ID: "cnx_b", Name: "b", Revision: 1}}
		f.unread = []string{"b"}
	})
	f.Checker.s.connections = &Connections{Logger: logTo(&said)}
	if got := f.look(); !slices.Equal(got, []string{"a read srv_1"}) {
		t.Errorf("with b unreadable: %v", got)
	}
	f.set(func() { f.at = f.at.Add(time.Minute) })
	f.look()
	f.set(func() { f.at = f.at.Add(time.Hour) })
	f.look()
	n := 0
	for _, line := range said {
		if strings.Contains(line, "can't open the connection") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("said b can't be opened %d times, want once: %v", n, said)
	}
}

// A connection deleted and saved again under its name while its check runs starts
// afresh: the old check's results and plan don't land on the new one (#799's
// review, L2).
func TestAConnectionSavedAgainStartsAfresh(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	release := make(chan struct{})
	f.set(func() {
		f.listed = []Saved{{ID: "cnx_old", Name: "uploads", Revision: 1}}
		f.answer = func(conn Saved, at Target, _ bool) (*dockwellv1.CheckConnectionResponse, error) {
			if conn.ID == "cnx_old" {
				<-release
			}
			return &dockwellv1.CheckConnectionResponse{Passed: conn.ID == "cnx_new", Server: at.Server}, nil
		}
	})
	f.Check(t.Context()) // the old one's check starts, and waits
	f.set(func() {
		f.listed = []Saved{{ID: "cnx_new", Name: "uploads", Revision: 1}}
		f.at = f.at.Add(time.Minute)
	})
	f.Check(t.Context()) // the new one's starts beside it
	close(release)
	f.Wait()
	if got := f.Of("cnx_new", 1); len(got) != 1 || !got[0].GetPassed() {
		t.Errorf("the new connection's checks are %v, want its own pass", got)
	}
	if got := f.Of("cnx_old", 1); len(got) != 0 {
		t.Errorf("the deleted one keeps %v", got)
	}
	f.mu.Lock()
	_, old := f.plans["cnx_old"]
	f.mu.Unlock()
	if old {
		t.Error("the deleted one's plan stays once its check ended")
	}
}

// logTo is a logger that keeps each message it's given in lines.
func logTo(lines *[]string) *slog.Logger {
	return slog.New(&lineHandler{lines: lines, mu: &sync.Mutex{}})
}

type lineHandler struct {
	lines *[]string
	mu    *sync.Mutex
}

func (h *lineHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *lineHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.lines = append(*h.lines, r.Message)
	return nil
}

func (h *lineHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *lineHandler) WithGroup(string) slog.Handler      { return h }
