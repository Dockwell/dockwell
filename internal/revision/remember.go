// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// ActionDesiredSent is the timeline's action for a fill that sent a server's
// agent the values of variables and secrets to keep (ADR 0054).
const ActionDesiredSent = "server.desired_sent"

// Rememberer is an agent that keeps what its server should run, so the server
// can be put back while the control plane is away (ADR 0054).
type Rememberer interface {
	// Holds is what the agent keeps, by resource and revision, its cache's ID,
	// and whether it keeps anything at all.
	Holds() (held map[resource.Ref]int64, cache string, keeps bool)
	// Remember has the agent keep what ref runs now, and says whether it did.
	Remember(ctx context.Context, ref resource.Ref, spec proto.Message, revision int, change string) (bool, error)
	// Forget has the agent forget ref, which its server no longer runs, if it
	// keeps it at revision, and says whether it did.
	Forget(ctx context.Context, ref resource.Ref, revision int64) (bool, error)
}

// Filled is what one fill did.
type Filled struct {
	// Kept counts the resources the agent kept, and WithValues those of them
	// sent with the values of variables and secrets.
	Kept, WithValues int
	// Forgot counts the resources the agent forgot, as its server no longer
	// runs them.
	Forgot int
	// NewCache says the agent's cache was new to the control plane.
	NewCache bool
}

// DesiredSent is what a fill's event keeps: counts, and the cache, never a value
// or a spec.
type DesiredSent struct {
	Resources  int    `json:"resources"`
	WithValues int    `json:"with_values"`
	Cache      string `json:"cache"`
	NewCache   bool   `json:"new_cache"`
}

// ReadDesiredSent reads a fill's event's detail.
func ReadDesiredSent(detail string) (DesiredSent, bool) {
	var d DesiredSent
	return d, json.Unmarshal([]byte(detail), &d) == nil
}

// Remember fills a server's agent's cache of what the server runs: it sends each
// resource whose latest revision is live there that the agent doesn't keep at
// that revision, as for an agent from before it kept anything, one reinstalled,
// or one whose records were lost; and has it forget each it keeps that isn't
// placed there, as after a deletion that failed or a server marked gone. It sends
// nothing for a resource the agent keeps a later revision of, as of a step that
// ended after its stream did: resuming that change decides. It changes nothing on
// the server, so it makes no change and no revision, but a fill that sends values
// goes on the server's timeline, saying when the agent's cache is new to the
// control plane. One fill of a server runs at a time.
func (e *Engine) Remember(ctx context.Context, server string) (Filled, error) {
	var n Filled
	agent, online := e.agents.Agent(server)
	if !online {
		return n, nil
	}
	r, ok := agent.(Rememberer)
	if !ok {
		return n, nil
	}
	turn, _ := e.fills.LoadOrStore(server, &sync.Mutex{})
	mu := turn.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	held, cache, keeps := r.Holds()
	if !keeps {
		return n, nil
	}
	type lacking struct {
		ref resource.Ref
		res store.Resource
	}
	var send []lacking
	placed := map[resource.Ref]bool{}
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		live, err := tx.ListLive(ctx, server)
		if err != nil {
			return err
		}
		for _, res := range live {
			ref := resource.Ref{Kind: resource.Kind(res.Kind), Scope: res.Scope, Name: res.Name}
			if held[ref] < int64(res.Revision) {
				send = append(send, lacking{ref: ref, res: res})
			}
		}
		// What a change placed there, applying now or not, isn't forgotten:
		// its step has the say.
		all, err := tx.ListPlaced(ctx, server)
		for _, res := range all {
			placed[resource.Ref{Kind: resource.Kind(res.Kind), Scope: res.Scope, Name: res.Name}] = true
		}
		return err
	}); err != nil {
		return n, err
	}
	var errs []error
	for i := range send {
		l := &send[i]
		// Opened one at a time, and dropped once sent.
		var spec proto.Message
		if err := e.db.View(ctx, func(tx *store.Tx) error {
			var err error
			spec, err = e.decode(ctx, tx, store.InRevision(l.res.ID, l.res.Kind, l.res.Revision), l.res.Spec)
			return err
		}); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", l.ref, err))
			continue
		}
		kept, err := r.Remember(ctx, l.ref, spec, l.res.Revision, l.res.ChangeID)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", l.ref, err))
		case kept:
			n.Kept++
			if t, err := e.kinds.Type(l.ref.Kind); err == nil && len(resource.Uses(t, spec)) > 0 {
				n.WithValues++
			}
		}
	}
	for ref, revision := range held {
		if placed[ref] {
			continue
		}
		gone, err := r.Forget(ctx, ref, revision)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", ref, err))
		case gone:
			n.Forgot++
		}
	}
	if err := e.noteCache(ctx, server, cache, &n); err != nil {
		errs = append(errs, err)
	}
	return n, errors.Join(errs...)
}

// noteCache keeps the ID of the server's agent's cache, and puts a fill that sent
// values on the server's timeline, saying whether the cache was new.
func (e *Engine) noteCache(ctx context.Context, server, cache string, n *Filled) error {
	return e.db.Update(ctx, func(tx *store.Tx) error {
		was, err := tx.GetSetting(ctx, store.SettingDesiredCache(server))
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		n.NewCache = string(was) != cache
		if n.NewCache {
			if err := tx.PutSetting(ctx, store.SettingDesiredCache(server), []byte(cache)); err != nil {
				return err
			}
		}
		if n.WithValues == 0 {
			return nil
		}
		detail, err := json.Marshal(DesiredSent{Resources: n.Kept, WithValues: n.WithValues, Cache: cache, NewCache: n.NewCache})
		if err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: e.now(), Actor: SystemActor, Action: ActionDesiredSent, Subject: server, Target: server,
			Surface: ViaAutomatic, Detail: string(detail),
		})
	})
}
