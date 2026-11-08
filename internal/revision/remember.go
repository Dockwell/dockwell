// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// Rememberer is an agent that keeps what its server should run, so the server
// can be put back while the control plane is away (ADR 0054).
type Rememberer interface {
	// Holds is what the agent keeps, by resource and revision, and whether it
	// keeps anything at all.
	Holds() (map[resource.Ref]int64, bool)
	// Remember has the agent keep what ref runs now, and says whether it did.
	Remember(ctx context.Context, ref resource.Ref, spec proto.Message, revision int, change string) (bool, error)
}

// Remember has a server's agent keep what the server runs, as each latest
// revision live there has it, where the agent doesn't keep that revision yet: one
// from before it kept anything, one reinstalled, or one whose records were lost or
// damaged. It sends nothing for a resource the agent keeps a later revision of, as
// of a step that ended after its stream did: resuming that change decides. It
// changes nothing on the server, so it makes no change and no revision. It says
// how many the agent kept.
func (e *Engine) Remember(ctx context.Context, server string) (int, error) {
	agent, online := e.agents.Agent(server)
	if !online {
		return 0, nil
	}
	r, ok := agent.(Rememberer)
	if !ok {
		return 0, nil
	}
	held, keeps := r.Holds()
	if !keeps {
		return 0, nil
	}
	type lacking struct {
		ref  resource.Ref
		res  store.Resource
		spec proto.Message
	}
	var send []lacking
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		live, err := tx.ListLive(ctx, server)
		if err != nil {
			return err
		}
		for _, res := range live {
			ref := resource.Ref{Kind: resource.Kind(res.Kind), Scope: res.Scope, Name: res.Name}
			if held[ref] >= int64(res.Revision) {
				continue
			}
			spec, err := e.decode(ctx, tx, store.InRevision(res.ID, res.Kind, res.Revision), res.Spec)
			if err != nil {
				return err
			}
			send = append(send, lacking{ref: ref, res: res, spec: spec})
		}
		return nil
	}); err != nil {
		return 0, err
	}
	kept := 0
	var errs []error
	for i := range send {
		l := &send[i]
		ok, err := r.Remember(ctx, l.ref, l.spec, l.res.Revision, l.res.ChangeID)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", l.ref, err))
		case ok:
			kept++
		}
	}
	return kept, errors.Join(errs...)
}
