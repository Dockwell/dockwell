// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/signinproviders"
)

// ListSigninProviders lists the sign-in providers: every one for an admin, with all
// but its secret; below an admin, those the caller's projects may use, which the
// API trims to their names (ADR 0087).
func (h *Handler) ListSigninProviders(ctx context.Context, req *connect.Request[dockwellv1.ListSigninProvidersRequest]) (*connect.Response[dockwellv1.ListSigninProvidersResponse], error) {
	out := &dockwellv1.ListSigninProvidersResponse{}
	if h.Providers == nil {
		return connect.NewResponse(out), nil
	}
	all, err := h.Providers.All(ctx)
	if err != nil {
		return nil, err
	}
	admin, mine := callerProjects(ctx)
	project := req.Msg.GetProject()
	for _, p := range all {
		switch {
		case project != "" && !signinproviders.Allows(p.Spec, project):
			continue
		case !admin && !usableByAny(p.Spec.GetProjects(), mine):
			continue
		}
		out.Providers = append(out.Providers, &dockwellv1.SigninProviderInfo{
			Id: p.ID, Name: p.Name, DisplayName: p.Spec.GetDisplayName(), Issuer: p.Spec.GetIssuer(), ClientId: p.Spec.GetClientId(),
			HasSecret: p.Spec.GetClientSecret() != "", Scopes: p.Spec.GetScopes(), GroupsClaim: p.Spec.GetGroupsClaim(),
			AnyAccount: p.Spec.GetAnyAccount(), Projects: p.Spec.GetProjects(),
		})
	}
	return connect.NewResponse(out), nil
}

// CheckSigninProvider reads an issuer's discovery document from the control
// server, and saves nothing (ADR 0087).
func (h *Handler) CheckSigninProvider(ctx context.Context, req *connect.Request[dockwellv1.CheckSigninProviderRequest]) (*connect.Response[dockwellv1.CheckSigninProviderResponse], error) {
	if h.Providers == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this control plane keeps no sign-in providers"))
	}
	got, err := h.Providers.Check(ctx, req.Msg.GetIssuer())
	if err != nil {
		return nil, authapi.Invalid(err)
	}
	return connect.NewResponse(got), nil
}

// usableByAny says whether named lets one of projects use what names them; no
// names, or nil projects, is every project.
func usableByAny(named []string, projects map[string]bool) bool {
	if projects == nil || len(named) == 0 {
		return true
	}
	for _, p := range named {
		if projects[p] {
			return true
		}
	}
	return false
}

// withoutProvider is how a route signs in, as anyone who reads the route may see it:
// the provider by name and who may, and nothing the control plane filled in of the
// provider, its secret least of all.
func withoutProvider(s *resourcev1.RouteSignIn) *resourcev1.RouteSignIn {
	if s == nil {
		return nil
	}
	out := proto.CloneOf(s)
	out.Filled = nil
	return out
}
