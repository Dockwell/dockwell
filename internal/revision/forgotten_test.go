// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// A plan says what an offline server holds only of a deletion that hides logs, and only
// of a server that can come back to hear of it.
func TestAPlanSaysWhatAnOfflineServerKeeps(t *testing.T) {
	e, f, db := setup(t, store.SQLite, "srv_A")
	first := propose(t, e, "Run web", web("nginx:1"))
	apply(t, e, first)
	it := item{ref: web("nginx:1").Ref, id: first.Resources[0].ID, runs: true}
	hiding := &Plan{Servers: []ServerPlan{{Steps: []resource.Step{{Ref: it.ref, Decision: resource.Decision{HidesLogs: true}}}}}}
	said := func(plan *Plan) []string {
		t.Helper()
		notes, err := e.keptLogsNotes(t.Context(), plan, []item{it}, []Note{{Reason: "before"}})
		if err != nil {
			t.Fatal(err)
		}
		if notes[0].Reason != "before" {
			t.Errorf("the notes it had went: %+v", notes)
		}
		var names []string
		for _, n := range notes[1:] {
			if n.Reason != "kept_logs_offline" || !strings.Contains(n.Message, "until it's back") {
				t.Errorf("note %+v", n)
			}
			names = append(names, n.Names...)
		}
		return names
	}
	if got := said(hiding); got != nil {
		t.Errorf("a server that is connected: %v", got)
	}
	f["srv_A"].online.Store(false)
	if got := said(hiding); !slices.Equal(got, []string{"srv_A"}) {
		t.Errorf("a server that is offline: %v", got)
	}
	if got := said(&Plan{Servers: []ServerPlan{{Steps: []resource.Step{{Ref: it.ref}}}}}); got != nil {
		t.Errorf("a deletion that hides no logs: %v", got)
	}
	if err := db.Update(t.Context(), func(tx *store.Tx) error { return tx.RevokeServer(t.Context(), "srv_A", time.Now()) }); err != nil {
		t.Fatal(err)
	}
	if got := said(hiding); got != nil {
		t.Errorf("a server that was revoked: %v", got)
	}
}
