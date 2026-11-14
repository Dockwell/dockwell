// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func aKey(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

// A session cookie reads back for its route at its generation until it runs out.
// Another route's key, another route, a raised generation, a changed byte, or a
// flow cookie in its place don't read, so no cookie stands in for another.
func TestASessionCookieReadsOnlyWhereItWasMade(t *testing.T) {
	key := aKey(1)
	s := Session{Route: "rte_admin", Generation: 3, Subject: "248289761001", Email: "mira@example.com", Groups: []string{"ops"}, Made: now, Ends: now.Add(24 * time.Hour)}
	value, err := SealSession(key, s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenSession(key, "rte_admin", 3, value, now.Add(time.Hour))
	if err != nil || got.Email != "mira@example.com" || got.Subject != s.Subject || len(got.Groups) != 1 {
		t.Fatalf("it reads %+v, %v", got, err)
	}
	flip := []byte(value)
	flip[3] ^= 1
	f, err := SealFlow(key, 3, Flow{Route: "rte_admin", State: "s", Nonce: "n", Verifier: "v", Back: "/", Ends: now.Add(flowFor)})
	if err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func() error{
		"another key":         func() error { _, err := OpenSession(aKey(2), "rte_admin", 3, value, now); return err },
		"another route":       func() error { _, err := OpenSession(key, "rte_other", 3, value, now); return err },
		"a raised generation": func() error { _, err := OpenSession(key, "rte_admin", 4, value, now); return err },
		"a changed byte":      func() error { _, err := OpenSession(key, "rte_admin", 3, string(flip), now); return err },
		"a flow cookie":       func() error { _, err := OpenSession(key, "rte_admin", 3, f, now); return err },
		"run out":             func() error { _, err := OpenSession(key, "rte_admin", 3, value, now.Add(25*time.Hour)); return err },
		"not a cookie":        func() error { _, err := OpenSession(key, "rte_admin", 3, "nonsense", now); return err },
	} {
		if read() == nil {
			t.Errorf("%s reads as a session", name)
		}
	}
}

// A flow cookie lasts 10 minutes, for its route alone.
func TestAFlowCookieLastsTenMinutes(t *testing.T) {
	key := aKey(1)
	value, err := SealFlow(key, 0, Flow{Route: "rte_admin", State: "st", Nonce: "n", Verifier: "v", Back: "/admin/x", Ends: now.Add(flowFor)})
	if err != nil {
		t.Fatal(err)
	}
	if f, err := OpenFlow(key, "rte_admin", 0, value, now.Add(9*time.Minute)); err != nil || f.Back != "/admin/x" {
		t.Errorf("it reads %+v, %v", f, err)
	}
	if _, err := OpenFlow(key, "rte_admin", 0, value, now.Add(11*time.Minute)); err == nil || !strings.Contains(err.Error(), "10 minutes") {
		t.Errorf("after 11 minutes it reads %v", err)
	}
	if _, err := OpenFlow(key, "rte_other", 0, value, now); err == nil {
		t.Error("another route reads the flow")
	}
}

// A route's cookies are __Host- cookies named by a hash, which says nothing of the
// route; a session too large for a cookie is refused, saying what to do.
func TestARoutesCookiesAreNamedByAHash(t *testing.T) {
	session, flow := Names("dockwell.env_01.admin")
	if !strings.HasPrefix(session, "__Host-dw_") || !strings.HasPrefix(flow, "__Host-dw_flow_") || strings.Contains(session, "admin") {
		t.Errorf("they're named %q and %q", session, flow)
	}
	if again, _ := Names("dockwell.env_01.admin"); again != session {
		t.Error("a route's cookie is named differently each time")
	}
	big := Session{Route: "r", Groups: []string{strings.Repeat("g", 5000)}, Ends: now.Add(time.Hour)}
	if _, err := SealSession(aKey(1), big); err == nil || !strings.Contains(err.Error(), "Name fewer groups") {
		t.Errorf("a session too large reads %v", err)
	}
	if _, err := SealSession([]byte("short"), Session{}); err == nil {
		t.Error("a short key seals a session")
	}
}
