// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

const maxPasskeyName = 64

// Passkey is a stored passkey, decoded.
type Passkey struct {
	store.Passkey
	Credential webauthn.Credential
}

// Synced reports whether the passkey can sync between devices.
func (p Passkey) Synced() bool { return p.Credential.Flags.BackupEligible }

func decodePasskey(p store.Passkey) (Passkey, error) {
	var c webauthn.Credential
	if err := json.Unmarshal(p.Record, &c); err != nil {
		return Passkey{}, fmt.Errorf("passkey %s has an unreadable record: %w", p.ID, err)
	}
	return Passkey{Passkey: p, Credential: c}, nil
}

func cleanPasskeyName(name string) string {
	n := strings.Join(strings.Fields(cleanText(name)), " ")
	if r := []rune(n); len(r) > maxPasskeyName {
		n = string(r[:maxPasskeyName])
	}
	return n
}

// storePasskey records a verified credential for user on site.
func (s *Service) storePasskey(ctx context.Context, tx *store.Tx, user store.User, site Site, cred *webauthn.Credential, name string) (store.Passkey, error) {
	record, err := json.Marshal(cred)
	if err != nil {
		return store.Passkey{}, err
	}
	pk := store.Passkey{
		ID:           id.New(id.Passkey),
		UserID:       user.ID,
		CredentialID: cred.ID,
		RPID:         site.RPID,
		Name:         cleanPasskeyName(name),
		Record:       record,
		CreatedAt:    s.now(),
	}
	if err := tx.CreatePasskey(ctx, pk); errors.Is(err, store.ErrConflict) {
		return store.Passkey{}, ErrPasskeyExists
	} else if err != nil {
		return store.Passkey{}, err
	}
	return pk, nil
}

// BeginSignIn starts signing in with a passkey. The options name no
// account: the browser offers the passkeys it holds for this address.
func (s *Service) BeginSignIn(ctx context.Context, client Client) (Ceremony, error) {
	now := s.now()
	if !s.begins.take(sourceKey(client.Addr), now) {
		return Ceremony{}, ErrTooManyRequests
	}
	rp, err := s.relyingParty(client.Site)
	if err != nil {
		return Ceremony{}, err
	}
	assertion, session, err := rp.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationRequired),
		webauthn.WithLoginOrigin(client.Site.Origin),
	)
	if err != nil {
		return Ceremony{}, fmt.Errorf("can't start a passkey sign-in: %w", err)
	}
	options, err := json.Marshal(assertion.Response)
	if err != nil {
		return Ceremony{}, err
	}
	key := s.ceremonies.put(&ceremony{kind: ceremonySignIn, site: client.Site, session: *session, expires: now.Add(ceremonyTTL)}, now)
	return Ceremony{ID: key, Options: options}, nil
}

// FinishSignIn checks a passkey's assertion and signs in. A passkey whose
// sign count went backwards is treated as copied: the sign-in is refused,
// the passkey is flagged and stops working, and the event is audited.
//
// Reading the passkey, checking the assertion against its stored count and
// storing the new count happen in one write transaction. Writes are
// serialized, so of two sign-ins racing with one count, from a copy and the
// original, the second sees the first's count and is refused as a copy, and
// the stored count never goes backwards. A passkey flagged or removed
// meanwhile doesn't sign in.
func (s *Service) FinishSignIn(ctx context.Context, client Client, ceremonyID string, credential []byte) (SignedIn, error) {
	now := s.now()
	cer, err := s.ceremonies.take(ceremonyID, ceremonySignIn, now)
	if err != nil {
		return SignedIn{}, err
	}
	if cer.site != client.Site {
		return SignedIn{}, ErrCeremonyExpired
	}
	rp, err := s.relyingParty(client.Site)
	if err != nil {
		return SignedIn{}, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(credential)
	if err != nil {
		s.logger.InfoContext(ctx, "refused a malformed passkey response", slog.String("from", client.Addr), slog.String("detail", protocolDetail(err)))
		return SignedIn{}, ErrPasskeyRejected
	}

	var (
		signedIn SignedIn
		refused  error  // why the sign-in was refused, after auditing it
		copied   string // the passkey being flagged as a copy, if any
	)
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		stored, err := tx.GetPasskeyByCredentialID(ctx, parsed.RawID)
		if err != nil {
			return err
		}
		pk, err := decodePasskey(stored)
		if err != nil {
			return err
		}
		user, err := tx.GetUser(ctx, stored.UserID)
		if err != nil {
			return err
		}
		refuse := func(reason string, why error) error {
			refused = why
			return s.audit(ctx, tx, client.anonymous(), "auth.sign_in_failed", user.ID,
				fmt.Sprintf("passkey %s on %s from %s: %s", pk.ID, client.Site.RPID, client.remoteIP(), reason))
		}
		flag := func(detail string) error {
			refused, copied = ErrPasskeyCopied, pk.ID
			if err := tx.FlagPasskeyClone(ctx, pk.ID, now); err != nil {
				return err
			}
			return s.audit(ctx, tx, client.anonymous(), "passkey.clone_suspected", user.ID, detail)
		}
		if pk.RPID != client.Site.RPID || !bytes.Equal(parsed.Response.UserHandle, user.WebAuthnHandle) {
			return refuse("made for another address or user", ErrPasskeyRejected)
		}
		if pk.CloneWarningAt != nil {
			return refuse("flagged as possibly copied", ErrPasskeyCopied)
		}
		// A copy was seen before, but flagging it failed: flag it now, and
		// refuse whichever copy this is.
		if detail, ok := s.unflagged.get(pk.ID); ok {
			return flag(detail)
		}
		handler := func(rawID, userHandle []byte) (webauthn.User, error) {
			if !bytes.Equal(rawID, pk.CredentialID) || !bytes.Equal(userHandle, user.WebAuthnHandle) {
				return nil, ErrPasskeyRejected
			}
			return &waUser{user: user, credentials: []webauthn.Credential{pk.Credential}}, nil
		}
		_, cred, err := rp.ValidatePasskeyLogin(handler, cer.session, parsed)
		if err != nil {
			return refuse(protocolDetail(err), ErrPasskeyRejected)
		}
		if cred.Authenticator.CloneWarning {
			detail := fmt.Sprintf("%s reported sign count %d after %d, from %s", pk.ID, parsed.Response.AuthenticatorData.Counter,
				pk.Credential.Authenticator.SignCount, client.remoteIP())
			s.unflagged.put(pk.ID, detail)
			return flag(detail)
		}
		record, err := json.Marshal(cred)
		if err != nil {
			return err
		}
		if err := tx.UsePasskey(ctx, pk.ID, record, now); err != nil {
			return err
		}
		signedIn, err = s.startSession(ctx, tx, user, client, MethodPasskey)
		return err
	})
	switch {
	case copied != "" && err != nil:
		// Refused all the same. The flag is tried again on the passkey's next
		// sign-in, whichever copy makes it.
		s.logger.ErrorContext(ctx, "can't flag a passkey that may have been copied; refusing it until it is flagged",
			slog.String("passkey", copied), slog.Any("error", err))
		return SignedIn{}, fmt.Errorf("can't flag passkey %s as possibly copied: %w", copied, err)
	case errors.Is(err, store.ErrNotFound):
		s.logger.InfoContext(ctx, "refused an unknown passkey", slog.String("from", client.Addr))
		return SignedIn{}, ErrPasskeyRejected
	case err != nil:
		return SignedIn{}, err
	case copied != "":
		s.unflagged.remove(copied)
		s.logger.WarnContext(ctx, "refused a passkey that may have been copied", slog.String("passkey", copied))
		return SignedIn{}, ErrPasskeyCopied
	case refused != nil:
		return SignedIn{}, refused
	}
	return signedIn, nil
}

// unflagged holds passkeys seen to be copied whose flag couldn't be stored,
// and the audit detail for when it is. They are refused until it is.
type unflagged struct {
	mu sync.Mutex
	m  map[string]string
}

func (u *unflagged) get(passkeyID string) (string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	d, ok := u.m[passkeyID]
	return d, ok
}

func (u *unflagged) put(passkeyID, detail string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.m == nil {
		u.m = map[string]string{}
	}
	if _, ok := u.m[passkeyID]; !ok {
		u.m[passkeyID] = detail
	}
}

func (u *unflagged) remove(passkeyID string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.m, passkeyID)
}

// ListPasskeys returns the principal's passkeys, oldest first.
func (s *Service) ListPasskeys(ctx context.Context, p Principal) ([]Passkey, error) {
	var out []Passkey
	err := s.db.View(ctx, func(tx *store.Tx) error {
		all, err := tx.ListPasskeys(ctx, p.User.ID)
		if err != nil {
			return err
		}
		for i := range all {
			pk, err := decodePasskey(all[i])
			if err != nil {
				return err
			}
			out = append(out, pk)
		}
		return nil
	})
	return out, err
}

// BeginAddPasskey starts adding a passkey for the principal on client's
// address. Passkeys they already have here are excluded, so the same
// authenticator isn't registered twice.
func (s *Service) BeginAddPasskey(ctx context.Context, client Client, p Principal) (Ceremony, error) {
	now := s.now()
	rp, err := s.relyingParty(client.Site)
	if err != nil {
		return Ceremony{}, err
	}
	existing, err := s.ListPasskeys(ctx, p)
	if err != nil {
		return Ceremony{}, err
	}
	var exclude []protocol.CredentialDescriptor
	for i := range existing {
		if existing[i].RPID == client.Site.RPID {
			exclude = append(exclude, existing[i].Credential.Descriptor())
		}
	}
	creation, session, err := rp.BeginRegistration(&waUser{user: p.User},
		webauthn.WithExclusions(exclude),
		webauthn.WithRegistrationOrigin(client.Site.Origin),
	)
	if err != nil {
		return Ceremony{}, fmt.Errorf("can't start creating a passkey: %w", err)
	}
	options, err := json.Marshal(creation.Response)
	if err != nil {
		return Ceremony{}, err
	}
	key := s.ceremonies.put(&ceremony{kind: ceremonyAddPasskey, site: client.Site, session: *session, expires: now.Add(ceremonyTTL), user: p.User}, now)
	return Ceremony{ID: key, Options: options}, nil
}

// FinishAddPasskey checks and saves a new passkey for the principal.
func (s *Service) FinishAddPasskey(ctx context.Context, client Client, p Principal, ceremonyID string, credential []byte, name string) (Passkey, error) {
	cer, err := s.ceremonies.take(ceremonyID, ceremonyAddPasskey, s.now())
	if err != nil {
		return Passkey{}, err
	}
	if cer.site != client.Site || cer.user.ID != p.User.ID {
		return Passkey{}, ErrCeremonyExpired
	}
	cred, err := s.verifyNewPasskey(client.Site, &waUser{user: p.User}, cer.session, credential)
	if err != nil {
		s.logger.InfoContext(ctx, "refused a new passkey", slog.String("user", p.User.ID), slog.Any("error", err))
		return Passkey{}, err
	}
	var pk store.Passkey
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		if pk, err = s.storePasskey(ctx, tx, p.User, client.Site, cred, name); err != nil {
			return err
		}
		return s.auditOn(ctx, tx, p.Actor(), "passkey.add", p.User.ID, pk.ID, pk.ID+" for "+pk.RPID+" from "+client.remoteIP())
	})
	if err != nil {
		return Passkey{}, err
	}
	return decodePasskey(pk)
}

// usablePasskeys counts the passkeys that still sign in at the panel's
// addresses now, leaving out the one with ID except: those for localhost or
// the current domain that aren't flagged as copied.
//
// With code sign-in, these are the ways in that count when someone removes
// one. Recovery codes come and go with code sign-in, so they count as part
// of it. dockwell recover doesn't count: it is the last resort, for whoever
// can reach the server, not a way in to keep.
func (s *Service) usablePasskeys(all []store.Passkey, except string) int {
	n := 0
	for i := range all {
		if all[i].ID != except && all[i].CloneWarningAt == nil && s.sites.Serves(all[i].RPID) {
			n++
		}
	}
	return n
}

// RemovePasskey removes one of the principal's passkeys. It refuses to
// remove the last way in: afterwards there must be another passkey that
// signs in at the panel's addresses now, or code sign-in.
func (s *Service) RemovePasskey(ctx context.Context, p Principal, passkeyID string) error {
	return s.db.Update(ctx, func(tx *store.Tx) error {
		all, err := tx.ListPasskeys(ctx, p.User.ID)
		if err != nil {
			return err
		}
		user, err := tx.GetUser(ctx, p.User.ID)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(all, func(pk store.Passkey) bool { return pk.ID == passkeyID }) {
			return fmt.Errorf("%w: you have no passkey %s. List them with ListPasskeys", ErrNotFound, passkeyID)
		}
		if s.usablePasskeys(all, passkeyID) == 0 && user.TOTPSecret == nil {
			return ErrLastWayIn
		}
		if err := tx.DeletePasskey(ctx, p.User.ID, passkeyID); err != nil {
			return err
		}
		return s.auditOn(ctx, tx, p.Actor(), "passkey.remove", p.User.ID, passkeyID, passkeyID)
	})
}
