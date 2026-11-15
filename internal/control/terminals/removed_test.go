// SPDX-License-Identifier: AGPL-3.0-only

package terminals

import (
	"log/slog"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A server's agent may say its terminals ended as it stopped before the control
// plane sees the server go. Whichever comes first, a terminal on a server that was
// removed goes on record as ended by the removal, as whoever reads the audit log
// needs to see that. A shell that ended otherwise, or on a server that stays, goes
// on record as it ended (#714).
func TestATerminalOnARemovedServerEndsAsRemovedWhateverItsAgentSays(t *testing.T) {
	t.Parallel()
	db, _ := storetest.Open(t, store.SQLite)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	if err := db.Update(ctx, func(tx *store.Tx) error {
		for _, id := range []string{"srv_kept", "srv_gone"} {
			if err := tx.CreateServer(ctx, store.Server{ID: id, Name: id, CreatedAt: now, CertSerial: id, CertNotAfter: now.Add(time.Hour)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hub, err := fleet.New(ctx, db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	// Removed as an admin removes it: its certificate is revoked, and the fleet
	// hasn't seen it go yet.
	if err := db.Update(ctx, func(tx *store.Tx) error { return tx.RevokeServer(ctx, "srv_gone", now) }); err != nil {
		t.Fatal(err)
	}
	h := &Handler{fleet: hub, db: db, logger: slog.New(slog.DiscardHandler), now: time.Now, terms: map[string]*entry{}}

	for _, tc := range []struct {
		server, terminal string
		said             dockwellv1.TerminalEndReason
		want             dockwellv1.TerminalEndReason
	}{
		{"srv_gone", "trm_restarted", dockwellv1.TerminalEndReason_TERMINAL_END_REASON_AGENT_RESTARTED, dockwellv1.TerminalEndReason_TERMINAL_END_REASON_SERVER_REMOVED},
		{"srv_gone", "trm_exited", dockwellv1.TerminalEndReason_TERMINAL_END_REASON_EXITED, dockwellv1.TerminalEndReason_TERMINAL_END_REASON_EXITED},
		{"srv_kept", "trm_kept", dockwellv1.TerminalEndReason_TERMINAL_END_REASON_AGENT_RESTARTED, dockwellv1.TerminalEndReason_TERMINAL_END_REASON_AGENT_RESTARTED},
	} {
		opening := "evt_" + tc.terminal
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.RecordEvent(ctx, store.Event{ID: opening, At: now, Actor: "user:usr_test", Action: eventOpening, Subject: tc.terminal, Target: tc.server})
		}); err != nil {
			t.Fatal(err)
		}
		h.ended(tc.server, &agentv1.TerminalEnded{
			Id: tc.terminal, Record: opening, Opened: timestamppb.New(now),
			End: &dockwellv1.TerminalEnd{Reason: tc.said, Time: timestamppb.New(now), Message: "The agent stopped, and its shell with it."},
		})
		got := closeOf(t, db, tc.terminal)
		if dockwellv1.TerminalEndReason(got.Reason) != tc.want {
			t.Errorf("%s on %s, which its agent said ended %v, is on record as %v: %q", tc.terminal, tc.server, tc.said, dockwellv1.TerminalEndReason(got.Reason), got.Message)
		}
		if tc.want == dockwellv1.TerminalEndReason_TERMINAL_END_REASON_SERVER_REMOVED && got.Message != "Its server was removed from Dockwell." {
			t.Errorf("%s says %q", tc.terminal, got.Message)
		}
	}
}

// closeOf is what the close of a terminal says on record.
func closeOf(t *testing.T, db *store.DB, terminal string) Told {
	t.Helper()
	var events []store.Event
	if err := db.View(t.Context(), func(tx *store.Tx) (err error) {
		events, err = tx.ListEvents(t.Context(), 100)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i := range events {
		if e := &events[i]; e.Action == EventClose && e.Subject == terminal {
			told, ok := ReadTold(e.Detail)
			if !ok {
				t.Fatalf("the close of %s says %q", terminal, e.Detail)
			}
			return told
		}
	}
	t.Fatalf("the close of %s isn't on record", terminal)
	return Told{}
}
