// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"context"
	"errors"
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
	// cancelled, or the control plane stopped waiting for it.
	CalledOff
)

// stepKey holds the context a step runs in (WithStep).
type stepKey struct{}

// WithStep marks ctx as the context a step runs in, so a failure inside it, under
// any wait of its own, can tell the step's time running out from that wait's, and
// from the step being called off (StopOf).
func WithStep(ctx context.Context) context.Context {
	return context.WithValue(ctx, stepKey{}, ctx)
}

// StopOf says what stopped a step that failed with err, in ctx or a context under
// it: the context marked as the step's (WithStep), or ctx itself where none is.
func StopOf(ctx context.Context, err error) Stop {
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return NotStopped
	}
	step, ok := ctx.Value(stepKey{}).(context.Context)
	if !ok {
		step = ctx
	}
	switch {
	case errors.Is(step.Err(), context.DeadlineExceeded):
		return StepTime
	case step.Err() != nil:
		return CalledOff
	case errors.Is(err, context.DeadlineExceeded):
		return InnerWait
	}
	// Something inside cancelled a wait of its own, with the step going on: its
	// error says what.
	return NotStopped
}
