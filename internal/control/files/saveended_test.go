// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
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

// N2: an archive job always writes a started row and its outcome row even when
// ArchiveJobEnded arrives before finishArchiveStart runs, or after a control-plane
// restart, or when an agent restart drops a running job.
func TestArchiveJobActivityBeforeTrackingAndAfterRestart(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		db, _ := storetest.Open(t, backend)
		ctx := t.Context()
		logger := slog.New(slog.DiscardHandler)
		hub, err := fleet.New(ctx, db, logger)
		if err != nil {
			t.Fatal(err)
		}
		h := New(hub, agentapi.NewAgents(resource.NewRegistry(), logger), nil, db, logger)
		srv := &dockwellv1.Server{Id: "srv_A", Name: "hz-1"}
		p := auth.Principal{User: store.User{ID: "usr_mira"}}
		w := &writing{h: h, p: p, srv: srv, record: h.recorder(ctx, p, srv), op: opCompress, giveBack: func() {}}

		listAll := func() []store.Event {
			t.Helper()
			var evs []store.Event
			if err := db.View(ctx, func(tx *store.Tx) error {
				var err error
				evs, err = tx.ListEvents(ctx, 50)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return evs
		}

		// 1) Job finishes BEFORE finishArchiveStart runs (early ArchiveJobEnded).
		earlyJobID := "AAAAAAAAAAAAAAAAAAAAAAAAAA"
		earlyRecID := "evt_early_start"
		h.beginArchiveTracking("srv_A", earlyJobID, "usr_mira", earlyRecID, "/srv/a.zip", w)
		doneSnap := &dockwellv1.ArchiveJob{
			JobId:       earlyJobID,
			Kind:        dockwellv1.ArchiveJobKind_ARCHIVE_JOB_KIND_COMPRESS,
			State:       dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE,
			Path:        "/srv/a.zip",
			SourcePaths: []string{"/srv/index.html"},
			SourceCount: 1,
			Bytes:       128,
		}
		// ArchiveJobEnded arrives first, while finishArchiveStart hasn't run yet.
		h.archiveJobEnded("srv_A", &agentv1.ArchiveJobEnded{Job: doneSnap, Record: earlyRecID})
		if got := listAll(); len(got) != 0 {
			t.Fatalf("early end wrote before started row: %+v", got)
		}
		// Now the RPC reply arrives with its initial RUNNING snapshot.
		runningSnap := &dockwellv1.ArchiveJob{
			JobId:       earlyJobID,
			Kind:        dockwellv1.ArchiveJobKind_ARCHIVE_JOB_KIND_COMPRESS,
			State:       dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING,
			Path:        "/srv/a.zip",
			SourcePaths: []string{"/srv/index.html"},
			SourceCount: 1,
		}
		h.finishArchiveStart(ctx, "srv_A", earlyJobID, earlyRecID, runningSnap, w)
		got := listAll()
		if len(got) != 2 {
			t.Fatalf("events after early finish = %d (%+v), want 2", len(got), got)
		}
		actions := map[string]bool{}
		for _, e := range got {
			actions[e.Action] = true
		}
		if !actions["file.compress_started"] || !actions["file.compress"] {
			t.Fatalf("missing started or done event: %+v", got)
		}
		// Duplicate ArchiveJobEnded must not write a second outcome row.
		h.archiveJobEnded("srv_A", &agentv1.ArchiveJobEnded{Job: doneSnap, Record: earlyRecID})
		if len(listAll()) != 2 {
			t.Fatalf("duplicate ArchiveJobEnded wrote a 3rd row")
		}

		// 2) Control-plane restart: brand new Handler with empty in-memory maps
		// settles a job whose file.extract_started row was persisted before restart.
		restartJobID := "BBBBBBBBBBBBBBBBBBBBBBBBBB"
		restartRecID := "evt_restart_start"
		startDetail, _ := json.Marshal(Told{JobID: restartJobID, To: "/srv/site", WayBackIn: true})
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.RecordEvent(ctx, store.Event{
				ID: restartRecID, At: time.Now(), Actor: "user:usr_mira", Action: "file.extract_started",
				Subject: "/srv/site.zip", Target: "srv_A", Surface: store.SurfacePanel, Detail: string(startDetail),
			})
		}); err != nil {
			t.Fatal(err)
		}
		h2 := New(hub, agentapi.NewAgents(resource.NewRegistry(), logger), nil, db, logger)
		restartedDone := &dockwellv1.ArchiveJob{
			JobId:      restartJobID,
			Kind:       dockwellv1.ArchiveJobKind_ARCHIVE_JOB_KIND_EXTRACT,
			State:      dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE,
			Path:       "/srv/site.zip",
			UnpackedTo: "/srv/site",
			WayBackIn:  true,
		}
		// Another server must not be able to settle srv_A's job.
		h2.archiveJobEnded("srv_B", &agentv1.ArchiveJobEnded{Job: restartedDone, Record: restartRecID})
		if len(listAll()) != 3 {
			t.Fatalf("wrong server settled srv_A's job")
		}
		h2.archiveJobEnded("srv_A", &agentv1.ArchiveJobEnded{Job: restartedDone, Record: restartRecID})
		afterRestart := listAll()
		if len(afterRestart) != 4 || afterRestart[0].Action != "file.extract" || afterRestart[0].Actor != "user:usr_mira" {
			t.Fatalf("after control-plane restart events = %+v", afterRestart)
		}
		// Even a third handler (second restart) must not write a duplicate outcome.
		h3 := New(hub, agentapi.NewAgents(resource.NewRegistry(), logger), nil, db, logger)
		h3.archiveJobEnded("srv_A", &agentv1.ArchiveJobEnded{Job: restartedDone, Record: restartRecID})
		if len(listAll()) != 4 {
			t.Fatalf("replayed ArchiveJobEnded after restart wrote duplicate row")
		}

		// 3) Agent restart drops a pending job -> file.compress_failed recorded.
		lostJobID := "CCCCCCCCCCCCCCCCCCCCCCCCCC"
		lostRecID := "evt_lost_start"
		h.beginArchiveTracking("srv_A", lostJobID, "usr_mira", lostRecID, "/srv/lost.zip", w)
		h.finishArchiveStart(ctx, "srv_A", lostJobID, lostRecID, &dockwellv1.ArchiveJob{
			JobId: lostJobID, Kind: dockwellv1.ArchiveJobKind_ARCHIVE_JOB_KIND_COMPRESS,
			State: dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING, Path: "/srv/lost.zip",
		}, w)
		h.failLostArchiveJob("srv_A", lostJobID)
		finalEvs := listAll()
		if finalEvs[0].Action != "file.compress_failed" || !strings.Contains(finalEvs[0].Detail, "agent restarted") {
			t.Fatalf("lost job event = %+v, want file.compress_failed mentioning agent restarted", finalEvs[0])
		}
	})
}
