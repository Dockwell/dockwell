// SPDX-License-Identifier: AGPL-3.0-only

// Package members serves dockwell.v1.MemberService: who can sign in, with their
// role and how they sign in. Today that is the owner alone. See
// docs/adr/0048-members-as-far-as-one-owner-goes.md.
package members

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
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
			out.Members = append(out.Members, member(u, in, sessions, u.ID == p.User.ID))
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
