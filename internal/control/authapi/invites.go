// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

// ReasonSignedIn says an invite was opened in a browser someone is signed in on.
const ReasonSignedIn = "signed_in"

// RoleName is the store's name of a role the API names, or "" for none.
func RoleName(r dockwellv1.Role) string {
	switch r {
	case dockwellv1.Role_ROLE_OWNER:
		return store.RoleOwner
	case dockwellv1.Role_ROLE_ADMIN:
		return store.RoleAdmin
	case dockwellv1.Role_ROLE_DEVELOPER:
		return store.RoleDeveloper
	case dockwellv1.Role_ROLE_VIEWER:
		return store.RoleViewer
	case dockwellv1.Role_ROLE_UNSPECIFIED:
	}
	return ""
}

// notSignedIn refuses accepting an invite in a browser someone is signed in
// on: accepting only ever makes a new person (ADR 0049).
func notSignedIn(ctx context.Context) error {
	if p, ok := PrincipalFrom(ctx); ok {
		return ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("you're signed in as %s. Sign out, or open the link in a private window", p.User.Name), ReasonSignedIn)
	}
	return nil
}

// CheckInvite reads an invite without spending it.
func (h *Handler) CheckInvite(ctx context.Context, req *connect.Request[dockwellv1.CheckInviteRequest]) (*connect.Response[dockwellv1.CheckInviteResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	info, err := h.auth.CheckInvite(ctx, client, req.Msg.GetSecret())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.CheckInviteResponse{
		Email: info.Email, Role: RoleProto(info.Role), ProjectName: info.ProjectName, ProjectId: info.ProjectID, InvitedBy: info.InvitedBy,
		ExpireTime: timestamppb.New(info.ExpiresAt),
	}), nil
}

// BeginAcceptInvite starts creating the invited person's passkey.
func (h *Handler) BeginAcceptInvite(ctx context.Context, req *connect.Request[dockwellv1.BeginAcceptInviteRequest]) (*connect.Response[dockwellv1.BeginAcceptInviteResponse], error) {
	if err := notSignedIn(ctx); err != nil {
		return nil, err
	}
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	cer, err := h.auth.BeginAcceptInvite(ctx, client, req.Msg.GetSecret(), req.Msg.GetName())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.BeginAcceptInviteResponse{CeremonyId: cer.ID, OptionsJson: string(cer.Options)}), nil
}

// FinishAcceptInvite spends the invite, makes the person and signs them in.
func (h *Handler) FinishAcceptInvite(ctx context.Context, req *connect.Request[dockwellv1.FinishAcceptInviteRequest]) (*connect.Response[dockwellv1.FinishAcceptInviteResponse], error) {
	if err := notSignedIn(ctx); err != nil {
		return nil, err
	}
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	signedIn, err := h.auth.FinishAcceptInvite(ctx, client, m.GetCeremonyId(), m.GetSecret(), []byte(m.GetCredentialJson()), m.GetPasskeyName())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return withCookie(connect.NewResponse(&dockwellv1.FinishAcceptInviteResponse{User: userProto(signedIn.User)}), client.Site, signedIn)
}
