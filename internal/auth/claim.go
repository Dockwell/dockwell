// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// Link is a one-time link's token and when it expires.
type Link struct {
	Token     string
	ExpiresAt time.Time
}

// CreateClaimLink makes a claim link that creates the owner, replacing any
// earlier one. It fails with ErrClaimed once the install has an owner.
// actor names who asked, for the audit log.
func (s *Service) CreateClaimLink(ctx context.Context, actor string) (Link, error) {
	token, hash := newToken()
	now := s.now()
	l := store.LinkToken{
		ID:        id.New(id.Link),
		Purpose:   store.LinkClaim,
		TokenHash: hash,
		CreatedAt: now,
		CreatedBy: actor,
		ExpiresAt: now.Add(ClaimLinkTTL),
	}
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		n, err := tx.CountUsers(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrClaimed
		}
		if err := tx.CreateLinkToken(ctx, l); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, "claim_link.create", l.ID, "expires "+l.ExpiresAt.UTC().Format(time.RFC3339))
	})
	if err != nil {
		return Link{}, err
	}
	return Link{Token: token, ExpiresAt: l.ExpiresAt}, nil
}

// WaitingClaimLink reports when the newest claim link that still works
// expires, if there is one. The link itself can't be shown again: only its
// hash is kept.
func (s *Service) WaitingClaimLink(ctx context.Context) (time.Time, bool, error) {
	var l store.LinkToken
	err := s.db.View(ctx, func(tx *store.Tx) (err error) {
		l, err = tx.LiveLinkToken(ctx, store.LinkClaim, "", s.now())
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return l.ExpiresAt, true, nil
}

// CreateSignInLink makes a one-time link that signs in as the user with
// email, or as the owner when email is empty, replacing any earlier link
// for them. It is for dockwell recover, which needs local access to the
// data directory: whoever has that already controls the install.
func (s *Service) CreateSignInLink(ctx context.Context, email, actor string) (Link, store.User, error) {
	token, hash := newToken()
	now := s.now()
	var user store.User
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		if email == "" {
			user, err = tx.Owner(ctx)
			if errors.Is(err, store.ErrNotFound) {
				return errors.New("this install has no owner yet. Claim it with the link from dockwell claim")
			}
		} else {
			e, nerr := normalizeEmail(email)
			if nerr != nil {
				return nerr
			}
			user, err = tx.GetUserByEmail(ctx, e)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%w: nobody signs in as %s here. Leave out --user to sign in as the owner", ErrNotFound, e)
			}
		}
		if err != nil {
			return err
		}
		l := store.LinkToken{
			ID:        id.New(id.Link),
			Purpose:   store.LinkRecover,
			TokenHash: hash,
			UserID:    user.ID,
			CreatedAt: now,
			CreatedBy: actor,
			ExpiresAt: now.Add(SignInLinkTTL),
		}
		if err := tx.CreateLinkToken(ctx, l); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, "sign_in_link.create", user.ID, l.ID+" expires "+l.ExpiresAt.UTC().Format(time.RFC3339))
	})
	if err != nil {
		return Link{}, store.User{}, err
	}
	return Link{Token: token, ExpiresAt: now.Add(SignInLinkTTL)}, user, nil
}

// usableLink returns the link for token if it is for purpose and still
// works. Any failure is one error, rejected.
func usableLink(ctx context.Context, tx *store.Tx, purpose, token string, now time.Time, rejected error) (store.LinkToken, error) {
	if token == "" {
		return store.LinkToken{}, rejected
	}
	l, err := tx.GetLinkTokenByHash(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return store.LinkToken{}, rejected
	}
	if err != nil {
		return store.LinkToken{}, err
	}
	if subtle.ConstantTimeCompare(l.TokenHash, hashToken(token)) != 1 || l.Purpose != purpose ||
		l.UsedAt != nil || l.RevokedAt != nil || !now.Before(l.ExpiresAt) {
		return store.LinkToken{}, rejected
	}
	return l, nil
}

// CheckClaim reports when a claim link expires, without spending it.
func (s *Service) CheckClaim(ctx context.Context, client Client, token string) (time.Time, error) {
	if !s.begins.take(sourceKey(client.Addr), s.now()) {
		return time.Time{}, ErrTooManyRequests
	}
	return s.checkClaim(ctx, client, token)
}

func (s *Service) checkClaim(ctx context.Context, client Client, token string) (time.Time, error) {
	var expires time.Time
	err := s.db.View(ctx, func(tx *store.Tx) error {
		n, err := tx.CountUsers(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrClaimed
		}
		l, err := usableLink(ctx, tx, store.LinkClaim, token, s.now(), ErrClaimLinkRejected)
		expires = l.ExpiresAt
		return err
	})
	if errors.Is(err, ErrClaimLinkRejected) {
		s.logger.InfoContext(ctx, "refused a claim link", slog.String("from", client.Addr))
	}
	return expires, err
}

// Ceremony is a started passkey ceremony: the options to hand to the
// browser, as WebAuthn JSON, and the ID to finish it with.
type Ceremony struct {
	ID      string
	Options []byte
}

// BeginClaim starts creating the owner's first passkey. The claim link must
// still work; it is spent only by FinishClaim.
func (s *Service) BeginClaim(ctx context.Context, client Client, token, name, email string) (Ceremony, error) {
	now := s.now()
	if !s.begins.take(sourceKey(client.Addr), now) {
		return Ceremony{}, ErrTooManyRequests
	}
	rp, err := s.relyingParty(client.Site)
	if err != nil {
		return Ceremony{}, err
	}
	name, err = normalizeName(name)
	if err != nil {
		return Ceremony{}, err
	}
	email, err = normalizeEmail(email)
	if err != nil {
		return Ceremony{}, err
	}
	if _, err := s.checkClaim(ctx, client, token); err != nil {
		return Ceremony{}, err
	}
	handle := make([]byte, handleSize)
	_, _ = rand.Read(handle)
	owner := store.User{
		ID:             id.New(id.User),
		Email:          email,
		Name:           name,
		Role:           store.RoleOwner,
		WebAuthnHandle: handle,
		CreatedAt:      now,
	}
	creation, session, err := rp.BeginRegistration(&waUser{user: owner}, webauthn.WithRegistrationOrigin(client.Site.Origin))
	if err != nil {
		return Ceremony{}, fmt.Errorf("can't start creating a passkey: %w", err)
	}
	options, err := json.Marshal(creation.Response)
	if err != nil {
		return Ceremony{}, err
	}
	key := s.ceremonies.put(&ceremony{
		kind:      ceremonyClaim,
		site:      client.Site,
		session:   *session,
		expires:   now.Add(ceremonyTTL),
		user:      owner,
		claimHash: hashToken(token),
	}, now)
	return Ceremony{ID: key, Options: options}, nil
}

// FinishClaim checks the new passkey, then in one transaction spends the
// claim link, creates the owner with the passkey, and signs them in. Two
// claims racing can't both succeed: the link is spent with a conditional
// update, and the schema allows one owner.
func (s *Service) FinishClaim(ctx context.Context, client Client, ceremonyID, token string, credential []byte, passkeyName string) (SignedIn, error) {
	now := s.now()
	cer, err := s.ceremonies.take(ceremonyID, ceremonyClaim, now)
	if err != nil {
		return SignedIn{}, err
	}
	if cer.site != client.Site || subtle.ConstantTimeCompare(cer.claimHash, hashToken(token)) != 1 {
		return SignedIn{}, ErrCeremonyExpired
	}
	cred, err := s.verifyNewPasskey(client.Site, &waUser{user: cer.user}, cer.session, credential)
	if err != nil {
		return SignedIn{}, err
	}
	var signedIn SignedIn
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		n, err := tx.CountUsers(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrClaimed
		}
		link, err := usableLink(ctx, tx, store.LinkClaim, token, now, ErrClaimLinkRejected)
		if err != nil {
			return err
		}
		if err := tx.SpendLinkToken(ctx, link.ID, now); errors.Is(err, store.ErrNotFound) {
			return ErrClaimLinkRejected
		} else if err != nil {
			return err
		}
		if err := tx.CreateUser(ctx, cer.user); errors.Is(err, store.ErrConflict) {
			return ErrClaimed
		} else if err != nil {
			return err
		}
		pk, err := s.storePasskey(ctx, tx, cer.user, client.Site, cred, passkeyName)
		if err != nil {
			return err
		}
		actor := "user:" + cer.user.ID
		if err := s.audit(ctx, tx, actor, "owner.claim", cer.user.ID,
			fmt.Sprintf("%s claimed the install with %s from %s on %s", cer.user.Email, link.ID, client.remoteIP(), client.Site.RPID)); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, actor, "passkey.add", cer.user.ID, pk.ID+" for "+pk.RPID); err != nil {
			return err
		}
		signedIn, err = s.startSession(ctx, tx, cer.user, client, MethodClaim)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrClaimLinkRejected) || errors.Is(err, ErrClaimed) {
			s.logger.WarnContext(ctx, "refused a claim", slog.String("from", client.Addr), slog.Any("error", err))
		}
		return SignedIn{}, err
	}
	s.logger.InfoContext(ctx, "the install has an owner", slog.String("user", signedIn.User.ID), slog.String("email", signedIn.User.Email))
	return signedIn, nil
}

// SignInWithLink spends a link from dockwell recover and signs in.
func (s *Service) SignInWithLink(ctx context.Context, client Client, token string) (SignedIn, error) {
	now := s.now()
	if !s.begins.take(sourceKey(client.Addr), now) {
		return SignedIn{}, ErrTooManyRequests
	}
	if !client.Site.CanSignIn() {
		return SignedIn{}, ErrNeedsHostname
	}
	var signedIn SignedIn
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		link, err := usableLink(ctx, tx, store.LinkRecover, token, now, ErrSignInLinkRejected)
		if err != nil {
			return err
		}
		if err := tx.SpendLinkToken(ctx, link.ID, now); errors.Is(err, store.ErrNotFound) {
			return ErrSignInLinkRejected
		} else if err != nil {
			return err
		}
		user, err := tx.GetUser(ctx, link.UserID)
		if err != nil {
			return err
		}
		signedIn, err = s.startSession(ctx, tx, user, client, MethodLink)
		return err
	})
	if errors.Is(err, ErrSignInLinkRejected) {
		s.logger.InfoContext(ctx, "refused a sign-in link", slog.String("from", client.Addr))
	}
	return signedIn, err
}

// verifyNewPasskey checks a registration response against its ceremony.
func (s *Service) verifyNewPasskey(site Site, user *waUser, session webauthn.SessionData, credential []byte) (*webauthn.Credential, error) {
	rp, err := s.relyingParty(site)
	if err != nil {
		return nil, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(credential)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrPasskeyRejected, protocolDetail(err))
	}
	cred, err := rp.CreateCredential(user, session, parsed)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrPasskeyRejected, protocolDetail(err))
	}
	return cred, nil
}

// protocolDetail extracts go-webauthn's description of what failed, for logs
// and for the person whose browser sent it.
func protocolDetail(err error) string {
	var perr *protocol.Error
	if errors.As(err, &perr) && perr.Details != "" {
		return perr.Details
	}
	return err.Error()
}
