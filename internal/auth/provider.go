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

// providerSource reads the providers, kept for a few seconds, and bounds the codes
// exchanged with them.
type providerSource struct {
	mu     sync.Mutex
	read   func(context.Context) ([]Provider, error)
	client *oidc.Client
	list   []Provider
	at     time.Time
	active int
	recent []time.Time
}

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
// fresh.
func (s *Service) allProviders(ctx context.Context, fresh bool) ([]Provider, error) {
	src := &s.providers
	src.mu.Lock()
	read, kept, at := src.read, src.list, src.at
	src.mu.Unlock()
	if read == nil {
		return nil, nil
	}
	now := s.now()
	if !fresh && !at.IsZero() && now.Sub(at) < providersFor {
		return kept, nil
	}
	list, err := read(ctx)
	if err != nil {
		return nil, fmt.Errorf("can't read the sign-in providers: %w", err)
	}
	src.mu.Lock()
	src.list, src.at = list, now
	src.mu.Unlock()
	return list, nil
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
	for _, p := range all {
		if p.Panel {
			out = append(out, p)
		}
	}
	return out, nil
}

// providerSite says whether site is the panel's own domain over HTTPS, where a
// provider can bring someone back to.
func (s *Service) providerSite(site Site) bool {
	return site.Secure && s.sites.Domain != "" && site.RPID == s.sites.Domain
}

// offered is the provider with id, read now, if it signs people in to the panel.
func (s *Service) offered(ctx context.Context, providerID string) (Provider, error) {
	all, err := s.allProviders(ctx, true)
	if err != nil {
		return Provider{}, err
	}
	i := slices.IndexFunc(all, func(p Provider) bool { return p.ID == providerID })
	if i < 0 || !all[i].Panel {
		return Provider{}, ErrProviderNotOffered
	}
	return all[i], nil
}

// redirectURI is where the provider brings someone back to, on site.
func redirectURI(site Site) string { return site.Origin + ProviderCallbackPath }

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
	if _, err := s.offered(ctx, providerID); err != nil {
		return "", "", err
	}
	if err := s.UseStepUp(ctx, client, p, proof, StepUpLinkProvider, providerID); err != nil {
		return "", "", err
	}
	return s.beginProvider(ctx, client, providerID, &ceremony{user: p.User, sessionID: p.Session.ID})
}

// beginProvider keeps cer as the flow, and says where the provider is.
func (s *Service) beginProvider(ctx context.Context, client Client, providerID string, cer *ceremony) (string, string, error) {
	provider, err := s.offered(ctx, providerID)
	if err != nil {
		return "", "", err
	}
	s.providers.mu.Lock()
	op := s.providers.client
	s.providers.mu.Unlock()
	doc, err := op.Discover(ctx, provider.Issuer)
	if err != nil {
		return "", "", fmt.Errorf("can't reach %s to sign in: %w. Try again in a minute, or sign in with your passkey", provider.Label(), err)
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
		return ProviderDone{}, fmt.Errorf("the provider said %q. Try again, or sign in with your passkey", clip(said, 80))
	}
	provider, err := s.offered(ctx, cer.provider)
	if err != nil {
		return ProviderDone{}, err
	}
	if provider.Issuer != cer.issuer {
		return ProviderDone{}, ErrCeremonyExpired
	}
	release, ok := s.providerExchange()
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
		return ProviderDone{}, fmt.Errorf("signing in with %s didn't finish: %w", provider.Label(), err)
	}
	if cer.sessionID != "" {
		return s.finishLink(ctx, client, cer, provider, claims)
	}
	return s.finishProviderSignIn(ctx, client, provider, claims)
}

// finishProviderSignIn signs in the person the account is linked to, who still has
// a role. Anything else is refused in the same words, and audited.
func (s *Service) finishProviderSignIn(ctx context.Context, client Client, provider Provider, claims oidc.Claims) (ProviderDone, error) {
	var done ProviderDone
	var refused error
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
	if refused != nil {
		return ProviderDone{}, refused
	}
	return done, nil
}

// finishLink links the account to the person who began linking it, while the
// session they began it in still works.
func (s *Service) finishLink(ctx context.Context, client Client, cer *ceremony, provider Provider, claims oidc.Claims) (ProviderDone, error) {
	now := s.now()
	link := store.ProviderLink{
		ID: id.New(id.ProviderLink), UserID: cer.user.ID, ProviderID: provider.ID, Issuer: provider.Issuer,
		Subject: claims.Subject, Account: clip(claims.Email, 254), CreatedAt: now,
	}
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		sess, err := tx.GetSession(ctx, cer.sessionID)
		if err != nil || !live(sess, client.Site, now) || sess.UserID != cer.user.ID {
			return ErrUnauthenticated
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
	return ProviderDone{Linked: &link, Provider: provider.Label()}, nil
}

// providerExchange takes a place for one code's exchange, or says there's none.
func (s *Service) providerExchange() (release func(), ok bool) {
	src := &s.providers
	src.mu.Lock()
	defer src.mu.Unlock()
	now := s.now()
	src.recent = slices.DeleteFunc(src.recent, func(t time.Time) bool { return now.Sub(t) >= time.Minute })
	if src.active >= providerExchangesAtOnce || len(src.recent) >= providerExchangesAMinute {
		return nil, false
	}
	src.active++
	src.recent = append(src.recent, now)
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
}

// stateOf is what providers leave link, and whether its provider is gone.
func stateOf(link store.ProviderLink, providers []Provider) (Provider, LinkState, bool) {
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

// ListProviderLinks lists p's links, oldest first, with whether each works. A link
// whose provider was deleted is deleted here, as its provider never comes back.
func (s *Service) ListProviderLinks(ctx context.Context, p Principal) ([]LinkView, error) {
	providers, err := s.allProviders(ctx, false)
	if err != nil {
		return nil, err
	}
	var out []LinkView
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		out = nil
		links, err := tx.ListProviderLinks(ctx, p.User.ID)
		if err != nil {
			return err
		}
		for _, l := range links {
			provider, state, gone := stateOf(l, providers)
			if gone {
				if err := s.dropLink(ctx, tx, l, "its provider was deleted"); err != nil {
					return err
				}
				continue
			}
			out = append(out, LinkView{ProviderLink: l, Provider: provider.Label(), State: state})
		}
		return nil
	})
	return out, err
}

// DeleteProviderLink unlinks one of p's accounts, and ends every session it began.
// It needs no passkey, as it only takes a way in away.
func (s *Service) DeleteProviderLink(ctx context.Context, p Principal, linkID string) error {
	if p.Token != nil || p.Session.ID == "" {
		return ErrNeedsSession
	}
	return s.db.Update(ctx, func(tx *store.Tx) error {
		link, err := tx.GetProviderLink(ctx, linkID)
		if err != nil || link.UserID != p.User.ID {
			return fmt.Errorf("provider link %s: %w", linkID, store.ErrNotFound)
		}
		if err := tx.DeleteProviderLink(ctx, p.User.ID, linkID, s.now()); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Actor(), "provider_link.delete", p.User.ID,
			fmt.Sprintf("%s, link %s, which ends the sessions it began", link.Account, link.ID))
	})
}

// dropLink deletes a link whose provider is gone, ending its sessions, and says so
// in the audit log.
func (s *Service) dropLink(ctx context.Context, tx *store.Tx, l store.ProviderLink, why string) error {
	if err := tx.DeleteProviderLink(ctx, "", l.ID, s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return s.audit(ctx, tx, "system", "provider_link.delete", l.UserID, fmt.Sprintf("%s, link %s: %s", l.Account, l.ID, why))
}

// errLinkStopped is a session whose link no longer signs anyone in, which
// Authenticate ends.
type errLinkStopped struct {
	link    store.ProviderLink
	why     string
	deleted bool
}

func (e *errLinkStopped) Error() string {
	return "the session's provider link stopped working: " + e.why
}

// checkLink says whether the link a session began from still signs its person in:
// its provider is there, signs people in to Dockwell, and has the link's issuer.
func (s *Service) checkLink(ctx context.Context, tx *store.Tx, sess store.Session) error {
	link, err := tx.GetProviderLink(ctx, sess.ProviderLinkID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrUnauthenticated
	}
	if err != nil {
		return err
	}
	providers, err := s.allProviders(ctx, false)
	if err != nil {
		return err
	}
	switch _, state, gone := stateOf(link, providers); {
	case gone:
		return &errLinkStopped{link: link, why: "its provider was deleted", deleted: true}
	case state == LinkProviderOff:
		return &errLinkStopped{link: link, why: "its provider no longer signs people in to Dockwell"}
	case state == LinkIssuerMoved:
		return &errLinkStopped{link: link, why: "its provider moved to another issuer"}
	}
	return nil
}

// endStoppedLink ends the sessions a link that stopped working began, and deletes
// it where its provider is gone, as Authenticate finds it.
func (s *Service) endStoppedLink(ctx context.Context, stopped *errLinkStopped) {
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		if stopped.deleted {
			return s.dropLink(ctx, tx, stopped.link, stopped.why)
		}
		n, err := tx.EndProviderLinkSessions(ctx, stopped.link.ID, s.now())
		if err != nil || n == 0 {
			return err
		}
		return s.audit(ctx, tx, "system", "auth.sessions_ended", stopped.link.UserID,
			fmt.Sprintf("%d begun with %s, link %s: %s", n, stopped.link.Account, stopped.link.ID, stopped.why))
	})
	if err != nil {
		s.logger.WarnContext(ctx, "can't end the sessions of a provider link that stopped working; each is refused all the same",
			slog.String("link", stopped.link.ID), slog.Any("error", err))
	}
}

// clip keeps what a provider says to a length a record holds.
func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
