// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/dockwell/dockwell/internal/store"
)

func purposeTOTP(userID string) string { return "totp-secret:" + userID }

// CodeSignIn is the result of signing in with a code.
type CodeSignIn struct {
	SignedIn
	UsedRecoveryCode  bool
	RecoveryCodesLeft int
}

// SignInWithCode signs in with an email and a code from an authenticator
// app or a recovery code.
//
// Authenticator codes are six digits, so wrong ones are limited three ways.
// Each source address and each email address may get a few wrong, and then
// every further try waits twice as long as the one before, up to 15
// minutes. The email's count is stored, so a restart doesn't reset it.
// After ten wrong codes in a row, code sign-in locks for that email until
// the owner unlocks it, which bounds what a patient guesser can ever try.
//
// Recovery codes carry 80 random bits, so guessing them is hopeless with or
// without limits. They are limited only by source address: anything kept
// for the account would be something anyone with the owner's email could
// trip, and a recovery code has to work exactly when someone has. A right
// one is always checked, whatever the email's count or lock.
//
// An email with no account gets the same answers: its wrong codes are
// counted and locked in the same table, by the same code.
func (s *Service) SignInWithCode(ctx context.Context, client Client, email, code string) (CodeSignIn, error) {
	now := s.now()
	if !client.Site.CanSignIn() {
		return CodeSignIn{}, ErrNeedsHostname
	}
	email, err := normalizeEmail(email)
	if err != nil {
		return CodeSignIn{}, err
	}
	code, kind := normalizeCode(code)
	if kind == codeInvalid {
		return CodeSignIn{}, fmt.Errorf("%w: enter the 6-digit code from your authenticator app, or a recovery code", ErrInvalid)
	}
	// Counted before the check and forgotten after a good code, so parallel
	// requests can't all slip in under one count.
	src := sourceKey(client.Addr)
	if w := s.sources.try(src, now); w > 0 {
		return CodeSignIn{}, &SlowDown{Wait: w}
	}

	// The email's count is read, checked and updated inside one write
	// transaction. Writes are serialized, so however many requests race,
	// each sees the failures before it, and the lock holds at ten.
	var out codeOutcome
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		a := codeAttempt{client: client, email: email, code: code, now: now}
		var err error
		a.user, err = tx.GetUserByEmail(ctx, email)
		switch {
		case err == nil:
			a.found = true
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		if kind == codeTOTP {
			out, err = s.checkTOTP(ctx, tx, a)
		} else {
			out, err = s.checkRecoveryCode(ctx, tx, a)
		}
		return err
	})
	if err != nil {
		return CodeSignIn{}, err
	}
	if out.refused != nil {
		return CodeSignIn{}, out.refused
	}
	s.sources.reset(src)
	return out.CodeSignIn, nil
}

// codeAttempt is one try at code sign-in. found is false when the email has
// no account; user is then the zero value.
type codeAttempt struct {
	client Client
	email  string
	code   string
	now    time.Time
	user   store.User
	found  bool
}

// guardBucket is the bucket of code_guards that counts email's wrong codes.
// Buckets bound the table however many addresses are tried. The hash isn't
// secret: every address shares the same table, so knowing which addresses
// share a bucket tells nobody whether one has an account.
func guardBucket(email string) int {
	sum := sha256.Sum256([]byte("dockwell code guard\x00" + email))
	return int(binary.BigEndian.Uint16(sum[:2]))
}

// guardWait is how long a bucket must wait before its next code.
func guardWait(g store.CodeGuard, now time.Time) time.Duration {
	if g.FailedAt == nil {
		return 0
	}
	return max(0, g.FailedAt.Add(backoff(g.Failures, freeAccountFailures)).Sub(now))
}

// codeOutcome is what checking a code decided: a sign-in, or the reason it
// was refused. A refusal isn't an error of the transaction, so the failure
// count it records commits.
type codeOutcome struct {
	CodeSignIn
	refused error
}

// checkTOTP runs inside the sign-in transaction. Whether the email has an
// account changes what is audited, never the answer to a wrong code.
func (s *Service) checkTOTP(ctx context.Context, tx *store.Tx, a codeAttempt) (codeOutcome, error) {
	bucket := guardBucket(a.email)
	g, err := tx.GetCodeGuard(ctx, bucket)
	if err != nil {
		return codeOutcome{}, err
	}
	if g.LockedAt != nil {
		return codeOutcome{refused: ErrCodesLocked}, s.refusedCode(ctx, tx, a, "authenticator code while code sign-in is locked")
	}
	if w := guardWait(g, a.now); w > 0 {
		return codeOutcome{refused: &SlowDown{Wait: w}}, nil
	}
	// An email with no account, or no authenticator app, is checked against
	// a random secret, so the work is the same.
	secret, known := newTOTPSecret(), a.found && a.user.TOTPSecret != nil
	if known {
		if secret, err = s.key.Open(purposeTOTP(a.user.ID), a.user.TOTPSecret); err != nil {
			return codeOutcome{}, fmt.Errorf("can't unseal the authenticator secret of %s: %w. The key file doesn't match this database", a.user.ID, err)
		}
	}
	step, ok := matchTOTP(secret, a.code, a.now)
	ok = ok && known
	reused := false
	if ok {
		if err := tx.SpendTOTPStep(ctx, a.user.ID, step); errors.Is(err, store.ErrConflict) {
			ok, reused = false, true
		} else if err != nil {
			return codeOutcome{}, err
		}
	}
	if !ok {
		g, err := tx.RecordCodeFailure(ctx, bucket, a.now, lockAfter)
		if err != nil {
			return codeOutcome{}, err
		}
		reason := ErrCodeRejected
		switch {
		case g.LockedAt != nil:
			reason = ErrCodesLocked
		case reused:
			reason = ErrCodeReused
		}
		return codeOutcome{refused: reason}, s.wrongCode(ctx, tx, a, g)
	}
	if err := tx.ResetCodeFailures(ctx, bucket); err != nil {
		return codeOutcome{}, err
	}
	signedIn, err := s.startSession(ctx, tx, a.user, a.client, MethodAuthenticator)
	return codeOutcome{CodeSignIn: CodeSignIn{SignedIn: signedIn}}, err
}

// checkRecoveryCode runs inside the sign-in transaction. Nothing about the
// account is consulted before the code is checked, so nothing anyone else
// does can make a right one fail.
func (s *Service) checkRecoveryCode(ctx context.Context, tx *store.Tx, a codeAttempt) (codeOutcome, error) {
	// An email with no account looks up the code all the same, for no one.
	err := tx.SpendRecoveryCode(ctx, a.user.ID, hashRecoveryCode(a.code), a.now)
	if errors.Is(err, store.ErrNotFound) {
		return codeOutcome{refused: ErrCodeRejected}, s.refusedCode(ctx, tx, a, "wrong recovery code")
	}
	if err != nil {
		return codeOutcome{}, err
	}
	// The person behind the account proved it: authenticator codes start
	// counting again. A lock stays for them to lift.
	if err := tx.ResetCodeFailures(ctx, guardBucket(a.email)); err != nil {
		return codeOutcome{}, err
	}
	left, err := tx.CountRecoveryCodes(ctx, a.user.ID)
	if err != nil {
		return codeOutcome{}, err
	}
	if err := s.audit(ctx, tx, "user:"+a.user.ID, "recovery_code.use", a.user.ID,
		fmt.Sprintf("from %s; %d left", a.client.remoteIP(), left)); err != nil {
		return codeOutcome{}, err
	}
	signedIn, err := s.startSession(ctx, tx, a.user, a.client, MethodRecoveryCode)
	return codeOutcome{CodeSignIn: CodeSignIn{SignedIn: signedIn, UsedRecoveryCode: true, RecoveryCodesLeft: left}}, err
}

// wrongCode audits a wrong authenticator code for an account, and the lock
// when it was the one that locked code sign-in. Each is bounded by the
// lock: at most lockAfter per decision by the owner. For an email with no
// account it only logs, so strangers can't fill the audit log.
func (s *Service) wrongCode(ctx context.Context, tx *store.Tx, a codeAttempt, g store.CodeGuard) error {
	if !a.found {
		s.logger.InfoContext(ctx, "refused a code for an email with no account", slog.String("from", a.client.Addr))
		return nil
	}
	if err := s.audit(ctx, tx, a.client.anonymous(), "auth.sign_in_failed", a.user.ID,
		fmt.Sprintf("wrong authenticator code from %s on %s, %d in a row", a.client.remoteIP(), a.client.Site.RPID, g.Failures)); err != nil {
		return err
	}
	if g.LockedAt == nil { // the caller returned early for a lock already in place
		return nil
	}
	s.logger.WarnContext(ctx, "locked code sign-in after too many wrong codes", slog.String("user", a.user.ID), slog.String("from", a.client.Addr))
	return s.audit(ctx, tx, "system", "totp.lock", a.user.ID, fmt.Sprintf("%d wrong codes in a row, the last from %s", g.Failures, a.client.remoteIP()))
}

// refusedCode records a code refused without being counted towards the
// lock: an authenticator code while code sign-in is locked, or a wrong
// recovery code. Anyone who knows the email can send these for as long as
// their address may, so they don't each get an audit event. The first in an
// hour does, and it carries the count of those since the last.
func (s *Service) refusedCode(ctx context.Context, tx *store.Tx, a codeAttempt, what string) error {
	if !a.found {
		s.logger.InfoContext(ctx, "refused a code for an email with no account", slog.String("from", a.client.Addr))
		return nil
	}
	u := a.user
	if last := u.CodesRefusedReportedAt; last != nil && a.now.Sub(*last) < refusedReportEvery {
		return tx.CountRefusedCode(ctx, u.ID)
	}
	detail := fmt.Sprintf("%s from %s on %s", what, a.client.remoteIP(), a.client.Site.RPID)
	if u.CodesRefused > 0 {
		detail += fmt.Sprintf(", and %d more codes refused since %s", u.CodesRefused, u.CodesRefusedReportedAt.UTC().Format(time.RFC3339))
		s.logger.WarnContext(ctx, "codes keep being refused for an account", slog.String("user", u.ID), slog.Int("since_last_report", u.CodesRefused+1))
	}
	if err := s.audit(ctx, tx, a.client.anonymous(), "auth.sign_in_failed", u.ID, detail); err != nil {
		return err
	}
	return tx.ReportedRefusedCodes(ctx, u.ID, a.now)
}

// TOTPStatus describes a user's code sign-in.
type TOTPStatus struct {
	Enabled           bool
	RecoveryCodesLeft int
	FailedAttempts    int
	LockedAt          *time.Time
}

// TOTP returns the principal's code sign-in state.
func (s *Service) TOTP(ctx context.Context, p Principal) (TOTPStatus, error) {
	var st TOTPStatus
	err := s.db.View(ctx, func(tx *store.Tx) error {
		u, err := tx.GetUser(ctx, p.User.ID)
		if err != nil {
			return err
		}
		n, err := tx.CountRecoveryCodes(ctx, u.ID)
		if err != nil {
			return err
		}
		g, err := tx.GetCodeGuard(ctx, guardBucket(u.Email))
		if err != nil {
			return err
		}
		st = TOTPStatus{Enabled: u.TOTPSecret != nil, RecoveryCodesLeft: n, FailedAttempts: g.Failures, LockedAt: g.LockedAt}
		return nil
	})
	return st, err
}

// TOTPSetup is a new authenticator secret, to show once.
type TOTPSetup struct {
	Secret string // base32
	URI    string // otpauth://
}

// BeginTOTP makes a new authenticator secret for the principal, with proof
// of a step-up for setting up code sign-in. It works only once ConfirmTOTP
// checks a code from it; until then any current secret keeps working.
func (s *Service) BeginTOTP(ctx context.Context, p Principal, proof string) (TOTPSetup, error) {
	if p.Token != nil || p.Session.ID == "" {
		return TOTPSetup{}, ErrNeedsSession
	}
	if err := s.confirmed(p, proof, StepUpSetUpCodes); err != nil {
		return TOTPSetup{}, err
	}
	secret := newTOTPSecret()
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		return tx.SetTOTPPending(ctx, p.User.ID, s.key.Seal(purposeTOTP(p.User.ID), secret))
	})
	if err != nil {
		return TOTPSetup{}, err
	}
	return TOTPSetup{Secret: totpEncoding.EncodeToString(secret), URI: totpURI(secret, p.User.Email, s.serverName())}, nil
}

// ConfirmTOTP checks a code from the pending secret, makes it the
// principal's, and returns new recovery codes, replacing any old ones.
func (s *Service) ConfirmTOTP(ctx context.Context, p Principal, code string) ([]string, error) {
	now := s.now()
	code, kind := normalizeCode(code)
	if kind != codeTOTP {
		return nil, fmt.Errorf("%w: enter the 6-digit code from your authenticator app", ErrInvalid)
	}
	var codes []string
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		u, err := tx.GetUser(ctx, p.User.ID)
		if err != nil {
			return err
		}
		if u.TOTPPending == nil {
			return ErrNoPendingTOTP
		}
		secret, err := s.key.Open(purposeTOTP(u.ID), u.TOTPPending)
		if err != nil {
			return fmt.Errorf("can't unseal the new authenticator secret: %w", err)
		}
		step, ok := matchTOTP(secret, code, now)
		if !ok {
			return ErrTOTPMismatch
		}
		if err := tx.EnableTOTP(ctx, u.ID, u.TOTPPending, step); err != nil {
			return err
		}
		// A new secret starts with a clean count and no lock.
		if err := tx.UnlockCodes(ctx, guardBucket(u.Email)); err != nil {
			return err
		}
		var hashes [][]byte
		codes, hashes = newRecoveryCodes()
		if err := tx.ReplaceRecoveryCodes(ctx, u.ID, hashes, now); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Actor(), "totp.enable", u.ID, fmt.Sprintf("with %d new recovery codes", len(codes)))
	})
	return codes, err
}

// DisableTOTP turns code sign-in off for the principal, deleting the secret
// and recovery codes, with proof of a step-up for turning it off. It
// refuses when no passkey that signs in at the panel's addresses now would
// be left, and a refusal leaves the proof unspent.
func (s *Service) DisableTOTP(ctx context.Context, p Principal, proof string) error {
	if p.Token != nil || p.Session.ID == "" {
		return ErrNeedsSession
	}
	return s.db.Update(ctx, func(tx *store.Tx) error {
		u, err := tx.GetUser(ctx, p.User.ID)
		if err != nil {
			return err
		}
		if u.TOTPSecret == nil {
			return ErrNoTOTP
		}
		all, err := tx.ListPasskeys(ctx, u.ID)
		if err != nil {
			return err
		}
		if s.usablePasskeys(all, "") == 0 {
			return ErrLastWayIn
		}
		if err := s.confirmed(p, proof, StepUpDisableCodes); err != nil {
			return err
		}
		if err := tx.DisableTOTP(ctx, u.ID); err != nil {
			return err
		}
		if err := tx.UnlockCodes(ctx, guardBucket(u.Email)); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Actor(), "totp.disable", u.ID, "")
	})
}

// RegenerateRecoveryCodes replaces the principal's recovery codes, with
// proof of a step-up for making them.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, p Principal, proof string) ([]string, error) {
	if p.Token != nil || p.Session.ID == "" {
		return nil, ErrNeedsSession
	}
	var codes []string
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		u, err := tx.GetUser(ctx, p.User.ID)
		if err != nil {
			return err
		}
		if u.TOTPSecret == nil {
			return ErrNoTOTP
		}
		if err := s.confirmed(p, proof, StepUpRecoveryCodes); err != nil {
			return err
		}
		var hashes [][]byte
		codes, hashes = newRecoveryCodes()
		if err := tx.ReplaceRecoveryCodes(ctx, u.ID, hashes, s.now()); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Actor(), "recovery_codes.regenerate", u.ID, "")
	})
	return codes, err
}

// UnlockCodes turns code sign-in back on after a lock. The unlock event
// also counts any refused codes no event has counted yet.
func (s *Service) UnlockCodes(ctx context.Context, p Principal) error {
	return s.db.Update(ctx, func(tx *store.Tx) error {
		u, err := tx.GetUser(ctx, p.User.ID)
		if err != nil {
			return err
		}
		if err := tx.UnlockCodes(ctx, guardBucket(u.Email)); err != nil {
			return err
		}
		detail := ""
		if u.CodesRefused > 0 && u.CodesRefusedReportedAt != nil {
			detail = fmt.Sprintf("%d codes refused since %s", u.CodesRefused, u.CodesRefusedReportedAt.UTC().Format(time.RFC3339))
			if err := tx.ReportedRefusedCodes(ctx, u.ID, s.now()); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, p.Actor(), "totp.unlock", u.ID, detail)
	})
}
