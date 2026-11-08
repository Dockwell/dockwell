// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// The audit log reads newest first, a page at a time, narrowed by when, by who,
// by where from, and by words in it or in the names of who acted and on what.
func TestTheAuditLogFiltersAndPages(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		base := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
		at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }
		events := []store.Event{
			{ID: "evt_01", At: at(1), Actor: "dockwell", Action: "install.create", Surface: store.SurfaceAutomatic},
			{ID: "evt_02", At: at(2), Actor: "user:usr_1", Action: "auth.sign_in", Subject: "usr_1", Surface: store.SurfacePanel, Detail: "passkey on localhost from 198.51.100.23, session ses_1"},
			{ID: "evt_03", At: at(3), Actor: "user:usr_1", Action: "token.create", Subject: "tok_1", Target: "tok_1", Surface: store.SurfacePanel, Detail: "read_write, expires never"},
			{ID: "evt_04", At: at(4), Actor: "token:tok_1", Action: "token.use", Target: "tok_1", Surface: store.SurfaceCLI, Detail: "first use from 192.0.2.41"},
			{ID: "evt_05", At: at(5), Actor: "anonymous:203.0.113.9", Action: "auth.sign_in_failed", Subject: "usr_1", Surface: store.SurfacePanel, Detail: "a wrong code, 100%_off"},
			{ID: "evt_06", At: at(6), Actor: "server:srv_web", Action: "server.join", Subject: "srv_web", Target: "srv_web", Surface: store.SurfaceCLI},
			{ID: "evt_07", At: at(6), Actor: "cli:mira@web-1", Action: "sign_in_link.create", Subject: "usr_1", Surface: store.SurfaceCLI},
		}
		now := time.Now()
		if err := db.Update(t.Context(), func(tx *store.Tx) error {
			if err := tx.CreateUser(t.Context(), store.User{ID: "usr_1", Email: "mira@example.com", Name: "Mira Chen", Role: store.RoleOwner, WebAuthnHandle: []byte("h"), CreatedAt: now}); err != nil {
				return err
			}
			if err := tx.CreateAPIToken(t.Context(), store.APIToken{ID: "tok_1", TokenHash: []byte("x"), UserID: "usr_1", Name: "GitHub Actions deploy", Hint: "dwt_…", Scope: store.ScopeReadOnly, CreatedAt: now}); err != nil {
				return err
			}
			if err := tx.CreateServer(t.Context(), store.Server{ID: "srv_web", Name: "web-2", CreatedAt: now, CertSerial: "s", CertNotAfter: now}); err != nil {
				return err
			}
			for _, e := range events {
				if err := tx.RecordEvent(t.Context(), e); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		ids := func(f store.AuditFilter, before store.TimelineCursor, limit int) []string {
			t.Helper()
			var got []store.Event
			if err := db.View(t.Context(), func(tx *store.Tx) error {
				var err error
				got, err = tx.AuditEvents(t.Context(), f, before, limit)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			out := make([]string, len(got))
			for i, e := range got {
				out[i] = e.ID
			}
			return out
		}
		for _, tt := range []struct {
			name string
			f    store.AuditFilter
			want []string
		}{
			{"everything, newest first, the same minute by ID", store.AuditFilter{}, []string{"evt_07", "evt_06", "evt_05", "evt_04", "evt_03", "evt_02", "evt_01"}},
			{"since", store.AuditFilter{Since: at(5)}, []string{"evt_07", "evt_06", "evt_05"}},
			{"people", store.AuditFilter{ActorPrefixes: []string{"user:"}}, []string{"evt_03", "evt_02"}},
			{"Dockwell, by either of its names", store.AuditFilter{ActorPrefixes: []string{"dockwell", "system"}}, []string{"evt_01"}},
			{"strangers", store.AuditFilter{ActorPrefixes: []string{"anonymous:"}}, []string{"evt_05"}},
			{"from the CLI", store.AuditFilter{Surface: store.SurfaceCLI}, []string{"evt_07", "evt_06", "evt_04"}},
			{"an action", store.AuditFilter{Query: "TOKEN.CREATE"}, []string{"evt_03"}},
			{"an address in the words", store.AuditFilter{Query: "198.51.100.23"}, []string{"evt_02"}},
			{"an address a stranger came from", store.AuditFilter{Query: "203.0.113.9"}, []string{"evt_05"}},
			{"a token by its name, as actor and as target", store.AuditFilter{Query: "actions deploy"}, []string{"evt_04", "evt_03"}},
			{"a server by its name", store.AuditFilter{Query: "web-2"}, []string{"evt_06"}},
			{"a person by name", store.AuditFilter{Query: "mira chen"}, []string{"evt_03", "evt_02"}},
			{"a shell account", store.AuditFilter{Query: "mira@web-1"}, []string{"evt_07"}},
			{"% and _ match themselves", store.AuditFilter{Query: "100%_"}, []string{"evt_05"}},
			{"% alone matches only a %", store.AuditFilter{Query: "%"}, []string{"evt_05"}},
			{"filters combine", store.AuditFilter{ActorPrefixes: []string{"user:"}, Query: "token"}, []string{"evt_03"}},
		} {
			if got := ids(tt.f, store.TimelineCursor{}, 50); !slices.Equal(got, tt.want) {
				t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
			}
		}
		// Pages carry on from the last event of the one before, even within one minute.
		first := ids(store.AuditFilter{}, store.TimelineCursor{}, 1)
		second := ids(store.AuditFilter{}, store.TimelineCursor{At: at(6), ID: first[0]}, 2)
		if !slices.Equal(first, []string{"evt_07"}) || !slices.Equal(second, []string{"evt_06", "evt_05"}) {
			t.Errorf("pages %v then %v, want evt_07, then evt_06 and evt_05", first, second)
		}
	})
}
