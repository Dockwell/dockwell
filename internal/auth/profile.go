// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/store"
)

// ErrEmailInvited is an email change to an address an open invite is for: whoever
// accepts it would be refused, as the email signs in here then.
var ErrEmailInvited = errors.New("an open invite is for that email")

// UpdateProfile changes the person's own name, email, or both, where name or
// email is set (#801). A name changes with no step-up, from a session or a token
// that may make changes. An email names someone for signing in with an
// authenticator code and to dockwell recover, so it changes only from a session,
// with a proof for StepUpChangeEmail on the new email each time, which no hour of
// cover stands in for. The proof comes before anything is said of the new email,
// so nobody learns who signs in here, or who is invited, without their passkey.
//
// Sessions, passkeys, the authenticator app, recovery codes and provider links
// stay as they are: none of them finds the person by email. Each change is
// audited with what it was and what it is.
func (s *Service) UpdateProfile(ctx context.Context, client Client, p Principal, name, email *string, proof string) (store.User, error) {
	var newName, newEmail string
	if name != nil {
		n, err := normalizeName(*name)
		if err != nil {
			return store.User{}, err
		}
		newName = n
	}
	if email != nil {
		e, err := normalizeEmail(*email)
		if err != nil {
			return store.User{}, err
		}
		newEmail = e
	}
	var user store.User
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		if user, err = tx.GetUser(ctx, p.User.ID); err != nil {
			return err
		}
		if user.RemovedAt != nil {
			return ErrUnauthenticated
		}
		if newEmail != "" && newEmail != user.Email {
			if err := s.changeEmail(ctx, tx, client, p, &user, newEmail, proof); err != nil {
				return err
			}
		}
		if newName != "" && newName != user.Name {
			if err := tx.SetUserName(ctx, user.ID, newName); err != nil {
				return err
			}
			if err := s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), "account.name", user.ID,
				fmt.Sprintf("from %q to %q", user.Name, newName)); err != nil {
				return err
			}
			user.Name = newName
		}
		return nil
	})
	if err != nil {
		return store.User{}, err
	}
	return user, nil
}

// changeEmail is UpdateProfile's change of email, in its transaction.
func (s *Service) changeEmail(ctx context.Context, tx *store.Tx, client Client, p Principal, user *store.User, email, proof string) error {
	if err := s.UseStepUpIn(ctx, tx, client, p, proof, StepUpChangeEmail, email); err != nil {
		return err
	}
	if _, err := tx.GetUserByEmail(ctx, email); err == nil {
		return fault.New(ErrMemberExists, "someone else signs in here as %s, so it can't be yours too. Choose another email", email)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	open, err := tx.ListOpenInvites(ctx, s.now())
	if err != nil {
		return err
	}
	for i := range open {
		if open[i].Email == email {
			return fault.New(ErrEmailInvited, "an open invite is for %s, so it can't be yours until that invite is accepted, revoked or expires. Choose another email, or ask an admin to revoke the invite in Members", email)
		}
	}
	if err := tx.SetUserEmail(ctx, user.ID, email); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return fault.New(ErrMemberExists, "someone else signs in here as %s, so it can't be yours too. Choose another email", email)
		}
		return err
	}
	if err := s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), "account.email", user.ID,
		fmt.Sprintf("from %s to %s", user.Email, email)); err != nil {
		return err
	}
	user.Email = email
	return nil
}
