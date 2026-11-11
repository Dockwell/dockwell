// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// invited is an install at its domain with its owner signed in there, who invites.
type invited struct {
	*fixture
	owner Principal
	keys  *authtest.Authenticator
}

func newInvited(t *testing.T, backend store.Backend) *invited {
	t.Helper()
	f := newFixture(t, backend, onDomain.Site.RPID)
	a := &authtest.Authenticator{Counter: true}
	return &invited{fixture: f, owner: ownerPrincipal(f.claim(t, onDomain, a)), keys: a}
}

// invite invites email to the whole install as role, and returns the invite and its secret.
func (f *invited) invite(t *testing.T, email, role string) (store.Invite, string) {
	t.Helper()
	proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpInvite, InviteTarget(email, role, store.Install)).Proof
	made, err := f.svc.CreateInvite(t.Context(), onDomain, f.owner, email, role, store.Install, proof)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, ok := strings.Cut(made.Link, "#")
	if !ok || !strings.HasPrefix(made.Link, "https://"+onDomain.Site.RPID+"/invite#") {
		t.Fatalf("the link is %q, want one at the panel's domain with the secret after #", made.Link)
	}
	return made.Invite, secret
}

// begun starts accepting an invite from addr, and returns the ceremony and the new passkey for it.
func (f *invited) begun(t *testing.T, secret, addr string) (string, string) {
	t.Helper()
	client := onDomain
	client.Addr = addr
	cer, err := f.svc.BeginAcceptInvite(t.Context(), client, secret, "Ada Park")
	if err != nil {
		t.Fatal(err)
	}
	cred, _, err := (&authtest.Authenticator{}).Create(string(cer.Options), onDomain.Site.Origin)
	if err != nil {
		t.Fatal(err)
	}
	return cer.ID, cred
}

func (f *invited) users(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.View(t.Context(), func(tx *store.Tx) (err error) {
		n, err = tx.CountUsers(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// The passkey made for one invite finishes that invite alone, at the address it
// was begun at: a ceremony begun with one secret can't be finished with another's,
// nor at another site.
func TestAnAcceptanceIsBoundToItsInviteAndItsSite(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		_, viewer := f.invite(t, "ada@example.com", store.RoleViewer)
		_, admin := f.invite(t, "sam@example.com", store.RoleAdmin)

		id, cred := f.begun(t, viewer, "203.0.113.5:1")
		if _, err := f.svc.FinishAcceptInvite(t.Context(), onDomain, id, admin, []byte(cred), ""); !errors.Is(err, ErrCeremonyExpired) {
			t.Errorf("finishing the viewer's ceremony with the admin's secret: %v, want it refused as no ceremony of that invite", err)
		}
		id, cred = f.begun(t, viewer, "203.0.113.5:1")
		if _, err := f.svc.FinishAcceptInvite(t.Context(), localhost, id, viewer, []byte(cred), ""); !errors.Is(err, ErrCeremonyExpired) {
			t.Errorf("finishing at another site: %v, want it refused", err)
		}
		if n := f.users(t); n != 1 {
			t.Fatalf("%d people after two refused acceptances, want the owner alone", n)
		}
		// The right secret at the right site makes the viewer, and nobody an admin.
		id, cred = f.begun(t, viewer, "203.0.113.5:1")
		in, err := f.svc.FinishAcceptInvite(t.Context(), onDomain, id, viewer, []byte(cred), "")
		if err != nil {
			t.Fatal(err)
		}
		grants, err := f.svc.Grants(t.Context(), Principal{User: in.User})
		if err != nil || len(grants) != 1 || grants[0].Role != store.RoleViewer || in.User.Email != "ada@example.com" {
			t.Errorf("%s joined holding %+v, %v; want a viewer of the install", in.User.Email, grants, err)
		}
	})
}

// An invite is opened and accepted only at the panel's domain, where the install
// keeps serving: nowhere else is a passkey even begun.
func TestAnInviteIsAcceptedOnlyAtTheDomain(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		_, secret := f.invite(t, "ada@example.com", store.RoleViewer)
		if _, err := f.svc.CheckInvite(t.Context(), localhost, secret); !errors.Is(err, ErrInviteElsewhere) {
			t.Errorf("opening it at localhost: %v, want it sent to the domain", err)
		}
		if _, err := f.svc.BeginAcceptInvite(t.Context(), localhost, secret, "Ada Park"); !errors.Is(err, ErrInviteElsewhere) {
			t.Errorf("beginning a passkey at localhost: %v, want it sent to the domain", err)
		}
	})
}

// Accepting only ever makes someone new: once the email signs in here, the invite
// neither begins a passkey nor finishes one begun before, and says why.
func TestAnInviteNeverMakesAMemberAgain(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		_, secret := f.invite(t, "ada@example.com", store.RoleViewer)
		id, cred := f.begun(t, secret, "203.0.113.5:1")
		// Ada comes to sign in here some other way meanwhile.
		err := f.db.Update(t.Context(), func(tx *store.Tx) error {
			return tx.CreateUser(t.Context(), store.User{ID: "usr_ada", Email: "ada@example.com", Name: "Ada", WebAuthnHandle: []byte("ada's handle"), CreatedAt: f.clock.now()})
		})
		if err != nil {
			t.Fatal(err)
		}
		const again = "so this invite can't make them again. Sign in as them"
		if _, err := f.svc.BeginAcceptInvite(t.Context(), onDomain, secret, "Ada Park"); !errors.Is(err, ErrMemberExists) || !strings.Contains(err.Error(), again) {
			t.Errorf("beginning a passkey for someone who signs in already: %v, want it refused with what to do", err)
		}
		if _, err := f.svc.FinishAcceptInvite(t.Context(), onDomain, id, secret, []byte(cred), ""); !errors.Is(err, ErrMemberExists) || !strings.Contains(err.Error(), again) {
			t.Errorf("finishing a passkey begun before they signed in here: %v, want it refused with what to do", err)
		}
		// Inviting them is refused before the passkey's proof is spent on it.
		target := InviteTarget("ada@example.com", store.RoleViewer, store.Install)
		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpInvite, target).Proof
		if _, err := f.svc.CreateInvite(t.Context(), onDomain, f.owner, "ada@example.com", store.RoleViewer, store.Install, proof); !errors.Is(err, ErrMemberExists) {
			t.Errorf("inviting someone who signs in already: %v", err)
		}
		if err := f.svc.UseStepUp(f.owner, proof, StepUpInvite, target); err != nil {
			t.Errorf("the proof was spent on an invite that was refused: %v", err)
		}
	})
}

// The calls anyone can make with a secret are limited by where they come from,
// so a secret can't be guessed at speed.
func TestOpeningAndBeginningAnInviteAreLimited(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		_, secret := f.invite(t, "ada@example.com", store.RoleViewer)
		limited := func(what, addr string, call func(Client) error) {
			t.Helper()
			client := onDomain
			client.Addr = addr
			for i := range 200 {
				err := call(client)
				if errors.Is(err, ErrTooManyRequests) {
					if i < 5 {
						t.Errorf("%s was limited after %d calls, which is too few to use", what, i)
					}
					return
				}
				if err != nil && !errors.Is(err, ErrInviteRejected) {
					t.Fatalf("%s: %v", what, err)
				}
			}
			t.Errorf("%s was never limited in 200 calls from one address", what)
		}
		limited("opening an invite", "203.0.113.7:1", func(c Client) error {
			_, err := f.svc.CheckInvite(t.Context(), c, "a guess")
			return err
		})
		limited("beginning its passkey", "203.0.113.8:1", func(c Client) error {
			_, err := f.svc.BeginAcceptInvite(t.Context(), c, secret, "Ada Park")
			return err
		})
	})
}

// Twelve acceptances of one invite racing make one person.
func TestRacingAcceptancesMakeOnePerson(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		_, secret := f.invite(t, "ada@example.com", store.RoleDeveloper)
		const racers = 12
		type ready struct{ id, cred string }
		var started []ready
		for i := range racers {
			id, cred := f.begun(t, secret, fmt.Sprintf("203.0.113.%d:1", 20+i))
			started = append(started, ready{id, cred})
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		won, lost := 0, 0
		for _, r := range started {
			wg.Go(func() {
				_, err := f.svc.FinishAcceptInvite(context.Background(), onDomain, r.id, secret, []byte(r.cred), "")
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					won++
				case errors.Is(err, ErrInviteRejected), errors.Is(err, ErrMemberExists):
					lost++
				default:
					t.Errorf("FinishAcceptInvite: %v", err)
				}
			})
		}
		wg.Wait()
		if won != 1 || lost != racers-1 {
			t.Errorf("%d acceptances won and %d lost, want 1 and %d", won, lost, racers-1)
		}
		if n := f.users(t); n != 2 {
			t.Errorf("%d people after racing acceptances, want the owner and one more", n)
		}
	})
}

// An invite revoked, or run out, between beginning its passkey and finishing it
// makes nobody.
func TestAnInviteGoneMidwayMakesNobody(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		inv, secret := f.invite(t, "ada@example.com", store.RoleViewer)
		id, cred := f.begun(t, secret, "203.0.113.5:1")
		if err := f.svc.RevokeInvite(t.Context(), onDomain, f.owner, inv.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.FinishAcceptInvite(t.Context(), onDomain, id, secret, []byte(cred), ""); !errors.Is(err, ErrInviteRejected) {
			t.Errorf("finishing an invite revoked since its passkey began: %v, want it rejected", err)
		}

		late, lateSecret := f.invite(t, "sam@example.com", store.RoleAdmin)
		// A minute before it runs out its passkey begins, and two minutes later it's finished.
		f.clock.advance(late.ExpiresAt.Sub(f.clock.now()) - time.Minute)
		id, cred = f.begun(t, lateSecret, "203.0.113.6:1")
		f.clock.advance(2 * time.Minute)
		if _, err := f.svc.FinishAcceptInvite(t.Context(), onDomain, id, lateSecret, []byte(cred), ""); !errors.Is(err, ErrInviteRejected) {
			t.Errorf("finishing an invite that ran out since its passkey began: %v, want it rejected", err)
		}
		if n := f.users(t); n != 1 {
			t.Errorf("%d people, want the owner alone", n)
		}
	})
}

// An invite that doesn't work says so in the same words whatever the reason, so
// a secret tells nothing of whether it was ever one: unknown, accepted, revoked,
// replaced by a newer one, or run out. A replaced one is on record as revoked.
func TestEveryInviteThatDoesntWorkSaysTheSame(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		_, spent := f.invite(t, "ada@example.com", store.RoleViewer)
		id, cred := f.begun(t, spent, "203.0.113.5:1")
		if _, err := f.svc.FinishAcceptInvite(t.Context(), onDomain, id, spent, []byte(cred), ""); err != nil {
			t.Fatal(err)
		}
		gone, revoked := f.invite(t, "sam@example.com", store.RoleViewer)
		if err := f.svc.RevokeInvite(t.Context(), onDomain, f.owner, gone.ID); err != nil {
			t.Fatal(err)
		}
		before := len(f.auditEvents(t, "member.invite_revoke"))
		first, replaced := f.invite(t, "lin@example.com", store.RoleViewer)
		_, newer := f.invite(t, "lin@example.com", store.RoleDeveloper)
		revokes := f.auditEvents(t, "member.invite_revoke")
		if len(revokes) != before+1 || revokes[len(revokes)-1].Subject != first.ID {
			t.Errorf("replacing lin's invite recorded %d revokes more, the last of %q; want one, of the one replaced", len(revokes)-before, revokes[len(revokes)-1].Subject)
		}
		if _, err := f.svc.CheckInvite(t.Context(), onDomain, newer); err != nil {
			t.Errorf("the newer invite: %v", err)
		}
		_, old := f.invite(t, "kim@example.com", store.RoleAdmin)
		f.clock.advance(adminInviteLasts + time.Second)

		said := map[string]string{}
		for what, secret := range map[string]string{
			"one never made": "never a secret", "one accepted": spent, "one revoked": revoked,
			"one replaced": replaced, "one run out": old,
		} {
			client := onDomain
			client.Addr = "203.0.113.9:1"
			_, err := f.svc.CheckInvite(t.Context(), client, secret)
			if !errors.Is(err, ErrInviteRejected) {
				t.Errorf("opening %s: %v, want it rejected", what, err)
				continue
			}
			said[err.Error()] = what
			if _, err := f.svc.BeginAcceptInvite(t.Context(), client, secret, "Ada Park"); !errors.Is(err, ErrInviteRejected) {
				t.Errorf("beginning a passkey for %s: %v, want it rejected", what, err)
			}
		}
		if len(said) != 1 {
			t.Errorf("invites that don't work say %d different things: %v", len(said), said)
		}
	})
}

// An email is written one way, the server's, for the passkey's target and the
// invite alike, so what a browser makes of a letter such as İ can't part them.
func TestAnInvitesEmailIsWrittenOneWay(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		typed := "  İda@Example.com"
		// What the panel sends: the email as typed, the role and the place.
		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpInvite, strings.TrimSpace(typed)+" viewer install").Proof
		made, err := f.svc.CreateInvite(t.Context(), onDomain, f.owner, typed, store.RoleViewer, store.Install, proof)
		if err != nil {
			t.Fatalf("inviting %q with a passkey for it as typed: %v", typed, err)
		}
		if made.Invite.Email != strings.ToLower(strings.TrimSpace(typed)) {
			t.Errorf("the invite is for %q", made.Invite.Email)
		}
		// A passkey for one email makes no invite for another, however alike.
		other := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpInvite, "ida@example.com viewer install").Proof
		if _, err := f.svc.CreateInvite(t.Context(), onDomain, f.owner, "ido@example.com", store.RoleViewer, store.Install, other); !errors.Is(err, ErrNeedsStepUp) {
			t.Errorf("a passkey for ida inviting ido: %v, want it to need its own", err)
		}
	})
}
