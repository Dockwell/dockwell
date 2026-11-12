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
	"strings"
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
// secret needs one, and so does every change to how a person signs in or
// to their API tokens that could let a stolen session keep its hold: adding
// or removing a passkey, setting up or turning off code sign-in, making
// recovery codes and creating a token. So does what could do the same on a
// server: applying a plan that's as good as root there, saving a file that's a
// way back onto it, and opening a terminal on it. So does opening an app's web
// UI through the panel, as an admin tool may sign whoever reaches it in by
// itself. See docs/adr/0022-secrets-encrypted-at-rest.md,
// docs/adr/0011-claim-and-sign-in.md, docs/adr/0032-files-on-a-server.md,
// docs/adr/0037-a-terminal-on-a-server.md and
// docs/adr/0061-opening-an-apps-web-ui-through-the-panel.md.
//
// For someone who asks to be asked less often, their session's cover stands in
// for the proof, except for what always asks (cover.go, ADR 0075).

// StepUpTTL is how long a step-up's proof works.
const StepUpTTL = time.Minute

// StepUpLasts is how long a proof for saving one path stands for the saves after
// it, in that session, so editing a file doesn't ask for a passkey each time.
const StepUpLasts = 10 * time.Minute

// StepUpAction is an action that needs a step-up.
type StepUpAction string

// Actions. Each takes a target: what it acts on, or for an action on the
// person's own account, the person's ID, which BeginStepUp fills in.
const (
	// StepUpRevealSecret reveals a secret's value. Its target is the
	// secret's ID.
	StepUpRevealSecret StepUpAction = "secret.reveal"
	// StepUpAddPasskey adds a passkey.
	StepUpAddPasskey StepUpAction = "passkey.add"
	// StepUpRemovePasskey removes a passkey. Its target is the passkey's
	// ID.
	StepUpRemovePasskey StepUpAction = "passkey.remove"
	// StepUpSetUpCodes sets up code sign-in with a new authenticator
	// secret, which ends with new recovery codes.
	StepUpSetUpCodes StepUpAction = "totp.enable"
	// StepUpDisableCodes turns code sign-in off.
	StepUpDisableCodes StepUpAction = "totp.disable"
	// StepUpRecoveryCodes makes new recovery codes.
	StepUpRecoveryCodes StepUpAction = "recovery_codes.regenerate"
	// StepUpCreateToken creates an API token.
	StepUpCreateToken StepUpAction = "token.create"
	// StepUpApplyRootChange applies a change that gives something as much as
	// root on a server. Its target is the change's ID.
	StepUpApplyRootChange StepUpAction = "change.apply_root"
	// StepUpSaveWayBackIn saves a file that's a way back onto a server. Its
	// target is the server's ID and the file's path, with every link followed.
	StepUpSaveWayBackIn StepUpAction = "file.save_way_back_in"
	// StepUpEndProcess ends a process on a server. Its target is the server's ID
	// and the process's, as "srv_…:1234".
	StepUpEndProcess StepUpAction = "process.end"
	// StepUpOpenTerminal opens a root shell on a server. Its target is the
	// server's ID. A proof opens one terminal (ADR 0037).
	StepUpOpenTerminal StepUpAction = "terminal.open"
	// StepUpApproveChange applies someone else's change in a protected
	// environment. Its target is the change's ID and its plan's fingerprint, as
	// "chg_…:fingerprint".
	StepUpApproveChange StepUpAction = "change.approve"
	// StepUpMakeWayBackIn makes a file or a folder that's a way back onto a server.
	// Its target is the server's ID and the path, as for a save.
	StepUpMakeWayBackIn StepUpAction = "file.make_way_back_in"
	// StepUpMoveWayBackIn moves what's at a path where either end is a way back onto
	// a server. Its target is the server's ID and both paths, on two lines.
	StepUpMoveWayBackIn StepUpAction = "file.move_way_back_in"
	// StepUpDeleteWayBackIn deletes what's a way back onto a server, or holds one.
	StepUpDeleteWayBackIn StepUpAction = "file.delete_way_back_in"
	// StepUpRestoreWayBackIn puts back what was deleted where that's a way back
	// onto a server. Its target is the server's ID and the path it goes back to.
	StepUpRestoreWayBackIn StepUpAction = "file.restore_way_back_in"
	// StepUpInvite invites someone. Its target is the email, the role and the
	// place, as InviteTarget writes them.
	StepUpInvite StepUpAction = "member.invite"
	// StepUpAddServer makes a ticket that adds a server to the fleet.
	StepUpAddServer StepUpAction = "server.add"
	// StepUpOpenService opens a service's web UI through the panel, which may sign
	// whoever reaches it in by itself, as an admin tool does. Its target is the
	// service's ID. A proof opens it once (ADR 0061).
	StepUpOpenService StepUpAction = "service.open"
	// StepUpSetPanelDomain sets or clears the panel's domain (ADR 0063). Its
	// target is the domain, or the one it clears.
	StepUpSetPanelDomain StepUpAction = "settings.panel_domain"
	// StepUpSignInOnPanelDomain makes a one-time link that signs in on the
	// panel's domain. Its target is the domain.
	StepUpSignInOnPanelDomain StepUpAction = "sign_in_link.panel_domain"
	// StepUpSetPolicy chooses how often the person is asked for their passkey
	// again (ADR 0075). It always asks.
	StepUpSetPolicy StepUpAction = "step_up_policy.set"
	// StepUpSetFloor sets the least anyone may choose for that. It always asks.
	StepUpSetFloor StepUpAction = "step_up_floor.set"
)

// stepUpActions says what each action is doing, for the audit log, and
// whether it acts on a target of its own rather than the person.
var stepUpActions = map[StepUpAction]struct {
	doing    string
	targeted bool
}{
	StepUpRevealSecret:    {"revealing", true},
	StepUpAddPasskey:      {"adding a passkey", false},
	StepUpRemovePasskey:   {"removing passkey", true},
	StepUpSetUpCodes:      {"setting up code sign-in", false},
	StepUpDisableCodes:    {"turning code sign-in off", false},
	StepUpRecoveryCodes:   {"making new recovery codes", false},
	StepUpCreateToken:     {"creating an API token", false},
	StepUpApplyRootChange: {"applying", true},
	StepUpSaveWayBackIn:   {"saving", true},
	StepUpEndProcess:      {"ending process", true},
	StepUpOpenTerminal:    {"opening a terminal on", true},
	StepUpApproveChange:   {"approving", true},
	// A move's target has its two paths on two lines, which doing puts in words.
	StepUpMakeWayBackIn:    {"making", true},
	StepUpMoveWayBackIn:    {"moving", true},
	StepUpDeleteWayBackIn:  {"deleting", true},
	StepUpRestoreWayBackIn: {"putting back", true},
	StepUpInvite:           {"inviting", true},
	StepUpAddServer:        {"adding a server", false},
	StepUpOpenService:      {"opening", true},
	StepUpSetPanelDomain:   {"setting the panel's domain to", true},
	// A sign-in link to the domain, for the person asking.
	StepUpSignInOnPanelDomain: {"signing in on", true},
	StepUpSetPolicy:           {"choosing how often to be asked for a passkey", false},
	StepUpSetFloor:            {"setting how often everyone is asked for a passkey at least", false},
}

// doing says what a step-up confirms, such as "revealing sec_…".
func doing(action StepUpAction, target string) string {
	a := stepUpActions[action]
	if a.targeted {
		return a.doing + " " + strings.ReplaceAll(target, "\n", " to ")
	}
	return a.doing
}

// RecentSignIn is how long a sign-in that needed no passkey here confirms
// adding one, once, as a step-up would (freshSignIn). Someone with no
// passkey for this address, or none they still have, as after losing a
// device or adding a domain, adds one so. A session stolen later can't.
const RecentSignIn = 10 * time.Minute

// Errors for step-ups.
var (
	ErrNeedsStepUp = errors.New("this needs you to confirm with your passkey just now, and the confirmation is missing, used or expired. " +
		"Try again, and confirm when your browser asks")
	ErrNoPasskeyHere = errors.New("confirming this needs a passkey for this address, and you have none here. " +
		"Add one under Profile and security, then try again")
	ErrNeedsFreshSignIn = errors.New("adding a passkey needs you to confirm with a passkey you have for this address, and you have none here. " +
		"Sign out, sign in again with a code, a recovery code or a link from dockwell recover, and add it within 10 minutes")
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

// stepUpFor is what one action on one target, by one person in one session, comes to.
type stepUpFor struct {
	user, session string
	action        StepUpAction
	target        string
}

// stepUps holds proofs in memory, at most max of them, what they've confirmed for
// a while, and what's waiting for one. A restart drops them, and the person
// confirms again.
type stepUps struct {
	mu     sync.Mutex
	max    int
	m      map[string]stepUpProof    // by hex of the proof's hash
	stands map[stepUpFor]time.Time   // what stays confirmed, until when
	asked  map[stepUpFor]*time.Timer // what's been asked for and not confirmed
}

func newStepUps(maxProofs int) *stepUps {
	return &stepUps{max: maxProofs, m: map[string]stepUpProof{}, stands: map[stepUpFor]time.Time{}, asked: map[stepUpFor]*time.Timer{}}
}

// stand has what a proof confirmed go on standing until until. Past the limit it
// drops what's over, and if that's nothing, the one that ends soonest.
func (s *stepUps) stand(key stepUpFor, until, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.stands) >= s.max {
		var soonest stepUpFor
		var at time.Time
		for k, v := range s.stands {
			if now.After(v) {
				delete(s.stands, k)
			} else if at.IsZero() || v.Before(at) {
				soonest, at = k, v
			}
		}
		if len(s.stands) >= s.max {
			delete(s.stands, soonest)
		}
	}
	s.stands[key] = until
}

// standing reports what's still confirmed.
func (s *stepUps) standing(key stepUpFor, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.stands[key]
	return ok && !now.After(until)
}

// settle says what was asked for has its answer.
func (s *stepUps) settle(key stepUpFor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.asked[key]; ok {
		t.Stop()
		delete(s.asked, key)
	}
}

// ask starts waiting for key, once however often it's asked, and reports whether
// there's room to. It calls missed if nothing settles it in time.
func (s *stepUps) ask(key stepUpFor, wait time.Duration, missed func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.asked[key]; ok {
		return true
	}
	if len(s.asked) >= s.max {
		return false
	}
	s.asked[key] = time.AfterFunc(wait, func() {
		s.mu.Lock()
		_, still := s.asked[key]
		delete(s.asked, key)
		s.mu.Unlock()
		if still {
			missed()
		}
	})
	return true
}

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
	a, ok := stepUpActions[action]
	switch {
	case !ok || a.targeted && target == "":
		return Ceremony{}, fmt.Errorf("%w: say which action to confirm, and on what, such as revealing a secret by its ID", ErrInvalid)
	case !a.targeted:
		target = p.User.ID // it acts on the person
	case action == StepUpInvite:
		// The email is written one way, the server's, so what the panel typed and what
		// CreateInvite checks against agree whatever the letters (İ lowercases two ways).
		var err error
		if target, err = normalizeInviteTarget(target); err != nil {
			return Ceremony{}, err
		}
	}
	rp, err := s.relyingParty(client.Site)
	if err != nil {
		return Ceremony{}, err
	}
	var creds []webauthn.Credential
	err = s.db.View(ctx, func(tx *store.Tx) error {
		var err error
		creds, err = s.stepUpCredentials(ctx, tx, p.User.ID, client.Site.RPID)
		return err
	})
	if err != nil {
		return Ceremony{}, err
	}
	if len(creds) == 0 {
		if action == StepUpAddPasskey {
			return Ceremony{}, ErrNeedsFreshSignIn
		}
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

// stepUpCredentials are the passkeys a step-up allows: the person's for
// this address that aren't flagged as copied.
func (s *Service) stepUpCredentials(ctx context.Context, tx *store.Tx, userID, rpID string) ([]webauthn.Credential, error) {
	all, err := tx.ListPasskeys(ctx, userID)
	if err != nil {
		return nil, err
	}
	var creds []webauthn.Credential
	for i := range all {
		if all[i].RPID != rpID || all[i].CloneWarningAt != nil {
			continue
		}
		pk, err := decodePasskey(all[i])
		if err != nil {
			return nil, err
		}
		creds = append(creds, pk.Credential)
	}
	return creds, nil
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
	what := doing(cer.action, cer.target)
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
		// The assertion is checked against every passkey the ceremony
		// allowed, as the library requires, and must come from pk.
		allowed, err := s.stepUpCredentials(ctx, tx, p.User.ID, client.Site.RPID)
		if err != nil {
			return err
		}
		cred, err := rp.ValidateLogin(&waUser{user: p.User, credentials: allowed}, cer.session, parsed)
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
		// It opens the session's hour, for someone who asks once an hour (ADR 0075).
		if err := tx.ConfirmSession(ctx, p.Session.ID, now); err != nil {
			return err
		}
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

// UseStepUp spends a proof for action on target, in the principal's session. It
// works once: a proof given for anything else is spent too. No API token can use
// one. For an action on the person's own account, the target is their ID.
//
// With no proof, the session's cover stands in where the person asked to be asked
// less often and the action doesn't always ask (ADR 0075). That goes on the audit
// log, from client, in a transaction of its own; an action that happens in a
// transaction already uses UseStepUpIn.
func (s *Service) UseStepUp(ctx context.Context, client Client, p Principal, proof string, action StepUpAction, target string) error {
	_, err := s.HoldStepUp(ctx, client, p, proof, action, target)
	return err
}

// UseStepUpIn is UseStepUp for an action that happens in tx, such as revealing a
// secret, so the record of what confirmed it is written with what it did, or
// neither is.
func (s *Service) UseStepUpIn(ctx context.Context, tx *store.Tx, client Client, p Principal, proof string, action StepUpAction, target string) error {
	if p.Token != nil || p.Session.ID == "" {
		return ErrNeedsSession
	}
	if proof == "" {
		if err := s.coverIn(ctx, tx, client, p, action, target); err != nil {
			return err
		}
		s.stepUps.settle(forPrincipal(p, action, target))
		return nil
	}
	_, err := s.holdProof(p, proof, action, target)
	return err
}

// HoldStepUp is UseStepUp for what can still be refused once it has a proof, and
// says how to give the proof back if it is. ADR 0022 spends a proof where the action
// happens, so one that confirmed nothing isn't used up, and holding it means no
// second call can use it meanwhile. A save happens on an agent, where no
// transaction reaches, and starting a change happens in the engine. Where the
// session's cover stands in, there's nothing to give back.
func (s *Service) HoldStepUp(ctx context.Context, client Client, p Principal, proof string, action StepUpAction, target string) (func(), error) {
	if p.Token != nil || p.Session.ID == "" {
		return nil, ErrNeedsSession
	}
	if proof == "" {
		if !mayCover(p, action) {
			return nil, ErrNeedsStepUp
		}
		if err := s.db.Update(ctx, func(tx *store.Tx) error { return s.coverIn(ctx, tx, client, p, action, target) }); err != nil {
			return nil, err
		}
		s.stepUps.settle(forPrincipal(p, action, target))
		return func() {}, nil
	}
	return s.holdProof(p, proof, action, target)
}

// holdProof takes a proof for action on target, and says how to give it back.
func (s *Service) holdProof(p Principal, proof string, action StepUpAction, target string) (func(), error) {
	if proof == "" {
		return nil, ErrNeedsStepUp
	}
	hash := hashToken(proof)
	got, ok := s.stepUps.take(hash)
	if !ok || s.now().After(got.expires) || got.action != action ||
		subtle.ConstantTimeCompare([]byte(got.user), []byte(p.User.ID)) != 1 ||
		subtle.ConstantTimeCompare([]byte(got.session), []byte(p.Session.ID)) != 1 ||
		got.target != target {
		return nil, ErrNeedsStepUp
	}
	s.stepUps.settle(forPrincipal(p, action, target))
	return func() { s.stepUps.put(hash, got, s.now()) }, nil
}

func forPrincipal(p Principal, action StepUpAction, target string) stepUpFor {
	return stepUpFor{user: p.User.ID, session: p.Session.ID, action: action, target: target}
}

// KeepStepUp has action on target stay confirmed in this session for StepUpLasts.
// It's for a save of one path, as a person saving a file is at it a while, and
// nothing else: the rest of a step-up is that it's for one thing, just now.
func (s *Service) KeepStepUp(p Principal, action StepUpAction, target string) {
	now := s.now()
	s.stepUps.stand(forPrincipal(p, action, target), now.Add(StepUpLasts), now)
}

// StepUpStands reports action on target still confirmed in this session.
func (s *Service) StepUpStands(p Principal, action StepUpAction, target string) bool {
	if p.Token != nil || p.Session.ID == "" {
		return false
	}
	return s.stepUps.standing(forPrincipal(p, action, target), s.now())
}

// AskedForStepUp notes action on target was refused for want of a step-up, and
// calls missed if nothing confirms it within a proof's minute, which is someone
// trying a way in with no passkey, or thinking better of it. Past the limit of what
// it keeps track of, it says so at once.
func (s *Service) AskedForStepUp(p Principal, action StepUpAction, target string, missed func()) {
	if !s.stepUps.ask(forPrincipal(p, action, target), s.askWait, missed) {
		missed()
	}
}

// confirmedIn spends a proof for an action on the principal's own account, in
// tx (UseStepUpIn).
func (s *Service) confirmedIn(ctx context.Context, tx *store.Tx, client Client, p Principal, proof string, action StepUpAction) error {
	return s.UseStepUpIn(ctx, tx, client, p, proof, action, p.User.ID)
}

// freshSignIn reports whether the principal's session may add a passkey
// for rpID with no step-up, and says what confirms it, for the audit log.
// Only a sign-in that needed no passkey here does, within RecentSignIn of
// signing in, and only for the first passkey added since:
//   - a link from dockwell recover, which takes a shell on the control
//     server;
//   - a recovery code, which is for getting back in after losing every
//     passkey, and is a security event on the timeline;
//   - an authenticator code, only when no usable passkey is registered for
//     this address, as after adding a domain, and a security event too. A
//     code is phishable, so with a passkey here, adding another takes that
//     passkey.
//
// Adding the passkey checks it again, in the transaction that stores it,
// and records that the session used it (store.Tx.AddPasskeyOnce), so
// neither the ceremony nor a second one stretches it.
func (s *Service) freshSignIn(ctx context.Context, tx *store.Tx, p Principal, rpID string) (string, bool, error) {
	if p.Token != nil || p.Session.ID == "" || p.Session.AddedPasskeyAt != nil || s.now().Sub(p.Session.CreatedAt) >= RecentSignIn {
		return "", false, nil
	}
	var by string
	switch p.Session.Method {
	case MethodLink:
		by = "confirmed by signing in with a link from dockwell recover"
	case MethodRecoveryCode:
		by = "confirmed by signing in with a recovery code"
	case MethodAuthenticator:
		by = "confirmed by signing in with an authenticator code, with no passkey for this address"
	default:
		return "", false, nil
	}
	if p.Session.Method == MethodAuthenticator {
		here, err := s.stepUpCredentials(ctx, tx, p.User.ID, rpID)
		if err != nil || len(here) > 0 {
			return "", false, err // a passkey here confirms it instead
		}
	}
	return by, true, nil
}
