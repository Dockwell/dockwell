// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
)

const defaultDeployTimeout = 30 * time.Minute

const (
	exitDeployTimeout    = 5
	exitDeployRolledBack = 6
	exitDeployDiscarded  = 7
)

// retryDeploy retries a call that failed on its way to the control server.
// The first such failure is said once, and a timeout names the last one, so a
// wrong address or certificate never fails silently.
func (r *runner) retryDeploy(ctx context.Context, call func() error) error {
	var last error
	for delay := 250 * time.Millisecond; ; delay = min(delay*2, 5*time.Second) {
		err := call()
		if !deployRetryable(err) {
			return err
		}
		if last == nil {
			fmt.Fprintf(r.env.Err, "Can't reach the control server (%v). Trying again.\n", err)
		}
		last = err
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return &unreachableError{ctx: ctx.Err(), last: last}
		case <-timer.C:
		}
	}
}

// unreachableError is a deadline reached while the control server couldn't
// be reached; it is the context's error, carrying the last failure.
type unreachableError struct{ ctx, last error }

func (e *unreachableError) Error() string { return e.last.Error() }
func (e *unreachableError) Unwrap() error { return e.ctx }

func deployRetryable(err error) bool {
	if err == nil {
		return false
	}
	// A name that doesn't exist or a certificate that doesn't verify won't
	// change by trying again.
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsNotFound {
		return false
	}
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &host) || errors.As(err, &invalid) {
		return false
	}
	code := connect.CodeOf(err)
	if code == connect.CodeUnavailable || code == connect.CodeDeadlineExceeded {
		return true
	}
	var transport net.Error
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &transport)
}

func (r *runner) deployWithWait(ctx context.Context, req *v1.DeployServiceRequest) error {
	ctx, cancel := context.WithTimeout(ctx, r.opts.deployTimeout)
	defer cancel()
	var res *v1.DeployServiceResponse
	propose := func() error {
		return r.withChoices(ctx, req, func() error {
			got, err := r.client.Services().DeployService(ctx, connect.NewRequest(req))
			if err == nil {
				res = got.Msg
			}
			return err
		})
	}

	var err error
	if req.GetDeployOrigin() != nil {
		err = r.retryDeploy(ctx, propose)
	} else {
		err = propose()
	}
	if err != nil {
		return r.deployWaitError(ctx, nil, err, false)
	}
	return r.waitDeploy(ctx, res)
}

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
	approval := false
	if c.GetState() == v1.ChangeState_CHANGE_STATE_PLANNED {
		var err error
		approval, err = r.beginDeploy(ctx, c)
		if err != nil {
			return r.deployWaitError(ctx, c, err, approval)
		}
	}
	last, err := r.watchTo(ctx, c.GetId(), false)
	if err != nil {
		if last == nil {
			last = c
		}
		return r.deployWaitError(ctx, last, err, approval)
	}
	if last.GetState() == v1.ChangeState_CHANGE_STATE_FAILED {
		last = r.awaitDeployRollback(ctx, last)
		if ctx.Err() != nil && rollbackPending(last) {
			return r.deployWaitError(ctx, last, ctx.Err(), false)
		}
	}
	outcome := r.outcome(last, began)
	switch {
	case last.GetState() == v1.ChangeState_CHANGE_STATE_APPLIED && last.GetUndoneBy() != "":
		return ExitError{exitDeployRolledBack, fmt.Errorf("%s was rolled back by %s. Open the change in the panel to see what it put back", last.GetId(), last.GetUndoneBy())}
	case last.GetRollbackStatus().GetState() == v1.RollbackState_ROLLBACK_STATE_APPLIED && outcome != nil:
		return ExitError{exitDeployRolledBack, outcome}
	case last.GetState() == v1.ChangeState_CHANGE_STATE_DISCARDED:
		return ExitError{exitDeployDiscarded, outcome}
	default:
		return outcome
	}
}

func (r *runner) beginDeploy(ctx context.Context, c *v1.Change) (bool, error) {
	switch {
	case c.GetWaits() != "":
		fmt.Fprintln(r.env.Err, c.GetWaits())
		return true, nil
	case needsPerson(c.GetPlan()):
		fmt.Fprintln(r.env.Err, whyPerson(c.GetPlan())+". Waiting for approval in the panel.")
		return true, nil
	}
	err := r.retryDeploy(ctx, func() error {
		return r.do(ctx, func() error {
			_, err := r.client.Changes().ApplyChange(ctx, connect.NewRequest(&v1.ApplyChangeRequest{
				Id: c.GetId(), Fingerprint: c.GetPlan().GetFingerprint(),
			}))
			return err
		})
	})
	switch Reason(err) {
	case authapi.ReasonNeedsSession, authapi.ReasonNeedsStepUp:
		fmt.Fprintln(r.env.Err, "Waiting for approval in the panel.")
		return true, nil
	case "not_planned":
		// Another caller applied this plan, or an apply's response was lost.
		return false, nil
	default:
		return false, err
	}
}

func (r *runner) deployWaitError(ctx context.Context, c *v1.Change, err error, approval bool) error {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return r.explain(err)
	}
	if c.GetState() == v1.ChangeState_CHANGE_STATE_PLANNED && (approval || c.GetWaits() != "" || needsPerson(c.GetPlan())) {
		return ExitError{ExitNeedsPerson, fmt.Errorf("%s is still waiting for approval. Open its panel link to review and apply it", c.GetId())}
	}
	if c.GetId() != "" {
		return ExitError{exitDeployTimeout, fmt.Errorf("timed out waiting for %s. Check its panel link or run the CI job again", c.GetId())}
	}
	var unreachable *unreachableError
	if errors.As(err, &unreachable) {
		return ExitError{exitDeployTimeout, fmt.Errorf("timed out reaching the control server, so nothing was deployed. The last try said: %w. Check the panel address and token, then run the CI job again", unreachable.last)}
	}
	return ExitError{exitDeployTimeout, errors.New("timed out waiting for a deploy plan. Check Activity or run the CI job again")}
}

func rollbackPending(c *v1.Change) bool {
	state := c.GetRollbackStatus().GetState()
	return state == v1.RollbackState_ROLLBACK_STATE_APPLYING || state == v1.RollbackState_ROLLBACK_STATE_WAITING
}

// Poll the original change for its rollback result. A CI token can follow
// that change without gaining access to plans that Dockwell makes on its own.
func (r *runner) awaitDeployRollback(ctx context.Context, c *v1.Change) *v1.Change {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for rollbackPending(c) {
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
