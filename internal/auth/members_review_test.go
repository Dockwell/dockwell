// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// proofHeld reports whether a step-up's proof is still there to spend.
func (f *fixture) proofHeld(proof string) bool {
	s := f.svc.stepUps
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[hex.EncodeToString(hashToken(proof))]
	return ok
}

// openInvitesBy counts the open invites a person made.
func (f *invited) openInvitesBy(t *testing.T, userID string) int {
	t.Helper()
	n := 0
	if err := f.db.View(t.Context(), func(tx *store.Tx) error {
		open, err := tx.ListOpenInvites(t.Context(), f.clock.now())
		for i := range open {
			if open[i].CreatedBy == "user:"+userID {
				n++
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// An invite asked for by an admin who's removed, or lowered, before its write
// is refused there: the sweep that revoked what they'd made has run, so nothing
// else would stop it. So is revoking one. Racing the removal leaves no open
// invite of theirs either way (ADR 0049, the members review's M1).
func TestAnInviteInFlightDiesWithItsMaker(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		ctx := t.Context()
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleAdmin, "203.0.113.5:1")
		sam := f.join(t, "sam@example.com", store.RoleAdmin, "203.0.113.6:1")
		owners, _ := f.invite(t, "kim@example.com", store.RoleViewer)
		// Each asked to invite someone, with a passkey for it, and passed the API's gate.
		adaTarget := InviteTarget("lin@example.com", store.RoleAdmin, store.Install)
		adaProof := f.stepUpAt(t, onDomain, ada.p, ada.keys, StepUpInvite, adaTarget).Proof
		samTarget := InviteTarget("lee@example.com", store.RoleViewer, store.Install)
		samProof := f.stepUpAt(t, onDomain, sam.p, sam.keys, StepUpInvite, samTarget).Proof

		// Then the owner removes ada and lowers sam, before either invite's write.
		remove := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, ada.p.User.ID).Proof
		if _, err := f.svc.RemoveMember(ctx, onDomain, f.owner, ada.p.User.ID, remove); err != nil {
			t.Fatal(err)
		}
		if _, err := f.setRole(t, sam.p.User.ID, store.RoleDeveloper); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.CreateInvite(ctx, onDomain, ada.p, "lin@example.com", store.RoleAdmin, store.Install, adaProof); !errors.Is(err, ErrAdminOnly) {
			t.Errorf("an invite from ada, removed before its write: %v", err)
		}
		if _, err := f.svc.CreateInvite(ctx, onDomain, sam.p, "lee@example.com", store.RoleViewer, store.Install, samProof); !errors.Is(err, ErrAdminOnly) {
			t.Errorf("an invite from sam, lowered before its write: %v", err)
		}
		if err := f.svc.RevokeInvite(ctx, onDomain, ada.p, owners.ID); !errors.Is(err, ErrAdminOnly) {
			t.Errorf("ada, removed, revoking the owner's invite: %v", err)
		}
		if n := f.openInvites(t); n != 1 {
			t.Errorf("%d open invites, want the owner's alone", n)
		}

		// Racing: whichever commits first, no invite of rob's is left open.
		rob := f.join(t, "rob@example.com", store.RoleAdmin, "203.0.113.7:1")
		robTarget := InviteTarget("nia@example.com", store.RoleViewer, store.Install)
		robProof := f.stepUpAt(t, onDomain, rob.p, rob.keys, StepUpInvite, robTarget).Proof
		removeRob := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, rob.p.User.ID).Proof
		var wg sync.WaitGroup
		wg.Go(func() {
			_, _ = f.svc.CreateInvite(context.Background(), onDomain, rob.p, "nia@example.com", store.RoleViewer, store.Install, robProof)
		})
		wg.Go(func() {
			if _, err := f.svc.RemoveMember(context.Background(), onDomain, f.owner, rob.p.User.ID, removeRob); err != nil {
				t.Errorf("removing rob: %v", err)
			}
		})
		wg.Wait()
		if n := f.openInvitesBy(t, rob.p.User.ID); n != 0 {
			t.Errorf("rob has %d open invites after the race with his removal", n)
		}
	})
}

// An invite works only while whoever made it may still invite: one left by a
// maker who's removed, or no longer an admin, however it outlived them, is
// refused as any invite that doesn't work is, and the owner's still works
// (ADR 0049, the members review's M1).
func TestAnInviteWorksOnlyWhileItsMakerMayInvite(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		ctx := t.Context()
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleAdmin, "203.0.113.5:1")
		sam := f.join(t, "sam@example.com", store.RoleAdmin, "203.0.113.6:1")
		remove := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, ada.p.User.ID).Proof
		if _, err := f.svc.RemoveMember(ctx, onDomain, f.owner, ada.p.User.ID, remove); err != nil {
			t.Fatal(err)
		}
		if _, err := f.setRole(t, sam.p.User.ID, store.RoleDeveloper); err != nil {
			t.Fatal(err)
		}
		// What a race could have left: an invite of each, open.
		plant := func(email, maker string) string {
			t.Helper()
			secret, hash := newToken()
			now := f.clock.now()
			if err := f.db.Update(ctx, func(tx *store.Tx) error {
				return tx.CreateInvite(ctx, store.Invite{
					ID: id.New(id.Invite), TokenHash: hash, Email: email, Role: store.RoleViewer, ProjectID: store.Install,
					CreatedAt: now, CreatedBy: maker, ExpiresAt: now.Add(time.Hour),
				})
			}); err != nil {
				t.Fatal(err)
			}
			return secret
		}
		for what, maker := range map[string]string{
			"removed":            "user:" + ada.p.User.ID,
			"no longer an admin": "user:" + sam.p.User.ID,
		} {
			secret := plant(strings.ReplaceAll(what, " ", "-")+"@example.com", maker)
			if _, err := f.svc.CheckInvite(ctx, onDomain, secret); !errors.Is(err, ErrInviteRejected) {
				t.Errorf("opening an invite whose maker is %s: %v", what, err)
			}
			if _, err := f.svc.BeginAcceptInvite(ctx, onDomain, secret, "Lin"); !errors.Is(err, ErrInviteRejected) {
				t.Errorf("accepting an invite whose maker is %s: %v", what, err)
			}
		}
		_, secret := f.invite(t, "kim@example.com", store.RoleViewer)
		if _, err := f.svc.CheckInvite(ctx, onDomain, secret); err != nil {
			t.Errorf("the owner's invite: %v", err)
		}
	})
}

// Someone removed is refused by every way a request or a stream is checked,
// whatever was left behind: a session or a token nobody revoked, as a race could
// leave. A token asked for before the removal isn't made after it (the members
// review's L4).
func TestARemovedPersonIsRefusedEverywhere(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		ctx := t.Context()
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleDeveloper, "203.0.113.5:1")
		userID := ada.p.User.ID
		late := f.stepUpAt(t, onDomain, ada.p, ada.keys, StepUpCreateToken, "").Proof
		remove := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, userID).Proof
		if _, err := f.svc.RemoveMember(ctx, onDomain, f.owner, userID, remove); err != nil {
			t.Fatal(err)
		}

		now := f.clock.now()
		sessionSecret, sessionHash := newToken()
		left := ada.p.Session
		left.ID, left.TokenHash, left.RevokedAt = "ses_left", sessionHash, nil
		left.CreatedAt, left.LastSeenAt, left.ExpiresAt = now, now, now.Add(time.Hour)
		tokenSecret, tokenHash := newTokenSecret()
		tok := store.APIToken{
			ID: "tok_left", TokenHash: tokenHash, UserID: userID, Name: "left", Hint: TokenHint(tokenSecret),
			Scope: store.ScopeReadOnly, CreatedAt: now,
		}
		if err := f.db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.CreateSession(ctx, left); err != nil {
				return err
			}
			return tx.CreateAPIToken(ctx, tok)
		}); err != nil {
			t.Fatal(err)
		}

		if _, err := f.svc.Authenticate(ctx, onDomain, sessionSecret); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("a session left behind for someone removed: %v", err)
		}
		if _, err := f.svc.AuthenticateToken(ctx, onDomain, tokenSecret, AccessRead, "/dockwell.v1.ServiceService/ListServices"); !errors.Is(err, ErrTokenRejected) {
			t.Errorf("a token left behind for someone removed: %v", err)
		}
		bySession, stop := f.svc.Track(ctx, onDomain, Principal{User: ada.p.User, Session: left})
		defer stop()
		byToken, stopToken := f.svc.Track(ctx, onDomain, Principal{User: ada.p.User, Token: &tok})
		defer stopToken()
		if !endsAtOnce(bySession) || !endsAtOnce(byToken) {
			t.Error("a stream on what was left behind for someone removed stays open")
		}

		if _, err := f.svc.CreateToken(ctx, onDomain, ada.p, "late", store.ScopeReadOnly, nil, nil, late); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("a token asked for before ada was removed, made after: %v", err)
		}
		if err := f.db.View(ctx, func(tx *store.Tx) error {
			live, err := tx.ListLiveAPITokens(ctx, userID, f.clock.now())
			if slices.ContainsFunc(live, func(t store.APIToken) bool { return t.Name == "late" }) {
				return errors.New("a token named late was made")
			}
			return err
		}); err != nil {
			t.Error(err)
		}
	})
}

// A removal says what its transaction ended, as its audit event does: a token
// that expired without being revoked is revoked too, and counted, though the
// plan listed the live ones alone (the members review's L1).
func TestARemovalSaysWhatItEnded(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		ctx := t.Context()
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleDeveloper, "203.0.113.5:1")
		f.token(t, ada)
		soon := f.clock.now().Add(time.Hour)
		proof := f.stepUpAt(t, onDomain, ada.p, ada.keys, StepUpCreateToken, "").Proof
		if _, err := f.svc.CreateToken(ctx, onDomain, ada.p, "expiring", store.ScopeReadOnly, nil, &soon, proof); err != nil {
			t.Fatal(err)
		}
		f.clock.advance(2 * time.Hour)
		remove := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, ada.p.User.ID).Proof
		gone, err := f.svc.RemoveMember(ctx, onDomain, f.owner, ada.p.User.ID, remove)
		if err != nil {
			t.Fatal(err)
		}
		if len(gone.Tokens) != 1 || gone.Ended.Tokens != 2 {
			t.Errorf("the removal planned %d tokens and ended %d; want the live one planned, and both revoked", len(gone.Tokens), gone.Ended.Tokens)
		}
		if d := f.auditDetails(t, "member.remove"); len(d) != 1 || !strings.Contains(d[0], `"tokens":2`) {
			t.Errorf("the audit log says of the removal: %v", d)
		}
	})
}

// The changes someone planned with any token they made are theirs to lose with
// them, whatever the token's state now: one planned with a token they revoked
// since is in the plan and in what the removal hands back to discard (the
// members review's L2).
func TestARemovalTakesWhatAnyOfTheirTokensPlanned(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		ctx := t.Context()
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleDeveloper, "203.0.113.5:1")
		tok := f.token(t, ada)
		now := f.clock.now()
		if err := f.db.Revise(ctx, func(tx *store.RevisionTx) error {
			for id, actor := range map[string]string{"chg_by_token": "token:" + tok.Token.ID, "chg_by_ada": "user:" + ada.p.User.ID} {
				if err := tx.CreateChange(ctx, store.Change{
					ID: id, Summary: id, Actor: actor, State: store.ChangePlanned, Fingerprint: "f", Plan: []byte("{}"),
					CreatedAt: now, Items: []store.ChangeItem{{Kind: "project", Scope: "org_1", Name: id, Spec: []byte{1}}},
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.db.Update(ctx, func(tx *store.Tx) error { return tx.RevokeAPIToken(ctx, ada.p.User.ID, tok.Token.ID, now) }); err != nil {
			t.Fatal(err)
		}
		ids := func(changes []store.Change) []string {
			var out []string
			for i := range changes {
				out = append(out, changes[i].ID)
			}
			slices.Sort(out)
			return out
		}
		want := []string{"chg_by_ada", "chg_by_token"}
		plan, err := f.svc.PlanMemberRemoval(ctx, f.owner, ada.p.User.ID)
		if err != nil || !slices.Equal(ids(plan.Changes), want) {
			t.Errorf("the plan to remove ada lists %v, %v; want %v", ids(plan.Changes), err, want)
		}
		remove := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, ada.p.User.ID).Proof
		gone, err := f.svc.RemoveMember(ctx, onDomain, f.owner, ada.p.User.ID, remove)
		if err != nil || !slices.Equal(ids(gone.Changes), want) {
			t.Errorf("removing ada hands back %v, %v; want %v", ids(gone.Changes), err, want)
		}
	})
}

// A member action refused in its write spends no passkey: the proof is spent
// there, once everything else holds, so whoever asked can try again with it.
// Here ada is removed in a write that holds the store while a change to her role
// comes in (the members review's L3).
func TestARefusalInTheWriteKeepsTheProof(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleViewer, "203.0.113.5:1")
		userID := ada.p.User.ID
		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeRole, RoleTarget(userID, store.RoleDeveloper, store.Install)).Proof
		holding, release := make(chan struct{}), make(chan struct{})
		removed := make(chan error, 1)
		go func() {
			removed <- f.db.Update(context.Background(), func(tx *store.Tx) error {
				close(holding)
				<-release
				_, err := tx.RemoveUser(context.Background(), userID, f.owner.Actor(), f.clock.now())
				return err
			})
		}()
		<-holding
		changed := make(chan error, 1)
		go func() {
			_, err := f.svc.SetMemberRole(context.Background(), onDomain, f.owner, userID, store.RoleDeveloper, store.Install, proof)
			changed <- err
		}()
		// Long enough for anything outside the write to have read ada and spent the proof.
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && f.proofHeld(proof); {
			time.Sleep(10 * time.Millisecond)
		}
		close(release)
		if err := <-removed; err != nil {
			t.Fatal(err)
		}
		if err := <-changed; !errors.Is(err, ErrNotFound) {
			t.Fatalf("changing the role of ada, removed meanwhile: %v", err)
		}
		if !f.proofHeld(proof) {
			t.Error("the change was refused in its write, and its passkey's proof is spent")
		}
	})
}

// Each link a removal deletes is on record as an unlink is, by provider and
// account (ADR 0095, the members review's L5).
func TestARemovalRecordsEachLink(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		s := standOver(t, f)
		ada := f.join(t, "ada@example.com", store.RoleDeveloper, "203.0.113.5:1")
		linkAs(t, s, ada, "ada-at-ours")
		remove := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, ada.p.User.ID).Proof
		if _, err := f.svc.RemoveMember(t.Context(), onDomain, f.owner, ada.p.User.ID, remove); err != nil {
			t.Fatal(err)
		}
		d := f.auditDetails(t, "provider_link.delete")
		if len(d) != 1 || !strings.Contains(d[0], "at Our Authentik") || !strings.Contains(d[0], "with its person removed") {
			t.Errorf("the audit log says of the links: %v; want one line for ada's at Our Authentik", d)
		}
	})
}

// The first confirmation of the protections is kept: confirming again changes
// nothing, reads nothing and says who confirmed first, and when (the members
// review's L7).
func TestTheFirstConfirmationIsKept(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newFixture(t, backend, onDomain.Site.RPID)
		owner := ownerPrincipal(f.claim(t, onDomain, &authtest.Authenticator{Counter: true}))
		first, err := f.svc.ConfirmProtections(t.Context(), onDomain, owner, standingOf("default/production protected"))
		if err != nil {
			t.Fatal(err)
		}
		f.clock.advance(time.Hour)
		again, err := f.svc.ConfirmProtections(t.Context(), onDomain, owner, func(*store.Tx) (string, error) {
			t.Error("confirming again read how the environments stand")
			return "default/production open", nil
		})
		if err != nil || !again.At.Equal(first.At) || again.By != first.By {
			t.Errorf("confirming again reads %+v, %v; want the first, %+v", again, err, first)
		}
		if d := f.auditDetails(t, "member.protections_confirm"); len(d) != 1 || d[0] != "default/production protected" {
			t.Errorf("the audit log says of the confirmations: %v; want the first alone", d)
		}
	})
}

// An admin holds the whole install, so nobody gives them a project's role
// beside it, which would outlast a later lowering nobody chose it for; the
// refusal spends no passkey (the members review's L9).
func TestAnAdminHoldsNoProjectRole(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleAdmin, "203.0.113.5:1")
		target := RoleTarget(ada.p.User.ID, store.RoleDeveloper, "prj_1")
		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeRole, target).Proof
		if _, err := f.svc.SetMemberRole(t.Context(), onDomain, f.owner, ada.p.User.ID, store.RoleDeveloper, "prj_1", proof); !errors.Is(err, ErrInvalid) {
			t.Errorf("giving an admin a project's role: %v", err)
		}
		if got := f.grants(t, ada.p.User.ID); len(got) != 1 || got[0] != "=admin" {
			t.Errorf("ada holds %v, want the install as an admin alone", got)
		}
		if !f.proofHeld(proof) {
			t.Error("the refusal spent the passkey's proof")
		}
	})
}

// Making a token in a session begun with a sign-in provider takes a passkey tap,
// whatever its person chose for how often: such a session has no passkey's hour,
// where one begun with a passkey makes it without one (ADR 0071, ADR 0095).
func TestAProviderSessionMakesATokenOnlyWithAPasskey(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		ctx := t.Context()
		s := newProviderStand(t, backend)
		if _, err := s.link(t, "248289761001"); err != nil {
			t.Fatal(err)
		}
		proof := s.stepUpAt(t, onDomain, s.owner, s.passkeys, StepUpSetPolicy, "").Proof
		if _, err := s.svc.SetStepUpPolicy(ctx, onDomain, s.owner, AskAtSignIn, proof); err != nil {
			t.Fatal(err)
		}
		viaProvider, err := providerSession(t, s, "248289761001")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.svc.CreateToken(ctx, onDomain, viaProvider, "CI", store.ScopeReadOnly, nil, nil, ""); !errors.Is(err, ErrNeedsStepUp) {
			t.Errorf("making a token in a provider's session with no passkey: %v, want it to ask for one", err)
		}
		viaPasskey, err := s.svc.Authenticate(ctx, onDomain, s.ownerToken(t))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.svc.CreateToken(ctx, onDomain, viaPasskey, "CI", store.ScopeReadOnly, nil, nil, ""); err != nil {
			t.Errorf("making a token in a passkey's session, asked at sign-in alone: %v", err)
		}
	})
}
