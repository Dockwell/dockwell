// SPDX-License-Identifier: AGPL-3.0-only

package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

func newHub(t *testing.T, names ...string) (*Hub, *store.DB) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now()
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		for i, n := range names {
			if err := tx.CreateServer(t.Context(), store.Server{
				ID: "srv_" + string(rune('A'+i)), Name: n, CreatedAt: now, CertSerial: n, CertNotAfter: now,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h, err := New(t.Context(), db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return h, db
}

var inventory = &dockwellv1.ServerInventory{Hostname: "hz-1", OsName: "Ubuntu 24.04.1 LTS", CpuCount: 4, MemoryBytes: 8 << 30}

func stats(cpu float64) *dockwellv1.ServerStats {
	return &dockwellv1.ServerStats{Time: timestamppb.Now(), CpuPercent: cpu}
}

func TestConnectionStates(t *testing.T) {
	h, _ := newHub(t, "hz-1")
	if got := h.List()[0].GetConnection(); got != dockwellv1.Connection_CONNECTION_PENDING {
		t.Errorf("before the agent connects: %v", got)
	}
	detach, err := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, &dockwellv1.BuildInfo{Version: "v0.4.0"})
	if err != nil {
		t.Fatal(err)
	}
	s := h.List()[0]
	if s.GetConnection() != dockwellv1.Connection_CONNECTION_CONNECTED || s.GetInventory().GetOsName() != "Ubuntu 24.04.1 LTS" || s.GetLastSeenTime() == nil {
		t.Errorf("connected: %v", s)
	}
	detach()
	detach() // twice is harmless
	if got := h.List()[0].GetConnection(); got != dockwellv1.Connection_CONNECTION_UNREACHABLE {
		t.Errorf("after the stream closes: %v", got)
	}
}

func TestTwoStreamsMustBothClose(t *testing.T) {
	h, _ := newHub(t, "hz-1")
	a, _ := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil)
	b, _ := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil)
	a()
	if got := h.List()[0].GetConnection(); got != dockwellv1.Connection_CONNECTION_CONNECTED {
		t.Errorf("with one stream left: %v", got)
	}
	b()
	if got := h.List()[0].GetConnection(); got != dockwellv1.Connection_CONNECTION_UNREACHABLE {
		t.Errorf("with none left: %v", got)
	}
}

func TestConnectTimeSpansTheConnection(t *testing.T) {
	h, _ := newHub(t, "hz-1")
	at := time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)
	h.now = func() time.Time { return at }
	if got := h.List()[0].GetConnectTime(); got != nil {
		t.Errorf("before the agent connects: %v", got)
	}

	first, _ := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil)
	if got := h.List()[0].GetConnectTime().AsTime(); !got.Equal(at) {
		t.Errorf("connected at %v, want %v", got, at)
	}

	// The agent moves to a renewed certificate: a second stream opens, then the first closes.
	at = at.Add(time.Hour)
	second, _ := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil)
	first()
	if got := h.List()[0].GetConnectTime().AsTime(); !got.Equal(at.Add(-time.Hour)) {
		t.Errorf("after moving streams, connected at %v, want the first stream's time", got)
	}

	second()
	if got := h.List()[0].GetConnectTime(); got != nil {
		t.Errorf("after the agent disconnects: %v", got)
	}

	at = at.Add(time.Hour)
	again, _ := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil)
	defer again()
	if got := h.List()[0].GetConnectTime().AsTime(); !got.Equal(at) {
		t.Errorf("reconnected at %v, want %v", got, at)
	}
}

// An agent moving to a renewed certificate closes its stream, then opens the next
// within a second: its server is connected throughout, since the first stream
// opened, and gone once the hold is out if no stream came.
func TestAHoldKeepsAMovingServerConnected(t *testing.T) {
	h, _ := newHub(t, "hz-1")
	at := time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)
	h.now = func() time.Time { return at }
	connection := func() dockwellv1.Connection { return h.List()[0].GetConnection() }

	first, _ := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil)
	h.Hold("srv_A", time.Hour)
	first()
	if got := connection(); got != dockwellv1.Connection_CONNECTION_CONNECTED {
		t.Errorf("between the two streams of a move: %v", got)
	}
	at = at.Add(time.Second)
	second, _ := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil)
	if got := h.List()[0].GetConnectTime().AsTime(); !got.Equal(at.Add(-time.Second)) {
		t.Errorf("after the move, connected at %v, want the first stream's time", got)
	}
	// The hold ended with the next stream, so that stream's end is the server's.
	second()
	if got := connection(); got != dockwellv1.Connection_CONNECTION_UNREACHABLE {
		t.Errorf("after the next stream closes: %v", got)
	}

	third, _ := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil)
	h.Hold("srv_A", 20*time.Millisecond)
	third()
	deadline := time.Now().Add(5 * time.Second)
	for connection() != dockwellv1.Connection_CONNECTION_UNREACHABLE {
		if time.Now().After(deadline) {
			t.Fatal("a server held for 20 ms still reads as connected after 5 s")
		}
		time.Sleep(time.Millisecond)
	}

	// A hold on a server that leaves the fleet comes to nothing.
	fourth, _ := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil)
	h.Hold("srv_A", time.Millisecond)
	fourth()
	h.Removed("srv_A")
	time.Sleep(10 * time.Millisecond)
	if n := len(h.List()); n != 0 {
		t.Errorf("after the server left, the fleet has %d", n)
	}
}

// Whether a server's agent can read Docker is said with each report of health, and a
// new stream says it anew.
func TestAServerSaysWhenItsAgentCantReadDocker(t *testing.T) {
	h, _ := newHub(t, "hz-1")
	since := timestamppb.New(time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC))
	first, _ := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil)
	if got := h.List()[0].GetDockerUnreadableSince(); got != nil {
		t.Errorf("before it says, Docker is unreadable since %v", got)
	}
	h.RecordDocker("srv_A", since)
	if got := h.List()[0].GetDockerUnreadableSince(); !got.AsTime().Equal(since.AsTime()) {
		t.Errorf("Docker is unreadable since %v, want %v", got, since)
	}
	first()
	if got := h.List()[0]; got.GetDockerUnreadableSince() != nil {
		t.Errorf("a server that isn't connected says Docker is unreadable since %v, which nobody can say now", got.GetDockerUnreadableSince())
	}
	second, _ := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil)
	defer second()
	if got := h.List()[0].GetDockerUnreadableSince(); got != nil {
		t.Errorf("a new stream starts with what the last said, %v", got)
	}
	h.RecordDocker("srv_A", since)
	h.RecordDocker("srv_A", nil)
	if got := h.List()[0].GetDockerUnreadableSince(); got != nil {
		t.Errorf("once Docker answers, it is unreadable since %v", got)
	}
}

func TestHistoryStartsAgainAfterAGap(t *testing.T) {
	h, _ := newHub(t, "hz-1")
	detach, err := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The agent's clock, an hour behind the control plane's: the window is
	// measured on the samples' own times.
	start := time.Now().Add(-time.Hour)
	at := func(d time.Duration, cpu float64) *dockwellv1.ServerStats {
		return &dockwellv1.ServerStats{Time: timestamppb.New(start.Add(d)), CpuPercent: cpu}
	}
	for i := range 10 {
		h.Record("srv_A", at(time.Duration(i)*StatsInterval, float64(i)))
	}
	detach()
	if s, _ := h.Get("srv_A"); len(s.GetHistory()) != 10 {
		t.Errorf("a server that stopped reporting keeps its last %d samples, want 10", len(s.GetHistory()))
	}

	// Back after two hours: the first sample starts the history again.
	if _, err := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil); err != nil {
		t.Fatal(err)
	}
	h.Record("srv_A", at(2*time.Hour, 50))
	if s, _ := h.Get("srv_A"); len(s.GetHistory()) != 1 || s.GetHistory()[0].GetCpuPercent() != 50 {
		t.Errorf("after a 2-hour gap the history is %v, want only the new sample", s.GetHistory())
	}

	// Back after one minute: the samples still inside the window stay.
	for i := range 3 {
		h.Record("srv_A", at(2*time.Hour+time.Minute+time.Duration(i)*StatsInterval, float64(60+i)))
	}
	if s, _ := h.Get("srv_A"); len(s.GetHistory()) != 4 {
		t.Errorf("after a 1-minute gap the history has %d samples, want 4", len(s.GetHistory()))
	}

	// Exactly a window later, the oldest drops out.
	h.Record("srv_A", at(2*time.Hour+HistoryWindow, 70))
	s, _ := h.Get("srv_A")
	if got := s.GetHistory(); len(got) != 4 || got[0].GetCpuPercent() != 60 {
		t.Errorf("a window on, the history is %v, want it to start at the sample a minute in", got)
	}
}

func TestHistoryIsCapped(t *testing.T) {
	h, _ := newHub(t, "hz-1")
	if _, err := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, nil); err != nil {
		t.Fatal(err)
	}
	for i := range HistoryLength + 15 {
		h.Record("srv_A", stats(float64(i)))
	}
	s, err := h.Get("srv_A")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.GetHistory()) != HistoryLength || s.GetHistory()[0].GetCpuPercent() != 15 || s.GetStats().GetCpuPercent() != HistoryLength+14 {
		t.Errorf("history has %d samples from %v to %v", len(s.GetHistory()), s.GetHistory()[0].GetCpuPercent(), s.GetStats().GetCpuPercent())
	}
	if len(h.List()[0].GetHistory()) != 0 {
		t.Error("lists carry no history")
	}
	if _, err := h.Get("srv_nope"); err == nil {
		t.Error("Get of an unknown server succeeded")
	}
}

func TestWatch(t *testing.T) {
	h, _ := newHub(t, "hz-1", "hz-2")
	ctx, cancel := context.WithCancel(t.Context())
	snapshot, updates := h.Watch(ctx)
	if len(snapshot) != 2 || snapshot[0].GetName() != "hz-1" {
		t.Fatalf("snapshot = %v", snapshot)
	}
	next := func(i int) Update {
		t.Helper()
		select {
		case u := <-updates:
			return u
		case <-time.After(time.Second):
			t.Fatalf("update %d never came", i)
			return Update{}
		}
	}

	if _, err := h.Attach(t.Context(), store.Server{ID: "srv_B"}, inventory, nil); err != nil {
		t.Fatal(err)
	}
	if u := next(0); u.Server.GetId() != "srv_B" || u.Server.GetConnection() != dockwellv1.Connection_CONNECTION_CONNECTED {
		t.Errorf("as it connects: %+v", u)
	}
	h.Record("srv_B", stats(42))
	if u := next(1); u.Server.GetStats().GetCpuPercent() != 42 || len(u.Server.GetHistory()) != 0 {
		t.Errorf("a sample: %+v, want it without the history", u)
	}
	h.Removed("srv_A")
	if u := next(2); !u.Removed || u.Server.GetId() != "srv_A" {
		t.Errorf("as it goes: %+v", u)
	}

	cancel()
	select {
	case _, open := <-updates:
		if open {
			t.Error("the channel should close once the watcher is done")
		}
	case <-time.After(time.Second):
		t.Fatal("the channel didn't close")
	}
}

// A watcher that doesn't read holds up no agent, and isn't dropped: once it reads,
// it's sent each server as it stands, in the order they first changed, and the
// last word of one that went.
func TestASlowWatcherGetsTheLatestOfEachServer(t *testing.T) {
	h, _ := newHub(t, "hz-1", "hz-2")
	_, updates := h.Watch(t.Context())
	if _, err := h.Attach(t.Context(), store.Server{ID: "srv_B"}, inventory, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for i := range 1000 {
			h.Record("srv_B", stats(float64(i)))
		}
		h.Removed("srv_A")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a watcher that never reads held up the agents")
	}
	// srv_B's connecting may have gone before the samples came, as the watcher
	// takes what changed as soon as anything does.
	var got []Update
	for len(got) == 0 || !got[len(got)-1].Removed {
		select {
		case u, open := <-updates:
			if !open {
				t.Fatalf("the watch closed after %v", got)
			}
			got = append(got, u)
		case <-time.After(time.Second):
			t.Fatalf("after %v, srv_A's going never came", got)
		}
	}
	n := len(got)
	if n < 2 || n > 3 || got[n-1].Server.GetId() != "srv_A" ||
		got[n-2].Server.GetId() != "srv_B" || got[n-2].Server.GetStats().GetCpuPercent() != 999 {
		t.Errorf("got %v, want srv_B as it stands, once or twice, then srv_A gone", got)
	}
}

// A watch that opens as a big fleet answers the faster pace at once, each agent
// with a sample, is sent each server, and stays open.
func TestAWatchKeepsUpWithAFleetAnsweringAtOnce(t *testing.T) {
	h, _ := newHub(t)
	const servers = 1000
	for i := range servers {
		id := fmt.Sprintf("srv_%04d", i)
		h.Joined(store.Server{ID: id, Name: id})
	}
	_, updates := h.Watch(t.Context())
	for i := range servers {
		h.Record(fmt.Sprintf("srv_%04d", i), stats(1))
	}
	seen := map[string]bool{}
	for len(seen) < servers {
		select {
		case u, open := <-updates:
			if !open {
				t.Fatalf("the watch closed after %d servers", len(seen))
			}
			seen[u.Server.GetId()] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d servers came", len(seen), servers)
		}
	}
}

func TestInventorySurvivesARestart(t *testing.T) {
	h, db := newHub(t, "hz-1")
	detach, err := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inventory, &dockwellv1.BuildInfo{Version: "v0.4.0"})
	if err != nil {
		t.Fatal(err)
	}
	detach()
	again, err := New(t.Context(), db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s := again.List()[0]
	if s.GetInventory().GetOsName() != "Ubuntu 24.04.1 LTS" || s.GetAgentBuild().GetVersion() != "v0.4.0" || s.GetConnection() != dockwellv1.Connection_CONNECTION_UNREACHABLE {
		t.Errorf("after a restart: %v", s)
	}
}

func TestAttachAddsServersThatJoinedLater(t *testing.T) {
	h, db := newHub(t)
	now := time.Now()
	late := store.Server{ID: "srv_late", Name: "hz-9", CreatedAt: now, CertSerial: "x", CertNotAfter: now}
	if err := db.Update(t.Context(), func(tx *store.Tx) error { return tx.CreateServer(t.Context(), late) }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Attach(t.Context(), late, inventory, nil); err != nil {
		t.Fatal(err)
	}
	h.SetControlServer("srv_late")
	if s := h.List(); len(s) != 1 || s[0].GetName() != "hz-9" || !s[0].GetControlServer() {
		t.Errorf("List = %v", s)
	}
}

// Agents sample often only while someone watches, and slow down a while after the
// last watch has gone, unless another opens first (ADR 0040).
func TestPaceFollowsWatchers(t *testing.T) {
	h, _ := newHub(t, "hz-1")
	h.idleAfter = 50 * time.Millisecond
	heard := make(chan time.Duration, 16)
	h.OnPace(func(d time.Duration) { heard <- d })
	next := func(want time.Duration) {
		t.Helper()
		select {
		case got := <-heard:
			if got != want {
				t.Fatalf("pace = %v, want %v", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no pace heard; want %v", want)
		}
	}
	quiet := func(within time.Duration) {
		t.Helper()
		select {
		case got := <-heard:
			t.Fatalf("pace changed to %v with nothing to change it", got)
		case <-time.After(within):
		}
	}
	if got := h.Pace(); got != IdleInterval {
		t.Fatalf("with nobody watching, pace = %v, want %v", got, IdleInterval)
	}

	first, stop1 := context.WithCancel(t.Context())
	h.Watch(first)
	next(StatsInterval)
	second, stop2 := context.WithCancel(t.Context())
	h.Watch(second)
	quiet(20 * time.Millisecond) // a second watch changes nothing

	stop1()
	quiet(150 * time.Millisecond) // one watch is still open
	stop2()
	next(IdleInterval)
	if got := h.Pace(); got != IdleInterval {
		t.Fatalf("after the last watch, pace = %v", got)
	}

	// A page that reloads opens its next watch before agents slow down.
	third, stop3 := context.WithCancel(t.Context())
	h.Watch(third)
	next(StatsInterval)
	stop3()
	again, stop4 := context.WithCancel(t.Context())
	defer stop4()
	time.Sleep(10 * time.Millisecond)
	h.Watch(again)
	quiet(150 * time.Millisecond)
	if got := h.Pace(); got != StatsInterval {
		t.Fatalf("with a watch open again, pace = %v", got)
	}
}
