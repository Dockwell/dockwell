// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// backupCost is a server whose volume grows or shrinks between a reviewed plan
// and applying it, as Grafana's does while its plugins write data at startup.
type backupCost struct {
	backingUp
	estimate int64
	downtime time.Duration
	change   func(*resource.Step)
	applied  *resource.Step
}

func (a *backupCost) Plan(ctx context.Context, desired []resource.Desired) ([]resource.Step, error) {
	steps, err := a.backingUp.Plan(ctx, desired)
	if err != nil {
		return nil, err
	}
	for i := range steps {
		step := &steps[i]
		if step.BackupFirst != nil {
			step.BackupFirst.Estimate = a.estimate
			step.BackupFirst.Stops = []string{"web"}
			step.Effect, step.Downtime = resource.EffectRestart, a.downtime
		}
		if a.change != nil {
			a.change(step)
		}
	}
	return steps, nil
}

func (a *backupCost) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if step.BackupFirst != nil {
		a.applied = &step
	}
	return a.backingUp.Apply(ctx, step, desired)
}

type backupCostBench struct {
	e     *Engine
	agent *backupCost
	plan  *Plan
	db    *store.DB
	did   []string
}

func withBackupCost(t *testing.T, backend store.Backend, estimate int64, downtime time.Duration) *backupCostBench {
	t.Helper()
	e, _, db := setupDocker(t, backend, "srv_A")
	env := production(t, db)
	placed := func(version string) []Edit {
		web := uses(env, "srv_A")
		app := web.Spec.(*resourcev1.Service).GetApp() //nolint:forcetypeassert // uses's
		app.Env, app.Networks = map[string]string{"VERSION": version}, nil
		return []Edit{data(env, "srv_A"), web}
	}
	apply(t, e, propose(t, e, "Run web", placed("1")...))
	b := &backupCostBench{e: e, db: db}
	agents := e.agents.(onDocker) //nolint:forcetypeassert // setupDocker's
	b.agent = &backupCost{
		backingUp: backingUp{Agent: agents["srv_A"], server: "srv_A", did: &b.did},
		estimate:  estimate, downtime: downtime,
	}
	agents["srv_A"] = b.agent
	edits := placed("2")
	edits[0].Backup = true
	b.plan = propose(t, e, "Upgrade web with a backup first", edits...)
	return b
}

func TestABackupCostChangingDoesntInvalidateTheReviewedUpgrade(t *testing.T) {
	for _, growing := range []bool{true, false} {
		name := "shrinking"
		if growing {
			name = "growing"
		}
		t.Run(name, func(t *testing.T) {
			storetest.Each(t, func(t *testing.T, backend store.Backend) {
				before, after := int64(250625191), int64(300<<20)
				firstTime, nextTime := 84*time.Second, 110*time.Second
				if !growing {
					before, after, firstTime, nextTime = after, before, nextTime, firstTime
				}
				b := withBackupCost(t, backend, before, firstTime)
				step := b.plan.Servers[0].Steps[0]
				if step.BackupFirst == nil || step.BackupFirst.Estimate != before || step.Downtime != firstTime {
					t.Fatalf("reviewed backup = %+v, want its original cost", step)
				}
				b.agent.estimate, b.agent.downtime = after, nextTime
				if err := b.e.Apply(t.Context(), b.plan.Change); err != nil {
					t.Fatalf("applying the reviewed upgrade after its backup cost changed: %v", err)
				}
				if got := stored(t, b.db, b.plan.Change); got.State != store.ChangeApplied {
					t.Errorf("upgrade state = %s, want applied: %s", got.State, got.Error)
				}
				if got := b.agent.applied; got == nil || got.BackupFirst.Estimate != after || got.Downtime != nextTime {
					t.Errorf("applied backup = %+v, want its current cost", got)
				}
				if !slices.Equal(b.did, []string{"back up on srv_A", "web on srv_A"}) {
					t.Errorf("did %v, want the backup before replacing web", b.did)
				}
			})
		})
	}
}

func TestABackupDoingSomethingElseStillInvalidatesTheReviewedUpgrade(t *testing.T) {
	for name, change := range map[string]func(*resource.Step){
		"id":       func(s *resource.Step) { s.BackupFirst.ID = "bkp_other" },
		"method":   func(s *resource.Step) { s.BackupFirst.Method = resource.BackupDump },
		"database": func(s *resource.Step) { s.BackupFirst.Database = "other" },
		"stops":    func(s *resource.Step) { s.BackupFirst.Stops = []string{"web", "worker"} },
		"effect":   func(s *resource.Step) { s.Effect = resource.EffectNone },
	} {
		t.Run(name, func(t *testing.T) {
			storetest.Each(t, func(t *testing.T, backend store.Backend) {
				b := withBackupCost(t, backend, 250625191, 84*time.Second)
				b.agent.change = func(s *resource.Step) {
					if s.BackupFirst != nil {
						change(s)
					}
				}
				if err := b.e.Apply(t.Context(), b.plan.Change); !errors.Is(err, ErrPlanChanged) {
					t.Fatalf("applying after the backup changed = %v, want ErrPlanChanged", err)
				}
				if got := stored(t, b.db, b.plan.Change); got.State != store.ChangePlanned || len(b.did) != 0 {
					t.Errorf("the refused upgrade is %s and did %v, want planned with nothing run", got.State, b.did)
				}
			})
		})
	}
}

func TestAServiceDowntimeChangingStillInvalidatesTheReviewedUpgrade(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := withBackupCost(t, backend, 250625191, 84*time.Second)
		b.agent.change = func(s *resource.Step) {
			if s.BackupFirst == nil {
				s.Downtime += time.Minute
			}
		}
		if err := b.e.Apply(t.Context(), b.plan.Change); !errors.Is(err, ErrPlanChanged) {
			t.Fatalf("applying after service downtime changed = %v, want ErrPlanChanged", err)
		}
		if got := stored(t, b.db, b.plan.Change); got.State != store.ChangePlanned || len(b.did) != 0 {
			t.Errorf("the refused upgrade is %s and did %v, want planned with nothing run", got.State, b.did)
		}
	})
}
