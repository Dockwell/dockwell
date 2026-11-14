// SPDX-License-Identifier: AGPL-3.0-only

// Package signinprovider is the "signin.provider" kind: an OpenID Connect provider
// that people sign in to private routes with. It lives in the organization, on the
// control plane: an admin saves it, and names the projects whose routes may use it.
// See docs/adr/0087-private-routes.md.
package signinprovider

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the sign-in provider kind.
const Kind resource.Kind = "signin.provider"

// Type is the kind.
type Type struct{}

var (
	_ resource.Type    = Type{}
	_ resource.Rotates = Type{}
	_ resource.Keeper  = Type{}
)

// Kind returns "signin.provider".
func (Type) Kind() resource.Kind { return Kind }

// Level is the organization: an admin saves a provider, and says which projects'
// routes may use it.
func (Type) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "sip".
func (Type) IDPrefix() string { return "sip" }

// New returns an empty SigninProvider.
func (Type) New() proto.Message { return &resourcev1.SigninProvider{} }

// RotatesSensitive says setting a client secret always plans as a change, never
// compared with what's there, so a plan can't confirm a guess at it (ADR 0022).
func (Type) RotatesSensitive() {}

// CheckName takes what fits in a URL and on a command line.
func (Type) CheckName(name string) error {
	return resource.CheckLabel("sign-in provider", name, "our-authentik")
}

// Asked are the scopes sign-in always asks for, which a provider doesn't list.
var Asked = []string{"openid", "email", "profile"}

const (
	maxDisplayName = 64
	maxClientID    = 255
	maxSecret      = 1024
	maxScopes      = 20
	maxClaim       = 128
)

// Issuer writes an issuer as typed the way a provider keeps it: HTTPS, with
// "auth.example.com" read as https://auth.example.com. It refuses what can't be
// one: another scheme, a user or password in it, a query or a fragment.
func Issuer(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("a sign-in provider needs its issuer, such as https://auth.example.com/application/o/dockwell/")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	switch {
	case err != nil, u.Host == "", u.Opaque != "":
		return "", fmt.Errorf("%q isn't an issuer's address. Write it as its discovery document says it, such as https://auth.example.com/application/o/dockwell/", s)
	case u.Scheme != "https":
		return "", fmt.Errorf("%s isn't HTTPS, so sign-in would send what proves who someone is in the clear. Use the provider's HTTPS address", s)
	case u.User != nil:
		return "", errors.New("an issuer holds no user or password. Give the provider's address alone")
	case u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(s, "?#"):
		return "", fmt.Errorf("%s has a query or a fragment, which an issuer never does. Give it as its discovery document says it", s)
	}
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

// Public reports an issuer anyone can have an account at, as Google's or Entra's
// common one: there, a route that lets in any account would let in the world.
func Public(issuer string) bool {
	u, err := url.Parse(issuer)
	if err != nil {
		return false
	}
	host, path := strings.ToLower(u.Hostname()), strings.Trim(strings.ToLower(u.Path), "/")
	switch host {
	case "accounts.google.com", "appleid.apple.com", "gitlab.com", "login.live.com":
		return true
	case "login.microsoftonline.com", "login.microsoftonline.us", "login.partner.microsoftonline.cn", "sts.windows.net":
		tenant, _, _ := strings.Cut(path, "/")
		// The common endpoints, and the tenant every personal account is in.
		return slices.Contains([]string{"", "common", "organizations", "consumers", "9188040d-6c67-4c5b-b112-36a304b66dad"}, tenant)
	}
	return false
}

// Mailbox reports a domain anyone can sign up for an address at, as gmail.com:
// at a public issuer, a route that lets in anyone at it would let in the world
// (#598's final check, L3). The panel keeps the same list, in
// web/src/features/signin/providers.ts.
func Mailbox(domain string) bool {
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), "@")
	return slices.Contains(mailboxes, strings.TrimSuffix(d, "."))
}

// mailboxes are the domains people most often have a free address at.
var mailboxes = []string{
	"gmail.com", "googlemail.com",
	"outlook.com", "hotmail.com", "live.com", "msn.com", "hotmail.co.uk", "outlook.de", "hotmail.fr",
	"yahoo.com", "ymail.com", "yahoo.co.uk", "yahoo.co.jp", "yahoo.fr", "yahoo.de",
	"icloud.com", "me.com", "mac.com",
	"aol.com", "proton.me", "protonmail.com", "pm.me", "tutanota.com", "tuta.io",
	"gmx.com", "gmx.net", "gmx.de", "web.de", "mail.com", "zoho.com", "fastmail.com", "hey.com",
	"yandex.com", "yandex.ru", "mail.ru",
	"qq.com", "163.com", "126.com", "foxmail.com", "sina.com", "naver.com",
}

// Validate refuses what can't sign anyone in, and writes what it takes the way a
// provider keeps it, in words that hold nothing of its secret.
func (Type) Validate(spec proto.Message) error {
	p := spec.(*resourcev1.SigninProvider) //nolint:forcetypeassert // the kind's own message
	issuer, err := Issuer(p.GetIssuer())
	if err != nil {
		return err
	}
	p.Issuer = issuer
	p.DisplayName = strings.TrimSpace(p.GetDisplayName())
	if p.GetDisplayName() == "" {
		p.DisplayName = hostOf(issuer)
	}
	switch {
	case len([]rune(p.GetDisplayName())) > maxDisplayName:
		return fmt.Errorf("a provider's display name is %d characters at most", maxDisplayName)
	case strings.IndexFunc(p.GetDisplayName(), unicode.IsControl) >= 0:
		return errors.New("a provider's display name is one line of text")
	}
	p.ClientId = strings.TrimSpace(p.GetClientId())
	switch id := p.GetClientId(); {
	case id == "":
		return fmt.Errorf("a provider needs the client ID Dockwell has at %s", hostOf(issuer))
	case len(id) > maxClientID || !printable(id):
		return fmt.Errorf("a client ID is up to %d printable characters with no spaces", maxClientID)
	}
	if len(p.GetClientSecret()) > maxSecret || strings.IndexFunc(p.GetClientSecret(), unicode.IsControl) >= 0 {
		return fmt.Errorf("a client secret is one line of up to %d characters", maxSecret)
	}
	scopes := make([]string, 0, len(p.GetScopes()))
	for _, scope := range p.GetScopes() {
		scope = strings.TrimSpace(scope)
		switch {
		case scope == "" || slices.Contains(Asked, scope) || slices.Contains(scopes, scope):
			continue
		case !scopeToken(scope):
			return fmt.Errorf("%q isn't a scope: a scope is one word of printable characters, with no quote or backslash", scope)
		}
		scopes = append(scopes, scope)
	}
	if len(scopes) > maxScopes {
		return fmt.Errorf("a provider asks for %d scopes at most beyond %s", maxScopes, strings.Join(Asked, ", "))
	}
	p.Scopes = scopes
	p.GroupsClaim = strings.TrimSpace(p.GetGroupsClaim())
	if c := p.GetGroupsClaim(); c != "" && (len(c) > maxClaim || !printable(c)) {
		return fmt.Errorf("a groups claim is a claim's name, up to %d printable characters with no spaces, such as groups", maxClaim)
	}
	if p.GetAnyAccount() && Public(issuer) {
		return fmt.Errorf("anyone can have an account at %s, so a route can't let in every account there. Leave any account off, and name who may sign in on each route", hostOf(issuer))
	}
	for _, project := range p.GetProjects() {
		if !strings.HasPrefix(project, "prj_") {
			return fmt.Errorf("%q isn't a project's ID. A provider names the projects that may use it by ID, such as prj_…", project)
		}
	}
	return nil
}

// Keep holds on to the client secret when an update leaves it out, so a form can
// change a provider's projects or scopes without asking for its secret again. It
// keeps only while the secret goes where it went: the same issuer and client.
// Moved, an update without a secret is a public client, as Dockwell won't send
// what it kept somewhere it hasn't sent it before.
func (Type) Keep(current, desired proto.Message) (resource.Kept, error) {
	was, ok := current.(*resourcev1.SigninProvider)
	is, same := desired.(*resourcev1.SigninProvider)
	if !ok || !same || is.GetClientSecret() != "" || was.GetClientSecret() == "" {
		return resource.Kept{}, nil
	}
	if is.GetIssuer() != was.GetIssuer() || is.GetClientId() != was.GetClientId() {
		return resource.Kept{}, nil
	}
	is.ClientSecret = was.GetClientSecret()
	return resource.Kept{Reason: "kept_secret", Words: "keeps the client secret it was saved with, as the update left that out"}, nil
}

// hostOf is an issuer's host, to name it in words.
func hostOf(issuer string) string {
	if u, err := url.Parse(issuer); err == nil && u.Host != "" {
		return u.Host
	}
	return issuer
}

// printable is visible ASCII alone, as an ID or a claim's name is.
func printable(s string) bool {
	for _, r := range s {
		if r <= ' ' || r > '~' {
			return false
		}
	}
	return true
}

// scopeToken is a scope as RFC 6749 writes one: visible ASCII but '"' and '\\'.
func scopeToken(s string) bool {
	return printable(s) && !strings.ContainsAny(s, "\"\\")
}
