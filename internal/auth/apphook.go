// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"cmp"
	"context"
	"errors"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// A hook whose pushes come through a GitHub App (ADR 0098) is a hook as any other:
// it acts for the person who turned deploy on push on, narrowed as slice 1's are
// to its service's project and the developer role, so a push into a protected
// environment waits for a person until its maker, an admin there, lets it deploy
// without waiting (ADR 0071); it ends with their access, and its pushes queue and
// apply as slice 1's do. Only how a push is proved differs: by the App's webhook
// secret, which the App's receiver checks, not by a secret of the hook's own,
// which nobody is shown.

// CreateAppGitHook turns deploy on push on for a service whose repository a GitHub
// App reaches, for a person, signed in, with a passkey that confirmed it for the
// service, narrowed as CreateGitHook narrows a hook. Its pushes come through the
// App.
func (s *Service) CreateAppGitHook(ctx context.Context, client Client, p Principal, serviceID, proof string) (store.GitHook, error) {
	if p.Token != nil || p.Session.ID == "" {
		return store.GitHook{}, ErrNeedsSession
	}
	giveBack, err := s.HoldStepUp(ctx, client, p, proof, StepUpCreateGitHook, serviceID)
	if err != nil {
		return store.GitHook{}, err
	}
	now := s.now()
	h := store.GitHook{ID: id.New(id.GitHook), ResourceID: serviceID, UserID: p.User.ID, CreatedAt: now, SecretAt: now, Via: store.ViaGitHub}
	// A secret nobody is shown, so the hook is like any other where hooks are read.
	h.Secret = s.key.Seal(purposeHook(h.ID), []byte(newHookSecret()))
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		project, perr := serviceProject(ctx, tx, serviceID)
		if perr != nil {
			return perr
		}
		h.Projects, h.Role = projectsOf(project), store.RoleDeveloper
		if err := tx.CreateGitHook(ctx, h); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return ErrHookExists
			}
			return err
		}
		return s.auditOn(ctx, tx, p.Actor(), "git_hook.create", p.User.ID, serviceID, "hook "+h.ID+", through the GitHub App, acting for "+p.User.ID)
	})
	if err != nil {
		giveBack()
		return store.GitHook{}, err
	}
	return h, nil
}

// AppHookPrincipal is who a push through a GitHub App acts as, for one of the hooks
// it reaches: the hook's maker, with an API token's rights narrowed to the hook's
// projects and role, as GitHookPrincipal gives once a hook's own proof matched. The App's receiver checked the App's
// signature first. A hook there isn't, one whose pushes don't come through an App,
// and one whose maker is removed act for nobody: ErrHookRejected.
func (s *Service) AppHookPrincipal(ctx context.Context, hookID string) (Principal, error) {
	now := s.now()
	var h store.GitHook
	var u store.User
	err := s.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if h, err = tx.GetGitHook(ctx, hookID); errors.Is(err, store.ErrNotFound) {
			return ErrHookRejected
		} else if err != nil {
			return err
		}
		if u, err = tx.GetUser(ctx, cmp.Or(h.UserID, "usr_none")); errors.Is(err, store.ErrNotFound) {
			return ErrHookRejected
		}
		return err
	})
	switch {
	case err != nil:
		return Principal{}, err
	case h.Via != store.ViaGitHub || u.RemovedAt != nil:
		return Principal{}, ErrHookRejected
	case !s.hookBuckets.take("hook:"+h.ID, now):
		return Principal{}, ErrTooManyRequests
	}
	return Principal{
		User:  u,
		Token: &store.APIToken{ID: h.ID, UserID: u.ID, Name: "Deploy on push", Scope: store.ScopeReadWrite, Projects: h.Projects, Role: h.Role},
		Hook:  &h,
	}, nil
}
