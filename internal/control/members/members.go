// SPDX-License-Identifier: AGPL-3.0-only

// Package members serves dockwell.v1.MemberService: who can sign in, with their
// roles and how they sign in, and the invites that ask others to (ADR 0048,
// ADR 0049).
package members

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/store"
)

// Handler implements MemberService.
type Handler struct {
	db     *store.DB
	people *auth.Service
	logger *slog.Logger
	now    func() time.Time
}

var _ dockwellv1connect.MemberServiceHandler = (*Handler)(nil)

// New returns a handler that reads who can sign in from db, and how each can
// as people decides it.
func New(db *store.DB, people *auth.Service, logger *slog.Logger) *Handler {
	return &Handler{db: db, people: people, logger: logger, now: time.Now}
}

// ListMembers returns everyone who can sign in, oldest first.
func (h *Handler) ListMembers(ctx context.Context, _ *connect.Request[dockwellv1.ListMembersRequest]) (*connect.Response[dockwellv1.ListMembersResponse], error) {
	p, _ := authapi.PrincipalFrom(ctx)
	out := &dockwellv1.ListMembersResponse{}
	now := h.now()
	err := h.db.View(ctx, func(tx *store.Tx) error {
		users, err := tx.ListUsers(ctx)
		if err != nil {
			return err
		}
		for i := range users {
			u := &users[i]
			in, err := h.people.SignInOf(ctx, tx, u)
			if err != nil {
				return err
			}
			sessions, err := tx.ListActiveSessions(ctx, u.ID, now)
			if err != nil {
				return err
			}
			m := member(u, in, sessions, u.ID == p.User.ID)
			if m.Grants, err = projectGrants(ctx, tx, u.ID); err != nil {
				return err
			}
			out.Members = append(out.Members, m)
		}
		invites, err := tx.ListOpenInvites(ctx, now)
		if err != nil {
			return err
		}
		for i := range invites {
			out.Invites = append(out.Invites, h.inviteProto(ctx, tx, &invites[i]))
		}
		return nil
	})
	if err != nil {
		h.logger.Error("can't list who signs in", slog.String("error", err.Error()))
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("can't read who signs in from the control plane's store. Try again in a moment, and if it keeps failing, check the control plane's log"))
	}
	return connect.NewResponse(out), nil
}

// member is a person as the members list has them: for everyone, who they are,
// the kinds of sign-in they have, and when they were last active; for the
// person asking, how many passkeys and where, their codes and their browsers.
// How someone else signs in, in detail, is theirs (ADR 0048).
func member(u *store.User, in auth.SignIn, sessions []store.Session, you bool) *dockwellv1.Member {
	m := &dockwellv1.Member{User: authapi.UserProto(*u), You: you}
	if in.Passkeys > 0 {
		m.SignIn = append(m.SignIn, dockwellv1.SignInKind_SIGN_IN_KIND_PASSKEY)
	}
	if in.Codes {
		m.SignIn = append(m.SignIn, dockwellv1.SignInKind_SIGN_IN_KIND_AUTHENTICATOR_APP)
	}
	var last time.Time
	for i := range sessions {
		if sessions[i].LastSeenAt.After(last) {
			last = sessions[i].LastSeenAt
		}
	}
	if !last.IsZero() {
		m.LastActiveTime = timestamppb.New(last)
	}
	if !you {
		return m
	}
	m.Passkeys = int32(in.Passkeys) //nolint:gosec // a person's passkeys
	m.PasskeyAddresses = in.Addresses
	m.Authenticator, m.CodesLocked = in.Codes, in.CodesLocked
	m.RecoveryCodesLeft = int32(in.RecoveryCodesLeft) //nolint:gosec // ten at most
	m.Sessions = int32(len(sessions))                 //nolint:gosec // a person's browsers
	return m
}

// projectGrants are the roles someone holds in projects, by project name.
func projectGrants(ctx context.Context, tx *store.Tx, userID string) ([]*dockwellv1.MemberGrant, error) {
	grants, err := tx.Grants(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []*dockwellv1.MemberGrant
	for _, g := range grants {
		if g.ProjectID == store.Install {
			continue
		}
		out = append(out, &dockwellv1.MemberGrant{ProjectId: g.ProjectID, ProjectName: projectName(ctx, tx, g.ProjectID), Role: authapi.RoleProto(g.Role)})
	}
	return out, nil
}

// projectName is a project's name, or "" for the install or one that's gone.
func projectName(ctx context.Context, tx *store.Tx, projectID string) string {
	if projectID == store.Install {
		return ""
	}
	r, err := tx.GetResourceByID(ctx, projectID)
	if err != nil {
		return ""
	}
	return r.Name
}

func (h *Handler) inviteProto(ctx context.Context, tx *store.Tx, inv *store.Invite) *dockwellv1.Invite {
	return &dockwellv1.Invite{
		Id: inv.ID, Email: inv.Email, Role: authapi.RoleProto(inv.Role), ProjectId: inv.ProjectID,
		ProjectName: projectName(ctx, tx, inv.ProjectID), InvitedBy: h.people.ActorName(ctx, tx, inv.CreatedBy),
		CreateTime: timestamppb.New(inv.CreatedAt), ExpireTime: timestamppb.New(inv.ExpiresAt),
	}
}

// CreateInvite invites someone by email to sign in with a role in a place.
func (h *Handler) CreateInvite(ctx context.Context, req *connect.Request[dockwellv1.CreateInviteRequest]) (*connect.Response[dockwellv1.CreateInviteResponse], error) {
	p, _ := authapi.PrincipalFrom(ctx)
	client, _ := authapi.ClientFrom(ctx)
	m := req.Msg
	projectID := store.Install
	if ref := m.GetProject(); ref != "" {
		err := h.db.View(ctx, func(tx *store.Tx) error {
			var err error
			projectID, err = place.Project(ctx, tx, ref)
			return err
		})
		if errors.Is(err, store.ErrNotFound) {
			return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no project %s. Check its name, or leave project out to invite them to the whole install", ref), "not_found")
		}
		if err != nil {
			return nil, authapi.AuthError(ctx, h.logger, err)
		}
	}
	made, err := h.people.CreateInvite(ctx, client, p, m.GetEmail(), authapi.RoleName(m.GetRole()), projectID, m.GetProof())
	if err != nil {
		return nil, authapi.AuthError(ctx, h.logger, err)
	}
	res := &dockwellv1.CreateInviteResponse{Link: made.Link}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		res.Invite = h.inviteProto(ctx, tx, &made.Invite)
		return nil
	})
	if err != nil {
		return nil, authapi.AuthError(ctx, h.logger, err)
	}
	return connect.NewResponse(res), nil
}

// RevokeInvite stops an open invite working.
func (h *Handler) RevokeInvite(ctx context.Context, req *connect.Request[dockwellv1.RevokeInviteRequest]) (*connect.Response[dockwellv1.RevokeInviteResponse], error) {
	p, _ := authapi.PrincipalFrom(ctx)
	client, _ := authapi.ClientFrom(ctx)
	err := h.people.RevokeInvite(ctx, client, p, req.Msg.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, authapi.ReasonError(connect.CodeNotFound,
			fmt.Errorf("there's no open invite %s: it was accepted, revoked or never made. Read Members again", req.Msg.GetId()), "not_found")
	}
	if err != nil {
		return nil, authapi.AuthError(ctx, h.logger, err)
	}
	return connect.NewResponse(&dockwellv1.RevokeInviteResponse{}), nil
}
