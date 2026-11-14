// SPDX-License-Identifier: AGPL-3.0-only

package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// MaxAnswer caps what a provider's answer reads.
	MaxAnswer = 256 << 10
	// discoveryFailFor is how long a provider that couldn't be read stays so, so a
	// crowd waits on one read, not one each (#598's review, L4).
	discoveryFailFor = 30 * time.Second
	discoveryFor     = time.Hour
	keysAgainAfter   = time.Minute
)

// Doer sends a request, as netguard's client does.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client reads providers' documents and keys, kept between sign-ins, and exchanges
// codes for tokens. Its zero value isn't ready: set HTTP.
type Client struct {
	// HTTP reads the provider, as netguard's client does, never this server's own
	// addresses or a metadata service.
	HTTP Doer
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu      sync.Mutex
	issuers map[string]*issuerState
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Discovery is what sign-in needs of an issuer's discovery document.
type Discovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	AuthMethods           []string `json:"token_endpoint_auth_methods_supported"`
	// EndSessionEndpoint is where someone signs out at the provider, if it says one.
	EndSessionEndpoint string `json:"end_session_endpoint"`
}

// issuerState is what's kept of an issuer: its document, or why it couldn't be read,
// and its keys, each with the read under way, if one is.
type issuerState struct {
	mu        sync.Mutex
	doc       *Discovery
	docAt     time.Time
	docFailed error
	failedAt  time.Time
	keys      []Key
	keysAt    time.Time
	reading   map[string]chan struct{}
}

func (c *Client) issuer(issuer string) *issuerState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.issuers == nil {
		c.issuers = map[string]*issuerState{}
	}
	st, ok := c.issuers[issuer]
	if !ok {
		st = &issuerState{reading: map[string]chan struct{}{}}
		c.issuers[issuer] = st
	}
	return st
}

// once has one read of what's named under way at a time. kept says, with st.mu held,
// whether what's kept will do; otherwise the caller reads, or waits for the read under
// way as long as its own request lasts and then looks again. The read goes on for
// those still waiting, bounded by the client's own timeout, whoever leaves. A caller
// that read takes what its read left, and doesn't read again.
func (c *Client) once(ctx context.Context, st *issuerState, what string, kept func() bool, read func(context.Context)) error {
	for {
		st.mu.Lock()
		if kept() {
			st.mu.Unlock()
			return nil
		}
		if wait, ok := st.reading[what]; ok {
			st.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		done := make(chan struct{})
		st.reading[what] = done
		st.mu.Unlock()
		read(context.WithoutCancel(ctx))
		st.mu.Lock()
		delete(st.reading, what)
		kept()
		st.mu.Unlock()
		close(done)
		return nil
	}
}

// Discover reads an issuer's discovery document, kept for an hour, or what kept it
// from being read, kept for 30 seconds.
func (c *Client) Discover(ctx context.Context, issuer string) (*Discovery, error) {
	st := c.issuer(issuer)
	var doc *Discovery
	var failed error
	kept := func() bool {
		switch {
		case st.doc != nil && c.now().Sub(st.docAt) < discoveryFor:
			doc = st.doc
			return true
		case st.docFailed != nil && c.now().Sub(st.failedAt) < discoveryFailFor:
			failed = st.docFailed
			return true
		}
		return false
	}
	err := c.once(ctx, st, "discovery", kept, func(ctx context.Context) {
		read, err := c.readDiscovery(ctx, issuer)
		st.mu.Lock()
		defer st.mu.Unlock()
		if err != nil {
			st.docFailed, st.failedAt = err, c.now()
			return
		}
		st.doc, st.docAt, st.docFailed = read, c.now(), nil
	})
	switch {
	case err != nil:
		return nil, err
	case doc == nil && failed == nil:
		return nil, &UnreachableError{Host: HostOf(issuer), Err: errors.New("its discovery document couldn't be read")}
	}
	return doc, failed
}

// readDiscovery reads an issuer's discovery document, and checks it.
func (c *Client) readDiscovery(ctx context.Context, issuer string) (*Discovery, error) {
	var doc Discovery
	if err := c.getJSON(ctx, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil {
		return nil, err
	}
	for _, endpoint := range []string{doc.AuthorizationEndpoint, doc.TokenEndpoint, doc.JWKSURI} {
		if u, err := url.Parse(endpoint); err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, errors.New("the provider's discovery document has an endpoint that isn't HTTPS")
		}
	}
	if doc.Issuer != issuer {
		return nil, errors.New("the provider's discovery document names another issuer than the one saved")
	}
	// Signing out at the provider is only ever over HTTPS; one that isn't is left out,
	// and signing out stays where it began.
	if u, err := url.Parse(doc.EndSessionEndpoint); err != nil || u.Scheme != "https" || u.Host == "" {
		doc.EndSessionEndpoint = ""
	}
	return &doc, nil
}

// keys reads an issuer's key set, kept until a token names a key it doesn't hold,
// and then read again at most once a minute.
func (c *Client) keys(ctx context.Context, issuer string, doc *Discovery, again bool) ([]Key, error) {
	st := c.issuer(issuer)
	var keys []Key
	var failed error
	kept := func() bool {
		if st.keys != nil && (!again || c.now().Sub(st.keysAt) < keysAgainAfter) {
			keys = st.keys
			return true
		}
		return false
	}
	err := c.once(ctx, st, "keys", kept, func(ctx context.Context) {
		var raw json.RawMessage
		if failed = c.getJSON(ctx, doc.JWKSURI, &raw); failed != nil {
			return
		}
		read, err := ParseKeys(raw)
		if failed = err; err != nil {
			return
		}
		keys = read
		st.mu.Lock()
		st.keys, st.keysAt = read, c.now()
		st.mu.Unlock()
	})
	switch {
	case err != nil:
		return nil, err
	case keys == nil:
		return nil, failed
	}
	return keys, nil
}

// getJSON reads a provider's document.
func (c *Client) getJSON(ctx context.Context, target string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return &UnreachableError{Host: HostOf(target), Err: err}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return &UnreachableError{Host: HostOf(target), Err: fmt.Errorf("it answered %d", res.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxAnswer))
	if err != nil {
		return &UnreachableError{Host: HostOf(target), Err: err}
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("what %s sent isn't JSON", HostOf(target))
	}
	return nil
}

// Begun is a sign-in sent to the provider: what the callback checks it against.
type Begun struct {
	State, Nonce, Verifier string
}

// Begin is a new sign-in's state, nonce and PKCE verifier.
func Begin() Begun {
	return Begun{State: Random(), Nonce: Random(), Verifier: Random()}
}

// Authorize is the address that sends someone to the provider to sign in, with
// PKCE (S256), the sign-in's state and nonce, and the scopes sign-in always asks for
// before scopes.
func (b Begun) Authorize(doc *Discovery, clientID, redirectURI string, scopes []string) string {
	challenge := sha256.Sum256([]byte(b.Verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(append([]string{"openid", "email", "profile"}, scopes...), " ")},
		"state":                 {b.State},
		"nonce":                 {b.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	return WithQuery(doc.AuthorizationEndpoint, q)
}

// WithQuery adds q to target's own query.
func WithQuery(target string, q url.Values) string {
	if strings.Contains(target, "?") {
		return target + "&" + q.Encode()
	}
	return target + "?" + q.Encode()
}

// Exchange is a code to exchange, and who it's for.
type Exchange struct {
	Issuer, ClientID, ClientSecret string
	Code, RedirectURI              string
	// Verifier and Nonce are the sign-in's, as Begin made them.
	Verifier, Nonce string
	// GroupsClaim is the claim the provider lists groups in; empty is "groups".
	GroupsClaim string
}

// Redeem exchanges a code for tokens at the provider, and checks the ID token.
func (c *Client) Redeem(ctx context.Context, x Exchange) (Claims, error) {
	if x.Code == "" {
		return Claims{}, errors.New("the provider sent no code")
	}
	doc, err := c.Discover(ctx, x.Issuer)
	if err != nil {
		return Claims{}, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {x.Code}, "redirect_uri": {x.RedirectURI}, "code_verifier": {x.Verifier}}
	basic := false
	switch {
	case x.ClientSecret == "":
		form.Set("client_id", x.ClientID)
	case slices.Contains(doc.AuthMethods, "client_secret_post") && !slices.Contains(doc.AuthMethods, "client_secret_basic"):
		form.Set("client_id", x.ClientID)
		form.Set("client_secret", x.ClientSecret)
	default:
		basic = true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Claims{}, err
	}
	if basic {
		req.SetBasicAuth(url.QueryEscape(x.ClientID), url.QueryEscape(x.ClientSecret))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return Claims{}, &UnreachableError{Host: HostOf(doc.TokenEndpoint), Err: err}
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxAnswer))
	if err != nil {
		return Claims{}, &UnreachableError{Host: HostOf(doc.TokenEndpoint), Err: err}
	}
	var tokens struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(body, &tokens)
	switch {
	case res.StatusCode != http.StatusOK && tokens.Error == "invalid_client":
		return Claims{}, errors.New("the provider refused Dockwell's client. Check its client ID and secret in Settings")
	case res.StatusCode != http.StatusOK:
		return Claims{}, fmt.Errorf("the provider answered %d for the code", res.StatusCode)
	case tokens.IDToken == "":
		return Claims{}, errors.New("the provider sent no ID token. Check that its client asks for openid")
	}
	want := Want{Issuer: x.Issuer, ClientID: x.ClientID, Nonce: x.Nonce, GroupsClaim: x.GroupsClaim}
	keys, err := c.keys(ctx, x.Issuer, doc, false)
	if err != nil {
		return Claims{}, err
	}
	claims, err := Verify(tokens.IDToken, keys, want, c.now())
	if errors.Is(err, ErrUnknownKey) {
		// The provider may have rotated its keys: read them again, at most once a minute.
		if keys, err = c.keys(ctx, x.Issuer, doc, true); err != nil {
			return Claims{}, err
		}
		claims, err = Verify(tokens.IDToken, keys, want, c.now())
	}
	return claims, err
}

// UnreachableError is a provider this server couldn't reach.
type UnreachableError struct {
	Host string
	Err  error
}

func (e *UnreachableError) Error() string { return "can't reach " + e.Host + ": " + e.Err.Error() }

func (e *UnreachableError) Unwrap() error { return e.Err }

// Random is 32 random bytes in base64url, for state, nonce and the PKCE verifier.
func Random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HostOf is an address's host, or "the provider".
func HostOf(target string) string {
	if u, err := url.Parse(target); err == nil && u.Host != "" {
		return u.Host
	}
	return "the provider"
}
