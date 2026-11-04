// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
)

// hanging is a driver whose check doesn't answer until released, as a
// Docker that takes the connection and says nothing.
type hanging struct {
	firewall.Driver
	release chan struct{}
	asked   atomic.Int32
}

func (h *hanging) Ready(ctx context.Context) error {
	h.asked.Add(1)
	select {
	case <-h.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Checking which kinds the agent can run never holds up the stream's
// loop, however long a driver takes to answer, and runs one check at a
// time.
func TestKindsAreCheckedOffTheLoop(t *testing.T) {
	h := &hanging{release: make(chan struct{})}
	checks := newKindsCheck(resource.NewRegistry(h))
	started := make(chan struct{})
	go func() {
		checks.start(t.Context())
		checks.start(t.Context())
		close(started)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("starting a check waited for the driver to answer")
	}
	close(h.release)
	select {
	case k := <-checks.done:
		checks.got()
		if len(k.ready) != 1 || len(k.unavailable) != 0 {
			t.Errorf("the check found %+v, want the firewall ready", k)
		}
	case <-time.After(time.Second):
		t.Fatal("the check's answer never came")
	}
	if n := h.asked.Load(); n != 1 {
		t.Errorf("two starts asked the driver %d times, want once, as the first was still under way", n)
	}
	checks.start(t.Context())
	<-checks.done
	if n := h.asked.Load(); n != 2 {
		t.Errorf("a start after the answer asked %d times in all, want 2", n)
	}
}
