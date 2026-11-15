// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/revision"
)

// A drain of the fleet's watch ends once nothing waits, and once the watch has
// closed, as it does when the control plane stops: one that read a closed channel as
// an update spun for good, and dockwell run never stopped.
func TestADrainOfAClosedWatchEnds(t *testing.T) {
	t.Parallel()
	waiting := make(chan revision.Update, 2)
	waiting <- revision.Update{}
	waiting <- revision.Update{}
	closed := make(chan revision.Update, 1)
	closed <- revision.Update{}
	close(closed)
	for name, updates := range map[string]chan revision.Update{"waiting": waiting, "closed": closed} {
		done := make(chan struct{})
		go func() {
			drain(updates)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("draining a %s watch never ended", name)
		}
		if len(updates) != 0 {
			t.Errorf("draining a %s watch left %d updates", name, len(updates))
		}
	}
}
