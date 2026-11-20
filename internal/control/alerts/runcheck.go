// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/checks"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/store"
)

// checkRunner runs one check now, as checks.Runner does.
type checkRunner interface {
	RunNowInSlot(ctx context.Context, id string) (checks.Status, error)
}

// RunAlertCheck runs one check now, as its own run would, and says how it stands
// after (#832). It plans nothing, as it changes no resource; it goes on Activity. A
// person runs TestsPerMinute at most, whatever token they use, and the run takes
// one of the runner's places, refused when none is free.
func (h *Handler) RunAlertCheck(ctx context.Context, req *connect.Request[dockwellv1.RunAlertCheckRequest]) (*connect.Response[dockwellv1.RunAlertCheckResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	client, _ := authapi.ClientFrom(ctx)
	name := req.Msg.GetName()
	if name == "" {
		return nil, authapi.Invalid(errors.New("say which check to run, by name, such as shop-example-com"))
	}
	runner, ok := h.eval.Checks.(checkRunner)
	if !ok {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this control plane runs no checks"))
	}
	var stored store.Resource
	var spec *resourcev1.AlertCheck
	err := h.db.View(ctx, func(tx *store.Tx) error {
		org, err := tx.Organization(ctx)
		if err != nil {
			return err
		}
		if stored, err = tx.GetResource(ctx, string(alert.CheckKind), org.ID, name); err != nil {
			return err
		}
		if stored.Spec == nil {
			return store.ErrNotFound
		}
		decoded, err := h.eval.Keeper.Decode(ctx, tx, store.InRevision(stored.ID, stored.Kind, stored.Revision), stored.Spec)
		spec, _ = decoded.(*resourcev1.AlertCheck)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no uptime check %s. List them to find its name", name), "not_found")
	case err != nil:
		return nil, h.internal(ctx, err)
	}
	now := h.eval.now()
	person := p.Actor()
	if p.User.ID != "" {
		person = "user:" + p.User.ID
	}
	// The place is taken before the run, at once, so requests at the same moment
	// can't each find one free (the review's M2). A run refused before it asked
	// anything gives it back.
	if wait := h.runs.allow(person, now); wait > 0 {
		return nil, authapi.ReasonError(connect.CodeResourceExhausted,
			fmt.Errorf("you've run %d checks in the last minute, the most Dockwell runs for one person. Try again in %d seconds", TestsPerMinute, int(wait.Round(time.Second)/time.Second)), "too_many_requests")
	}
	st, err := runner.RunNowInSlot(ctx, stored.ID)
	switch {
	case errors.Is(err, checks.ErrNotKept):
		// It asked the address, so it counts, and goes on Activity as cut short.
	case err != nil:
		h.runs.release(person, now)
	}
	switch {
	case err == nil, errors.Is(err, checks.ErrNotKept):
	case errors.Is(err, checks.ErrBusy), errors.Is(err, checks.ErrRunning):
		return nil, authapi.ReasonError(connect.CodeResourceExhausted, err, "too_many_requests")
	case errors.Is(err, checks.ErrUnknown):
		return nil, authapi.ReasonError(connect.CodeUnavailable, err, "unavailable")
	case errors.Is(err, context.Canceled):
		return nil, connect.NewError(connect.CodeCanceled, err)
	default:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, err, "failed_precondition")
	}
	// On Activity, as a channel's test is: who ran it, of which host, and whether it
	// answered, was refused or was cut short. What the address said stays with the
	// check's runs, not the event (the review's L1). It's recorded whether or not the
	// person is still there to read the answer (the review's M7).
	where := ""
	if u, err := url.Parse(spec.GetAddress()); err == nil {
		where = u.Hostname()
	}
	told := Told{SubjectName: name, Facts: Facts{Where: where, Variant: RunNotKept}}
	if !errors.Is(err, checks.ErrNotKept) {
		told.Facts.Variant = RunFailed
		if len(st.Runs) > 0 && st.Runs[0].GetOk() {
			told.Facts.Variant, told.Facts.Value = RunAnswered, 1
		}
	}
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordWithin)
	defer cancel()
	if rerr := h.db.Update(record, func(tx *store.Tx) error {
		detail, err := json.Marshal(told)
		if err != nil {
			return err
		}
		return tx.RecordEvent(record, store.Event{ID: id.New(id.Event), At: now, Actor: p.Actor(), Action: ActionCheckRun, Detail: string(detail), Surface: client.Surface(p)})
	}); rerr != nil {
		return nil, h.internal(ctx, rerr)
	}
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeAborted, err, "aborted")
	}
	info := &dockwellv1.AlertCheckInfo{Id: stored.ID, Name: stored.Name, Spec: spec, From: checks.From, State: st.State, Runs: st.Runs, Problem: st.Problem}
	if !st.Since.IsZero() {
		info.Since = timestamppb.New(st.Since)
	}
	return connect.NewResponse(&dockwellv1.RunAlertCheckResponse{Check: info}), nil
}

// What a run a person asked for came to, as its event says (Facts.Variant).
const (
	RunAnswered = "answered"
	RunFailed   = "failed"
	RunNotKept  = "not_kept"
)

// recordWithin is how long recording a run on Activity may take, once the run is
// done, whether or not the request is still there.
const recordWithin = 10 * time.Second
