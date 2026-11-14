// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/labels"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// ReasonServersOffline is a plan's note of servers left out of a set, as they
// aren't connected (ADR 0094).
const ReasonServersOffline = "servers_offline"

// maxPause is the longest pause a rollout takes between waves.
const maxPause = 24 * time.Hour

// maxBatch is the most servers a rollout's wave holds.
const maxBatch = 1000

// targets are the servers an action on a set runs on (ADR 0094), in the order
// they go: by name, with the control server last, so the panel and the API stop
// only once the rest are done.
type targets struct {
	servers []store.Server
	// rollout is how the change runs over them, for more than one.
	rollout *revision.Rollout
	// notes say what the plan should of servers left out, as they're offline.
	notes []revision.Note
}

// names are the targets' names, in order.
func (t targets) names() []string {
	out := make([]string, 0, len(t.servers))
	for i := range t.servers {
		out = append(out, t.servers[i].Name)
	}
	return out
}

// on says which servers, for a change's summary: by name up to three, "web-1,
// web-2 and web-3", and by count past that, as "12 servers" or "12 servers with
// region:eu" for a selector.
func (t targets) on(selector string) string {
	if len(t.servers) <= 3 {
		return human.List(t.names())
	}
	if selector = strings.TrimSpace(selector); selector != "" {
		return fmt.Sprintf("%d servers with %s", len(t.servers), selector)
	}
	return fmt.Sprintf("%d servers", len(t.servers))
}

// targetsOf reads the servers a request names, by ID or by a selector of their
// labels, for what, such as "reboot", and how it rolls out over them. A set of
// more than one leaves out each server that's offline, as its agent can't plan
// its step, and the plan says so; a set of one is as it was, as an action on one
// server has always been.
func (h *Handler) targetsOf(ctx context.Context, tx *store.Tx, ids []string, selector string, rollout *dockwellv1.FleetRollout, what string) (targets, error) {
	var t targets
	selector = strings.TrimSpace(selector)
	switch {
	case selector != "" && len(ids) > 0:
		return t, authapi.Invalid(errors.New("name the servers by ID or by a selector, not both"))
	case selector == "" && len(ids) == 0:
		return t, authapi.Invalid(fmt.Errorf("name the servers to %s, by ID or by a selector of their labels, such as region:eu", what))
	}
	if err := checkRollout(rollout); err != nil {
		return t, err
	}
	if selector != "" {
		sel, err := labels.ParseSelector(selector)
		if err != nil {
			return t, authapi.Invalid(err)
		}
		if t.servers, err = matching(ctx, tx, sel); err != nil {
			return t, err
		}
		if len(t.servers) == 0 {
			return t, authapi.Invalid(fmt.Errorf("no server's labels match %s. List the servers it should match with ListServers and their labels", sel))
		}
	} else {
		for _, id := range ids {
			s, err := tx.GetServer(ctx, id)
			if errors.Is(err, store.ErrNotFound) || err == nil && s.RevokedAt != nil {
				return t, fault.New(scope.ErrNotFound, "there's no server %s in the fleet. List them with ListServers", id)
			}
			if err != nil {
				return t, err
			}
			if !slices.ContainsFunc(t.servers, func(o store.Server) bool { return o.ID == s.ID }) {
				t.servers = append(t.servers, s)
			}
		}
	}
	control := h.engine.ControlServer()
	slices.SortFunc(t.servers, func(a, b store.Server) int {
		if (a.ID == control) != (b.ID == control) {
			if a.ID == control {
				return 1
			}
			return -1
		}
		return cmp.Compare(a.Name, b.Name)
	})
	if len(t.servers) < 2 {
		return t, nil
	}
	var away []string
	t.servers = slices.DeleteFunc(t.servers, func(s store.Server) bool {
		if h.engine.Connected(s.ID) {
			return false
		}
		away = append(away, s.Name)
		return true
	})
	if len(t.servers) == 0 {
		return t, fault.New(revision.ErrOffline, "none of these servers is connected, so nothing can plan the %s. Try again once their agents reconnect", what)
	}
	switch len(away) {
	case 0:
	case 1:
		t.notes = append(t.notes, revision.Note{
			Reason: ReasonServersOffline, Names: away, Named: true,
			Message: away[0] + " isn't connected, so it isn't in this change. Run it again once it's back",
		})
	default:
		t.notes = append(t.notes, revision.Note{
			Reason: ReasonServersOffline, Names: away, Named: true,
			Message: human.List(away) + " aren't connected, so they aren't in this change. Run it again once they're back",
		})
	}
	t.rollout = &revision.Rollout{
		Canary: !rollout.GetNoCanary(), Batch: int(rollout.GetBatchSize()),
		Pause: rollout.GetPause().AsDuration(), KeepGoing: rollout.GetKeepGoing(),
	}
	for i := range t.servers {
		t.rollout.Servers = append(t.rollout.Servers, t.servers[i].ID)
	}
	return t, nil
}

// checkRollout refuses a rollout it can't run.
func checkRollout(r *dockwellv1.FleetRollout) error {
	if r == nil {
		return nil
	}
	if p := r.GetPause().AsDuration(); p < 0 || p > maxPause {
		return authapi.Invalid(fmt.Errorf("a pause between waves can be up to %s, not %s", maxPause, p))
	}
	if r.GetBatchSize() > maxBatch {
		return authapi.Invalid(fmt.Errorf("a wave can hold up to %d servers, not %d. Leave the batch size unset for a quarter of them", maxBatch, r.GetBatchSize()))
	}
	return nil
}

// matching are the servers in the fleet whose labels sel matches, as their hosts'
// latest revisions have them (ADR 0091).
func matching(ctx context.Context, tx *store.Tx, sel labels.Selector) ([]store.Server, error) {
	servers, err := tx.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := tx.ListKind(ctx, string(host.Kind))
	if err != nil {
		return nil, err
	}
	of := map[string]map[string]string{}
	for i := range hosts {
		r := &hosts[i]
		if r.Spec == nil {
			continue
		}
		hs := &resourcev1.ServerHost{}
		if err := proto.Unmarshal(r.Spec, hs); err != nil {
			return nil, fmt.Errorf("can't read the labels of %s: %w", r.Scope, err)
		}
		of[r.Scope] = hs.GetLabels()
	}
	var out []store.Server
	for i := range servers {
		if s := servers[i]; s.RevokedAt == nil && sel.Matches(of[s.ID]) {
			out = append(out, s)
		}
	}
	return out, nil
}
