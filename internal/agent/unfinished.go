// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// unfinished is what outlives the call that began it, across an agent's streams.
//
// A step can go on after its call ends, as one that was stopping a container has to
// start it again, for as long as that container takes to stop. The control plane
// plans again as soon as the agent is back, or as soon as its own deadline passes,
// and what it finds then is halfway through: the next step would stop, start and
// rename the same container, beside this one. An agent can't wait for it, as it
// may never end. So the resource waits instead, and nothing else does.
type unfinished struct {
	// left counts the calls left behind.
	left atomic.Int32
	// back signals that what held calls up ended, for the control plane to hear on
	// whichever stream is up. It holds one, as hearing once covers them all, and one
	// sent with no stream up waits for the next.
	back chan struct{}

	mu    sync.Mutex
	steps map[*step]struct{}
}

// step is an apply that hasn't returned.
type step struct {
	ref resource.Ref
	// call is its call's context: once that ends, the step goes on by itself.
	call context.Context
	// refused says something was turned away for it, and waits to hear it ended.
	refused bool
}

func newUnfinished() *unfinished {
	return &unfinished{back: make(chan struct{}, 1), steps: map[*step]struct{}{}}
}

// wake has the control plane hear that what held calls up ended.
func (u *unfinished) wake() {
	select {
	case u.back <- struct{}{}:
	default:
	}
}

// begin notes an apply as it starts, or refuses a plan or an apply for a resource
// with a step that outlived its call. Its answer is the refusal.
func (u *unfinished) begin(ctx context.Context, call *agentv1.Call) (*step, *agentv1.CallResult) {
	refs, applies := touches(call)
	u.mu.Lock()
	defer u.mu.Unlock()
	for s := range u.steps {
		if s.call.Err() != nil && slices.Contains(refs, s.ref) {
			s.refused = true
			return nil, &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{Unfinished: true, Message: fmt.Sprintf(
				"the last step on %s hasn't ended on this server, so this can't go ahead yet. What waits for it goes on once it ends; anything else, try again then. "+
					"If it doesn't end, check Docker and the server's mounts, then restart the agent", s.ref.Name)}}}
		}
	}
	if !applies {
		return nil, nil
	}
	s := &step{ref: refs[0], call: ctx}
	u.steps[s] = struct{}{}
	return s, nil
}

// end notes a step returned, however late, and wakes what it turned away.
func (u *unfinished) end(s *step) {
	if s == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.steps, s)
	if s.refused {
		u.wake()
	}
}

// touches says which resources a call plans or changes, and whether it changes them.
func touches(call *agentv1.Call) ([]resource.Ref, bool) {
	if apply := call.GetApply(); apply != nil {
		return []resource.Ref{resourcepb.FromRef(apply.GetDesired().GetResource())}, true
	}
	desired := call.GetPlan().GetDesired()
	refs := make([]resource.Ref, 0, len(desired))
	for _, d := range desired {
		refs = append(refs, resourcepb.FromRef(d.GetResource()))
	}
	return refs, false
}
