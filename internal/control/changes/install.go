// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// PlanInstallUpdates plans installing the updates waiting on servers: an edit of
// each server's host that makes no revision, its install simulated by the
// server's agent (ADR 0064). A plan with nothing to install is discarded, and
// says so.
func (h *Handler) PlanInstallUpdates(ctx context.Context, req *connect.Request[dockwellv1.PlanInstallUpdatesRequest]) (*connect.Response[dockwellv1.PlanInstallUpdatesResponse], error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if len(m.GetServerIds()) == 0 {
		return nil, authapi.Invalid(errors.New("name the servers to install updates on, by ID"))
	}
	picked, err := packageNames(m.GetPackages())
	if err != nil {
		return nil, authapi.Invalid(err)
	}
	if len(picked) > 0 && m.GetAll() {
		return nil, authapi.Invalid(errors.New("name the packages to install, or ask for every update, not both"))
	}
	var edits []revision.Edit
	var names []string
	err = h.db.View(ctx, func(tx *store.Tx) error {
		for _, id := range m.GetServerIds() {
			s, err := tx.GetServer(ctx, id)
			if errors.Is(err, store.ErrNotFound) || err == nil && s.RevokedAt != nil {
				return fault.New(scope.ErrNotFound, "there's no server %s in the fleet. List them with ListServers", id)
			}
			if err != nil {
				return err
			}
			names = append(names, s.Name)
			edits = append(edits, revision.Edit{
				Ref:     resource.Ref{Kind: host.Kind, Scope: id, Name: host.Name},
				Install: &resource.InstallAsked{All: m.GetAll(), Names: picked},
			})
		}
		return nil
	})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		on := strings.Join(names, ", ")
		switch {
		case len(picked) > 0:
			summary = "Update " + namedOrCounted(picked) + " on " + on
		case m.GetAll():
			summary = "Install updates on " + on
		default:
			summary = "Install security updates on " + on
		}
	}
	plan, err := h.engine.Propose(ctx, revision.Proposal{Summary: summary, Actor: who.Actor, Via: who.Via, Edits: edits})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if !slices.ContainsFunc(plan.Resources, func(r revision.ResourcePlan) bool { return r.Install != nil }) {
		_ = h.engine.Discard(ctx, plan.Change, who)
		on := strings.Join(names, ", ")
		said := "no security updates wait on " + on + ", so there's nothing to install. Check again on the Updates tab"
		switch {
		case len(picked) > 0:
			said = on + " has no update waiting for " + namedOrCounted(picked) + " any more, so there's nothing to install. Check again on the Updates tab"
		case m.GetAll():
			said = "no updates wait on " + on + ", so there's nothing to install. Check again on the Updates tab"
		}
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New(said), "nothing_to_install")
	}
	c, err := h.change(ctx, plan.Change)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.PlanInstallUpdatesResponse{Change: c}), nil
}

// maxPackages is the most packages one install may name.
const maxPackages = 1000

// packageName is what a package's name may hold, as the package managers Dockwell
// reads have them, an architecture after a colon included: the server's agent
// checks each against what its own read lists.
var packageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._~-]*(:[a-z0-9_-]+)?$`)

// packageNames are the packages an install names, each once, sorted, or an error
// saying which can't be a package's name.
func packageNames(in []string) ([]string, error) {
	if len(in) > maxPackages {
		return nil, fmt.Errorf("%d packages are more than one install takes, which is %d. Install every update, or fewer at a time", len(in), maxPackages)
	}
	out := make([]string, 0, len(in))
	for _, n := range in {
		n = strings.TrimSpace(n)
		if n == "" || len(n) > 200 || !packageName.MatchString(n) {
			return nil, fmt.Errorf("%q isn't a package's name. Name packages as GetServerUpdates lists them, such as openssl", n)
		}
		out = append(out, n)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// namedOrCounted says packages by name up to three, "openssl, curl and wget", and
// by count past that, "5 packages".
func namedOrCounted(names []string) string {
	if len(names) > 3 {
		return fmt.Sprintf("%d packages", len(names))
	}
	return human.List(names)
}
