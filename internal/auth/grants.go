// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/dockwell/dockwell/internal/store"
)

// Grants returns what a principal may act with: a person's own grants, or for an
// API token, its maker's, as they stand now, so a token loses what its maker
// loses (ADR 0049).
func (s *Service) Grants(ctx context.Context, p Principal) ([]store.Grant, error) {
	var out []store.Grant
	err := s.db.View(ctx, func(tx *store.Tx) error {
		var err error
		out, err = tx.Grants(ctx, p.User.ID)
		return err
	})
	return out, err
}

// Highest is the highest role among grants, anywhere, or "" for none.
func Highest(grants []store.Grant) string {
	best := ""
	for _, g := range grants {
		if store.RoleRank(g.Role) > store.RoleRank(best) {
			best = g.Role
		}
	}
	return best
}

// errNoOwner says an install has no owner, and how to give it one: claim it
// when nobody signs in, or make someone who does the owner.
func errNoOwner(ctx context.Context, tx *store.Tx, what string) error {
	n, err := tx.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("this install has people but no owner, so %s. Make one of them the owner with dockwell recover --owner EMAIL", what)
	}
	return fmt.Errorf("this install has no owner yet, so %s. Claim it with the link from dockwell claim", what)
}

// MakeOwner makes the person with email the install's owner, for dockwell
// recover --owner on the control server: whoever can run that there controls
// the install already (ADR 0048). The owner before, if there was one, becomes
// an admin, and the CLI's local tokens that acted for them are revoked (ADR
// 0049).
func (s *Service) MakeOwner(ctx context.Context, email, actor string) (store.User, error) {
	e, err := normalizeEmail(email)
	if err != nil {
		return store.User{}, err
	}
	var user store.User
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		user, err = tx.GetUserByEmail(ctx, e)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: nobody signs in here as %s. Make the owner someone who already does", ErrNotFound, e)
		}
		if err != nil {
			return err
		}
		before, err := tx.Owner(ctx)
		switch {
		case err == nil && before.ID == user.ID:
			return nil
		case err == nil:
			if err := tx.RevokeLocalTokens(ctx, before.ID, s.now()); err != nil {
				return err
			}
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		if err := tx.TransferOwnership(ctx, user.ID, s.now(), actor); err != nil {
			return err
		}
		detail := "made " + user.Email + " the owner"
		if before.ID != "" {
			detail += ", in place of " + before.Email + ", who is an admin now"
		}
		return s.audit(ctx, tx, actor, "owner.transfer", user.ID, detail)
	})
	if err != nil {
		return store.User{}, err
	}
	user.Role = store.RoleOwner
	return user, nil
}

// RefuseRole audits a call refused because the caller's role, or a token's
// maker's, doesn't cover it: held is the highest role they hold and needs the
// lowest the call takes. It audits at most once an hour for each person or
// token and method, with a count of the rest, as refused tokens are, so a
// script calling in a loop doesn't write on every call (ADR 0049).
func (s *Service) RefuseRole(ctx context.Context, client Client, p Principal, call, held, needs string) {
	who, key := "the caller", "user:"+p.User.ID
	if p.Token != nil {
		who, key = "token "+p.Token.ID+"'s maker", "token:"+p.Token.ID
	}
	now := s.now()
	more, report := s.roleRefusals.count(key+" "+call, now)
	s.logger.InfoContext(ctx, "refused a call the caller's role doesn't cover", slog.String("actor", p.Actor()),
		slog.String("call", call), slog.String("holds", held), slog.String("needs", needs))
	if !report {
		return
	}
	holds := "holds no role"
	if held != "" {
		holds = "is " + aRole(held)
	}
	detail := fmt.Sprintf("%s, which needs %s, from %s: %s %s", call, aRole(needs), client.remoteIP(), who, holds)
	if more > 0 {
		detail += fmt.Sprintf(", and %d more refused since the last report", more)
	}
	if err := s.db.Update(ctx, func(tx *store.Tx) error {
		return s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), "role.refuse", p.User.ID, detail)
	}); err != nil {
		s.logger.WarnContext(ctx, "can't audit a call refused for its role", slog.String("actor", p.Actor()), slog.Any("error", err))
	}
}

// aRole names a role with its article: "a viewer", "an admin".
func aRole(role string) string {
	if role == store.RoleAdmin || role == store.RoleOwner {
		return "an " + role
	}
	return "a " + role
}

// View runs fn in a read transaction of the store, for checks the API makes of
// what a call names, such as where an object belongs.
func (s *Service) View(ctx context.Context, fn func(*store.Tx) error) error {
	return s.db.View(ctx, fn)
}
