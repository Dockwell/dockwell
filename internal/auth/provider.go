// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/oidc"
	"github.com/dockwell/dockwell/internal/store"
)

// Provider is a sign-in provider, as signing in to the panel needs it (ADR 0095).
type Provider struct {
	ID, Name, DisplayName          string
	Issuer, ClientID, ClientSecret string
	Scopes                         []string
	GroupsClaim                    string
	// Panel says people sign in to Dockwell with it.
	Panel bool
}

// Label is what the provider is called: its display name, or its name.
func (p Provider) Label() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Name
}

const (
	// ProviderCallbackPath is where a provider brings someone back to the panel,
	// which an admin registers there.
	ProviderCallbackPath = "/auth/provider/callback"
	// ProviderFlowCookie holds a sign-in's flow, which the way back is checked
	// against. It's SameSite=Lax, so the provider's top-level redirect carries it.
	ProviderFlowCookie = "__Host-dockwell_provider_flow"
	// ProviderFlowTTL is how long someone has at the provider.
	ProviderFlowTTL = 10 * time.Minute
	// providersFor is how long the providers read are kept, so a provider's change
	// reaches the sessions its links began within it.
	providersFor = 5 * time.Second
	// providerExchangesAtOnce and providerExchangesAMinute bound the codes the
	// install exchanges, as ADR 0087 bounds a route's.
	providerExchangesAtOnce  = 4
	providerExchangesAMinute = 30
	// providerExchangesAnAddress bounds one address's exchanges a minute, so one
	// visitor can't spend the install's on made-up callbacks, as ADR 0087's routes
	// bound a visitor's (#646's review, M2). maxExchangeAddresses bounds how many
	// addresses are kept to tell them apart.
	providerExchangesAnAddress = 5
	maxExchangeAddresses       = 4096
	// providerReadWait bounds a read of the providers, which those waiting on it
	// wait for whoever leaves.
	providerReadWait = 30 * time.Second
	// providersAtMost is how old a list of providers is still used while another
	// read runs; past it, a caller waits for that read (#646's re-check, N2).
	providersAtMost = 2 * providersFor
	// stoppedLinkWait bounds ending a stopped link's sessions, which goes on
	// whoever asked leaving (#646's re-check, N3).
	stoppedLinkWait = 10 * time.Second
)

// Errors a sign-in or a link with a provider is refused with. Each says what to do.
var (
	ErrNoProviderHere      = errors.New("signing in with a provider works only on the panel's own domain, over HTTPS. Sign in there, or with your passkey")
	ErrProviderNotOffered  = errors.New("that provider doesn't sign people in to Dockwell. Sign in with your passkey, or ask an admin to turn it on in Settings, Integrations")
	ErrProviderNotLinked   = errors.New("this account isn't linked to anyone here. Sign in with your passkey, then link it in Profile and security")
	ErrProviderLinkedElse  = errors.New("this account is linked to someone else here. Sign in at the provider as yourself, then link again")
	ErrProviderLinkedTwice = errors.New("you have an account at this provider linked already. Unlink it in Profile and security first")
	ErrProviderBusy        = errors.New("too many people are signing in with a provider at once. Wait a minute, then try again")
)

// providerSource reads the providers, kept for a few seconds, one read at a time,
// and bounds the codes exchanged with them.
type providerSource struct {
	mu     sync.Mutex
	read   func(context.Context) ([]Provider, error)
	client *oidc.Client
	list   []Provider
	at     time.Time
	// reading is the read under way, if one is; failed is what the last one met.
	reading chan struct{}
	failed  error
	active  int
	recent  []time.Time
	// byAddress is each address's exchanges in the last minute.
	byAddress map[string][]time.Time
}

// ProviderError is what a provider, or reaching it, said of a sign-in, which the
// visitor may read: it names the provider and how to go on, and holds nothing of
// Dockwell's own.
type ProviderError struct{ Err error }

func (e *ProviderError) Error() string { return e.Err.Error() }

func (e *ProviderError) Unwrap() error { return e.Err }

// UseProviders lets people sign in to the panel with the providers read reads, at
// what client reaches. The control server sets it once, before it serves.
func (s *Service) UseProviders(read func(context.Context) ([]Provider, error), client oidc.Doer) {
	s.providers.mu.Lock()
	defer s.providers.mu.Unlock()
	s.providers.read = read
	s.providers.client = &oidc.Client{HTTP: client, Now: s.now}
	s.providers.list, s.providers.at = nil, time.Time{}
}

// allProviders is every provider saved, read again after providersFor, or now when
// fresh. It reads in a transaction of its own, so it's never called inside one: a
// read pool's connections each held by a transaction waiting for a second would
// wait forever (#646's review, H1). One read runs at a time. While it does, a caller
// goes on with a list under providersAtMost old, and any other waits for that read.
func (s *Service) allProviders(ctx context.Context, fresh bool) ([]Provider, error) {
	src := &s.providers
	src.mu.Lock()
	if src.read == nil {
		src.mu.Unlock()
		return nil, nil
	}
	if !fresh && !src.at.IsZero() && s.now().Sub(src.at) < providersFor {
		list := src.list
		src.mu.Unlock()
		return list, nil
	}
	if wait := src.reading; wait != nil {
		if !fresh && !src.at.IsZero() && s.now().Sub(src.at) < providersAtMost {
			list := src.list
			src.mu.Unlock()
			return list, nil
		}
		src.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		src.mu.Lock()
		defer src.mu.Unlock()
		if src.failed != nil {
			return nil, src.failed
		}
		return src.list, nil
	}
	done := make(chan struct{})
	src.reading = done
	read := src.read
	src.mu.Unlock()
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), providerReadWait)
	list, err := read(readCtx)
	cancel()
	if err != nil {
		err = fmt.Errorf("can't read the sign-in providers: %w", err)
	}
	src.mu.Lock()
	src.reading, src.failed = nil, err
	if err == nil {
		src.list, src.at = list, s.now()
	}
	src.mu.Unlock()
	close(done)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// endStreams ends the streams of sessions that ended, once what ended them commits.
func (s *Service) endStreams(sessions []string) {
	for _, id := range sessions {
		s.streams.end(id)
	}
}

// SignInProviders are the providers someone at client can sign in to the panel
// with: on the panel's own domain, over HTTPS, those that sign people in to it.
func (s *Service) SignInProviders(ctx context.Context, client Client) ([]Provider, error) {
	if !s.providerSite(client.Site) {
		return nil, nil
	}
	all, err := s.allProviders(ctx, false)
	if err != nil {
		return nil, err
	}
	var out []Provider
	for i := range all {
		if all[i].Panel {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// providerSite says whether site is the panel's own domain over HTTPS, where a
// provider can bring someone back to.
func (s *Service) providerSite(site Site) bool {
	return site.Secure && s.sites.Domain != "" && site.RPID == s.sites.Domain
}

// offered is the provider with id, read now, if it signs people in to the panel,
// and every provider as that read found them.
func (s *Service) offered(ctx context.Context, providerID string) (Provider, []Provider, error) {
	all, err := s.allProviders(ctx, true)
	if err != nil {
		return Provider{}, nil, err
	}
	i := slices.IndexFunc(all, func(p Provider) bool { return p.ID == providerID })
	if i < 0 || !all[i].Panel {
		return Provider{}, nil, ErrProviderNotOffered
	}
	return all[i], all, nil
}

// gone says whether a link's provider is gone from providers, a fresh read of them.
// Its links are kept, as an undo brings a provider back with its ID, and deleted
// only where one stands in the way of an account at another provider (#646's
// re-check, N1).
func gone(link *store.ProviderLink, providers []Provider) bool {
	return !slices.ContainsFunc(providers, func(p Provider) bool { return p.ID == link.ProviderID })
}

// redirectURI is where the provider brings someone back to, on site.
func redirectURI(site Site) string { return site.Origin + ProviderCallbackPath }

// ProviderRedirect is the address a provider brings someone back to, which an admin
// registers there: as the panel answers on its domain, from client on it, and on its
// usual port from anywhere else, such as the SSH tunnel. Empty with no domain.
func (s *Service) ProviderRedirect(client Client) string {
	switch {
	case s.providerSite(client.Site):
		return redirectURI(client.Site)
	case s.sites.Domain != "":
		return "https://" + s.sites.Domain + ProviderCallbackPath
	}
	return ""
}

// BeginProviderSignIn starts signing in with a provider. It spends from the same
// allowance for client's address as starting a passkey sign-in. It returns the
// address to go to, and the flow the cookie holds.
func (s *Service) BeginProviderSignIn(ctx context.Context, client Client, providerID string) (target, flow string, err error) {
	if !s.providerSite(client.Site) {
		return "", "", ErrNoProviderHere
	}
	if !s.begins.take(sourceKey(client.Addr), s.now()) {
		return "", "", ErrTooManyRequests
	}
	return s.beginProvider(ctx, client, providerID, &ceremony{})
}

// BeginLinkProvider starts linking an account at a provider to p, which a passkey
// confirms each time, as a way to sign in outlives the session that made it.
func (s *Service) BeginLinkProvider(ctx context.Context, client Client, p Principal, providerID, proof string) (target, flow string, err error) {
	if p.Token != nil || p.Session.ID == "" {
		return "", "", ErrNeedsSession
	}
	if !s.providerSite(client.Site) {
		return "", "", ErrNoProviderHere
	}
	if _, _, err := s.offered(ctx, providerID); err != nil {
		return "", "", err
	}
	// The proof is held, and given back if the provider can't be reached, so trying
	// again doesn't take the passkey again (#646's review, L2).
	giveBack, err := s.HoldStepUp(ctx, client, p, proof, StepUpLinkProvider, providerID)
	if err != nil {
		return "", "", err
	}
	target, flow, err = s.beginProvider(ctx, client, providerID, &ceremony{user: p.User, sessionID: p.Session.ID})
	if err != nil {
		giveBack()
	}
	return target, flow, err
}

// beginProvider keeps cer as the flow, and says where the provider is.
func (s *Service) beginProvider(ctx context.Context, client Client, providerID string, cer *ceremony) (string, string, error) {
	provider, _, err := s.offered(ctx, providerID)
	if err != nil {
		return "", "", err
	}
	s.providers.mu.Lock()
	op := s.providers.client
	s.providers.mu.Unlock()
	doc, err := op.Discover(ctx, provider.Issuer)
	if err != nil {
		return "", "", &ProviderError{fmt.Errorf("can't reach %s to sign in: %w. Try again in a minute, or sign in with your passkey", provider.Label(), err)}
	}
	now := s.now()
	cer.kind, cer.site, cer.expires = ceremonyProvider, client.Site, now.Add(ProviderFlowTTL)
	cer.provider, cer.issuer, cer.begun = provider.ID, provider.Issuer, oidc.Begin()
	flow := s.ceremonies.put(cer, now)
	return cer.begun.Authorize(doc, provider.ClientID, redirectURI(client.Site), provider.Scopes), flow, nil
}

// ProviderDone is what the way back from a provider did: signed someone in, or
// linked an account to them.
type ProviderDone struct {
	SignedIn *SignedIn
	Linked   *store.ProviderLink
	// Provider is what the provider is called.
	Provider string
}

// FinishProvider takes the way back from a provider: the flow the cookie holds must
// be one this server began, for this site, with the state the provider sends back,
// and it's used once. The code is exchanged, the ID token checked, and the account
// signs its person in, or is linked to the person who began linking it.
func (s *Service) FinishProvider(ctx context.Context, client Client, flow string, query url.Values) (ProviderDone, error) {
	now := s.now()
	cer, err := s.ceremonies.take(flow, ceremonyProvider, now)
	if err != nil {
		return ProviderDone{}, err
	}
	if cer.site != client.Site || subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(cer.begun.State)) != 1 {
		return ProviderDone{}, ErrCeremonyExpired
	}
	if said := query.Get("error"); said != "" {
		return ProviderDone{}, &ProviderError{fmt.Errorf("the provider said %q. Try again, or sign in with your passkey", clip(said, 80))}
	}
	// No code spends nothing of the install's exchanges (#646's review, M2).
	if query.Get("code") == "" {
		return ProviderDone{}, &ProviderError{errors.New("the provider sent no code. Try again, or sign in with your passkey")}
	}
	provider, providers, err := s.offered(ctx, cer.provider)
	if err != nil {
		return ProviderDone{}, err
	}
	if provider.Issuer != cer.issuer {
		return ProviderDone{}, ErrCeremonyExpired
	}
	release, ok := s.providerExchange(sourceKey(client.Addr))
	if !ok {
		return ProviderDone{}, ErrProviderBusy
	}
	s.providers.mu.Lock()
	op := s.providers.client
	s.providers.mu.Unlock()
	claims, err := op.Redeem(ctx, oidc.Exchange{
		Issuer: provider.Issuer, ClientID: provider.ClientID, ClientSecret: provider.ClientSecret,
		Code: query.Get("code"), RedirectURI: redirectURI(client.Site),
		Verifier: cer.begun.Verifier, Nonce: cer.begun.Nonce, GroupsClaim: provider.GroupsClaim,
	})
	release()
	if err != nil {
		return ProviderDone{}, &ProviderError{fmt.Errorf("signing in with %s didn't finish: %w", provider.Label(), err)}
	}
	if cer.sessionID != "" {
		return s.finishLink(ctx, client, cer, provider, providers, claims)
	}
	return s.finishProviderSignIn(ctx, client, provider, providers, claims)
}

// finishProviderSignIn signs in the person the account is linked to, who still has
// a role. Anything else is refused in the same words, and audited.
func (s *Service) finishProviderSignIn(ctx context.Context, client Client, provider Provider, providers []Provider, claims oidc.Claims) (ProviderDone, error) {
	var done ProviderDone
	var refused error
	var ended []string
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		refuse := func(subject, why string) error {
			refused = ErrProviderNotLinked
			return s.audit(ctx, tx, client.anonymous(), "auth.sign_in_failed", subject,
				fmt.Sprintf("provider %s on %s from %s: %s", provider.Name, client.Site.RPID, client.remoteIP(), why))
		}
		link, err := tx.GetProviderLinkByAccount(ctx, provider.Issuer, claims.Subject)
		if errors.Is(err, store.ErrNotFound) {
			return refuse("", "an account linked to no one")
		}
		if err != nil {
			return err
		}
		if link.ProviderID != provider.ID {
			// A link through a provider that's gone holds the account for nothing:
			// it goes, so the account can link here (#646's re-check, N1).
			if gone(&link, providers) {
				if ended, err = s.dropLink(ctx, tx, link, "its provider was deleted, and the account signed in at another"); err != nil {
					return err
				}
			}
			return refuse(link.UserID, "an account linked through another provider")
		}
		user, err := tx.GetUser(ctx, link.UserID)
		if err != nil {
			return err
		}
		grants, err := tx.Grants(ctx, user.ID)
		if err != nil {
			return err
		}
		if len(grants) == 0 {
			return refuse(user.ID, "linked to someone with no role left")
		}
		if err := tx.UseProviderLink(ctx, link.ID, s.now()); err != nil {
			return err
		}
		signedIn, err := s.startLinkedSession(ctx, tx, user, client, MethodProvider, link, provider.Name)
		done = ProviderDone{SignedIn: &signedIn, Provider: provider.Label()}
		return err
	})
	if err != nil {
		return ProviderDone{}, err
	}
	s.endStreams(ended)
	if refused != nil {
		return ProviderDone{}, refused
	}
	return done, nil
}

// finishLink links the account to the person who began linking it, while the
// session they began it in still works.
func (s *Service) finishLink(ctx context.Context, client Client, cer *ceremony, provider Provider, providers []Provider, claims oidc.Claims) (ProviderDone, error) {
	now := s.now()
	link := store.ProviderLink{
		ID: id.New(id.ProviderLink), UserID: cer.user.ID, ProviderID: provider.ID, Issuer: provider.Issuer,
		Subject: claims.Subject, Account: clip(claims.Email, 254), CreatedAt: now,
	}
	var ended []string
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		ended = nil
		sess, err := tx.GetSession(ctx, cer.sessionID)
		if err != nil || !live(sess, client.Site, now) || sess.UserID != cer.user.ID {
			return ErrUnauthenticated
		}
		// A link in the way through a provider that's gone, as a fresh read finds
		// them, holds the account or the place for nothing: it goes, and the link
		// is made (#646's re-check, N1).
		if held, err := tx.GetProviderLinkByAccount(ctx, link.Issuer, link.Subject); err == nil && gone(&held, providers) {
			if ended, err = s.dropLink(ctx, tx, held, "its provider was deleted, and the account linked at another"); err != nil {
				return err
			}
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := tx.CreateProviderLink(ctx, link); errors.Is(err, store.ErrConflict) {
			mine, lerr := tx.ListProviderLinks(ctx, cer.user.ID)
			if lerr != nil {
				return lerr
			}
			if slices.ContainsFunc(mine, func(l store.ProviderLink) bool { return l.ProviderID == provider.ID }) {
				return ErrProviderLinkedTwice
			}
			return ErrProviderLinkedElse
		} else if err != nil {
			return err
		}
		return s.audit(ctx, tx, "user:"+cer.user.ID, "provider_link.create", cer.user.ID,
			fmt.Sprintf("%s at %s, link %s, from %s", link.Account, provider.Name, link.ID, client.remoteIP()))
	})
	if err != nil {
		return ProviderDone{}, err
	}
	s.endStreams(ended)
	return ProviderDone{Linked: &link, Provider: provider.Label()}, nil
}

// providerExchange takes a place for one code's exchange from address, or says
// there's none: at once and in a minute for the install, and in a minute for one
// address.
func (s *Service) providerExchange(address string) (release func(), ok bool) {
	src := &s.providers
	src.mu.Lock()
	defer src.mu.Unlock()
	now := s.now()
	old := func(t time.Time) bool { return now.Sub(t) >= time.Minute }
	src.recent = slices.DeleteFunc(src.recent, old)
	if src.byAddress == nil {
		src.byAddress = map[string][]time.Time{}
	}
	if len(src.byAddress) >= maxExchangeAddresses {
		for a, times := range src.byAddress {
			if times = slices.DeleteFunc(times, old); len(times) == 0 {
				delete(src.byAddress, a)
			}
		}
	}
	mine := slices.DeleteFunc(src.byAddress[address], old)
	if src.active >= providerExchangesAtOnce || len(src.recent) >= providerExchangesAMinute ||
		len(mine) >= providerExchangesAnAddress || len(src.byAddress) >= maxExchangeAddresses {
		return nil, false
	}
	src.active++
	src.recent = append(src.recent, now)
	src.byAddress[address] = append(mine, now)
	return func() {
		src.mu.Lock()
		src.active--
		src.mu.Unlock()
	}, true
}

// LinkState is whether a link signs its person in.
type LinkState int

// What a link's provider leaves it.
const (
	LinkWorks LinkState = iota + 1
	// LinkProviderOff is a provider that no longer signs people in to Dockwell.
	LinkProviderOff
	// LinkIssuerMoved is a provider at another issuer, where this account isn't.
	LinkIssuerMoved
)

// LinkView is a link, with its provider's name and whether it works.
type LinkView struct {
	store.ProviderLink
	Provider string
	State    LinkState
	// Here says it began the session asking, so unlinking it signs them out here.
	Here bool
}

// stateOf is what providers leave link, and whether its provider is gone.
func stateOf(link *store.ProviderLink, providers []Provider) (Provider, LinkState, bool) {
	i := slices.IndexFunc(providers, func(p Provider) bool { return p.ID == link.ProviderID })
	switch {
	case i < 0:
		return Provider{}, 0, true
	case !providers[i].Panel:
		return providers[i], LinkProviderOff, false
	case providers[i].Issuer != link.Issuer:
		return providers[i], LinkIssuerMoved, false
	}
	return providers[i], LinkWorks, false
}

// ListProviderLinks lists p's links, oldest first, with whether each works. It
// writes nothing (#646's review, L3). A link whose provider is gone is left out: it's
// kept for an undo to bring back, and goes if it stands in the way.
func (s *Service) ListProviderLinks(ctx context.Context, p Principal) ([]LinkView, error) {
	providers, err := s.allProviders(ctx, false)
	if err != nil {
		return nil, err
	}
	var out []LinkView
	err = s.db.View(ctx, func(tx *store.Tx) error {
		out = nil
		links, err := tx.ListProviderLinks(ctx, p.User.ID)
		for i := range links {
			l := &links[i]
			if provider, state, gone := stateOf(l, providers); !gone {
				out = append(out, LinkView{ProviderLink: *l, Provider: provider.Label(), State: state, Here: l.ID == p.Session.ProviderLinkID})
			}
		}
		return err
	})
	return out, err
}

// DeleteProviderLink unlinks one of p's accounts, and ends every session it began,
// and their streams, at once. It needs no passkey, as it only takes a way in away.
func (s *Service) DeleteProviderLink(ctx context.Context, p Principal, linkID string) error {
	if p.Token != nil || p.Session.ID == "" {
		return ErrNeedsSession
	}
	var ended []string
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		link, err := tx.GetProviderLink(ctx, linkID)
		if err != nil || link.UserID != p.User.ID {
			return fmt.Errorf("provider link %s: %w", linkID, store.ErrNotFound)
		}
		if ended, err = tx.DeleteProviderLink(ctx, p.User.ID, linkID, s.now()); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Actor(), "provider_link.delete", p.User.ID,
			fmt.Sprintf("%s, link %s, which ends the sessions it began", link.Account, link.ID))
	})
	if err != nil {
		return err
	}
	s.endStreams(ended)
	return nil
}

// dropLink deletes a link whose provider is gone, ending its sessions, says so in
// the audit log, and says which sessions ended, for their streams to end once it
// commits.
func (s *Service) dropLink(ctx context.Context, tx *store.Tx, l store.ProviderLink, why string) ([]string, error) {
	ended, err := tx.DeleteProviderLink(ctx, "", l.ID, s.now())
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ended, s.audit(ctx, tx, "system", "provider_link.delete", l.UserID, fmt.Sprintf("%s, link %s: %s", l.Account, l.ID, why))
}

// errLinkStopped is a session whose link no longer signs anyone in, which
// Authenticate ends.
type errLinkStopped struct {
	link store.ProviderLink
	why  string
}

func (e *errLinkStopped) Error() string {
	return "the session's provider link stopped working: " + e.why
}

// linkStops says why a session's link no longer signs its person in, or nil while it
// does: its provider is there, signs people in to Dockwell, and has the link's
// issuer. It reads nothing, so the providers are read outside any transaction.
func linkStops(link *store.ProviderLink, providers []Provider) *errLinkStopped {
	switch _, state, gone := stateOf(link, providers); {
	case gone:
		return &errLinkStopped{link: *link, why: "its provider was deleted"}
	case state == LinkProviderOff:
		return &errLinkStopped{link: *link, why: "its provider no longer signs people in to Dockwell"}
	case state == LinkIssuerMoved:
		return &errLinkStopped{link: *link, why: "its provider moved to another issuer"}
	}
	return nil
}

// sessionLinkStops reads the providers, outside any transaction, and says whether
// the link a session began from still signs its person in.
func (s *Service) sessionLinkStops(ctx context.Context, link *store.ProviderLink) (*errLinkStopped, error) {
	providers, err := s.allProviders(ctx, false)
	if err != nil {
		return nil, err
	}
	return linkStops(link, providers), nil
}

// endStoppedLink ends the sessions a link that stopped working began, and their
// streams, as Authenticate finds it. The link stays: a provider deleted and brought
// back by an undo has its ID again, and its links work again (#646's re-check, N1).
// It goes on whoever asked leaving, as one who did isn't a reason to leave the
// sessions be (#646's re-check, N3).
func (s *Service) endStoppedLink(ctx context.Context, stopped *errLinkStopped) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stoppedLinkWait)
	defer cancel()
	var ended []string
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		if ended, err = tx.EndProviderLinkSessions(ctx, stopped.link.ID, s.now()); err != nil || len(ended) == 0 {
			return err
		}
		return s.audit(ctx, tx, "system", "auth.sessions_ended", stopped.link.UserID,
			fmt.Sprintf("%d begun with %s, link %s: %s", len(ended), stopped.link.Account, stopped.link.ID, stopped.why))
	})
	if err != nil {
		s.logger.WarnContext(ctx, "can't end the sessions of a provider link that stopped working; each is refused all the same",
			slog.String("link", stopped.link.ID), slog.Any("error", err))
		return
	}
	s.endStreams(ended)
}

// clip keeps what a provider says to a length a record holds.
func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
