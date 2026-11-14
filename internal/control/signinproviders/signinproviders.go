// SPDX-License-Identifier: AGPL-3.0-only

// Package signinproviders reads the saved sign-in providers, and an issuer's
// discovery document, as saving a provider checks it live (ADR 0087). It never
// logs a provider's secret, and nothing it says holds one.
package signinproviders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource/signinprovider"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Doer sends a request, as netguard's client does.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Providers reads the saved sign-in providers, and issuers' discovery documents.
type Providers struct {
	db     *store.DB
	keeper *secrets.Keeper
	// Client reads discovery documents: netguard's, which reaches the internet and
	// the private networks an admin may name, and never this server's own
	// addresses, a link-local one or a metadata service. A test gives its own.
	Client Doer
}

// New reads providers from db, opened with keeper.
func New(db *store.DB, keeper *secrets.Keeper) *Providers {
	return &Providers{db: db, keeper: keeper, Client: netguard.Client(netguard.Options{Private: true, MaxBody: maxDocument, Timeout: readWait})}
}

const (
	// maxDocument caps a discovery document, which is a few kilobytes.
	maxDocument = 256 << 10
	// readWait bounds reading one, as a form waits on it.
	readWait = 10 * time.Second
)

// Saved is a provider as saved, with its secret: it goes no further than the
// server a route that uses it runs on, sealed.
type Saved struct {
	ID, Name string
	// Revision is the provider's revision.
	Revision int
	Spec     *resourcev1.SigninProvider
}

// All reads every saved provider, sorted by name.
func (p *Providers) All(ctx context.Context) ([]Saved, error) {
	var out []Saved
	err := p.db.View(ctx, func(tx *store.Tx) error {
		stored, err := tx.ListKind(ctx, string(signinprovider.Kind))
		if err != nil {
			return err
		}
		for i := range stored {
			r := &stored[i]
			if r.Spec == nil {
				continue
			}
			spec, err := p.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return fmt.Errorf("can't read the sign-in provider %s: %w", r.Name, err)
			}
			if provider, ok := spec.(*resourcev1.SigninProvider); ok {
				out = append(out, Saved{ID: r.ID, Name: r.Name, Revision: r.Revision, Spec: provider})
			}
		}
		return nil
	})
	slices.SortFunc(out, func(a, b Saved) int { return strings.Compare(a.Name, b.Name) })
	return out, err
}

// Allows says whether a provider lets project's routes use it: one that names no
// project lets every project.
func Allows(s *resourcev1.SigninProvider, project string) bool {
	return len(s.GetProjects()) == 0 || slices.Contains(s.GetProjects(), project)
}

// Algorithms are the ID token signatures Dockwell checks, strongest first: never
// "none", and never an HMAC, which signs with what the client also holds.
var Algorithms = []string{"EdDSA", "ES256", "PS256", "RS256"}

// document is what a discovery document holds that sign-in needs.
type document struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	ResponseTypes         []string `json:"response_types_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	SigningAlgorithms     []string `json:"id_token_signing_alg_values_supported"`
}

// Check reads an issuer's discovery document, as typed, and says what it found,
// or why Dockwell can't sign people in with it. It saves nothing.
func (p *Providers) Check(ctx context.Context, typed string) (*dockwellv1.CheckSigninProviderResponse, error) {
	issuer, err := signinprovider.Issuer(typed)
	if err != nil {
		return nil, err
	}
	host := hostOf(issuer)
	out := &dockwellv1.CheckSigninProviderResponse{Issuer: issuer, Host: host, Public: signinprovider.Public(issuer)}
	unusable := func(format string, args ...any) (*dockwellv1.CheckSigninProviderResponse, error) {
		out.State, out.Detail = dockwellv1.SigninProviderState_SIGNIN_PROVIDER_STATE_UNUSABLE, fmt.Sprintf(format, args...)
		return out, nil
	}
	doc, err := p.read(ctx, issuer)
	var said *answered
	switch {
	case errors.As(err, &said):
		return unusable("%s answered %d for its discovery document, at %s. Check the issuer's address", host, said.status, path(wellKnown(issuer)))
	case errors.Is(err, errNotDocument):
		return unusable("What %s sent isn't a discovery document. Check the issuer's address", host)
	case errors.Is(err, netguard.ErrForbidden):
		// Refused for where it leads, which no setting changes: the words say why.
		out.State, out.Detail = dockwellv1.SigninProviderState_SIGNIN_PROVIDER_STATE_UNREACHABLE,
			fmt.Sprintf("Dockwell won't read %s's discovery document: %s. Give the provider's address as people reach it", host, netguard.Words(err))
		return out, nil
	case err != nil:
		out.State, out.Detail = dockwellv1.SigninProviderState_SIGNIN_PROVIDER_STATE_UNREACHABLE,
			fmt.Sprintf("Dockwell can't read %s's discovery document: %s. Check that the control server reaches it", host, netguard.Words(err))
		return out, nil
	}
	// The issuer a token names must be the document's, exactly. Only a trailing
	// "/" is let differ from what was typed, as people write it either way.
	if doc.Issuer != issuer && strings.TrimSuffix(doc.Issuer, "/") != strings.TrimSuffix(issuer, "/") {
		return unusable("%s's document names another issuer, %s, which its tokens would carry. Give that as the issuer", host, doc.Issuer)
	}
	found, err := signinprovider.Issuer(doc.Issuer)
	if err != nil || found != doc.Issuer {
		return unusable("%s's document names its issuer as %q, which isn't an HTTPS address Dockwell can check tokens against", host, doc.Issuer)
	}
	out.Issuer, out.Public = doc.Issuer, signinprovider.Public(doc.Issuer)
	for _, endpoint := range []struct{ name, at string }{
		{"authorization endpoint", doc.AuthorizationEndpoint},
		{"token endpoint", doc.TokenEndpoint},
		{"keys (jwks_uri)", doc.JWKSURI},
	} {
		u, err := url.Parse(endpoint.at)
		switch {
		case endpoint.at == "":
			return unusable("%s's document has no %s, which signing in needs", host, endpoint.name)
		case err != nil || u.Scheme != "https" || u.Host == "":
			return unusable("%s's %s isn't HTTPS, so signing in would cross the network in the clear. Serve it over HTTPS at the provider", host, endpoint.name)
		}
	}
	if !slices.Contains(doc.ResponseTypes, "code") {
		return unusable("%s doesn't offer the authorization code flow, which is how Dockwell signs people in. Turn it on for Dockwell's client at the provider", host)
	}
	// A document that leaves PKCE out may still do it, as Entra's does, which the
	// first sign-in shows; one that names its methods without S256 doesn't.
	if len(doc.CodeChallengeMethods) > 0 && !slices.Contains(doc.CodeChallengeMethods, "S256") {
		return unusable("%s doesn't take PKCE with S256, which keeps a sign-in from being replayed. Turn it on for Dockwell's client at the provider", host)
	}
	for _, alg := range Algorithms {
		if slices.Contains(doc.SigningAlgorithms, alg) {
			out.Algorithms = append(out.Algorithms, alg)
		}
	}
	if len(out.Algorithms) == 0 {
		return unusable("%s signs ID tokens with %s, and Dockwell takes %s. Choose one of those for Dockwell's client at the provider",
			host, strings.Join(doc.SigningAlgorithms, ", "), strings.Join(Algorithms, ", "))
	}
	out.State = dockwellv1.SigninProviderState_SIGNIN_PROVIDER_STATE_FOUND
	return out, nil
}

// errNotDocument is an answer that isn't a discovery document.
var errNotDocument = errors.New("not a discovery document")

// answered is an answer other than 200.
type answered struct{ status int }

func (a *answered) Error() string { return fmt.Sprintf("answered %d", a.status) }

// read gets an issuer's discovery document.
func (p *Providers) read(ctx context.Context, issuer string) (*document, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown(issuer), http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, &answered{status: res.StatusCode}
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	var doc document
	if err := json.Unmarshal(body, &doc); err != nil || doc.Issuer == "" {
		return nil, errNotDocument
	}
	return &doc, nil
}

// wellKnown is where an issuer's discovery document is, as OpenID Connect
// Discovery puts it: the issuer, without a trailing "/", then the well-known path.
func wellKnown(issuer string) string {
	return strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
}

// path is a URL's path, to name where on a host something is.
func path(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Path
	}
	return raw
}

// hostOf is an issuer's host.
func hostOf(issuer string) string {
	if u, err := url.Parse(issuer); err == nil && u.Host != "" {
		return u.Host
	}
	return issuer
}
