// SPDX-License-Identifier: AGPL-3.0-only

package services

import (
	"fmt"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
)

// The review's L3: a person's checks are counted whatever token they use, a full
// table refuses someone new rather than drop a live count, and a server answers
// portChecksAtOnce checks at once.
func TestPortChecksAreLimitedByPersonAndServer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	c := &portCallers{now: func() time.Time { return now }}

	// Two tokens of one person count as one.
	tokenA := authapi.As(t.Context(), auth.Client{}, auth.Principal{User: store.User{ID: "usr_lee"}, Token: &store.APIToken{ID: "tok_a"}})
	tokenB := authapi.As(t.Context(), auth.Client{}, auth.Principal{User: store.User{ID: "usr_lee"}, Token: &store.APIToken{ID: "tok_b"}})
	if personOf(tokenA) != personOf(tokenB) || personOf(tokenA) != "user:usr_lee" {
		t.Fatalf("two tokens of one person: %q and %q", personOf(tokenA), personOf(tokenB))
	}
	for i := range portChecksPerMinute {
		if !c.allow(personOf(tokenA)) {
			t.Fatalf("check %d refused", i+1)
		}
	}
	if c.allow(personOf(tokenB)) {
		t.Error("another token of the same person passed the limit")
	}
	now = now.Add(time.Minute)
	if !c.allow(personOf(tokenB)) {
		t.Error("a minute later, still refused")
	}

	// A full table refuses someone new, and keeps every live count.
	full := &portCallers{now: func() time.Time { return now }}
	for i := range mostPortCallers {
		full.allow(fmt.Sprintf("user:usr_%d", i))
	}
	if full.allow("user:usr_new") {
		t.Error("someone new passed a full table")
	}
	if len(full.m) != mostPortCallers {
		t.Errorf("the table holds %d", len(full.m))
	}
	now = now.Add(time.Minute)
	if !full.allow("user:usr_new") {
		t.Error("once the minute was over, someone new was still refused")
	}

	// A server's checks at once.
	var releases []func()
	for range portChecksAtOnce {
		release, ok := c.hold("srv_1")
		if !ok {
			t.Fatal("a server refused a check under its limit")
		}
		releases = append(releases, release)
	}
	if _, ok := c.hold("srv_1"); ok {
		t.Error("a server took more checks at once than it may")
	}
	if release, ok := c.hold("srv_2"); !ok {
		t.Error("another server was refused")
	} else {
		release()
	}
	releases[0]()
	if release, ok := c.hold("srv_1"); !ok {
		t.Error("a check given back didn't free its place")
	} else {
		release()
	}
}
