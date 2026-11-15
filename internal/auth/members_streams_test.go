// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/oidc/oidctest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// standOver puts a sign-in provider on the panel's domain over an invited
// install, as #646's stand does, for a member to link an account and sign in
// with it.
func standOver(t *testing.T, f *invited) *providerStand {
	t.Helper()
	s := &providerStand{fixture: f.fixture, idp: oidctest.NewProvider(t), owner: f.owner, provider: "sip_ours"}
	s.saved = []Provider{{
		ID: "sip_ours", Name: "ours", DisplayName: "Our Authentik", Issuer: s.idp.Issuer(),
		ClientID: oidctest.Client, ClientSecret: oidctest.Secret, Panel: true,
	}}
	f.svc.UseProviders(func(context.Context) ([]Provider, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return append([]Provider(nil), s.saved...), nil
	}, s.idp.Srv.Client())
	return s
}

// linkAs links who's account with subject at the stand's provider, with their passkey.
func linkAs(t *testing.T, s *providerStand, who joined, subject string) {
	t.Helper()
	proof := s.stepUpAt(t, onDomain, who.p, who.keys, StepUpLinkProvider, s.provider).Proof
	target, flow, err := s.svc.BeginLinkProvider(t.Context(), onDomain, who.p, s.provider, proof)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.FinishProvider(t.Context(), onDomain, flow, s.through(t, target, subject)); err != nil {
		t.Fatal(err)
	}
}

// providerSession signs in with the stand's provider as subject, or tries to,
// and returns the session's principal.
func providerSession(t *testing.T, s *providerStand, subject string) (Principal, error) {
	t.Helper()
	target, flow, err := s.svc.BeginProviderSignIn(t.Context(), onDomain, s.provider)
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.svc.FinishProvider(t.Context(), onDomain, flow, s.through(t, target, subject))
	if err != nil {
		return Principal{}, err
	}
	return s.svc.Authenticate(t.Context(), onDomain, done.SignedIn.Token)
}

// endsAtOnce says whether a stream ended within two seconds: well inside any
// recheck, so only what ends it at once does.
func endsAtOnce(stream context.Context) bool {
	select {
	case <-stream.Done():
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

// Removing someone deletes their accounts at sign-in providers, so a provider
// signs them in no more, and ends every stream they have open at once: on a
// passkey's session, on a provider's, and on an API token. Nobody else's ends,
// and the plan said the link would go (ADR 0049, ADR 0095).
func TestRemovingSomeoneEndsTheirLinksAndStreams(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		s := standOver(t, f)
		ada := f.join(t, "ada@example.com", store.RoleDeveloper, "203.0.113.5:1")
		id := ada.p.User.ID
		linkAs(t, s, ada, "ada-at-ours")
		viaProvider, err := providerSession(t, s, "ada-at-ours")
		if err != nil {
			t.Fatal(err)
		}
		tok := f.token(t, ada)
		viaToken, err := f.svc.AuthenticateToken(t.Context(), onDomain, tok.Secret, AccessRead, "/dockwell.v1.ServiceService/ListServices")
		if err != nil {
			t.Fatal(err)
		}
		streams := map[string]context.Context{}
		for what, p := range map[string]Principal{"her passkey's session": ada.p, "her provider's session": viaProvider, "her API token": viaToken} {
			stream, stop := f.svc.Track(t.Context(), onDomain, p)
			defer stop()
			streams[what] = stream
		}
		owners, stop := f.svc.Track(t.Context(), onDomain, f.owner)
		defer stop()
		for what, stream := range streams {
			if stream.Err() != nil {
				t.Fatalf("the stream on %s ended before anything was removed", what)
			}
		}

		plan, err := f.svc.PlanMemberRemoval(t.Context(), f.owner, id)
		if err != nil || plan.Sessions != 2 || len(plan.Links) != 1 || plan.Links[0].Provider != "Our Authentik" || plan.Links[0].Account == "" {
			t.Fatalf("the plan to remove ada reads %d sessions, links %+v, %v; want two, and her link at Our Authentik", plan.Sessions, plan.Links, err)
		}
		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, id).Proof
		if _, err := f.svc.RemoveMember(t.Context(), onDomain, f.owner, id, proof); err != nil {
			t.Fatal(err)
		}
		for what, stream := range streams {
			if !endsAtOnce(stream) {
				t.Errorf("the stream on %s stays open after she was removed", what)
			}
		}
		if owners.Err() != nil {
			t.Error("removing ada ended the owner's stream")
		}

		// Her account at the provider signs nobody in, and her link is gone.
		if _, err := providerSession(t, s, "ada-at-ours"); err == nil {
			t.Error("her account at the provider still signs in after she was removed")
		}
		if err := f.db.View(t.Context(), func(tx *store.Tx) error {
			links, err := tx.ListProviderLinks(t.Context(), id)
			if err != nil || len(links) != 0 {
				t.Errorf("ada has %d provider links after she was removed, %v", len(links), err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if d := f.auditDetails(t, "member.remove"); len(d) != 1 || !strings.Contains(d[0], `"links":1`) || !strings.Contains(d[0], `"sessions":2`) {
			t.Errorf("the audit log says of the removal: %v; want her link and her two sessions", d)
		}
	})
}

// Any change to what someone holds ends their open streams at once, on every
// session and token, as each was narrowed to what they held when it opened: a
// raise as much as a lowering, in a project as on the install. A raise leaves
// them signed in, so a page opens its streams again as they now may. Nobody
// else's stream ends (ADR 0049).
func TestChangingARoleEndsTheirStreams(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleDeveloper, "203.0.113.5:1")
		sam := f.join(t, "sam@example.com", store.RoleViewer, "203.0.113.6:1")
		tok := f.token(t, ada)
		viaToken, err := f.svc.AuthenticateToken(t.Context(), onDomain, tok.Secret, AccessRead, "/dockwell.v1.ServiceService/ListServices")
		if err != nil {
			t.Fatal(err)
		}
		track := func(p Principal) context.Context {
			stream, stop := f.svc.Track(t.Context(), onDomain, p)
			t.Cleanup(stop)
			return stream
		}
		adas, adasToken, sams, owners := track(ada.p), track(viaToken), track(sam.p), track(f.owner)

		if _, err := f.setRole(t, ada.p.User.ID, store.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		if !endsAtOnce(adas) || !endsAtOnce(adasToken) {
			t.Error("raising ada leaves a stream of hers open, narrowed to what she held")
		}
		if sams.Err() != nil || owners.Err() != nil {
			t.Error("raising ada ended someone else's stream")
		}
		if !f.signedIn(t, ada.secret) {
			t.Error("a raise signed ada out")
		}

		// A project's role, for sam, who holds the install as a viewer.
		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeRole, RoleTarget(sam.p.User.ID, store.RoleDeveloper, "prj_1")).Proof
		if _, err := f.svc.SetMemberRole(t.Context(), onDomain, f.owner, sam.p.User.ID, store.RoleDeveloper, "prj_1", proof); err != nil {
			t.Fatal(err)
		}
		if !endsAtOnce(sams) {
			t.Error("a project's role for sam leaves his stream open")
		}
		if owners.Err() != nil {
			t.Error("sam's new role ended the owner's stream")
		}

		// Lowering ends her sessions, and the stream she opened again with them.
		adas = track(ada.p)
		if adas.Err() != nil {
			t.Fatal("ada's stream, opened again as an admin, ended at once")
		}
		if _, err := f.setRole(t, ada.p.User.ID, store.RoleViewer); err != nil {
			t.Fatal(err)
		}
		if !endsAtOnce(adas) {
			t.Error("lowering ada leaves her stream open")
		}
		if owners.Err() != nil {
			t.Error("lowering ada ended the owner's stream")
		}
	})
}

// The owner, the install's only one, gives ownership away only by a transfer,
// to someone who signs in already, never to someone only invited. An invite
// pending across the transfer holds: whoever was the owner made it and still
// may, as an admin, so it stays open and joins its person with the role it
// names, never the owner's. The new owner is then the last owner, whom whoever
// was the owner neither lowers nor removes. Both people's streams end with the
// transfer (ADR 0049).
func TestATransferWithTheLastOwnerAndAPendingInvite(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleAdmin, "203.0.113.5:1")
		inv, secret := f.invite(t, "lin@example.com", store.RoleViewer)
		was := f.owner.User.ID
		owners := func() []string {
			t.Helper()
			var out []string
			if err := f.db.View(t.Context(), func(tx *store.Tx) error {
				all, err := tx.ListGrants(t.Context())
				for _, g := range all {
					if g.Role == store.RoleOwner {
						out = append(out, g.UserID)
					}
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return out
		}

		// The only owner doesn't remove themselves, which would leave the install with none.
		self := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, was).Proof
		if _, err := f.svc.RemoveMember(t.Context(), onDomain, f.owner, was, self); !errors.Is(err, ErrOnYourself) {
			t.Errorf("the only owner removing themselves: %v", err)
		}
		// Nor is ownership given to someone only invited.
		toInvite := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpTransferOwnership, inv.ID).Proof
		if _, err := f.svc.TransferOwnership(t.Context(), onDomain, f.owner, inv.ID, toInvite); !errors.Is(err, ErrNotFound) {
			t.Errorf("transferring to someone only invited: %v", err)
		}
		if got := owners(); len(got) != 1 || got[0] != was {
			t.Fatalf("after refused calls the owners are %v", got)
		}

		mine, stop := f.svc.Track(t.Context(), onDomain, f.owner)
		defer stop()
		adas, stop := f.svc.Track(t.Context(), onDomain, ada.p)
		defer stop()
		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpTransferOwnership, ada.p.User.ID).Proof
		if _, err := f.svc.TransferOwnership(t.Context(), onDomain, f.owner, ada.p.User.ID, proof); err != nil {
			t.Fatal(err)
		}
		if !endsAtOnce(mine) || !endsAtOnce(adas) {
			t.Error("a stream of the old or the new owner stays open after the transfer")
		}
		if got := owners(); len(got) != 1 || got[0] != ada.p.User.ID {
			t.Fatalf("after the transfer the owners are %v, want ada alone", got)
		}

		// The invite pending across the transfer still joins its person, as a viewer.
		if n := f.openInvites(t); n != 1 {
			t.Fatalf("%d open invites after the transfer, want the one to lin", n)
		}
		cer, err := f.svc.BeginAcceptInvite(t.Context(), onDomain, secret, "Lin")
		if err != nil {
			t.Fatal(err)
		}
		keys := &authtest.Authenticator{Counter: true}
		cred, _, err := keys.Create(string(cer.Options), onDomain.Site.Origin)
		if err != nil {
			t.Fatal(err)
		}
		lin, err := f.svc.FinishAcceptInvite(t.Context(), onDomain, cer.ID, secret, []byte(cred), "")
		if err != nil {
			t.Fatalf("accepting the invite made before the transfer: %v", err)
		}
		if got := f.grants(t, lin.User.ID); len(got) != 1 || got[0] != "=viewer" {
			t.Errorf("lin holds %v, want a viewer's role", got)
		}
		if got := owners(); len(got) != 1 || got[0] != ada.p.User.ID {
			t.Errorf("after lin joined the owners are %v, want ada alone", got)
		}

		// Whoever was the owner, an admin now, neither lowers nor removes the new one.
		lower := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeRole, RoleTarget(ada.p.User.ID, store.RoleViewer, store.Install)).Proof
		if _, err := f.svc.SetMemberRole(t.Context(), onDomain, f.owner, ada.p.User.ID, store.RoleViewer, store.Install, lower); !errors.Is(err, ErrTheOwner) {
			t.Errorf("the old owner lowering the new one: %v", err)
		}
		remove := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, ada.p.User.ID).Proof
		if _, err := f.svc.RemoveMember(t.Context(), onDomain, f.owner, ada.p.User.ID, remove); !errors.Is(err, ErrTheOwner) {
			t.Errorf("the old owner removing the new one: %v", err)
		}
		if got := owners(); len(got) != 1 || got[0] != ada.p.User.ID {
			t.Errorf("at the end the owners are %v, want ada alone", got)
		}
	})
}
