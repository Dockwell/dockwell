// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/labels"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// PlanServerLabels plans setting and removing labels on servers: an edit of each
// server's host, whose labels only the control plane keeps, so it takes no step on
// any server (ADR 0091). Each edit starts from the host's latest revision, so one
// that changed meanwhile is refused as stale rather than undone.
func (h *Handler) PlanServerLabels(ctx context.Context, req *connect.Request[dockwellv1.PlanServerLabelsRequest]) (*connect.Response[dockwellv1.PlanServerLabelsResponse], error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if len(m.GetServerIds()) == 0 {
		return nil, authapi.Invalid(errors.New("name the servers to label, by ID"))
	}
	if len(m.GetSet()) == 0 && len(m.GetRemove()) == 0 {
		return nil, authapi.Invalid(errors.New("say a label to set, as key:value, or a key to remove"))
	}
	// Each label checks before any server is read, so a bad one says so once.
	if _, err := labels.Apply(nil, m.GetSet(), m.GetRemove()); err != nil {
		return nil, authapi.Invalid(err)
	}
	var edits []revision.Edit
	var names []string
	changes := false
	err = h.db.View(ctx, func(tx *store.Tx) error {
		var joined map[string]map[string]string
		for _, id := range m.GetServerIds() {
			s, err := tx.GetServer(ctx, id)
			if errors.Is(err, store.ErrNotFound) || err == nil && s.RevokedAt != nil {
				return fault.New(scope.ErrNotFound, "there's no server %s in the fleet. List them with ListServers", id)
			}
			if err != nil {
				return err
			}
			ref := resource.Ref{Kind: host.Kind, Scope: id, Name: host.Name}
			now, base, err := h.hostOf(ctx, tx, ref)
			if err != nil {
				return err
			}
			if base == 0 {
				// A server whose host isn't made yet starts from what it joined with,
				// which making its host would have given it.
				if joined == nil {
					if joined, err = tx.JoinLabels(ctx); err != nil {
						return err
					}
				}
				now.Labels = joined[id]
			}
			next, err := labels.Apply(now.GetLabels(), m.GetSet(), m.GetRemove())
			if err != nil {
				return fault.New(revision.ErrInvalid, "%s: %w", s.Name, err)
			}
			changes = changes || !maps.Equal(now.GetLabels(), next)
			spec := proto.CloneOf(now)
			spec.Labels = next
			if len(next) == 0 {
				spec.Labels = nil
			}
			names = append(names, s.Name)
			edits = append(edits, revision.Edit{Ref: ref, Spec: spec, Base: base})
		}
		return nil
	})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if !changes {
		whose := names[0]
		if len(names) > 1 {
			whose = "each of " + strings.Join(names, ", ")
		}
		return nil, authapi.Invalid(fmt.Errorf("%s has those labels already, so there's nothing to change", whose))
	}
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		summary = labelsSummary(m.GetSet(), m.GetRemove(), names)
	}
	plan, err := h.engine.Propose(ctx, revision.Proposal{Summary: summary, Actor: who.Actor, Via: who.Via, Edits: edits})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	c, err := h.change(ctx, plan.Change)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.PlanServerLabelsResponse{Change: c}), nil
}

// hostOf is a server's host as its latest revision has it, and that revision's
// number, or an empty host and 0 for one not made yet.
func (h *Handler) hostOf(ctx context.Context, tx *store.Tx, ref resource.Ref) (*resourcev1.ServerHost, int, error) {
	r, err := tx.GetResource(ctx, string(ref.Kind), ref.Scope, ref.Name)
	if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
		return &resourcev1.ServerHost{}, r.Revision, nil
	}
	if err != nil {
		return nil, 0, err
	}
	decoded, err := h.engine.Kinds().Decode(ref.Kind, r.Spec)
	if err != nil {
		return nil, 0, fmt.Errorf("can't read the host of %s: %w", ref.Scope, err)
	}
	spec, ok := decoded.(*resourcev1.ServerHost)
	if !ok {
		return nil, 0, fmt.Errorf("the host of %s is a %T, not a ServerHost", ref.Scope, decoded)
	}
	return spec, r.Revision, nil
}

// labelsSummary says what a change of labels does, for the timeline: "Label hz-1
// region:eu, without arch".
func labelsSummary(set, remove, servers []string) string {
	var parts []string
	if len(set) > 0 {
		parts = append(parts, strings.Join(set, " "))
	}
	if len(remove) > 0 {
		parts = append(parts, "without "+strings.Join(remove, " "))
	}
	return "Label " + strings.Join(servers, ", ") + " " + strings.Join(parts, ", ")
}
