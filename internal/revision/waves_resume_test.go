// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// A resume runs no step on a server that took none when the change was reviewed, as
// it's in no wave, though it would take one now: here, srv_C had its rule already,
// and lost it by hand while the change was stopped partway (ADR 0094, review L1).
func TestAResumeRunsNoStepOnAServerInNoWave(t *testing.T) {
	e, f, db := setup(t, store.SQLite, "srv_A", "srv_B", "srv_C")
	apply(t, e, propose(t, e, "C has it", rule("srv_C", "http", 80)))
	if f["srv_C"].firewall.Len() != 1 {
		t.Fatalf("srv_C has %d rules before the change", f["srv_C"].firewall.Len())
	}
	plain := f["srv_A"].agent
	started := make(chan struct{}, 1)
	f["srv_A"].agent = hooked{Agent: plain, apply: func(ctx context.Context, _ resource.Step, _ proto.Message) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}
	p, err := e.Propose(t.Context(), Proposal{
		Summary: "everywhere", Actor: "usr_test",
		Edits:   []Edit{rule("srv_A", "http", 80), rule("srv_B", "http", 80), rule("srv_C", "http", 80)},
		Rollout: &Rollout{Servers: []string{"srv_A", "srv_B", "srv_C"}, Canary: true, Batch: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Waves == nil || p.Waves.waveOf("srv_C") != 0 || p.Waves.waveOf("srv_A") != 1 || p.Waves.waveOf("srv_B") != 2 {
		t.Fatalf("the waves are %+v, want srv_A then srv_B, and srv_C in none", p.Waves)
	}
	if err := e.Start(t.Context(), p.Change, By{Actor: "user:usr_mira", Via: ViaPanel}); err != nil {
		t.Fatal(err)
	}
	<-started
	grace, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := e.Stop(grace); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if c := stored(t, db, p.Change); c.State != store.ChangeApplying {
		t.Fatalf("stopped partway, the change is %s", c.State)
	}
	// srv_C loses its rule by hand meanwhile, so planned again it would take a step.
	if err := f["srv_C"].firewall.Delete(t.Context(), "http"); err != nil || f["srv_C"].firewall.Len() != 0 {
		t.Fatalf("taking srv_C's rule away: %v, %d left", err, f["srv_C"].firewall.Len())
	}
	f["srv_A"].agent = plain
	next := newEngine(db, f)
	if err := next.Resume(t.Context()); err != nil {
		t.Fatalf("Resume = %v", err)
	}
	next.Wait()
	if c := stored(t, db, p.Change); c.State != store.ChangeApplied {
		t.Fatalf("after the resume the change is %s", c.State)
	}
	if f["srv_A"].firewall.Len() != 1 || f["srv_B"].firewall.Len() != 1 {
		t.Errorf("the waves' servers have %d and %d rules, want 1 each", f["srv_A"].firewall.Len(), f["srv_B"].firewall.Len())
	}
	if f["srv_C"].firewall.Len() != 0 {
		t.Error("the resume ran a step on srv_C, which is in no wave")
	}
}
