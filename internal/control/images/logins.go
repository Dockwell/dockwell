// SPDX-License-Identifier: AGPL-3.0-only

package images

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/control/registrylogins"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/registrylogin"
	"github.com/dockwell/dockwell/internal/store"
)

// ListRegistryLogins lists the registry logins: every one for an admin, with all
// but what signs in; below an admin, those the caller's projects may use, which the
// API trims to their names and registries (ADR 0086).
func (s *Service) ListRegistryLogins(ctx context.Context, req *connect.Request[dockwellv1.ListRegistryLoginsRequest]) (*connect.Response[dockwellv1.ListRegistryLoginsResponse], error) {
	out := &dockwellv1.ListRegistryLoginsResponse{}
	if s.logins == nil {
		return connect.NewResponse(out), nil
	}
	all, err := s.logins.All(ctx)
	if err != nil {
		return nil, err
	}
	project := ""
	narrowed := req.Msg.GetProject() != "" || req.Msg.GetEnvironment() != ""
	if narrowed {
		if project, err = s.logins.ProjectFor(ctx, req.Msg.GetEnvironment(), req.Msg.GetProject()); err != nil {
			return nil, err
		}
	}
	admin, mine := callerProjects(ctx)
	for _, l := range all {
		switch {
		case narrowed && !registrylogins.Allows(l.Spec, project):
			continue
		case !admin && !usableByAny(l.Spec, mine):
			continue
		}
		out.Logins = append(out.Logins, &dockwellv1.RegistryLoginInfo{
			Id: l.ID, Name: l.Name, Registry: l.Spec.GetRegistry(), Username: l.Spec.GetUsername(),
			Ecr: l.Spec.GetEcr() != nil, TokenHost: l.Spec.GetTokenHost(), Projects: l.Spec.GetProjects(),
		})
	}
	return connect.NewResponse(out), nil
}

// callerProjects says whether the caller is an admin, who sees every login, and
// otherwise the projects they hold a grant on; nil for every project, from a grant
// on the whole install.
func callerProjects(ctx context.Context) (admin bool, projects map[string]bool) {
	c, ok := place.CallerFrom(ctx)
	if !ok {
		// A call the API didn't gate, as the control plane's own.
		return true, nil
	}
	if store.RoleRank(place.Holds(c.Grants, place.Install)) >= store.RoleRank(store.RoleAdmin) {
		return true, nil
	}
	projects = map[string]bool{}
	for _, g := range c.Grants {
		if g.ProjectID == store.Install {
			return false, nil
		}
		projects[g.ProjectID] = true
	}
	return false, projects
}

// usableByAny says whether a login lets one of projects use it; nil projects is
// every project.
func usableByAny(l *resourcev1.RegistryLogin, projects map[string]bool) bool {
	if projects == nil || len(l.GetProjects()) == 0 {
		return true
	}
	for _, p := range l.GetProjects() {
		if projects[p] {
			return true
		}
	}
	return false
}

// CheckRegistryLogin signs in at a registry from a server's agent, as its pull
// would, and saves nothing (ADR 0086). A saved login checked again with its own
// password goes only to the token service it was saved with; a password typed now
// may go to the one the registry names, which the answer says, for the plan.
func (s *Service) CheckRegistryLogin(ctx context.Context, req *connect.Request[dockwellv1.CheckRegistryLoginRequest]) (*connect.Response[dockwellv1.CheckRegistryLoginResponse], error) {
	if s.logins == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this control plane keeps no registry logins"))
	}
	if req.Msg.GetProbe() {
		return s.probe(ctx, req.Msg)
	}
	spec, first, err := s.loginToCheck(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	out := &dockwellv1.CheckRegistryLoginResponse{Registry: spec.GetRegistry()}
	a := registry.Auth{Name: req.Msg.GetName(), Registry: spec.GetRegistry(), Username: spec.GetUsername(), Password: spec.GetPassword(), TokenHost: spec.GetTokenHost()}
	if e := spec.GetEcr(); e != nil {
		host, password, got := s.logins.ECR.Registry(ctx, e.GetRegion(), e)
		// What AWS said of the key is the check's answer, not the call's failure.
		if state := ecrState(got); state != dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_SIGNED_IN {
			out.State, out.Detail = state, got.Error()
			return connect.NewResponse(out), nil
		}
		if spec.GetRegistry() != "" && spec.GetRegistry() != host {
			out.State, out.Detail = dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_REFUSED, fmt.Sprintf("The key signs in to %s, not %s", host, spec.GetRegistry())
			return connect.NewResponse(out), nil
		}
		out.Registry = host
		a.Registry, a.Username, a.Password, a.TokenHost = host, "AWS", password, ""
		first = false
	}
	session, srv, err := s.session(ctx, req.Msg.GetServer())
	if err != nil {
		return nil, err
	}
	got, err := session.CheckRegistryLogin(ctx, a, first)
	switch {
	case errors.Is(err, agentapi.ErrOlderAgent):
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s can't check a registry login: %w", srv.GetName(), err))
	case err != nil:
		var se *resource.ServerError
		if errors.As(err, &se) {
			err = se.Err
		}
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't check a registry login now: %w", srv.GetName(), err), authapi.ReasonUnavailable)
	}
	out.State, out.TokenHost, out.Detail = got.GetState(), got.GetTokenHost(), got.GetDetail()
	if !first && out.State == dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_MOVED {
		out.Detail = fmt.Sprintf("%s now sends clients to %s for tokens, where Dockwell sent nothing. Type the password again to check it there", a.Registry, got.GetTokenHost())
	}
	return connect.NewResponse(out), nil
}

// probe asks a registry's /v2/ with no login, from a server's agent: whether it's a
// registry, and what it asks of a client, before a token typed for it goes there.
// It takes the host alone, and anything that signs in is refused.
func (s *Service) probe(ctx context.Context, req *dockwellv1.CheckRegistryLoginRequest) (*connect.Response[dockwellv1.CheckRegistryLoginResponse], error) {
	if req.GetPassword() != "" || req.GetEcr() != nil || req.GetName() != "" || req.GetUsername() != "" {
		return nil, authapi.Invalid(errors.New("a probe asks a registry with no login, so it takes its host alone. Leave out the username, the password, the key and the name"))
	}
	host := registrylogin.Host(req.GetRegistry())
	if host == "" {
		return nil, authapi.Invalid(fmt.Errorf("%q isn't a registry's host. Write it as an image names it, such as ghcr.io or registry.example.com:5000", req.GetRegistry()))
	}
	session, srv, err := s.session(ctx, req.GetServer())
	if err != nil {
		return nil, err
	}
	got, err := session.ProbeRegistry(ctx, host)
	switch {
	case errors.Is(err, agentapi.ErrOlderAgent):
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s can't ask a registry: %w", srv.GetName(), err))
	case err != nil:
		var se *resource.ServerError
		if errors.As(err, &se) {
			err = se.Err
		}
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't ask a registry now: %w", srv.GetName(), err), authapi.ReasonUnavailable)
	}
	return connect.NewResponse(&dockwellv1.CheckRegistryLoginResponse{State: got.GetState(), TokenHost: got.GetTokenHost(), Detail: got.GetDetail(), Registry: host}), nil
}

// ecrState is how a check reads what getting ECR's token came to.
func ecrState(err error) dockwellv1.RegistryLoginState {
	switch {
	case err == nil:
		return dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_SIGNED_IN
	case registrylogins.IsECRRefused(err):
		return dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_REFUSED
	}
	return dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_UNREACHABLE
}

// loginToCheck is the login a check signs in with: as typed, or a saved one with
// what's typed in its place. It says whether the login may go to a token service
// the registry names now, as one typed may; a saved password goes only where it
// was saved to go.
func (s *Service) loginToCheck(ctx context.Context, req *dockwellv1.CheckRegistryLoginRequest) (*resourcev1.RegistryLogin, bool, error) {
	typed := &resourcev1.RegistryLogin{Registry: req.GetRegistry(), Username: req.GetUsername(), Password: req.GetPassword(), Ecr: req.GetEcr()}
	if typed.GetEcr() != nil && typed.GetRegistry() == "" {
		// ECR names the registry from the key's account and region.
		if !registrylogin.IsRegion(typed.GetEcr().GetRegion()) {
			return nil, false, authapi.Invalid(errors.New("an ECR login needs its region, such as eu-west-1"))
		}
		if typed.GetEcr().GetAccessKey() == "" || typed.GetEcr().GetSecretKey() == "" {
			return nil, false, authapi.Invalid(errors.New("an ECR login needs an AWS access key and its secret"))
		}
		return typed, false, nil
	}
	if req.GetName() == "" {
		if err := (registrylogin.Type{}).Validate(typed); err != nil {
			return nil, false, authapi.Invalid(err)
		}
		if err := (registrylogin.Type{}).CheckNew(typed); err != nil {
			return nil, false, authapi.Invalid(err)
		}
		return typed, typed.GetEcr() == nil, nil
	}
	all, err := s.logins.All(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, l := range all {
		if l.Name != req.GetName() {
			continue
		}
		if typed.GetRegistry() == "" {
			typed.Registry = l.Spec.GetRegistry()
		}
		if typed.GetUsername() == "" && typed.GetEcr() == nil {
			typed.Username = l.Spec.GetUsername()
		}
		if typed.GetEcr() == nil && l.Spec.GetEcr() != nil && typed.GetPassword() == "" {
			typed.Ecr = l.Spec.GetEcr()
		}
		if err := (registrylogin.Type{}).Validate(typed); err != nil {
			return nil, false, authapi.Invalid(err)
		}
		typed.TokenHost = l.Spec.GetTokenHost()
		// What's kept goes only where it was saved to go, as an update keeps it.
		if _, err := (registrylogin.Type{}).Keep(l.Spec, typed); err != nil {
			return nil, false, authapi.Invalid(err)
		}
		first := req.GetPassword() != ""
		return typed, first, nil
	}
	return nil, false, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no registry login %s. List them with ListRegistryLogins", req.GetName()), "not_found")
}
