// SPDX-License-Identifier: AGPL-3.0-only

package gitproviders

import (
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/store"
)

// A connection's state comes back once, in the session that began it, by the person
// who did, within 10 minutes; anything else is refused, and uses it up.
func TestAStateIsForItsSessionOnce(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	h := New(Config{Now: func() time.Time { return now }})
	mira := auth.Principal{User: store.User{ID: "usr_mira"}, Session: store.Session{ID: "ses_1"}}
	begin := func(state string) {
		h.starting[hashOf(state)] = starting{user: "usr_mira", session: "ses_1", hookID: "ghw_x", at: now}
	}

	begin("s1")
	if _, ok := h.take(mira, "s1"); !ok {
		t.Error("the state didn't come back in its own session")
	}
	if _, ok := h.take(mira, "s1"); ok {
		t.Error("the state came back twice")
	}

	begin("s2")
	other := mira
	other.Session.ID = "ses_2"
	if _, ok := h.take(other, "s2"); ok {
		t.Error("another session took the state")
	}
	if _, ok := h.take(mira, "s2"); ok {
		t.Error("a state another session tried was still there")
	}

	begin("s3")
	someone := auth.Principal{User: store.User{ID: "usr_other"}, Session: store.Session{ID: "ses_1"}}
	if _, ok := h.take(someone, "s3"); ok {
		t.Error("another person took the state")
	}

	begin("s4")
	now = now.Add(stateLasts + time.Second)
	if _, ok := h.take(mira, "s4"); ok {
		t.Error("a state past 10 minutes came back")
	}
}
