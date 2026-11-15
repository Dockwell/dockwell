// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"bytes"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/dockwell/dockwell/internal/store"
)

// Applying a change reads its stored plan as often for 40 items as for 2: once for
// all its items, not about 13 times for each, which made applying grow with the
// square of a change's size (#686).
func TestApplyingReadsAPlanAsOftenForFortyItemsAsForTwo(t *testing.T) {
	e, _, _ := setup(t, store.SQLite, "srv_A")
	decodes := func(n int, port uint32) int32 {
		t.Helper()
		edits := make([]Edit, n)
		for i := range edits {
			edits[i] = rule("srv_A", fmt.Sprintf("rule-%d-%d", n, i), port+uint32(i))
		}
		p := propose(t, e, fmt.Sprintf("Open %d ports", n), edits...)
		// Only this change's plan counts: one of another test's changes doesn't.
		its := []byte(`"` + p.Change + `"`)
		var count atomic.Int32
		heard := func(b []byte) {
			if bytes.Contains(b, its) {
				count.Add(1)
			}
		}
		planDecoded.Store(&heard)
		defer planDecoded.Store(nil)
		apply(t, e, p)
		return count.Load()
	}
	two, forty := decodes(2, 2000), decodes(40, 3000)
	t.Logf("reads of the change's plan: %d applying 2 items, %d applying 40", two, forty)
	if two == 0 {
		t.Fatal("applying read no plan, so nothing here counts how often it does")
	}
	if forty != two {
		t.Errorf("applying 40 items read the change's plan %d times, and 2 items %d: it reads it again for each item", forty, two)
	}
}
