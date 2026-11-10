// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Text a browser sends is stored as valid UTF-8, or the session list can't
// be sent back.
func TestClientTextIsValidUTF8(t *testing.T) { storetest.Each(t, testClientTextIsValidUTF8) }

func testClientTextIsValidUTF8(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	client := localhost
	client.UserAgent = "x" + strings.Repeat("é", 300) + "\xff\xfe\x00"
	owner := f.claim(t, client, &authtest.Authenticator{})
	ua := owner.Session.UserAgent
	if !utf8.ValidString(ua) || len(ua) > maxUserAgent || !strings.HasPrefix(ua, "xéé") {
		t.Errorf("stored User-Agent is %d bytes, valid UTF-8 %v: %q", len(ua), utf8.ValidString(ua), ua)
	}
	for in, want := range map[string]string{
		"Chrome on macOS": "Chrome on macOS",
		"bad \xff name":   "bad � name",
		"tab\tand\x00nul": "tab and nul",
	} {
		if got := cleanPasskeyName(in); got != want {
			t.Errorf("cleanPasskeyName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := clientText("ab\xffcd", 3); got != "ab" {
		t.Errorf("cut inside a replaced byte: %q", got)
	}
}

// Recording that a session was used is best effort: when it fails, the
// request still goes through.
func TestAFailedTouchStillAuthenticates(t *testing.T) {
	storetest.Each(t, testAFailedTouchStillAuthenticates)
}

func testAFailedTouchStillAuthenticates(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	storetest.FailUpdates(t, f.path, "sessions", "last_seen_at")
	f.clock.advance(2 * touchEvery)
	if p, err := f.svc.Authenticate(t.Context(), localhost, owner.Token); err != nil || p.User.ID != owner.User.ID {
		t.Errorf("Authenticate when the touch fails = %v, %v", p.User.ID, err)
	}
}

// A page that only holds a stream open is in use: its session lasts until
// the absolute limit, and no longer.
func TestAnOpenStreamKeepsItsSession(t *testing.T) {
	storetest.Each(t, testAnOpenStreamKeepsItsSession)
}

func testAnOpenStreamKeepsItsSession(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	f.svc.recheck = 2 * time.Millisecond
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	ctx, stop := f.svc.Track(t.Context(), localhost, ownerPrincipal(owner), nil)
	defer stop()
	seen := func() time.Time {
		var s store.Session
		_ = f.db.View(t.Context(), func(tx *store.Tx) (err error) {
			s, err = tx.GetSession(t.Context(), owner.Session.ID)
			return err
		})
		return s.LastSeenAt
	}
	start := f.clock.now()
	for f.clock.now().Sub(start) < SessionMax-SessionIdle {
		f.clock.advance(SessionIdle / 2)
		deadline := time.Now().Add(5 * time.Second)
		for !seen().Equal(f.clock.now()) {
			if time.Now().After(deadline) {
				t.Fatalf("after %s the stream's recheck didn't count as use", f.clock.now().Sub(start))
			}
			time.Sleep(time.Millisecond)
		}
		if ctx.Err() != nil {
			t.Fatalf("the stream closed after %s", f.clock.now().Sub(start))
		}
	}
	if list, err := f.svc.ListSessions(t.Context(), ownerPrincipal(owner)); err != nil || len(list) != 1 {
		t.Errorf("sessions after %s with a stream open = %d, %v", f.clock.now().Sub(start), len(list), err)
	}
	f.clock.advance(SessionMax - f.clock.now().Sub(start))
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Error("the stream outlived the absolute limit")
	}
	if _, err := f.svc.Authenticate(t.Context(), localhost, owner.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("the session after 7 days = %v, want ErrUnauthenticated", err)
	}
}

// What follows a session without being a page in use, as a terminal nobody is
// attached to, checks it as a stream does, but its checks aren't use: they keep
// no session past its idle limit, and never move its last-seen time or address.
// Revoking still ends it at once.
func TestFollowingASessionIsntUsingIt(t *testing.T) {
	storetest.Each(t, testFollowingASessionIsntUsingIt)
}

func testFollowingASessionIsntUsingIt(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	f.svc.recheck = 2 * time.Millisecond
	a := &authtest.Authenticator{}
	owner := f.claim(t, localhost, a)
	elsewhere := localhost
	elsewhere.Addr = "198.51.100.7:40000"
	read := func() store.Session {
		var s store.Session
		_ = f.db.View(t.Context(), func(tx *store.Tx) (err error) {
			s, err = tx.GetSession(t.Context(), owner.Session.ID)
			return err
		})
		return s
	}
	before := read()
	ctx, stop := f.svc.Follow(t.Context(), elsewhere, ownerPrincipal(owner))
	defer stop()
	f.clock.advance(SessionIdle / 2)
	time.Sleep(50 * time.Millisecond) // many rechecks
	if after := read(); !after.LastSeenAt.Equal(before.LastSeenAt) || after.LastIP != before.LastIP {
		t.Errorf("following the session moved it from %v at %s to %v at %s", before.LastSeenAt, before.LastIP, after.LastSeenAt, after.LastIP)
	}
	if ctx.Err() != nil {
		t.Fatal("it ended within the idle limit")
	}
	f.clock.advance(SessionIdle/2 + time.Minute)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Error("following a session kept it past its idle limit")
	}

	again, err := f.passkeySignIn(t, localhost, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.svc.recheck = time.Hour // so only signing out can end it in time
	ctx, stop = f.svc.Follow(t.Context(), localhost, ownerPrincipal(again))
	defer stop()
	if err := f.svc.SignOut(t.Context(), ownerPrincipal(again)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Error("signing out didn't end what follows the session")
	}
}

// Signing out the other sessions counts only those that still worked.
func TestRevokeOtherSessionsSkipsEndedOnes(t *testing.T) {
	storetest.Each(t, testRevokeOtherSessionsSkipsEndedOnes)
}

func testRevokeOtherSessionsSkipsEndedOnes(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	a := &authtest.Authenticator{}
	current := f.claim(t, localhost, a)
	if _, err := f.passkeySignIn(t, localhost, a, nil); err != nil { // left idle
		t.Fatal(err)
	}
	f.clock.advance(SessionIdle / 2)
	if _, err := f.svc.Authenticate(t.Context(), localhost, current.Token); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(SessionIdle/2 + time.Minute)
	if _, err := f.passkeySignIn(t, localhost, a, nil); err != nil { // still open
		t.Fatal(err)
	}
	n, err := f.svc.RevokeOtherSessions(t.Context(), ownerPrincipal(current))
	if err != nil || n != 1 {
		t.Errorf("RevokeOtherSessions = %d, %v, want 1: the idle one had already ended", n, err)
	}
}

// A stream whose session is revoked after Authenticate read it, but before
// the stream registered with Track, still ends at once, not at the next
// recheck.
func TestARevocationBeforeTrackStillEndsTheStream(t *testing.T) {
	storetest.Each(t, testARevocationBeforeTrackStillEndsTheStream)
}

func testARevocationBeforeTrackStillEndsTheStream(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	f.svc.recheck = time.Hour
	a := &authtest.Authenticator{}
	owner := f.claim(t, localhost, a)
	other, err := f.passkeySignIn(t, localhost, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, end := range map[string]func(p Principal) error{
		"revoked":    func(p Principal) error { return f.svc.RevokeSession(t.Context(), ownerPrincipal(owner), p.Session.ID) },
		"signed out": func(p Principal) error { return f.svc.SignOut(t.Context(), p) },
	} {
		signedIn := other
		if name == "signed out" {
			signedIn = owner
		}
		p, err := f.svc.Authenticate(t.Context(), localhost, signedIn.Token)
		if err != nil {
			t.Fatal(err)
		}
		if err := end(p); err != nil { // commits and ends the streams registered so far: none
			t.Fatal(err)
		}
		ctx, stop := f.svc.Track(t.Context(), localhost, p, nil)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Errorf("a stream whose session was %s just before it registered stayed open", name)
		}
		stop()
	}
}
