// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
)

// How a destination stands goes by when its backups ended, not when they began,
// and a failure from before it was connected again doesn't count against it.
func TestHowADestinationStands(t *testing.T) {
	at := func(minute int) time.Time { return time.Date(2026, 11, 7, 9, minute, 0, 0, time.UTC) }
	ended := func(minute int) *time.Time { e := at(minute); return &e }
	got := func(id string, start, end int) store.Backup {
		return store.Backup{ID: id, Destination: "r2", StartedAt: at(start), FinishedAt: ended(end), CopyKey: id}
	}
	refused := func(id string, start, end int) store.Backup {
		return store.Backup{ID: id, Destination: "r2", StartedAt: at(start), FinishedAt: ended(end), CopyError: "AccessDenied"}
	}
	for name, c := range map[string]struct {
		all             []store.Backup // newest started first, as the store lists them
		since           time.Time
		upload, failure *time.Time
	}{
		// A long one began first and got there last, after a short one that began
		// later failed: the destination took the last that ended.
		"a long upload ends after a later one fails": {
			all:    []store.Backup{refused("b", 2, 5), got("a", 1, 10)},
			upload: ended(10), failure: ended(5),
		},
		// Two that got there, the one begun first ending last, and a failure between.
		"the newest started isn't the newest ended": {
			all:    []store.Backup{got("c", 3, 4), refused("b", 2, 6), got("a", 1, 9)},
			upload: ended(9), failure: ended(6),
		},
		"a failure ends last": {
			all:    []store.Backup{got("c", 3, 4), refused("b", 1, 8)},
			upload: ended(4), failure: ended(8),
		},
		// Removed, a backup found it gone, and it was connected again at 7.
		"a failure from before it was connected again": {
			all:    []store.Backup{refused("b", 5, 6), got("a", 1, 2)},
			since:  at(7),
			upload: ended(2),
		},
		"another destination's backups": {
			all: []store.Backup{{ID: "x", Destination: "b2", StartedAt: at(1), FinishedAt: ended(2), CopyError: "AccessDenied"}},
		},
	} {
		s := standing(c.all, "r2", c.since)
		if !sameTime(s.upload, c.upload) || !sameTime(s.failure, c.failure) {
			t.Errorf("%s: upload %v, failure %v; want %v, %v", name, s.upload, s.failure, c.upload, c.failure)
		}
		if c.failure != nil && s.why != "AccessDenied" {
			t.Errorf("%s: why %q", name, s.why)
		}
	}
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
