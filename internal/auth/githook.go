// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// A service's hook deploys it on a push from GitHub, Gitea or Forgejo: a
// credential, as an API token is, made by a person with a passkey, acting for them
// with an API token's rights. See docs/adr/0046-builds-from-git.md.

var (
	// ErrHookExists says a service deploys on push already.
	ErrHookExists = errors.New("this service deploys on push already. Give its hook a new secret, or turn deploy on push off first")
	// ErrNoHook says a service doesn't deploy on push.
	ErrNoHook = errors.New("this service doesn't deploy on push. Turn it on first")
	// ErrHookRejected is the one answer to a delivery whose signature doesn't
	// match, and to one for a hook there isn't, so it says nothing of which hooks
	// there are.
	ErrHookRejected = errors.New("the delivery's signature doesn't match the hook's secret")
)

// hookSecretBytes is how much a hook's secret holds, in random bytes.
const hookSecretBytes = 32

func purposeHook(hookID string) string { return "git-hook:" + hookID }

// NewGitHook is a hook just made, or given a new secret, with that secret, which
// nothing else has.
type NewGitHook struct {
	Hook   store.GitHook
	Secret string
}

func newHookSecret() string {
	b := make([]byte, hookSecretBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateGitHook makes a service's hook, for a person, signed in, with a passkey
// that confirmed it for the service. It acts for them.
func (s *Service) CreateGitHook(ctx context.Context, client Client, p Principal, serviceID, proof string) (NewGitHook, error) {
	if p.Token != nil || p.Session.ID == "" {
		return NewGitHook{}, ErrNeedsSession
	}
	giveBack, err := s.HoldStepUp(ctx, client, p, proof, StepUpCreateGitHook, serviceID)
	if err != nil {
		return NewGitHook{}, err
	}
	now, secret := s.now(), newHookSecret()
	h := store.GitHook{ID: id.New(id.GitHook), ResourceID: serviceID, UserID: p.User.ID, CreatedAt: now, SecretAt: now}
	h.Secret = s.key.Seal(purposeHook(h.ID), []byte(secret))
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.CreateGitHook(ctx, h); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return ErrHookExists
			}
			return err
		}
		return s.auditOn(ctx, tx, p.Actor(), "git_hook.create", p.User.ID, h.ID, "deploy on push for "+serviceID+", acting for "+p.User.ID)
	})
	if err != nil {
		giveBack()
		return NewGitHook{}, err
	}
	return NewGitHook{Hook: h, Secret: secret}, nil
}

// RotateGitHookSecret gives a service's hook a new secret, for a person, signed in,
// with a passkey that confirmed it for the service. The old one stops working.
func (s *Service) RotateGitHookSecret(ctx context.Context, client Client, p Principal, serviceID, proof string) (NewGitHook, error) {
	if p.Token != nil || p.Session.ID == "" {
		return NewGitHook{}, ErrNeedsSession
	}
	giveBack, err := s.HoldStepUp(ctx, client, p, proof, StepUpRotateGitHook, serviceID)
	if err != nil {
		return NewGitHook{}, err
	}
	secret := newHookSecret()
	var h store.GitHook
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		if h, err = tx.GitHookOf(ctx, serviceID); errors.Is(err, store.ErrNotFound) {
			return ErrNoHook
		} else if err != nil {
			return err
		}
		h.SecretAt = s.now()
		h.Secret = s.key.Seal(purposeHook(h.ID), []byte(secret))
		if err := tx.SetGitHookSecret(ctx, h.ID, h.Secret, h.SecretAt); err != nil {
			return err
		}
		return s.auditOn(ctx, tx, p.Actor(), "git_hook.rotate", p.User.ID, h.ID, "a new secret for the hook of "+serviceID)
	})
	if err != nil {
		giveBack()
		return NewGitHook{}, err
	}
	return NewGitHook{Hook: h, Secret: secret}, nil
}

// DeleteGitHook turns deploy on push off for a service. Taking a way in away needs
// no passkey, so a write token may.
func (s *Service) DeleteGitHook(ctx context.Context, p Principal, serviceID string) error {
	return s.db.Update(ctx, func(tx *store.Tx) error {
		h, err := tx.GitHookOf(ctx, serviceID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrNoHook
		} else if err != nil {
			return err
		}
		if err := tx.DeleteGitHook(ctx, h.ID); err != nil {
			return err
		}
		return s.auditOn(ctx, tx, p.Actor(), "git_hook.delete", p.User.ID, h.ID, "deploy on push off for "+serviceID)
	})
}

// TakeHookDelivery spends one of a source address's deliveries, as it starts
// sending one, before anything reads it, and reports false when it has none. An
// IPv6 /64 counts as one address.
func (s *Service) TakeHookDelivery(client Client) bool {
	return s.hookSources.take(sourceKey(client.Addr), s.now())
}

// GitHookPrincipal checks a delivery's signature, the HMAC-SHA-256 of body under the
// hook's secret, in constant time, and gives who it acts as: the person who made
// the hook, with an API token's rights, as the hook. A hook there isn't, or whose
// person is gone, gets the same work with a key of its own and the same answer as
// a wrong signature, ErrHookRejected. A hook may take 60 deliveries a minute; a
// refused signature is in the audit log at most once an hour for each hook.
func (s *Service) GitHookPrincipal(ctx context.Context, hookID string, body, signature []byte) (Principal, error) {
	now := s.now()
	var h store.GitHook
	var u store.User
	found := false
	err := s.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if h, err = tx.GetGitHook(ctx, hookID); errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		if u, err = tx.GetUser(ctx, h.UserID); errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return Principal{}, err
	}
	if found && !s.hookBuckets.take("hook:"+h.ID, now) {
		return Principal{}, ErrTooManyRequests
	}
	key := s.hookDecoy
	if found {
		secret, err := s.key.Open(purposeHook(h.ID), h.Secret)
		if err != nil {
			s.logger.WarnContext(ctx, "can't open a hook's secret, so its deliveries are refused", slog.String("hook", h.ID), slog.String("error", err.Error()))
			found = false
		} else {
			key = secret
		}
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), signature) || !found {
		if found {
			if _, report := s.refusals.count(h.ID, now); report {
				if err := s.db.Update(ctx, func(tx *store.Tx) error {
					return s.auditOn(ctx, tx, "hook:"+h.ID, "git_hook.refused", h.UserID, h.ID, "a delivery whose signature doesn't match the hook's secret")
				}); err != nil {
					s.logger.WarnContext(ctx, "can't record a refused delivery", slog.String("hook", h.ID), slog.String("error", err.Error()))
				}
			}
		}
		return Principal{}, ErrHookRejected
	}
	return Principal{
		User:  u,
		Token: &store.APIToken{ID: h.ID, UserID: u.ID, Name: "Deploy on push", Scope: store.ScopeReadWrite},
		Hook:  &h,
	}, nil
}

// decoyKey is a key as long as a hook's secret, so a delivery for a hook there
// isn't takes the work one for a hook does.
func decoyKey() []byte { return []byte(newHookSecret()) }
