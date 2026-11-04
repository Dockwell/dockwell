// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
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
// Wrong codes are limited three ways. Each source address and each account
// may get a few wrong, and then every further try waits twice as long as
// the one before, up to 15 minutes. The account's count is stored, so a
// restart doesn't reset it. After ten wrong codes in a row, code sign-in
// locks for the account until the owner unlocks it, which bounds what a
// patient guesser can ever try. Recovery codes, which can't be guessed,
// still work while it is locked.
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
	if w, _ := s.sources.try(src, now); w > 0 {
		return CodeSignIn{}, &SlowDown{Wait: w}
	}

	// The account's own count is read, checked and updated inside one write
	// transaction. Writes are serialized, so however many requests race,
	// each sees the failures before it, and the lock holds at ten.
	var out codeOutcome
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		user, err := tx.GetUserByEmail(ctx, email)
		if err != nil {
			return err
		}
		if kind == codeTOTP {
			out, err = s.checkTOTP(ctx, tx, user, client, code, now)
		} else {
			out, err = s.checkRecoveryCode(ctx, tx, user, client, code, now)
		}
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return CodeSignIn{}, s.unknownAccount(ctx, client, email, code, kind, now)
	}
	if err != nil {
		return CodeSignIn{}, err
	}
	if out.refused != nil {
		return CodeSignIn{}, out.refused
	}
	s.sources.reset(src)
	return out.CodeSignIn, nil
}

// unknownAccount answers a code for an email with no account as it would a
// wrong code for a real one, slowing down and locking alike, so the answer
// doesn't say whether the email has an account.
func (s *Service) unknownAccount(ctx context.Context, client Client, email, code string, kind codeKind, now time.Time) error {
	_, _ = matchTOTP(newTOTPSecret(), code, now)
	s.logger.InfoContext(ctx, "refused a code for an email with no account", slog.String("from", client.Addr))
	w, n := s.accounts.try("acct:"+email, now)
	switch {
	case kind == codeTOTP && n >= lockAfter:
		return ErrCodesLocked
	case w > 0:
		return &SlowDown{Wait: w}
	default:
		return ErrCodeRejected
	}
}

// accountWait is how long an account must wait before its next code.
func accountWait(u store.User, now time.Time) time.Duration {
	if u.CodeFailedAt == nil {
		return 0
	}
	return max(0, u.CodeFailedAt.Add(backoff(u.CodeFailures, freeAccountFailures)).Sub(now))
}

// codeOutcome is what checking a code decided: a sign-in, or the reason it
// was refused. A refusal isn't an error of the transaction, so the failure
// count it records commits.
type codeOutcome struct {
	CodeSignIn
	refused error
}

// checkTOTP runs inside the sign-in transaction.
func (s *Service) checkTOTP(ctx context.Context, tx *store.Tx, user store.User, client Client, code string, now time.Time) (codeOutcome, error) {
	if user.CodesLocked != nil {
		return codeOutcome{refused: ErrCodesLocked}, s.audit(ctx, tx, "user:"+user.ID, "auth.sign_in_failed", user.ID,
			"authenticator code while locked, from "+client.remoteIP())
	}
	if w := accountWait(user, now); w > 0 {
		return codeOutcome{refused: &SlowDown{Wait: w}}, nil
	}
	step, ok := int64(0), false
	if user.TOTPSecret != nil {
		secret, err := s.key.Open(purposeTOTP(user.ID), user.TOTPSecret)
		if err != nil {
			return codeOutcome{}, fmt.Errorf("can't unseal the authenticator secret of %s: %w. The key file doesn't match this database", user.ID, err)
		}
		step, ok = matchTOTP(secret, code, now)
	}
	reused := false
	if ok {
		if err := tx.SpendTOTPStep(ctx, user.ID, step); errors.Is(err, store.ErrConflict) {
			ok, reused = false, true
		} else if err != nil {
			return codeOutcome{}, err
		}
	}
	if !ok {
		reason := ErrCodeRejected
		if reused {
			reason = ErrCodeReused
		}
		locked, err := s.recordCodeFailure(ctx, tx, user, client, "authenticator code", now)
		if locked {
			reason = ErrCodesLocked
		}
		return codeOutcome{refused: reason}, err
	}
	if err := tx.ResetCodeFailures(ctx, user.ID); err != nil {
		return codeOutcome{}, err
	}
	signedIn, err := s.startSession(ctx, tx, user, client, MethodAuthenticator)
	return codeOutcome{CodeSignIn: CodeSignIn{SignedIn: signedIn}}, err
}

func (s *Service) checkRecoveryCode(ctx context.Context, tx *store.Tx, user store.User, client Client, code string, now time.Time) (codeOutcome, error) {
	if w := accountWait(user, now); w > 0 {
		return codeOutcome{refused: &SlowDown{Wait: w}}, nil
	}
	err := tx.SpendRecoveryCode(ctx, user.ID, hashRecoveryCode(code), now)
	if errors.Is(err, store.ErrNotFound) {
		// Recovery codes can't be guessed, so they still work when code
		// sign-in locks; a wrong one counts towards the lock all the same.
		_, err := s.recordCodeFailure(ctx, tx, user, client, "recovery code", now)
		return codeOutcome{refused: ErrCodeRejected}, err
	}
	if err != nil {
		return codeOutcome{}, err
	}
	if err := tx.ResetCodeFailures(ctx, user.ID); err != nil {
		return codeOutcome{}, err
	}
	left, err := tx.CountRecoveryCodes(ctx, user.ID)
	if err != nil {
		return codeOutcome{}, err
	}
	if err := s.audit(ctx, tx, "user:"+user.ID, "recovery_code.use", user.ID,
		fmt.Sprintf("from %s; %d left", client.remoteIP(), left)); err != nil {
		return codeOutcome{}, err
	}
	signedIn, err := s.startSession(ctx, tx, user, client, MethodRecoveryCode)
	return codeOutcome{CodeSignIn: CodeSignIn{SignedIn: signedIn, UsedRecoveryCode: true, RecoveryCodesLeft: left}}, err
}

// recordCodeFailure counts a wrong code against the account and audits it.
// It reports whether this failure locked code sign-in.
func (s *Service) recordCodeFailure(ctx context.Context, tx *store.Tx, user store.User, client Client, what string, now time.Time) (bool, error) {
	n, err := tx.RecordCodeFailure(ctx, user.ID, now, lockAfter)
	if err != nil {
		return false, err
	}
	if err := s.audit(ctx, tx, "user:"+user.ID, "auth.sign_in_failed", user.ID,
		fmt.Sprintf("wrong %s from %s on %s, %d in a row", what, client.remoteIP(), client.Site.RPID, n)); err != nil {
		return false, err
	}
	if n != lockAfter || user.CodesLocked != nil {
		return false, nil
	}
	s.logger.WarnContext(ctx, "locked code sign-in after too many wrong codes", slog.String("user", user.ID), slog.String("from", client.Addr))
	err = s.audit(ctx, tx, "system", "totp.lock", user.ID, fmt.Sprintf("%d wrong codes in a row, the last from %s", n, client.remoteIP()))
	return true, err
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
		st = TOTPStatus{Enabled: u.TOTPSecret != nil, RecoveryCodesLeft: n, FailedAttempts: u.CodeFailures, LockedAt: u.CodesLocked}
		return nil
	})
	return st, err
}

// TOTPSetup is a new authenticator secret, to show once.
type TOTPSetup struct {
	Secret string // base32
	URI    string // otpauth://
}

// BeginTOTP makes a new authenticator secret for the principal. It works
// only once ConfirmTOTP checks a code from it; until then any current
// secret keeps working.
func (s *Service) BeginTOTP(ctx context.Context, p Principal) (TOTPSetup, error) {
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
// and recovery codes. It refuses when no passkey would be left to sign in
// with.
func (s *Service) DisableTOTP(ctx context.Context, p Principal) error {
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
		usable := 0
		for i := range all {
			if all[i].CloneWarningAt == nil {
				usable++
			}
		}
		if usable == 0 {
			return ErrLastWayIn
		}
		if err := tx.DisableTOTP(ctx, u.ID); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Actor(), "totp.disable", u.ID, "")
	})
}

// RegenerateRecoveryCodes replaces the principal's recovery codes.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, p Principal) ([]string, error) {
	var codes []string
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		u, err := tx.GetUser(ctx, p.User.ID)
		if err != nil {
			return err
		}
		if u.TOTPSecret == nil {
			return ErrNoTOTP
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

// UnlockCodes turns code sign-in back on after a lock.
func (s *Service) UnlockCodes(ctx context.Context, p Principal) error {
	return s.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.UnlockCodes(ctx, p.User.ID); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Actor(), "totp.unlock", p.User.ID, "")
	})
}
