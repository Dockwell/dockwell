// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"

	"github.com/dockwell/dockwell/internal/store"
)

// ReusableCIDeploy finds an active attempt or the rollout still current for
// the service. An empty ID means the caller can plan a new attempt.
func (e *Engine) ReusableCIDeploy(ctx context.Context, key store.CIDeployKey) (string, error) {
	var change string
	err := e.db.View(ctx, func(tx *store.Tx) (err error) {
		change, err = e.reusableCIDeploy(ctx, tx, key)
		return err
	})
	return change, err
}

func (e *Engine) reusableCIDeploy(ctx context.Context, tx *store.Tx, key store.CIDeployKey) (string, error) {
	id, err := tx.CIDeployChange(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	c, err := tx.GetChange(ctx, id)
	if err != nil {
		return "", err
	}
	switch c.State {
	case store.ChangeApplying:
		return id, nil
	case store.ChangePlanned:
		_, err := e.current(ctx, tx, c, readPlan(c))
		switch {
		case err == nil || errors.Is(err, ErrBusy):
			return id, nil
		case staleCIDeploy(err), errors.Is(err, ErrInvalid):
			// A plan that no longer holds isn't reused: a new one reports
			// its own problem instead of the old one sticking to the key.
			return "", nil
		default:
			return "", err
		}
	case store.ChangeApplied:
		svc, err := tx.GetResourceByID(ctx, key.Service)
		if err != nil || svc.ChangeID != id || svc.Spec == nil {
			return "", err
		}
		deployments, err := tx.Deployments(ctx, svc.ID, svc.Revision)
		if err != nil || len(deployments) == 0 {
			return "", err
		}
		for i := range deployments {
			if deployments[i].State != store.DeploymentLive {
				return "", nil
			}
		}
		// This revision belongs to the keyed change, which pins this digest.
		return id, nil
	default:
		return "", nil
	}
}

func staleCIDeploy(err error) bool {
	return errors.Is(err, ErrStale) || errors.Is(err, ErrPlanChanged)
}

// Agent-side drift can invalidate a plan even when all its base revisions
// match. Release its key when apply proves it stale.
func (e *Engine) releaseStaleCIDeploy(ctx context.Context, id string, err error) {
	if !staleCIDeploy(err) {
		return
	}
	writeCtx := context.WithoutCancel(ctx)
	if releaseErr := e.db.Revise(writeCtx, func(tx *store.RevisionTx) error {
		return tx.ForgetStaleCIDeploy(writeCtx, id)
	}); releaseErr != nil {
		e.logger.WarnContext(ctx, "couldn't release a stale CI deploy", "change", id, "error", releaseErr)
	}
}
