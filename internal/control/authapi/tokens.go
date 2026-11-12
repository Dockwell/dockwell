// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
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
	case dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE:
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
	client, _ := ClientFrom(ctx)
	made, err := h.auth.CreateToken(ctx, client, p, m.GetName(), scope, grants, expires, m.GetProof())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.CreateTokenResponse{Token: tokenProto(&made.Token), Secret: made.Secret}), nil
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
	out := make([]*dockwellv1.Token, 0, len(all))
	for i := range all {
		out = append(out, tokenProto(&all[i]))
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
	if t.HasGrant(store.GrantLogs) {
		out.Grants = append(out.Grants, dockwellv1.TokenGrant_TOKEN_GRANT_LOGS)
	}
	if t.ExpiresAt != nil {
		out.ExpireTime = timestamppb.New(*t.ExpiresAt)
	}
	if t.LastUsedAt != nil {
		out.LastUsedTime = timestamppb.New(*t.LastUsedAt)
	}
	return out
}
