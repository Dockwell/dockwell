// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Task runs, as the control plane keeps them (ADR 0053).
func TestTaskRuns(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		servers(t, db, "srv_A")
		now := time.Now().UTC().Truncate(time.Millisecond)
		weekAgo := now.Add(-8 * 24 * time.Hour)
		// Three tasks: one deleted over a week ago, one deleted yesterday, which
		// undoing the delete could bring back, and one that's there.
		revise(t, db, func(tx *store.RevisionTx) error {
			if err := tx.CreateOrganization(ctx, store.Organization{ID: "org_1", Name: "default", CreatedAt: weekAgo}); err != nil {
				return err
			}
			for _, c := range []struct {
				id string
				at time.Time
			}{{"chg_1", weekAgo.Add(-time.Hour)}, {"chg_2", weekAgo}, {"chg_3", now.Add(-24 * time.Hour)}} {
				if err := tx.CreateChange(ctx, store.Change{ID: c.id, Summary: "tasks", Actor: "usr_1", State: store.ChangeApplying, Fingerprint: "f", Plan: []byte("{}"), CreatedAt: c.at}); err != nil {
					return err
				}
			}
			for i, n := range []store.NewRevision{
				{Kind: "project", Scope: "org_1", Name: "default", ID: "prj_1", Spec: []byte{1}, Change: "chg_1", At: weekAgo.Add(-time.Hour)},
				{Kind: "environment", Scope: "prj_1", Name: "production", ID: "env_1", Spec: []byte{2}, Change: "chg_1", At: weekAgo.Add(-time.Hour)},
				{Kind: "task", Scope: "env_1", Name: "old", ID: "tsk_old", Spec: []byte{3}, Change: "chg_1", At: weekAgo.Add(-time.Hour)},
				{Kind: "task", Scope: "env_1", Name: "recent", ID: "tsk_recent", Spec: []byte{4}, Change: "chg_1", At: weekAgo.Add(-time.Hour)},
				{Kind: "task", Scope: "env_1", Name: "kept", ID: "tsk_kept", Spec: []byte{5}, Change: "chg_1", At: weekAgo.Add(-time.Hour)},
				{Kind: "task", Scope: "env_1", Name: "old", Change: "chg_2", At: weekAgo},
				{Kind: "task", Scope: "env_1", Name: "recent", Change: "chg_3", At: now.Add(-24 * time.Hour)},
			} {
				n.Position = i
				if _, err := tx.AddRevision(ctx, n); err != nil {
					return err
				}
			}
			return nil
		})
		put := func(r store.TaskRun) {
			t.Helper()
			if err := db.Update(ctx, func(tx *store.Tx) error { return tx.PutTaskRun(ctx, r) }); err != nil {
				t.Fatal(err)
			}
		}
		const running, succeeded = 1, 2

		// A run that ended stays ended, whatever comes after: Run now's answer can
		// arrive after the agent's word that it ended.
		put(store.TaskRun{ID: "run_1", TaskID: "tsk_kept", ServerID: "srv_A", State: succeeded, StartedAt: now, EndedAt: now.Add(time.Second), Run: []byte("ended")})
		put(store.TaskRun{ID: "run_1", TaskID: "tsk_kept", ServerID: "srv_A", State: running, StartedAt: now, Run: []byte("running")})
		// One that's running ends as ever.
		put(store.TaskRun{ID: "run_2", TaskID: "tsk_kept", ServerID: "srv_A", State: running, StartedAt: now, Run: []byte("running")})
		put(store.TaskRun{ID: "run_2", TaskID: "tsk_kept", ServerID: "srv_A", State: succeeded, StartedAt: now, EndedAt: now.Add(time.Second), Run: []byte("ended")})
		_ = db.View(ctx, func(tx *store.Tx) error {
			for _, id := range []string{"run_1", "run_2"} {
				r, err := tx.GetTaskRun(ctx, id)
				if err != nil || r.State != succeeded || r.EndedAt.IsZero() || string(r.Run) != "ended" {
					t.Errorf("%s = %+v, %v", id, r, err)
				}
			}
			return nil
		})

		// The runs of a task deleted over a week ago go, and of one that's gone
		// altogether; the others stay.
		put(store.TaskRun{ID: "run_old", TaskID: "tsk_old", ServerID: "srv_A", State: succeeded, StartedAt: weekAgo, EndedAt: weekAgo, Run: []byte("x")})
		put(store.TaskRun{ID: "run_recent", TaskID: "tsk_recent", ServerID: "srv_A", State: succeeded, StartedAt: now, EndedAt: now, Run: []byte("x")})
		put(store.TaskRun{ID: "run_unknown", TaskID: "tsk_unknown", ServerID: "srv_A", State: succeeded, StartedAt: now, EndedAt: now, Run: []byte("x")})
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.PruneGoneTaskRuns(ctx, "task", now.Add(-7*24*time.Hour))
		}); err != nil {
			t.Fatal(err)
		}
		_ = db.View(ctx, func(tx *store.Tx) error {
			var left []string
			for _, task := range []string{"tsk_old", "tsk_recent", "tsk_kept", "tsk_unknown"} {
				runs, err := tx.ListTaskRuns(ctx, store.TaskRunFilter{TaskID: task})
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range runs {
					left = append(left, r.ID)
				}
			}
			slices.Sort(left)
			if want := []string{"run_1", "run_2", "run_recent"}; !slices.Equal(left, want) {
				t.Errorf("after pruning, the runs are %v, want %v", left, want)
			}
			return nil
		})
	})
}
