// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/store"
)

// Errors for a change of profile (#801).
var (
	// ErrEmailInvited is an email change to an address an open invite is for: whoever
	// accepts it would be refused, as the email signs in here then.
	ErrEmailInvited = errors.New("an open invite is for that email")
	// ErrEmailLookalike is an email that reads the same as one someone else here
	// signs in with or is invited as, which would pass for them (#801's review, M1).
	ErrEmailLookalike = errors.New("that email reads the same as someone else's")
	// ErrNameTaken is a name that reads the same as someone else's here, as a copy
	// of theirs would read in Activity, Members and an approval (#801's review, M1).
	ErrNameTaken = errors.New("someone else here goes by that name")
)

// UpdateProfile changes the person's own name, email, or time zone, where set
// (#801, #834). Every change takes a signed-in session, as no API token acts as
// who someone is. An email names someone for signing in with an authenticator
// code and to dockwell recover, so it also takes a proof for StepUpChangeEmail on
// the new email each time, which no hour of cover stands in for. The proof comes
// before anything is said of the new email, so nobody learns who signs in here,
// or who is invited, without their passkey.
//
// A name or an email that reads the same as another person's here, by skeleton,
// is refused, so nobody passes for someone else. Sessions, passkeys, the
// authenticator app, recovery codes and provider links stay as they are: none of
// them finds the person by email. Each change is audited with what it was and
// what it is, and is on Activity for admins.
func (s *Service) UpdateProfile(ctx context.Context, client Client, p Principal, name, email, timeZone *string, proof string) (store.User, error) {
	if p.Token != nil || p.Session.ID == "" {
		return store.User{}, ErrNeedsSession
	}
	var newName, newEmail, newZone string
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
	if timeZone != nil {
		z, err := normalizeTimeZone(*timeZone)
		if err != nil {
			return store.User{}, err
		}
		newZone = z
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
			if err := s.changeName(ctx, tx, client, p, &user, newName); err != nil {
				return err
			}
		}
		if timeZone != nil && newZone != user.TimeZone {
			if err := s.changeTimeZone(ctx, tx, client, p, &user, newZone); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return store.User{}, err
	}
	return user, nil
}

// normalizeTimeZone checks an IANA time zone name as SettingsService.SetTimeZone
// does: empty clears it back to the browser's own, and "Local" or paths are refused.
func normalizeTimeZone(zone string) (string, error) {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return "", nil
	}
	// Go's time.LoadLocation accepts "Local" and paths into the host's filesystem;
	// neither is an IANA zone name.
	if strings.EqualFold(zone, "local") || strings.HasPrefix(zone, "/") {
		return "", fault.New(ErrInvalid, "%q is not a time zone: use an IANA name such as America/Los_Angeles or UTC", zone)
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return "", fault.New(ErrInvalid, "%q is not a time zone: use an IANA name such as America/Los_Angeles or UTC", zone)
	}
	return zone, nil
}

// changeTimeZone is UpdateProfile's change of time zone, in its transaction (#834).
func (s *Service) changeTimeZone(ctx context.Context, tx *store.Tx, client Client, p Principal, user *store.User, zone string) error {
	if err := tx.SetUserTimeZone(ctx, user.ID, zone); err != nil {
		return err
	}
	if err := s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), ActionAccountTimeZone, user.ID, AccountDetail(user.TimeZone, zone)); err != nil {
		return err
	}
	user.TimeZone = zone
	return nil
}

// changeName is UpdateProfile's change of name, in its transaction.
func (s *Service) changeName(ctx context.Context, tx *store.Tx, client Client, p Principal, user *store.User, name string) error {
	if err := nameFree(ctx, tx, user.ID, name); err != nil {
		return err
	}
	if err := tx.SetUserName(ctx, user.ID, name); err != nil {
		return err
	}
	if err := s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), ActionAccountName, user.ID, AccountDetail(user.Name, name)); err != nil {
		return err
	}
	user.Name = name
	return nil
}

// nameFree refuses a name that reads the same as another active member's, by
// skeleton, for anyone but the person with userID: one choosing a name at an
// invite, or changing theirs (#801's review, M1).
func nameFree(ctx context.Context, tx *store.Tx, userID, name string) error {
	others, err := tx.ListUsers(ctx)
	if err != nil {
		return err
	}
	looks := skeleton(name)
	for i := range others {
		if others[i].ID != userID && skeleton(others[i].Name) == looks {
			return fault.New(ErrNameTaken, "%s goes by a name that reads as %s here, so the two of you would be hard to tell apart. Choose another name", others[i].Name, name)
		}
	}
	return nil
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
	// One that only reads the same, as with a Cyrillic letter, would pass for theirs.
	looks := skeleton(email)
	others, err := tx.ListUsers(ctx)
	if err != nil {
		return err
	}
	for i := range others {
		if others[i].ID != user.ID && skeleton(others[i].Email) == looks {
			return fault.New(ErrEmailLookalike, "%s reads the same as an email someone else here signs in with, so it would pass for theirs. Choose another email", email)
		}
	}
	for i := range open {
		if skeleton(open[i].Email) == looks {
			return fault.New(ErrEmailLookalike, "%s reads the same as an email an open invite is for, so it would pass for theirs. Choose another email", email)
		}
	}
	if err := tx.SetUserEmail(ctx, user.ID, email); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return fault.New(ErrMemberExists, "someone else signs in here as %s, so it can't be yours too. Choose another email", email)
		}
		return err
	}
	if err := s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), ActionAccountEmail, user.ID, AccountDetail(user.Email, email)); err != nil {
		return err
	}
	user.Email = email
	return nil
}

// The audit log's words for a change of profile, which Activity shows admins too
// (#801's review, M1; #834).
const (
	ActionAccountName     = "account.name"
	ActionAccountEmail    = "account.email"
	ActionAccountTimeZone = "account.time_zone"
)

// AccountDetail is how a change of profile is recorded: what it was and what it is.
func AccountDetail(was, is string) string { return fmt.Sprintf("from %q to %q", was, is) }

// ReadAccountDetail reads what AccountDetail wrote, as Activity shows it.
func ReadAccountDetail(detail string) (was, is string, ok bool) {
	if _, err := fmt.Sscanf(detail, "from %q to %q", &was, &is); err != nil {
		return "", "", false
	}
	return was, is, true
}
