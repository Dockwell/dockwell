// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/store"
)

var _ dockwellv1connect.TokenServiceHandler = (*Handler)(nil)

// CreateToken makes an API token for the signed-in user.
func (h *Handler) CreateToken(ctx context.Context, req *connect.Request[dockwellv1.CreateTokenRequest]) (*connect.Response[dockwellv1.CreateTokenResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	var scope string
	switch m.GetScope() {
	case dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY:
		scope = store.ScopeReadOnly
	case dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE, dockwellv1.TokenScope_TOKEN_SCOPE_DEPLOY:
		scope = store.ScopeReadWrite
	case dockwellv1.TokenScope_TOKEN_SCOPE_UNSPECIFIED:
		// CreateToken refuses an empty scope, and says which to choose.
	}
	var expires *time.Time
	switch e := m.GetExpiration().(type) {
	case *dockwellv1.CreateTokenRequest_ExpireTime:
		if err := e.ExpireTime.CheckValid(); err != nil {
			return nil, h.rpcError(ctx, fmt.Errorf("%w: the expiry isn't a valid time", auth.ErrInvalid))
		}
		at := e.ExpireTime.AsTime()
		expires = &at
	case *dockwellv1.CreateTokenRequest_Ttl:
		if err := e.Ttl.CheckValid(); err != nil || e.Ttl.AsDuration() <= 0 {
			return nil, h.rpcError(ctx, fmt.Errorf("%w: choose a ttl longer than zero, or no expiry", auth.ErrInvalid))
		}
		// On the control plane's clock, whatever the client's says.
		at := h.auth.Now().Add(e.Ttl.AsDuration())
		expires = &at
	}
	grants := make([]string, 0, len(m.GetGrants()))
	for _, g := range m.GetGrants() {
		if g != dockwellv1.TokenGrant_TOKEN_GRANT_LOGS {
			return nil, h.rpcError(ctx, fmt.Errorf("%w: a token can't be granted %s", auth.ErrInvalid, g))
		}
		grants = append(grants, store.GrantLogs)
	}
	reach := auth.TokenReach{}
	switch m.GetRole() {
	case dockwellv1.TokenRole_TOKEN_ROLE_DEVELOPER:
		reach.Role = store.RoleDeveloper
	case dockwellv1.TokenRole_TOKEN_ROLE_VIEWER:
		reach.Role = store.RoleViewer
	case dockwellv1.TokenRole_TOKEN_ROLE_UNSPECIFIED:
	}
	if m.GetScope() == dockwellv1.TokenScope_TOKEN_SCOPE_DEPLOY {
		if len(m.GetProjects()) != 0 || m.GetRole() != dockwellv1.TokenRole_TOKEN_ROLE_UNSPECIFIED || len(grants) != 0 {
			return nil, Invalid(errors.New("the app sets a deploy token's project, environment and role"))
		}
		reach, err = h.deployTokenReach(ctx, m.GetDeployService())
	} else {
		if m.GetDeployService() != "" {
			return nil, Invalid(errors.New("choose deploy scope to bind a token to one app"))
		}
		reach.Projects, err = h.projectIDs(ctx, p, m.GetProjects())
	}
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	client, _ := ClientFrom(ctx)
	made, err := h.auth.CreateTokenWithin(ctx, client, p, m.GetName(), scope, grants, expires, reach, m.GetProof())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	out, err := h.tokenProtos(ctx, []store.APIToken{made.Token})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.CreateTokenResponse{Token: out[0], Secret: made.Secret}), nil
}

func (h *Handler) deployTokenReach(ctx context.Context, id string) (auth.TokenReach, error) {
	var reach auth.TokenReach
	err := h.auth.View(ctx, func(tx *store.Tx) error {
		ps, err := place.Of(ctx, tx, id)
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, place.ErrUnknown) ||
			err == nil && (len(ps) != 1 || ps[0].Environment == "" || ps[0].Project == "") {
			return fmt.Errorf("%w: choose an app you can deploy in this environment", auth.ErrInvalid)
		}
		if err != nil {
			return err
		}
		reach = auth.TokenReach{DeployService: id, DeployEnvironment: ps[0].Environment, Projects: []string{ps[0].Project}, Role: store.RoleDeveloper}
		return nil
	})
	return reach, err
}

// projectIDs are the projects refs name, each by its ID or its name. One that
// names nothing, or one p holds nothing in, is refused in the same words, as
// they're typed: what's out of reach answers as missing does (ADR 0049, ADR
// 0071). The auth service checks again as it makes the token.
func (h *Handler) projectIDs(ctx context.Context, p auth.Principal, refs []string) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	held, err := h.auth.Grants(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	err = h.auth.View(ctx, func(tx *store.Tx) error {
		for _, ref := range refs {
			ref = strings.TrimSpace(ref)
			projectID, err := place.Project(ctx, tx, ref)
			if errors.Is(err, store.ErrNotFound) || err == nil && place.Holds(held, place.Place{Project: projectID}) == "" {
				return fmt.Errorf("%w: there's no project %s. Name one you hold, by its name or its ID", auth.ErrInvalid, ref)
			}
			if err != nil {
				return err
			}
			out = append(out, projectID)
		}
		return nil
	})
	return out, err
}

// ListTokens lists the signed-in user's API tokens that still work.
func (h *Handler) ListTokens(ctx context.Context, _ *connect.Request[dockwellv1.ListTokensRequest]) (*connect.Response[dockwellv1.ListTokensResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	all, err := h.auth.ListTokens(ctx, p)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	out, err := h.tokenProtos(ctx, all)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.ListTokensResponse{Tokens: out}), nil
}

// RevokeToken stops one of the signed-in user's API tokens working.
func (h *Handler) RevokeToken(ctx context.Context, req *connect.Request[dockwellv1.RevokeTokenRequest]) (*connect.Response[dockwellv1.RevokeTokenResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.auth.RevokeToken(ctx, p, req.Msg.GetId()); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.RevokeTokenResponse{}), nil
}

// tokenProtos are tokens as the API shows them, each project they act in with
// its name as it is now, or none once it's deleted: its row stays, without a spec.
func (h *Handler) tokenProtos(ctx context.Context, tokens []store.APIToken) ([]*dockwellv1.Token, error) {
	out := make([]*dockwellv1.Token, 0, len(tokens))
	names := map[string]string{}
	err := h.auth.View(ctx, func(tx *store.Tx) error {
		for i := range tokens {
			t := tokenProto(&tokens[i])
			for _, projectID := range tokens[i].Projects {
				name, ok := names[projectID]
				if !ok {
					r, err := tx.GetResourceByID(ctx, projectID)
					switch {
					case errors.Is(err, store.ErrNotFound):
					case err != nil:
						return err
					case r.Spec != nil:
						name = r.Name
					}
					names[projectID] = name
				}
				t.Projects = append(t.Projects, &dockwellv1.TokenProject{Id: projectID, Name: name})
			}
			out = append(out, t)
		}
		return nil
	})
	return out, err
}

func tokenProto(t *store.APIToken) *dockwellv1.Token {
	out := &dockwellv1.Token{
		Id:          t.ID,
		Name:        t.Name,
		Hint:        t.Hint,
		CreateTime:  timestamppb.New(t.CreatedAt),
		LastAddress: t.LastIP,
	}
	switch t.Scope {
	case store.ScopeReadOnly:
		out.Scope = dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY
	case store.ScopeReadWrite:
		out.Scope = dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE
	}
	if t.DeployService != "" {
		out.Scope = dockwellv1.TokenScope_TOKEN_SCOPE_DEPLOY
		out.DeployService, out.DeployEnvironment = t.DeployService, t.DeployEnvironment
	}
	if t.HasGrant(store.GrantLogs) {
		out.Grants = append(out.Grants, dockwellv1.TokenGrant_TOKEN_GRANT_LOGS)
	}
	switch t.Role {
	case store.RoleDeveloper:
		out.Role = dockwellv1.TokenRole_TOKEN_ROLE_DEVELOPER
	case store.RoleViewer:
		out.Role = dockwellv1.TokenRole_TOKEN_ROLE_VIEWER
	}
	if t.ExpiresAt != nil {
		out.ExpireTime = timestamppb.New(*t.ExpiresAt)
	}
	if t.LastUsedAt != nil {
		out.LastUsedTime = timestamppb.New(*t.LastUsedAt)
	}
	return out
}
