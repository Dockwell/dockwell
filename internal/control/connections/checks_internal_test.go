// SPDX-License-Identifier: AGPL-3.0-only

package connections

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// A connection is checked once it's saved or changed, and then no more often than
// every 6 hours; a provider that turns checks away for its rate is asked half as
// often each time, down to once a day, and as often again once it answers (#799).
func TestAConnectionIsCheckedOnItsSchedule(t *testing.T) {
	t.Parallel()
	c := newChecker(&Service{})
	at := time.Unix(1_800_000_000, 0)
	p := &plan{}
	if !c.due(p, 3, at) {
		t.Error("a connection never checked isn't due")
	}
	p.revision, p.tried = 3, at
	for _, after := range []time.Duration{0, time.Minute, 5*time.Hour + 59*time.Minute} {
		if c.due(p, 3, at.Add(after)) {
			t.Errorf("checked again %v after, before its 6 hours", after)
		}
	}
	if !c.due(p, 3, at.Add(6*time.Hour)) {
		t.Error("not due 6 hours after")
	}
	if !c.due(p, 4, at.Add(time.Minute)) {
		t.Error("a changed connection isn't due at once")
	}
	p.limited = 1
	if c.due(p, 3, at.Add(6*time.Hour)) || !c.due(p, 3, at.Add(12*time.Hour)) {
		t.Error("turned away once, it isn't asked again at 12 hours")
	}
	p.limited = 2
	if c.due(p, 3, at.Add(23*time.Hour)) || !c.due(p, 3, at.Add(24*time.Hour)) {
		t.Error("turned away twice, it isn't asked again at 24 hours")
	}
}

// A provider's limit reads as one: S3's 429 and SlowDown, a mail server's 421 or a
// 4.7 code. A refused key or a passed check doesn't.
func TestAProvidersLimitIsKnown(t *testing.T) {
	t.Parallel()
	for _, r := range []*dockwellv1.CheckConnectionResponse{
		{Status: 429},
		{Code: "SlowDown", Status: 503},
		{Code: "TooManyRequests"},
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
	// A refused key or sign-in puts the next check off too, so a revoked password
	// isn't tried until its account locks; a server that's away doesn't.
	for _, r := range []*dockwellv1.CheckConnectionResponse{
		{Code: "SignatureDoesNotMatch", Status: 403},
		{Code: "InvalidAccessKeyId", Status: 403},
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
		{Code: "550 5.1.1"},
		{Passed: true},
	} {
		if TurnedAway(r) {
			t.Errorf("%v puts the next check off", r)
		}
	}
}

// What's kept is each server's last check of the connection's revision, newest
// first; a person's check adds to it, and one of an older revision isn't shown as
// the newer's. A provider's limit on a person's check moves the schedule too.
func TestAConnectionsChecksAreKeptByServerAndRevision(t *testing.T) {
	t.Parallel()
	c := newChecker(&Service{})
	at := time.Unix(1_800_000_000, 0)
	check := func(server string, after time.Duration, passed bool) *dockwellv1.CheckConnectionResponse {
		return &dockwellv1.CheckConnectionResponse{Passed: passed, Server: server, Time: timestamppb.New(at.Add(after))}
	}
	c.record("uploads", 2, "srv_1", check("web-1", 0, true))
	c.record("uploads", 2, "srv_2", check("web-2", time.Minute, false))
	c.record("uploads", 1, "srv_3", check("web-3", 2*time.Minute, true))
	c.record("uploads", 0, "srv_4", check("web-4", 0, true)) // as typed: never kept
	got := c.Of("uploads", 2)
	if len(got) != 2 || got[0].GetServer() != "web-2" || got[1].GetServer() != "web-1" {
		t.Fatalf("revision 2's checks are %v, want web-2's then web-1's", got)
	}
	got[0].Server = "changed"
	if c.Of("uploads", 2)[0].GetServer() != "web-2" {
		t.Error("what Of returns is what's kept, not a copy")
	}
	// The same server's newer check takes the older's place.
	c.record("uploads", 2, "srv_1", check("web-1", 3*time.Minute, false))
	if got := c.Of("uploads", 2); len(got) != 2 || got[0].GetServer() != "web-1" || got[0].GetPassed() {
		t.Errorf("after web-1's second check: %v", got)
	}
	c.plans = map[string]*plan{"uploads": {revision: 2, tried: at}}
	c.record("uploads", 2, "srv_1", &dockwellv1.CheckConnectionResponse{Code: "SlowDown", Status: 503, Time: timestamppb.New(at)})
	if c.plans["uploads"].limited != 1 {
		t.Error("a person's check the provider turned away doesn't put the schedule off")
	}
}

// A connection has one check at a time, and Slots run at once in all: a look while
// they run starts none of the same again. One a provider turns away for its rate
// stops there, its other servers too, and its next look waits twice as long (#799).
func TestConnectionChecksTakeTurns(t *testing.T) {
	t.Parallel()
	c := newChecker(&Service{})
	at := time.Unix(1_800_000_000, 0)
	c.Now = func() time.Time { return at }
	names := []string{"a", "b", "c", "d"}
	c.listed = func(context.Context) ([]Saved, error) {
		var out []Saved
		for _, n := range names {
			out = append(out, Saved{Name: n, Revision: 1})
		}
		return out, nil
	}
	c.saved = func(context.Context) ([]Saved, error) {
		var out []Saved
		for _, n := range names {
			out = append(out, Saved{Name: n, Revision: 1})
		}
		return out, nil
	}
	c.servers = func(context.Context, string) []string { return []string{"srv_1", "srv_2"} }
	release := make(chan struct{})
	var mu sync.Mutex
	running, most := 0, 0
	calls := map[string]int{}
	limitedName := "d"
	c.probe = func(ctx context.Context, conn Saved, server string, _ bool) (*dockwellv1.CheckConnectionResponse, error) {
		name := conn.Name
		mu.Lock()
		running++
		most = max(most, running)
		calls[name]++
		mu.Unlock()
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		if name == limitedName {
			return &dockwellv1.CheckConnectionResponse{Status: 429, Server: server, Time: timestamppb.New(at)}, nil
		}
		return &dockwellv1.CheckConnectionResponse{Passed: true, Server: server, Time: timestamppb.New(at)}, nil
	}
	c.Check(t.Context())
	c.Check(t.Context()) // while they run: starts nothing again
	// Two take the slots; the rest wait for one.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		n := running
		mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d checks run at once, want 2", n)
		}
	}
	time.Sleep(20 * time.Millisecond) // a third, past the slots, would start here
	close(release)
	c.Wait()
	mu.Lock()
	if most > 2 {
		t.Errorf("%d checks ran at once, past the 2 slots", most)
	}
	for _, n := range names[:3] {
		if calls[n] != 2 {
			t.Errorf("%s was checked %d times, want once from each of its 2 servers", n, calls[n])
		}
	}
	if calls["d"] != 1 {
		t.Errorf("turned away for its rate, d was still checked %d times", calls["d"])
	}
	if p := c.plans["d"]; p.limited != 1 || p.busy {
		t.Errorf("d's plan is %+v, want once turned away, not running", *p)
	}
	if len(c.Of("a", 1)) != 2 || len(c.Of("d", 1)) != 1 {
		t.Errorf("kept %v and %v", c.Of("a", 1), c.Of("d", 1))
	}
	mu.Unlock()
	// Looked at again at once, nothing is due.
	c.Check(t.Context())
	c.Wait()
	mu.Lock()
	defer mu.Unlock()
	if calls["a"] != 2 {
		t.Error("checked again before its 6 hours")
	}
}

// The first look after a start reads each connection, as nothing says which changed
// while it was stopped; one saved or changed since gets Check's full check, which
// writes; every 6 hours after, a read again, which writes nothing (ADR 0099, #799).
func TestOnlyASavedOrChangedConnectionGetsTheFullCheck(t *testing.T) {
	t.Parallel()
	c := newChecker(&Service{})
	at := time.Unix(1_800_000_000, 0)
	c.Now = func() time.Time { return at }
	saved := []Saved{{Name: "uploads", Revision: 1}}
	c.listed = func(context.Context) ([]Saved, error) { return saved, nil }
	c.saved = c.listed
	c.servers = func(context.Context, string) []string { return []string{"srv_1"} }
	var mu sync.Mutex
	var ran []string
	c.probe = func(_ context.Context, conn Saved, _ string, full bool) (*dockwellv1.CheckConnectionResponse, error) {
		kind := "read"
		if full {
			kind = "full"
		}
		mu.Lock()
		ran = append(ran, conn.Name+" "+kind)
		mu.Unlock()
		return &dockwellv1.CheckConnectionResponse{Passed: true, Time: timestamppb.New(at)}, nil
	}
	look := func() {
		c.Check(t.Context())
		c.Wait()
	}
	look() // after a start
	saved = []Saved{{Name: "uploads", Revision: 2}, {Name: "mail", Revision: 1}}
	at = at.Add(time.Minute)
	look() // uploads changed, mail saved since
	at = at.Add(6 * time.Hour)
	look() // both on their schedule
	want := []string{"uploads read", "mail full", "uploads full", "mail read", "uploads read"}
	slices.Sort(want[1:3])
	slices.Sort(want[3:])
	got := slices.Clone(ran)
	if len(got) == 5 {
		slices.Sort(got[1:3])
		slices.Sort(got[3:])
	}
	if !slices.Equal(got, want) {
		t.Errorf("the looks ran %v, want %v", ran, want)
	}
}
