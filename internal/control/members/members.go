// SPDX-License-Identifier: AGPL-3.0-only

// Package members serves dockwell.v1.MemberService: who can sign in, with their
// role and how they sign in. Today that is the owner alone. See
// docs/adr/0048-members-as-far-as-one-owner-goes.md.
package members

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
)

// Handler implements MemberService.
type Handler struct {
	db     *store.DB
	logger *slog.Logger
	now    func() time.Time
}

var _ dockwellv1connect.MemberServiceHandler = (*Handler)(nil)

// New returns a handler that reads who can sign in from db.
func New(db *store.DB, logger *slog.Logger) *Handler {
	return &Handler{db: db, logger: logger, now: time.Now}
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
			m := &dockwellv1.Member{User: authapi.UserProto(*u), You: u.ID == p.User.ID, Authenticator: u.TOTPSecret != nil}
			passkeys, err := tx.ListPasskeys(ctx, u.ID)
			if err != nil {
				return err
			}
			for i := range passkeys {
				pk := &passkeys[i]
				if pk.CloneWarningAt != nil {
					continue // a copy never signs in again
				}
				m.Passkeys++
				if !slices.Contains(m.PasskeyAddresses, pk.RPID) {
					m.PasskeyAddresses = append(m.PasskeyAddresses, pk.RPID)
				}
			}
			if m.Authenticator {
				n, err := tx.CountRecoveryCodes(ctx, u.ID)
				if err != nil {
					return err
				}
				m.RecoveryCodesLeft = int32(n) //nolint:gosec // ten at most
			}
			sessions, err := tx.ListActiveSessions(ctx, u.ID, now)
			if err != nil {
				return err
			}
			m.Sessions = int32(len(sessions)) //nolint:gosec // a person's browsers
			var last time.Time
			for i := range sessions {
				if sessions[i].LastSeenAt.After(last) {
					last = sessions[i].LastSeenAt
				}
			}
			if !last.IsZero() {
				m.LastActiveTime = timestamppb.New(last)
			}
			out.Members = append(out.Members, m)
		}
		return nil
	})
	if err != nil {
		h.logger.Error("can't list who signs in", slog.String("error", err.Error()))
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("can't read who signs in from the control plane's store. Try again in a moment, and if it keeps failing, check the control plane's log"))
	}
	return connect.NewResponse(out), nil
}
