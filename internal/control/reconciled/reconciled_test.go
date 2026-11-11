// SPDX-License-Identifier: AGPL-3.0-only

package reconciled

import (
	"maps"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func events(t *testing.T, db *store.DB) []store.Event {
	t.Helper()
	var out []store.Event
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		out, err = tx.ListEvents(t.Context(), 100)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// What an agent hands back is on the timeline once, however often it's sent, made
// fit to show, at times that are neither ahead nor far behind.
func TestWhatAnAgentDidIsRecordedOnce(t *testing.T) {
	storetest.Each(t, testWhatAnAgentDidIsRecordedOnce)
}

func testWhatAnAgentDidIsRecordedOnce(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	r := &Recorder{DB: db, Now: func() time.Time { return now }}
	web := &dockwellv1.ResourceRef{Kind: "container", Scope: "srv_1", Name: "web"}
	rec := &agentv1.Reconciled{Dropped: 3, Entries: []*agentv1.LocalEntry{
		{Id: "boot-1", Resource: web, Action: agentv1.LocalAction_LOCAL_ACTION_MADE, Time: timestamppb.New(now.Add(-time.Hour))},
		{Id: "boot-2", Resource: web, Action: agentv1.LocalAction_LOCAL_ACTION_WAITING, Reason: "its volume\nis gone\x1b[31m", Time: timestamppb.New(now.Add(time.Hour))},
		{Id: "boot-3", Resource: &dockwellv1.ResourceRef{Kind: "container", Scope: "srv_1", Name: strings.Repeat("w", 500)}, Action: agentv1.LocalAction_LOCAL_ACTION_STARTED, Time: timestamppb.New(now.Add(-90 * 24 * time.Hour))},
		{Id: "boot-4", Resource: web, Action: agentv1.LocalAction_LOCAL_ACTION_UNSPECIFIED},
	}}
	for range 2 {
		if err := r.Heard(t.Context(), "srv_1", rec); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]store.Event{}
	all := events(t, db)
	for i := range all {
		got[all[i].Action] = all[i]
	}
	if n := len(events(t, db)); n != 4 {
		t.Fatalf("recorded %d events, want 4: three entries and what was dropped", n)
	}
	made, waiting, started, dropped := got[ActionMade], got[ActionWaiting], got[ActionStarted], got[ActionDropped]
	if !made.At.Equal(now.Add(-time.Hour)) || made.Actor != "server:srv_1" || made.Subject != "srv_1" {
		t.Errorf("made: %+v", made)
	}
	if told, _ := ReadTold(waiting.Detail); told.Reason != "its volume is gone[31m" || told.Name != "web" || !waiting.At.Equal(now) {
		t.Errorf("waiting, from ahead of now with a reason of two lines: %+v, %+v", waiting, told)
	}
	if told, _ := ReadTold(started.Detail); len([]rune(told.Name)) != maxName || !started.At.Equal(now.Add(-maxAge)) {
		t.Errorf("started, long ago with a long name: %d runes, at %v", len([]rune(told.Name)), started.At)
	}
	if told, _ := ReadTold(dropped.Detail); told.Dropped != 3 {
		t.Errorf("dropped: %+v", told)
	}
}

// A journal sent again seconds later, as after the agent didn't hear it was kept,
// is on Activity once: an entry from a clock ahead of the control plane's, and the
// count of those dropped, too, as neither is named by when it's heard (#357's
// first review, M4).
func TestAJournalSentAgainLaterIsRecordedOnce(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	r := &Recorder{DB: db, Now: func() time.Time { return now }}
	web := &dockwellv1.ResourceRef{Kind: "container", Scope: "srv_1", Name: "web"}
	rec := &agentv1.Reconciled{Dropped: 3, DroppedTo: "boot-0", Entries: []*agentv1.LocalEntry{
		{Id: "boot-1", Resource: web, Action: agentv1.LocalAction_LOCAL_ACTION_MADE, Time: timestamppb.New(now.Add(-time.Hour))},
		{Id: "boot-2", Resource: web, Action: agentv1.LocalAction_LOCAL_ACTION_WAITING, Reason: "Its volume is gone.", Time: timestamppb.New(now.Add(2 * time.Second))},
		{Id: "boot-3", Resource: web, Action: agentv1.LocalAction_LOCAL_ACTION_STARTED, Time: timestamppb.New(now.Add(-90 * 24 * time.Hour))},
	}}
	for range 3 {
		if err := r.Heard(t.Context(), "srv_1", rec); err != nil {
			t.Fatal(err)
		}
		now = now.Add(5 * time.Second)
	}
	counts := map[string]int{}
	for _, e := range events(t, db) {
		counts[e.Action]++
	}
	want := map[string]int{ActionMade: 1, ActionWaiting: 1, ActionStarted: 1, ActionDropped: 1}
	if !maps.Equal(counts, want) {
		t.Errorf("after three sends 5 s apart: %v, want each once", counts)
	}
}
