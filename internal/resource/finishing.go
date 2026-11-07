// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"context"
	"sync"
	"time"
)

// A step's call can end with the step still owing the server something: what it
// began stopping has to come back, and what it made has to go. That goes on past
// the call's context, on a bound of its own, and whoever runs the step has to know
// of it, as the two ways it can go want opposite things. A cleanup that's slow is
// waited for. One that outruns even its own bound is left behind, and from then on
// the server can move on without it, so it must touch nothing more.

// Overtime is what a step says of the time it takes past its call's end, and how
// whoever runs it calls that off.
type Overtime struct {
	left  context.Context // ends once the step is left behind
	leave context.CancelFunc

	mu    sync.Mutex
	until time.Time // how late what the step is finishing can run
}

// NewOvertime is a step's, before it starts.
func NewOvertime() *Overtime {
	left, leave := context.WithCancel(context.Background())
	return &Overtime{left: left, leave: leave}
}

type overtimeKey struct{}

// WithOvertime has the step that runs with ctx report to o.
func WithOvertime(ctx context.Context, o *Overtime) context.Context {
	return context.WithValue(ctx, overtimeKey{}, o)
}

// Finishing returns the context of what a step has to finish whatever becomes of
// its call, for up to limit. It outlives ctx, and ends at the limit, or as soon as
// the step is left behind. One asked for after that has ended already, so check it
// before anything that isn't a call to Docker: the server may have moved on without
// this step, and the next step to the resource finishes what's left.
func Finishing(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	out, cancel := context.WithTimeout(context.WithoutCancel(ctx), limit)
	o, _ := ctx.Value(overtimeKey{}).(*Overtime)
	if o == nil {
		return out, cancel
	}
	if !o.extend(limit) {
		cancel()
		return out, cancel
	}
	unhook := context.AfterFunc(o.left, cancel)
	return out, func() { unhook(); cancel() }
}

// extend says what the step is finishing can run for limit from now, and reports
// false once it's been left behind. It and Leave take turns, so a step is either
// waited for or told it was left, whichever came first.
func (o *Overtime) extend(limit time.Duration) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.left.Err() != nil {
		return false
	}
	if until := time.Now().Add(limit); until.After(o.until) {
		o.until = until
	}
	return true
}

// Leave leaves the step behind, unless what it's finishing has time yet, grace past
// its bound. Then it says how much longer that is, and leaves nothing.
func (o *Overtime) Leave(grace time.Duration) time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	if wait := time.Until(o.until.Add(grace)); wait > 0 {
		return wait
	}
	o.leave()
	return 0
}
