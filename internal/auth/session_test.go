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
)

// Text a browser sends is stored as valid UTF-8, or the session list can't
// be sent back.
func TestClientTextIsValidUTF8(t *testing.T) {
	f := newFixture(t, "")
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
	f := newFixture(t, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	f.execSQL(t, `CREATE TRIGGER no_touch BEFORE UPDATE OF last_seen_at ON sessions BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`)
	f.clock.advance(2 * touchEvery)
	if p, err := f.svc.Authenticate(t.Context(), localhost, owner.Token); err != nil || p.User.ID != owner.User.ID {
		t.Errorf("Authenticate when the touch fails = %v, %v", p.User.ID, err)
	}
}

// A page that only holds a stream open is in use: its session lasts until
// the absolute limit, and no longer.
func TestAnOpenStreamKeepsItsSession(t *testing.T) {
	f := newFixture(t, "")
	f.svc.recheck = 2 * time.Millisecond
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	ctx, stop := f.svc.Track(t.Context(), localhost, ownerPrincipal(owner))
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

// Signing out the other sessions counts only those that still worked.
func TestRevokeOtherSessionsSkipsEndedOnes(t *testing.T) {
	f := newFixture(t, "")
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
