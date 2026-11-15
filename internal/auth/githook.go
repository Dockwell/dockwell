// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

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
	// ErrHookNotYours says someone a hook doesn't act for chose whether its pushes
	// wait, which is its maker's alone, as nobody widens a credential for someone
	// else (ADR 0071).
	ErrHookNotYours = errors.New("this hook acts for someone else, and only they choose whether its pushes wait, so nothing changed. Give it a new secret to make it yours")
	// ErrHookAdminOnly says a hook's maker who isn't an admin where its app is let
	// it deploy without waiting.
	ErrHookAdminOnly = errors.New("only an admin where the app is lets its pushes deploy without waiting, and you're not one, so each still waits for a person. An admin applies them from the panel")
	// ErrHookRejected is the one answer to a delivery whose signature doesn't
	// match, and to one for a hook there isn't, so it says nothing of which hooks
	// there are.
	ErrHookRejected = errors.New("the delivery's signature or token doesn't match the hook's secret")
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
// that confirmed it for the service. It acts for them, narrowed as an API token
// is to the service's project, as a developer at most, so a push deploys there
// alone, and what's protected waits for a person (ADR 0071).
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
		project, perr := serviceProject(ctx, tx, serviceID)
		if perr != nil {
			return perr
		}
		h.Projects, h.Role = []string{project}, store.RoleDeveloper
		if err := tx.CreateGitHook(ctx, h); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return ErrHookExists
			}
			return err
		}
		// Each hook's row names its service, where it's placed and read, and
		// which outlives the hook; the detail names the hook.
		return s.auditOn(ctx, tx, p.Actor(), "git_hook.create", p.User.ID, serviceID, "hook "+h.ID+", acting for "+p.User.ID)
	})
	if err != nil {
		giveBack()
		return NewGitHook{}, err
	}
	return NewGitHook{Hook: h, Secret: secret}, nil
}

// SetGitHookRole sets the role a service's hook acts as at most in its app's
// project (ADR 0071): developer, as it's made, so a push into a protected
// environment plans and waits for a person, or admin, so it deploys there
// without waiting. It's the hook's maker's choice: letting it deploy without
// waiting takes their being an admin where the app is, and a passkey that
// confirmed it for the service; making it wait again takes neither.
func (s *Service) SetGitHookRole(ctx context.Context, client Client, p Principal, serviceID, role, proof string) (store.GitHook, error) {
	if p.Token != nil || p.Session.ID == "" {
		return store.GitHook{}, ErrNeedsSession
	}
	if role != store.RoleDeveloper && role != store.RoleAdmin {
		return store.GitHook{}, fmt.Errorf("%w: a hook acts as a developer, so a push into a protected environment waits for a person, or as an admin, so it deploys there without waiting", ErrInvalid)
	}
	giveBack := func() {}
	if role == store.RoleAdmin {
		var err error
		if giveBack, err = s.HoldStepUp(ctx, client, p, proof, StepUpWidenGitHook, serviceID); err != nil {
			return store.GitHook{}, err
		}
	}
	var h store.GitHook
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		if h, err = tx.GitHookOf(ctx, serviceID); errors.Is(err, store.ErrNotFound) {
			return ErrNoHook
		} else if err != nil {
			return err
		}
		if h.UserID != p.User.ID {
			return ErrHookNotYours
		}
		if role == store.RoleAdmin {
			project, perr := serviceProject(ctx, tx, serviceID)
			if perr != nil {
				return perr
			}
			grants, gerr := tx.Grants(ctx, p.User.ID)
			if gerr != nil {
				return gerr
			}
			if store.RoleRank(heldIn(grants, project)) < store.RoleRank(store.RoleAdmin) {
				return ErrHookAdminOnly
			}
		}
		h.Role = role
		if err := tx.SetGitHookRole(ctx, h.ID, role); err != nil {
			return err
		}
		detail := "hook " + h.ID + " deploys as a developer at most now, so a push into a protected environment waits for a person"
		if role == store.RoleAdmin {
			detail = "hook " + h.ID + " deploys as an admin at most now, so a push into a protected environment deploys without waiting"
		}
		return s.auditOn(ctx, tx, p.Actor(), "git_hook.role", p.User.ID, serviceID, detail)
	})
	if err != nil {
		giveBack()
		return store.GitHook{}, err
	}
	return h, nil
}

// heldIn is the role grants hold in a project: the higher of the install's and
// the project's own.
func heldIn(grants []store.Grant, projectID string) string {
	held := roleAt(grants, store.Install)
	if r := roleAt(grants, projectID); store.RoleRank(r) > store.RoleRank(held) {
		held = r
	}
	return held
}

// serviceProject is the project a service is in: its environment's.
func serviceProject(ctx context.Context, tx *store.Tx, serviceID string) (string, error) {
	service, err := tx.GetResourceByID(ctx, serviceID)
	if err != nil {
		return "", fmt.Errorf("can't read service %s: %w", serviceID, err)
	}
	environment, err := tx.GetResourceByID(ctx, service.Scope)
	if err != nil {
		return "", fmt.Errorf("can't read the environment of service %s: %w", serviceID, err)
	}
	return environment.Scope, nil
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
		// The hook acts for whoever holds its secret now, as nobody makes a
		// credential for someone else (ADR 0049). Whether its pushes wait is its
		// maker's choice, so a new maker chooses again (ADR 0071).
		if h.UserID != p.User.ID && h.Role == store.RoleAdmin {
			h.Role = store.RoleDeveloper
			if err := tx.SetGitHookRole(ctx, h.ID, h.Role); err != nil {
				return err
			}
		}
		h.SecretAt, h.UserID = s.now(), p.User.ID
		h.Secret = s.key.Seal(purposeHook(h.ID), []byte(secret))
		if err := tx.SetGitHookSecret(ctx, h.ID, h.UserID, h.Secret, h.SecretAt); err != nil {
			return err
		}
		return s.auditOn(ctx, tx, p.Actor(), "git_hook.rotate", p.User.ID, serviceID, "a new secret for hook "+h.ID+", which acts for "+p.User.ID+" now")
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
		return s.auditOn(ctx, tx, p.Actor(), "git_hook.delete", p.User.ID, serviceID, "hook "+h.ID+" deleted")
	})
}

// auditHooksEnded puts each hook that ended with its maker's access in the audit
// log, as turning one off is: why says how their access ended.
func (s *Service) auditHooksEnded(ctx context.Context, tx *store.Tx, p Principal, userID string, hooks []store.GitHook, why string) error {
	for i := range hooks {
		if err := s.auditOn(ctx, tx, p.Actor(), "git_hook.delete", userID, hooks[i].ResourceID, "hook "+hooks[i].ID+" deleted, as "+why); err != nil {
			return err
		}
	}
	return nil
}

// TakeHookDelivery spends one of the deliveries a source address may send to
// hookID, and one of all it may send, as it starts sending one, before anything
// reads it, and reports false when either has none. An IPv6 /64 counts as one
// address. Keyed by hook as well as address, a Git host's shared senders are held
// to each hook's own share; the address's own bound is for deliveries whose proof
// doesn't match, as a good one gives its share back (ReturnHookDelivery).
func (s *Service) TakeHookDelivery(client Client, hookID string) bool {
	now, source := s.now(), sourceKey(client.Addr)
	// The ID is the sender's to choose: only as much of it as a hook's keys it.
	return s.hookFloods.take(source, now) && s.hookSources.take(source+" "+hookKey(hookID), now)
}

// hookKey is as much of a hook ID as a bucket is keyed by: one far longer is no
// hook's, and keyed so, as all those are, it can't grow the buckets' keys.
func hookKey(hookID string) string {
	if len(hookID) > 64 {
		return "long"
	}
	return hookID
}

// HookProof is how a delivery shows it knows its hook's secret: the HMAC-SHA-256
// of its body under the secret, as GitHub, Gitea, Forgejo and Bitbucket send, or
// the secret itself, as GitLab sends in X-Gitlab-Token.
type HookProof struct {
	Signature []byte
	Token     string
}

// GitHookPrincipal checks a delivery's proof against the hook's secret, in
// constant time, and gives who it acts as: the person who made the hook, with an
// API token's rights, as the hook, narrowed to its projects and role as a token
// is (ADR 0071). A hook there isn't, or whose person is gone or
// removed, gets the same reads, a decrypt of a decoy and the same answer as a
// wrong proof, ErrHookRejected, so neither the answer nor its time says which IDs
// are hooks. Only a delivery whose proof matches spends from the hook's 60 a
// minute, so nobody who merely knows its URL can use them up; wrong proofs are
// bounded by their address's bucket. A refused proof is in the audit log at most
// once an hour for each hook.
func (s *Service) GitHookPrincipal(ctx context.Context, hookID string, body []byte, proof HookProof) (Principal, error) {
	now := s.now()
	var h store.GitHook
	var u store.User
	found := false
	err := s.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if h, err = tx.GetGitHook(ctx, hookID); errors.Is(err, store.ErrNotFound) {
			h = store.GitHook{}
		} else if err != nil {
			return err
		}
		// A maker is read whether or not there's a hook, as one is, so the time
		// a delivery takes says nothing of whether its ID is a hook's.
		if u, err = tx.GetUser(ctx, cmp.Or(h.UserID, "usr_none")); errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		found = h.ID != "" && u.RemovedAt == nil
		return nil
	})
	if err != nil {
		return Principal{}, err
	}
	sealed, purpose := s.hookDecoySealed, purposeHook("decoy")
	if h.ID != "" {
		sealed, purpose = h.Secret, purposeHook(h.ID)
	}
	key := s.hookDecoy
	secret, openErr := s.key.Open(purpose, sealed)
	switch {
	case found && openErr != nil:
		s.logger.WarnContext(ctx, "can't open a hook's secret, so its deliveries are refused", slog.String("hook", h.ID), slog.String("error", openErr.Error()))
		found = false
	case found:
		key = secret
	}
	if !proof.matches(key, body) || !found {
		if found {
			return Principal{}, &HookRefusal{s: s, hook: h, at: now}
		}
		return Principal{}, ErrHookRejected
	}
	if !s.hookBuckets.take("hook:"+h.ID, now) {
		return Principal{}, ErrTooManyRequests
	}
	return Principal{
		User:  u,
		Token: &store.APIToken{ID: h.ID, UserID: u.ID, Name: "Deploy on push", Scope: store.ScopeReadWrite, Projects: h.Projects, Role: h.Role},
		Hook:  &h,
	}, nil
}

// HookRefusal is a refused delivery to a hook there is: ErrHookRejected, with its
// record for the audit log, which the caller writes once it has answered, so a
// refusal is answered as soon as one for a hook there isn't (#253's re-check, N5).
type HookRefusal struct {
	s    *Service
	hook store.GitHook
	at   time.Time
}

func (r *HookRefusal) Error() string { return ErrHookRejected.Error() }

func (r *HookRefusal) Unwrap() error { return ErrHookRejected }

// Record writes the refusal to the audit log, at most once an hour for each hook.
func (r *HookRefusal) Record(ctx context.Context) {
	if _, report := r.s.refusals.count(r.hook.ID, r.at); !report {
		return
	}
	h := r.hook
	if err := r.s.db.Update(ctx, func(tx *store.Tx) error {
		return r.s.auditOn(ctx, tx, "hook:"+h.ID, "git_hook.refused", h.UserID, h.ResourceID, "a delivery to hook "+h.ID+" whose signature or token doesn't match its secret")
	}); err != nil {
		r.s.logger.WarnContext(ctx, "can't record a refused delivery", slog.String("hook", h.ID), slog.String("error", err.Error()))
	}
}

// ReturnHookDelivery gives back what a delivery spent of its address's own bound,
// once its proof matched: GitHub, GitLab.com and Bitbucket send every install's
// hooks from shared addresses, so only wrong proofs count against one. Its share
// of its hook's stays spent.
func (s *Service) ReturnHookDelivery(client Client) {
	s.hookFloods.give(sourceKey(client.Addr), s.now())
}

// matches reports whether a proof is of key: a signature of body under it, or a
// token that is it. Either is compared in constant time, and a token by its own
// HMAC under key, so the time taken doesn't depend on how much of it is right,
// nor on its length.
func (p HookProof) matches(key, body []byte) bool {
	mac := hmac.New(sha256.New, key)
	if p.Token == "" {
		mac.Write(body)
		return hmac.Equal(mac.Sum(nil), p.Signature)
	}
	mac.Write([]byte(p.Token))
	got := mac.Sum(nil)
	mac.Reset()
	mac.Write(key)
	return hmac.Equal(got, mac.Sum(nil))
}

// decoyKey is a key as long as a hook's secret, so a delivery for a hook there
// isn't takes the work one for a hook does.
func decoyKey() []byte { return []byte(newHookSecret()) }
