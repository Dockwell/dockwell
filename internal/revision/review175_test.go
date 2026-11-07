// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"testing"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Review of PR 175: a restore that replaced the volume's data and then failed, as
// one does when what it stopped won't start, can't be undone, though its server
// keeps what it replaced for 7 days.
func TestReviewARestoreThatFailedAfterReplacingCantBeUndone(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		x := withVolume(t, b)
		e, r, ref, env, db := x.e, x.r, x.ref, x.env, x.db
		withData(t, e, env)
		// What backup.Runner.ApplyRestore returns once the volume holds the backup.
		r.apply = func(context.Context) error {
			return errors.New("the volume is restored, and what stopped didn't all start: can't start web: port is already allocated")
		}
		plan, err := e.Restore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Server: "srv_A"}, restoreBy(), "")
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Apply(t.Context(), plan.Change); err == nil {
			t.Fatal("it applied")
		}
		c := stored(t, db, plan.Change)
		t.Logf("change %s: %s", c.State, c.Error)
		if _, err := e.Undo(t.Context(), plan.Change, restoreBy()); err != nil {
			t.Errorf("undoing a restore that replaced the data: %v", err)
		}
	})
}
