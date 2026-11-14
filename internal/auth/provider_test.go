// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/oidc/oidctest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// providerStand is a fixture on the panel's domain, claimed there, with a fake
// provider whose saved state the test changes.
type providerStand struct {
	*fixture
	idp   *oidctest.Provider
	owner Principal
	mu    sync.Mutex
	saved []Provider
	// provider is the one the helpers sign in and link with.
	provider string
}

func newProviderStand(t *testing.T, backend store.Backend) *providerStand {
	t.Helper()
	f := newFixture(t, backend, onDomain.Site.RPID)
	signedIn := f.claim(t, onDomain, &authtest.Authenticator{})
	owner, err := f.svc.Authenticate(t.Context(), onDomain, signedIn.Token)
	if err != nil {
		t.Fatal(err)
	}
	s := &providerStand{fixture: f, idp: oidctest.NewProvider(t), owner: owner, provider: "sip_ours"}
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

// edit changes the saved provider, as an admin's applied change does, and lets the
// sign-in's few seconds of keeping it pass.
func (s *providerStand) edit(edit func(p *Provider)) {
	s.mu.Lock()
	if edit == nil {
		s.saved = nil
	} else {
		edit(&s.saved[0])
	}
	s.mu.Unlock()
	s.clock.advance(providersFor + time.Second)
}

// through goes to the provider at target, as an account with subject, and back:
// the query the provider brings back to the panel.
func (s *providerStand) through(t *testing.T, target, subject string) url.Values {
	t.Helper()
	to, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	q := to.Query()
	if q.Get("redirect_uri") != onDomain.Site.Origin+ProviderCallbackPath || q.Get("code_challenge_method") != "S256" || q.Get("client_id") != oidctest.Client {
		t.Fatalf("it goes to the provider with %v", q)
	}
	now := s.clock.now()
	s.idp.ID = func(url.Values) map[string]any {
		c := oidctest.GoodClaims()
		c["sub"], c["nonce"], c["iat"], c["exp"] = subject, q.Get("nonce"), now.Unix(), now.Add(time.Hour).Unix()
		return c
	}
	return url.Values{"code": {"abc"}, "state": {q.Get("state")}}
}

// signIn signs in with the provider as the account Mira linked, or tries to.
func (s *providerStand) signIn(t *testing.T) (ProviderDone, error) {
	t.Helper()
	return s.signInFrom(t, onDomain)
}

// signInFrom is signIn from client.
func (s *providerStand) signInFrom(t *testing.T, client Client) (ProviderDone, error) {
	t.Helper()
	target, flow, err := s.svc.BeginProviderSignIn(t.Context(), client, s.provider)
	if err != nil {
		t.Fatal(err)
	}
	return s.svc.FinishProvider(t.Context(), client, flow, s.through(t, target, "248289761001"))
}

// link links the owner's account with subject, confirmed with their passkey.
func (s *providerStand) link(t *testing.T, subject string) (ProviderDone, error) {
	t.Helper()
	proof := s.stepUpAt(t, onDomain, s.owner, s.passkeys, StepUpLinkProvider, s.provider).Proof
	target, flow, err := s.svc.BeginLinkProvider(t.Context(), onDomain, s.owner, s.provider, proof)
	if err != nil {
		t.Fatal(err)
	}
	return s.svc.FinishProvider(t.Context(), onDomain, flow, s.through(t, target, subject))
}

func TestAProviderSignsInOnlyALinkedMember(t *testing.T) {
	storetest.Each(t, testAProviderSignsInOnlyALinkedMember)
}

// A provider signs in to the panel only on its own domain, over HTTPS, and only
// someone who linked the account while signed in, with their passkey. Such a
// session confirms nothing a passkey does, and a flow is used once, on its site,
// with its state (ADR 0095).
func testAProviderSignsInOnlyALinkedMember(t *testing.T, backend store.Backend) {
	s := newProviderStand(t, backend)
	ctx := t.Context()
	if got, _ := s.svc.SignInProviders(ctx, localhost); len(got) != 0 {
		t.Errorf("on localhost it offers %+v", got)
	}
	if got, _ := s.svc.SignInProviders(ctx, onDomain); len(got) != 1 || got[0].Label() != "Our Authentik" {
		t.Errorf("on the domain it offers %+v", got)
	}
	if _, _, err := s.svc.BeginProviderSignIn(ctx, localhost, "sip_ours"); !errors.Is(err, ErrNoProviderHere) {
		t.Errorf("on localhost a sign-in begins: %v", err)
	}

	// Nobody joins by signing in: the account is linked to no one.
	if _, err := s.signIn(t); !errors.Is(err, ErrProviderNotLinked) {
		t.Fatalf("an account linked to no one signs in: %v", err)
	}
	if got := s.auditDetails(t, "auth.sign_in_failed"); len(got) != 1 {
		t.Errorf("the refusal is audited as %v", got)
	}

	// Linking takes a passkey, each time.
	if _, _, err := s.svc.BeginLinkProvider(ctx, onDomain, s.owner, "sip_ours", ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("linking with no passkey begins: %v", err)
	}
	linked, err := s.link(t, "248289761001")
	if err != nil || linked.Linked == nil || linked.Linked.Account != "mira@example.com" {
		t.Fatalf("linking gives %+v, %v", linked, err)
	}
	if _, err := s.link(t, "another-account"); !errors.Is(err, ErrProviderLinkedTwice) {
		t.Errorf("a second account at the same provider links: %v", err)
	}

	done, err := s.signIn(t)
	if err != nil || done.SignedIn == nil {
		t.Fatalf("the linked account signs in: %+v, %v", done, err)
	}
	if sess := done.SignedIn.Session; sess.Method != MethodProvider || sess.ProviderLinkID != linked.Linked.ID || sess.PasskeyAt != nil {
		t.Errorf("its session is %+v", sess)
	}
	p, err := s.svc.Authenticate(ctx, onDomain, done.SignedIn.Token)
	if err != nil {
		t.Fatal(err)
	}
	// What a passkey confirms, it still asks for: a risky action, and adding a passkey.
	if err := s.svc.UseStepUp(ctx, onDomain, p, "", StepUpRevealSecret, "sec_x"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a provider's session reveals a secret with no passkey: %v", err)
	}
	if _, err := s.svc.BeginAddPasskey(ctx, onDomain, p, ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a provider's session adds a passkey with no passkey: %v", err)
	}

	// A flow is used once, on its own site, with its own state.
	target, flow, err := s.svc.BeginProviderSignIn(ctx, onDomain, "sip_ours")
	if err != nil {
		t.Fatal(err)
	}
	back := s.through(t, target, "248289761001")
	forged := url.Values{"code": {"abc"}, "state": {"forged"}}
	if _, err := s.svc.FinishProvider(ctx, onDomain, flow, forged); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("a forged state signs in: %v", err)
	}
	if _, err := s.svc.FinishProvider(ctx, onDomain, flow, back); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("a flow refused once signs in after: %v", err)
	}
	other := onDomain
	other.Site.Origin = "https://elsewhere.example"
	if target, flow, err = s.svc.BeginProviderSignIn(ctx, onDomain, "sip_ours"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.FinishProvider(ctx, other, flow, s.through(t, target, "248289761001")); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("a flow finished on another site signs in: %v", err)
	}

	// Unlinking ends what the link began, and nothing else.
	if err := s.svc.DeleteProviderLink(ctx, s.owner, linked.Linked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.Authenticate(ctx, onDomain, done.SignedIn.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("after unlinking, the provider's session works: %v", err)
	}
	if _, err := s.signIn(t); !errors.Is(err, ErrProviderNotLinked) {
		t.Errorf("an unlinked account signs in: %v", err)
	}
}

func TestAProvidersEditStopsItsLinks(t *testing.T) {
	storetest.Each(t, testAProvidersEditStopsItsLinks)
}

// A provider turned off for the panel, or moved to another issuer, stops its links
// signing anyone in, and ends the sessions they began; turned back, they work again.
// A deleted provider's links are deleted (ADR 0095).
func testAProvidersEditStopsItsLinks(t *testing.T, backend store.Backend) {
	s := newProviderStand(t, backend)
	ctx := t.Context()
	if _, err := s.link(t, "248289761001"); err != nil {
		t.Fatal(err)
	}
	signedIn := func() string {
		t.Helper()
		done, err := s.signIn(t)
		if err != nil {
			t.Fatal(err)
		}
		return done.SignedIn.Token
	}
	state := func() LinkState {
		t.Helper()
		links, err := s.svc.ListProviderLinks(ctx, s.owner)
		if err != nil || len(links) != 1 {
			t.Fatalf("the links are %+v, %v", links, err)
		}
		return links[0].State
	}
	token := signedIn()
	s.edit(func(p *Provider) { p.Panel = false })
	if _, err := s.svc.Authenticate(ctx, onDomain, token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("turned off for the panel, its session works: %v", err)
	}
	if got := s.auditDetails(t, "auth.sessions_ended"); len(got) != 1 {
		t.Errorf("the sessions it ended are audited as %v", got)
	}
	if got := state(); got != LinkProviderOff {
		t.Errorf("turned off, the link reads %v", got)
	}
	if _, err := s.svc.Authenticate(ctx, onDomain, s.ownerToken(t)); err != nil {
		t.Errorf("the owner's passkey session ends with the provider's: %v", err)
	}

	s.edit(func(p *Provider) { p.Panel = true })
	token = signedIn()
	s.edit(func(p *Provider) { p.Issuer = "https://moved.example.com/" })
	if _, err := s.svc.Authenticate(ctx, onDomain, token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("moved to another issuer, its session works: %v", err)
	}
	if got := state(); got != LinkIssuerMoved {
		t.Errorf("moved, the link reads %v", got)
	}

	s.edit(func(p *Provider) { p.Issuer = s.idp.Issuer() })
	token = signedIn()
	s.edit(nil)
	if _, err := s.svc.Authenticate(ctx, onDomain, token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("its provider deleted, its session works: %v", err)
	}
	if links, err := s.svc.ListProviderLinks(ctx, s.owner); err != nil || len(links) != 0 {
		t.Errorf("its provider deleted, the links are %+v, %v", links, err)
	}
}

// ownerToken signs the owner in again with their passkey, for a token to check.
func (s *providerStand) ownerToken(t *testing.T) string {
	t.Helper()
	done, err := s.passkeySignIn(t, onDomain, s.passkeys, nil)
	if err != nil {
		t.Fatal(err)
	}
	return done.Token
}

func TestStartingAProviderSignInIsBounded(t *testing.T) {
	storetest.Each(t, testStartingAProviderSignInIsBounded)
}

// Starting a sign-in with a provider spends from the same allowance for an address
// as starting one with a passkey (ADR 0095).
func testStartingAProviderSignInIsBounded(t *testing.T, backend store.Backend) {
	s := newProviderStand(t, backend)
	var err error
	for range 40 {
		if _, _, err = s.svc.BeginProviderSignIn(t.Context(), onDomain, "sip_ours"); err != nil {
			break
		}
	}
	if !errors.Is(err, ErrTooManyRequests) {
		t.Errorf("40 starts from one address end with %v", err)
	}
	if _, err := s.svc.BeginSignIn(t.Context(), onDomain); !errors.Is(err, ErrTooManyRequests) {
		t.Errorf("after them, a passkey sign-in starts: %v", err)
	}
}

func TestProviderSessionsNeverNestReads(t *testing.T) {
	storetest.Each(t, testProviderSessionsNeverNestReads)
}

// Many requests at once on a session begun with a provider, while the providers are
// read again, never wait on a second connection from the read pool their own
// transaction holds one of: each reads the providers once its transaction is over,
// and one read serves them all. A read of the providers that reaches the database,
// and more callers than the read pool holds, would otherwise wait forever (#646's
// review, H1).
func testProviderSessionsNeverNestReads(t *testing.T, backend store.Backend) {
	s := newProviderStand(t, backend)
	if _, err := s.link(t, "248289761001"); err != nil {
		t.Fatal(err)
	}
	done, err := s.signIn(t)
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	s.svc.UseProviders(func(ctx context.Context) ([]Provider, error) {
		reads.Add(1)
		// A real read, which takes a while, as decrypting the saved providers does.
		err := s.db.View(ctx, func(tx *store.Tx) error {
			_, err := tx.ListUsers(ctx)
			time.Sleep(20 * time.Millisecond)
			return err
		})
		s.mu.Lock()
		defer s.mu.Unlock()
		return append([]Provider(nil), s.saved...), err
	}, s.idp.Srv.Client())
	callers := 3 * max(4, runtime.GOMAXPROCS(0))
	wave := func(what string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		failed := make(chan error, callers)
		for range callers {
			wg.Go(func() {
				if _, err := s.svc.Authenticate(ctx, onDomain, done.SignedIn.Token); err != nil {
					failed <- err
				}
			})
		}
		wg.Wait()
		close(failed)
		for err := range failed {
			t.Fatalf("%s, %d requests at once on the provider's session: %v", what, callers, err)
		}
	}
	wave("with no providers read yet")
	s.clock.advance(providersFor + time.Second)
	wave("with the providers read too long ago")
	if n := reads.Load(); n > 4 {
		t.Errorf("two waves of %d requests read the providers %d times, want one read each", callers, n)
	}

	// With every read connection but one held, a request that needed two at once,
	// one for its session and one for the providers, would wait forever: it reads
	// one, then the other.
	s.clock.advance(providersFor + time.Second)
	hold, held := make(chan struct{}), make(chan struct{})
	var holding sync.WaitGroup
	for range max(4, runtime.GOMAXPROCS(0)) - 1 {
		holding.Go(func() {
			_ = s.db.View(t.Context(), func(*store.Tx) error {
				held <- struct{}{}
				<-hold
				return nil
			})
		})
	}
	for range max(4, runtime.GOMAXPROCS(0)) - 1 {
		<-held
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	_, err = s.svc.Authenticate(ctx, onDomain, done.SignedIn.Token)
	cancel()
	close(hold)
	holding.Wait()
	if err != nil {
		t.Errorf("with one read connection free, the provider's session reads %v", err)
	}
}

func TestUnlinkingEndsItsStreamsAtOnce(t *testing.T) {
	storetest.Each(t, testUnlinkingEndsItsStreamsAtOnce)
}

// Unlinking ends the streams of the sessions the link began at once, as signing out
// does, and says which link began the session asking (#646's review, M1 and L4).
func testUnlinkingEndsItsStreamsAtOnce(t *testing.T, backend store.Backend) {
	s := newProviderStand(t, backend)
	linked, err := s.link(t, "248289761001")
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.signIn(t)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.svc.Authenticate(t.Context(), onDomain, done.SignedIn.Token)
	if err != nil {
		t.Fatal(err)
	}
	if links, err := s.svc.ListProviderLinks(t.Context(), p); err != nil || len(links) != 1 || !links[0].Here {
		t.Errorf("from the provider's session, the links read %+v, %v", links, err)
	}
	if links, err := s.svc.ListProviderLinks(t.Context(), s.owner); err != nil || len(links) != 1 || links[0].Here {
		t.Errorf("from a passkey's session, the links read %+v, %v", links, err)
	}
	stream, stop := s.svc.Track(t.Context(), onDomain, p)
	defer stop()
	if err := s.svc.DeleteProviderLink(t.Context(), s.owner, linked.Linked.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.Done():
	case <-time.After(2 * time.Second):
		t.Error("unlinking leaves the stream of the session the link began open")
	}
}

func TestOneAddressCantSpendTheExchanges(t *testing.T) {
	storetest.Each(t, testOneAddressCantSpendTheExchanges)
}

// A way back with no code spends nothing of the exchanges, and one address
// exchanges at most 5 codes a minute, leaving the rest of the install's for
// everyone else (#646's review, M2).
func testOneAddressCantSpendTheExchanges(t *testing.T, backend store.Backend) {
	s := newProviderStand(t, backend)
	if _, err := s.link(t, "248289761001"); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		target, flow, err := s.svc.BeginProviderSignIn(t.Context(), onDomain, s.provider)
		if err != nil {
			t.Fatal(err)
		}
		back := s.through(t, target, "248289761001")
		back.Del("code")
		if _, err := s.svc.FinishProvider(t.Context(), onDomain, flow, back); !errors.As(err, new(*ProviderError)) {
			t.Fatalf("a way back with no code reads %v", err)
		}
	}
	// Linking spent one of the address's exchanges.
	for i := range providerExchangesAnAddress - 1 {
		if _, err := s.signIn(t); err != nil {
			t.Fatalf("sign-in %d from one address: %v", i+1, err)
		}
	}
	if _, err := s.signIn(t); !errors.Is(err, ErrProviderBusy) {
		t.Errorf("a sixth exchange in a minute from one address reads %v", err)
	}
	other := onDomain
	other.Addr = "203.0.113.9:4000"
	if _, err := s.signInFrom(t, other); err != nil {
		t.Errorf("then another address can't sign in: %v", err)
	}
}

func TestADeletedProviderLetsItsIssuerComeBack(t *testing.T) {
	storetest.Each(t, testADeletedProviderLetsItsIssuerComeBack)
}

// A provider deleted and added again at the same issuer, as a new provider, takes
// the accounts its old links held: reading the providers deletes the links whose
// provider is gone, so the account links again and signs in (#646's review, L1).
func testADeletedProviderLetsItsIssuerComeBack(t *testing.T, backend store.Backend) {
	s := newProviderStand(t, backend)
	if _, err := s.link(t, "248289761001"); err != nil {
		t.Fatal(err)
	}
	// Deleted, and a while later added again: a new provider, at the same issuer.
	s.clock.advance(time.Minute)
	s.mu.Lock()
	again := s.saved[0]
	again.ID = "sip_again"
	s.saved = []Provider{again}
	s.mu.Unlock()
	s.provider = "sip_again"
	if linked, err := s.link(t, "248289761001"); err != nil || linked.Linked == nil {
		t.Fatalf("at the provider added again, the account links: %+v, %v", linked, err)
	}
	if done, err := s.signIn(t); err != nil || done.SignedIn == nil {
		t.Errorf("then it signs in: %+v, %v", done, err)
	}
	if got := s.auditDetails(t, "provider_link.delete"); len(got) != 1 || !strings.Contains(got[0], "its provider was deleted") {
		t.Errorf("the old link's deletion is audited as %v", got)
	}
}

func TestAProofOutlastsAProviderOutOfReach(t *testing.T) {
	storetest.Each(t, testAProofOutlastsAProviderOutOfReach)
}

// Linking holds the passkey's proof, and gives it back when the provider can't be
// reached, so trying again doesn't take the passkey again (#646's review, L2).
func testAProofOutlastsAProviderOutOfReach(t *testing.T, backend store.Backend) {
	s := newProviderStand(t, backend)
	proof := s.stepUpAt(t, onDomain, s.owner, s.passkeys, StepUpLinkProvider, s.provider).Proof
	s.edit(func(p *Provider) { p.Issuer = "https://127.0.0.1:1/" })
	if _, _, err := s.svc.BeginLinkProvider(t.Context(), onDomain, s.owner, s.provider, proof); !errors.As(err, new(*ProviderError)) {
		t.Fatalf("linking at a provider out of reach reads %v", err)
	}
	s.edit(func(p *Provider) { p.Issuer = s.idp.Issuer() })
	if _, _, err := s.svc.BeginLinkProvider(t.Context(), onDomain, s.owner, s.provider, proof); err != nil {
		t.Errorf("once it's back, the same proof links: %v", err)
	}
}
