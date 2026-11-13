// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// Stop is what stopped a step that failed as a wait ended: its own time running
// out, a wait inside it running out first, or the step being called off. Each
// reads differently, as only the first is the step's time (#612).
type Stop int

const (
	// NotStopped is a failure no wait ended: its error says what it is.
	NotStopped Stop = iota
	// StepTime is the step's own time running out.
	StepTime
	// InnerWait is a wait inside the step running out, such as for Docker to
	// answer, with the step's own time left.
	InnerWait
	// CalledOff is a step stopped before its time ran out: its change was
	// cancelled, the control plane stopped waiting for it, or the agent stopped.
	CalledOff
)

// step is the context a step runs in, and when it was called off, if it was.
type step struct {
	ctx context.Context
	// called is when the step's context was cancelled, in Unix nanoseconds, or 0.
	called atomic.Int64
}

// stepKey holds the context a step runs in (WithStep).
type stepKey struct{}

// WithStep marks ctx as the context a step runs in, so a failure inside it, under
// any wait of its own, can tell the step's time running out from that wait's, and
// from the step being called off (StopOf). It notes when the step ended, as what
// failed may be read only after a cleanup that outlasts it.
func WithStep(ctx context.Context) context.Context {
	s := &step{ctx: ctx}
	context.AfterFunc(ctx, func() { s.called.Store(time.Now().UnixNano()) })
	return context.WithValue(ctx, stepKey{}, s)
}

// failedAt is an error with when it happened, for its cause to be read after a
// cleanup (FailedAt).
type failedAt struct {
	err error
	at  time.Time
}

func (f *failedAt) Error() string { return f.err.Error() }

func (f *failedAt) Unwrap() error { return f.err }

// FailedAt notes when err happened, as a driver does before a cleanup that can
// take longer than the step has left: what stopped the step is read as of then,
// not as of when the cleanup ends (StopOf). It reads as err does.
func FailedAt(err error, at time.Time) error {
	if err == nil {
		return nil
	}
	return &failedAt{err: err, at: at}
}

// endSlack is how long after a step was called off a failure it caused may come,
// as the note of when it was runs beside whatever saw it first.
const endSlack = time.Second

// StopOf says what stopped a step that failed with err, in ctx or a context under
// it: the context marked as the step's (WithStep), or ctx itself where none is. It
// goes by when err happened, where the driver noted it (FailedAt), and by now
// otherwise: a step that ended after its failure didn't cause it.
func StopOf(ctx context.Context, err error) Stop {
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return NotStopped
	}
	at := time.Now()
	if f := (*failedAt)(nil); errors.As(err, &f) {
		at = f.at
	}
	s, marked := ctx.Value(stepKey{}).(*step)
	stepCtx := ctx
	if marked {
		stepCtx = s.ctx
	}
	switch cause := context.Cause(stepCtx); {
	case stepCtx.Err() == nil:
	case errors.Is(cause, context.DeadlineExceeded):
		if deadline, ok := stepCtx.Deadline(); !ok || !deadline.After(at) {
			return StepTime
		}
	case errors.Is(cause, context.Canceled):
		if !marked || s.called.Load() == 0 || !time.Unix(0, s.called.Load()).After(at.Add(endSlack)) {
			return CalledOff
		}
	default:
		// The step's context ended for a cause of its own, whose words say what.
		return NotStopped
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return InnerWait
	}
	// Something inside cancelled a wait of its own, with the step going on: its
	// error says what.
	return NotStopped
}
