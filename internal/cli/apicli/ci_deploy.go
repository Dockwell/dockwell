// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
)

func (r *runner) waitDeploy(ctx context.Context, res *v1.DeployServiceResponse) error {
	c := res.GetChange()
	if c.GetId() == "" {
		return errors.New("the control server returned no deploy change. Check Activity before retrying")
	}
	fmt.Fprintln(r.env.Err, r.client.PanelURL("/changes/"+c.GetId()))
	if err := r.print(res, true); err != nil {
		return err
	}
	began := r.env.now()
	if c.GetState() == v1.ChangeState_CHANGE_STATE_PLANNED {
		switch {
		case c.GetWaits() != "":
			fmt.Fprintln(r.env.Err, c.GetWaits())
		case needsPerson(c.GetPlan()):
			fmt.Fprintln(r.env.Err, whyPerson(c.GetPlan())+". Waiting for approval in the panel.")
		default:
			_, err := r.client.Changes().ApplyChange(ctx, connect.NewRequest(&v1.ApplyChangeRequest{
				Id: c.GetId(), Fingerprint: c.GetPlan().GetFingerprint(),
			}))
			switch Reason(err) {
			case authapi.ReasonNeedsSession, "needs_step_up":
				fmt.Fprintln(r.env.Err, "Waiting for approval in the panel.")
			case "not_planned":
				// Another caller took the same durable deploy key first.
			default:
				if err != nil {
					return r.explain(err)
				}
			}
		}
	}
	last, err := r.watchTo(ctx, c.GetId(), false)
	if err != nil {
		return err
	}
	if last.GetState() == v1.ChangeState_CHANGE_STATE_FAILED {
		last = r.awaitDeployRollback(ctx, last)
	}
	if last.GetState() == v1.ChangeState_CHANGE_STATE_APPLIED && last.GetUndoneBy() != "" {
		return fmt.Errorf("%s was rolled back by %s. Open the change in the panel to see what it put back", last.GetId(), last.GetUndoneBy())
	}
	return r.outcome(last, began)
}

// Poll the original change for its rollback result. A CI token can follow
// that change without gaining access to plans that Dockwell makes on its own.
func (r *runner) awaitDeployRollback(ctx context.Context, c *v1.Change) *v1.Change {
	ctx, cancel := context.WithTimeout(ctx, rollbackFollow)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for c.GetRollbackStatus().GetState() == v1.RollbackState_ROLLBACK_STATE_APPLYING ||
		c.GetRollbackStatus().GetState() == v1.RollbackState_ROLLBACK_STATE_WAITING {
		select {
		case <-ctx.Done():
			return c
		case <-tick.C:
			res, err := r.client.Changes().GetChange(ctx, connect.NewRequest(&v1.GetChangeRequest{Id: c.GetId()}))
			if err == nil && res.Msg.GetChange() != nil {
				c = res.Msg.GetChange()
			}
		}
	}
	return c
}
