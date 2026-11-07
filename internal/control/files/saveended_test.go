// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// An agent speaks for its own server and nothing more: one that's been taken over
// can't put a save on record for another server, or as someone who didn't save.
func TestASaveEndedOnlyGoesOnRecordForASaveOnThatServer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		db, _ := storetest.Open(t, backend)
		ctx := t.Context()
		logger := slog.New(slog.DiscardHandler)
		hub, err := fleet.New(ctx, db, logger)
		if err != nil {
			t.Fatal(err)
		}
		h := New(hub, agentapi.NewAgents(resource.NewRegistry(), logger), nil, db, logger)
		if err := db.Update(ctx, func(tx *store.Tx) error {
			for _, e := range []store.Event{
				{ID: "evt_saving", At: time.Now(), Actor: "user:mira", Action: eventSaving, Subject: "/a", Target: "srv_A", Surface: store.SurfacePanel},
				{ID: "evt_read", At: time.Now(), Actor: "user:mira", Action: eventRead, Subject: "/a", Target: "srv_A", Surface: store.SurfacePanel},
			} {
				if err := tx.RecordEvent(ctx, e); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		recorded := func() []store.Event {
			t.Helper()
			var events []store.Event
			if err := db.View(ctx, func(tx *store.Tx) error {
				var err error
				events, err = tx.ListEvents(ctx, 10)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return events
		}

		h.saveEnded("srv_B", &agentv1.SaveEnded{Record: "evt_saving", Path: "/a", Landed: true})
		h.saveEnded("srv_A", &agentv1.SaveEnded{Record: "evt_read", Path: "/a", Landed: true})
		h.saveEnded("srv_A", &agentv1.SaveEnded{Record: "evt_nothing", Path: "/a", Landed: true})
		if got := recorded(); len(got) != 2 {
			t.Fatalf("what an agent had no business saying went on record: %+v", got)
		}

		h.saveEnded("srv_A", &agentv1.SaveEnded{Record: "evt_saving", Path: "/a\n\x1b[31mb", Landed: true})
		got := recorded()
		if len(got) != 3 {
			t.Fatalf("a save that landed isn't on record: %+v", got)
		}
		e := got[0]
		if e.Action != eventSave || e.Actor != "user:mira" || e.Target != "srv_A" || e.Surface != store.SurfacePanel || strings.ContainsAny(e.Subject, "\n\x1b") {
			t.Errorf("it's on record as %+v", e)
		}

		h.saveEnded("srv_A", &agentv1.SaveEnded{Record: "evt_saving", Path: "/a", Reason: "no space left on device"})
		if e := recorded()[0]; e.Action != eventSaveFailed || !strings.Contains(e.Detail, "no space left") {
			t.Errorf("a save that didn't land is on record as %+v", e)
		}
	})
}
