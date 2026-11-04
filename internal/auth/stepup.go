// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/dockwell/dockwell/internal/store"
)

// A step-up is a fresh passkey assertion that confirms one action, on one
// target, in one session: a person at the keyboard, with their
// authenticator, just now. It ends in a proof that works once, within
// StepUpTTL, for that action and target in that session alone. Revealing a
// secret needs one. See docs/adr/0022-secrets-encrypted-at-rest.md.

// StepUpTTL is how long a step-up's proof works.
const StepUpTTL = time.Minute

// StepUpAction is an action that needs a step-up.
type StepUpAction string

// Actions.
const (
	// StepUpRevealSecret reveals a secret's value. Its target is the
	// secret's ID.
	StepUpRevealSecret StepUpAction = "secret.reveal"
)

var stepUpWords = map[StepUpAction]string{StepUpRevealSecret: "revealing"}

// Errors for step-ups.
var (
	ErrNeedsStepUp = errors.New("this needs you to confirm with your passkey just now, and the confirmation is missing, used or expired. " +
		"Try again, and confirm when your browser asks")
	ErrNoPasskeyHere = errors.New("confirming this needs a passkey for this address, and you have none here. " +
		"Add one under Profile and security, then try again")
)

// StepUp is a step-up's proof.
type StepUp struct {
	Proof   string
	Expires time.Time
}

// stepUpProof is what a proof confirms. Only the proof's hash is kept.
type stepUpProof struct {
	user, session string
	action        StepUpAction
	target        string
	expires       time.Time
}

// stepUps holds proofs in memory, at most max of them. A restart drops
// them, and the person confirms again.
type stepUps struct {
	mu  sync.Mutex
	max int
	m   map[string]stepUpProof // by hex of the proof's hash
}

func newStepUps(maxProofs int) *stepUps { return &stepUps{max: maxProofs, m: map[string]stepUpProof{}} }

func (s *stepUps) put(hash []byte, p stepUpProof, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) >= s.max {
		var oldest string
		for k, v := range s.m {
			if now.After(v.expires) {
				delete(s.m, k)
			} else if oldest == "" || v.expires.Before(s.m[oldest].expires) {
				oldest = k
			}
		}
		if len(s.m) >= s.max {
			delete(s.m, oldest)
		}
	}
	s.m[hex.EncodeToString(hash)] = p
}

// take removes a proof and returns it: it works once, whatever it is used
// for.
func (s *stepUps) take(hash []byte) (stepUpProof, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hex.EncodeToString(hash)
	p, ok := s.m[key]
	delete(s.m, key)
	return p, ok
}

// BeginStepUp starts a step-up for action on target: an assertion that
// allows only the principal's passkeys for this address that aren't
// flagged as copied, with user verification. No API token can begin one.
func (s *Service) BeginStepUp(ctx context.Context, client Client, p Principal, action StepUpAction, target string) (Ceremony, error) {
	if p.Token != nil || p.Session.ID == "" {
		return Ceremony{}, ErrNeedsSession
	}
	if _, ok := stepUpWords[action]; !ok || target == "" {
		return Ceremony{}, fmt.Errorf("%w: say which action to confirm, and on what, such as revealing a secret by its ID", ErrInvalid)
	}
	rp, err := s.relyingParty(client.Site)
	if err != nil {
		return Ceremony{}, err
	}
	var creds []webauthn.Credential
	err = s.db.View(ctx, func(tx *store.Tx) error {
		all, err := tx.ListPasskeys(ctx, p.User.ID)
		if err != nil {
			return err
		}
		for i := range all {
			if all[i].RPID != client.Site.RPID || all[i].CloneWarningAt != nil {
				continue
			}
			pk, err := decodePasskey(all[i])
			if err != nil {
				return err
			}
			creds = append(creds, pk.Credential)
		}
		return nil
	})
	if err != nil {
		return Ceremony{}, err
	}
	if len(creds) == 0 {
		return Ceremony{}, ErrNoPasskeyHere
	}
	now := s.now()
	assertion, session, err := rp.BeginLogin(&waUser{user: p.User, credentials: creds},
		webauthn.WithUserVerification(protocol.VerificationRequired),
		webauthn.WithLoginOrigin(client.Site.Origin),
	)
	if err != nil {
		return Ceremony{}, fmt.Errorf("can't start confirming with a passkey: %w", err)
	}
	options, err := json.Marshal(assertion.Response)
	if err != nil {
		return Ceremony{}, err
	}
	key := s.ceremonies.put(&ceremony{
		kind: ceremonyStepUp, site: client.Site, session: *session, expires: now.Add(ceremonyTTL), user: p.User,
		sessionID: p.Session.ID, action: action, target: target,
	}, now)
	return Ceremony{ID: key, Options: options}, nil
}

// FinishStepUp checks a step-up's assertion and returns its proof. As at
// sign-in, a passkey whose sign count goes backwards is flagged as copied
// and refused, and reading, checking and storing the count is one write
// transaction. The step-up is audited, and so is a refusal.
func (s *Service) FinishStepUp(ctx context.Context, client Client, p Principal, ceremonyID string, credential []byte) (StepUp, error) {
	if p.Token != nil || p.Session.ID == "" {
		return StepUp{}, ErrNeedsSession
	}
	now := s.now()
	cer, err := s.ceremonies.take(ceremonyID, ceremonyStepUp, now)
	if err != nil {
		return StepUp{}, err
	}
	if cer.site != client.Site || cer.user.ID != p.User.ID || cer.sessionID != p.Session.ID {
		return StepUp{}, ErrCeremonyExpired
	}
	rp, err := s.relyingParty(client.Site)
	if err != nil {
		return StepUp{}, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(credential)
	if err != nil {
		s.logger.InfoContext(ctx, "refused a malformed passkey response to a step-up", slog.String("user", p.User.ID), slog.String("detail", protocolDetail(err)))
		return StepUp{}, ErrPasskeyRejected
	}
	var (
		refused error
		copied  string
		used    string
	)
	what := fmt.Sprintf("%s %s", stepUpWords[cer.action], cer.target)
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		stored, err := tx.GetPasskeyByCredentialID(ctx, parsed.RawID)
		if errors.Is(err, store.ErrNotFound) {
			refused = ErrPasskeyRejected
			return s.auditOn(ctx, tx, p.Actor(), "auth.step_up_failed", p.User.ID, cer.target,
				fmt.Sprintf("confirming %s with an unknown passkey, from %s", what, client.remoteIP()))
		}
		if err != nil {
			return err
		}
		pk, err := decodePasskey(stored)
		if err != nil {
			return err
		}
		refuse := func(reason string, why error) error {
			refused = why
			return s.auditOn(ctx, tx, p.Actor(), "auth.step_up_failed", p.User.ID, cer.target,
				fmt.Sprintf("confirming %s with passkey %s from %s: %s", what, pk.ID, client.remoteIP(), reason))
		}
		if stored.UserID != p.User.ID || pk.RPID != client.Site.RPID {
			return refuse("not one of this person's passkeys for this address", ErrPasskeyRejected)
		}
		if pk.CloneWarningAt != nil {
			return refuse("flagged as possibly copied", ErrPasskeyCopied)
		}
		flag := func(detail string) error {
			refused, copied = ErrPasskeyCopied, pk.ID
			if err := tx.FlagPasskeyClone(ctx, pk.ID, now); err != nil {
				return err
			}
			return s.audit(ctx, tx, p.Actor(), "passkey.clone_suspected", p.User.ID, detail)
		}
		if detail, ok := s.unflagged.get(pk.ID); ok {
			return flag(detail)
		}
		cred, err := rp.ValidateLogin(&waUser{user: p.User, credentials: []webauthn.Credential{pk.Credential}}, cer.session, parsed)
		if err != nil {
			return refuse(protocolDetail(err), ErrPasskeyRejected)
		}
		if !bytes.Equal(cred.ID, pk.CredentialID) {
			return refuse("answered for another credential", ErrPasskeyRejected)
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
		used = pk.ID
		return s.auditOn(ctx, tx, p.Actor(), "auth.step_up", p.User.ID, cer.target,
			fmt.Sprintf("confirmed %s with passkey %s, from %s", what, pk.ID, client.remoteIP()))
	})
	switch {
	case copied != "" && err != nil:
		s.logger.ErrorContext(ctx, "can't flag a passkey that may have been copied; refusing it until it is flagged",
			slog.String("passkey", copied), slog.Any("error", err))
		return StepUp{}, fmt.Errorf("can't flag passkey %s as possibly copied: %w", copied, err)
	case err != nil:
		return StepUp{}, err
	case copied != "":
		s.unflagged.remove(copied)
		return StepUp{}, ErrPasskeyCopied
	case refused != nil:
		return StepUp{}, refused
	}
	proof, hash := newToken()
	expires := now.Add(StepUpTTL)
	s.stepUps.put(hash, stepUpProof{user: p.User.ID, session: p.Session.ID, action: cer.action, target: cer.target, expires: expires}, now)
	s.logger.InfoContext(ctx, "a passkey confirmed an action", slog.String("user", p.User.ID), slog.String("passkey", used),
		slog.String("action", string(cer.action)), slog.String("target", cer.target))
	return StepUp{Proof: proof, Expires: expires}, nil
}

// UseStepUp spends a proof for action on target, in the principal's
// session. It works once: a proof given for anything else is spent too.
// No API token can use one.
func (s *Service) UseStepUp(p Principal, proof string, action StepUpAction, target string) error {
	if p.Token != nil || p.Session.ID == "" {
		return ErrNeedsSession
	}
	if proof == "" {
		return ErrNeedsStepUp
	}
	got, ok := s.stepUps.take(hashToken(proof))
	if !ok || s.now().After(got.expires) || got.action != action ||
		subtle.ConstantTimeCompare([]byte(got.user), []byte(p.User.ID)) != 1 ||
		subtle.ConstantTimeCompare([]byte(got.session), []byte(p.Session.ID)) != 1 ||
		got.target != target {
		return ErrNeedsStepUp
	}
	return nil
}
