// SPDX-License-Identifier: AGPL-3.0-only

package route

import (
	"cmp"
	"fmt"
	"net/mail"
	"net/url"
	"slices"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Limits on who a route lets sign in (ADR 0087).
const (
	maxSignInNames = 100
	maxGroupName   = 128
	maxSessionHrs  = 720
)

// checkSignIn checks who may sign in to a route with a provider, and writes it the
// way a route keeps it: emails and domains in lower case, each once.
func checkSignIn(r *resourcev1.Route) error {
	s := r.GetAccess().GetSignIn()
	if s == nil {
		return nil
	}
	if err := resource.CheckLabel("sign-in provider", s.GetProvider(), "our-authentik"); err != nil {
		return fmt.Errorf("%s signs people in with a provider it names: %w", Where(r), err)
	}
	// An address or a domain in letters outside ASCII is spelled more than one way, as
	// the Kelvin sign lowers to k, so a route names them in ASCII, a domain as punycode
	// (#598's review, L1).
	ascii := func(what, s string) error {
		if strings.ContainsFunc(s, func(c rune) bool { return c >= 0x80 }) {
			return fmt.Errorf("%q has letters outside ASCII, which a provider may spell another way. Write the %s in ASCII, its domain as punycode (xn--)", s, what)
		}
		return nil
	}
	emails, err := eachOnce(s.GetEmails(), func(e string) (string, error) {
		if err := ascii("address", e); err != nil {
			return "", err
		}
		e = strings.ToLower(strings.TrimSpace(e))
		if a, err := mail.ParseAddress(e); err != nil || a.Address != e || len(e) > 254 {
			return "", fmt.Errorf("%q isn't an email address. Name someone as their provider knows them, such as mira@example.com", e)
		}
		return e, nil
	})
	if err != nil {
		return err
	}
	domains, err := eachOnce(s.GetEmailDomains(), func(d string) (string, error) {
		if err := ascii("domain", d); err != nil {
			return "", err
		}
		d = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(d)), "@")
		if CheckDomain(d) != nil || strings.HasPrefix(d, "*.") {
			return "", fmt.Errorf("%q isn't an email domain. Name it as an address ends, such as example.com", d)
		}
		return d, nil
	})
	if err != nil {
		return err
	}
	groups, err := eachOnce(s.GetGroups(), func(g string) (string, error) {
		g = strings.TrimSpace(g)
		if g == "" || len(g) > maxGroupName || strings.ContainsFunc(g, func(c rune) bool { return c < ' ' || c == 0x7f }) {
			return "", fmt.Errorf("%q isn't a group's name: one line of up to %d characters, as the provider's groups claim lists it", g, maxGroupName)
		}
		return g, nil
	})
	if err != nil {
		return err
	}
	switch {
	case len(emails)+len(domains)+len(groups) > maxSignInNames:
		return fmt.Errorf("%s names %d emails, domains and groups, and a route names %d at most. Name a domain or a group in place of many emails", Where(r), len(emails)+len(domains)+len(groups), maxSignInNames)
	case len(emails)+len(domains)+len(groups) == 0 && !s.GetFilled().GetAnyAccount():
		return fmt.Errorf("%s signs people in, and names no one who may: name them by email, domain or group. Only a provider whose every account is your team's may let in anyone who signs in", Where(r))
	case s.GetSessionHours() > maxSessionHrs:
		return fmt.Errorf("a sign-in lasts %d hours at most, as someone taken off a group at the provider is let in until it runs out", maxSessionHrs)
	case PlainAtTheBrowser(r):
		return fmt.Errorf("%s is reached over plain HTTP, and signing in needs HTTPS at the browser for its cookie. Turn on HTTPS for this domain, or name the tunnel or proxy in front of it, whose edge serves HTTPS", Where(r))
	}
	s.Emails, s.EmailDomains, s.Groups = emails, domains, groups
	return nil
}

// eachOnce checks and writes each name, and leaves out repeats.
func eachOnce(names []string, check func(string) (string, error)) ([]string, error) {
	var out []string
	for _, n := range names {
		w, err := check(n)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out, nil
}

// signInOpens says how a route's sign-in lets in someone it kept out: taken off
// with nothing left in its place, another provider, someone added by email, domain
// or group, or every account at its provider (ADR 0087).
func signInOpens(from, to *resourcev1.RouteAccess) []string {
	was, is := from.GetSignIn(), to.GetSignIn()
	if was == nil {
		return nil
	}
	if is == nil {
		if len(to.GetAllow())+len(to.GetUsers()) == 0 {
			return []string{resource.OpensSignInOff}
		}
		return nil
	}
	var out []string
	if is.GetProvider() != was.GetProvider() {
		out = append(out, resource.OpensProvider+is.GetProvider())
	}
	for _, e := range is.GetEmails() {
		if !slices.Contains(was.GetEmails(), e) {
			out = append(out, resource.OpensEmail+e)
		}
	}
	for _, d := range is.GetEmailDomains() {
		if !slices.Contains(was.GetEmailDomains(), d) {
			out = append(out, resource.OpensEmailDomain+d)
		}
	}
	for _, g := range is.GetGroups() {
		if !slices.Contains(was.GetGroups(), g) {
			out = append(out, resource.OpensGroup+url.PathEscape(g))
		}
	}
	if is.GetFilled().GetAnyAccount() && !was.GetFilled().GetAnyAccount() && len(is.GetEmails())+len(is.GetEmailDomains())+len(is.GetGroups()) == 0 {
		out = append(out, resource.OpensAnyAccount)
	}
	// The same provider, edited: who its accounts are, or which of their groups count,
	// is another's to say now (ADR 0087).
	if is.GetProvider() == was.GetProvider() && was.GetFilled() != nil {
		now, then := is.GetFilled(), was.GetFilled()
		if now.GetIssuer() != then.GetIssuer() {
			out = append(out, resource.OpensIssuer+now.GetIssuer())
		}
		if now.GetClientId() != then.GetClientId() {
			out = append(out, resource.OpensClient+now.GetClientId())
		}
		if claim := cmp.Or(now.GetGroupsClaim(), "groups"); len(is.GetGroups()) > 0 && claim != cmp.Or(then.GetGroupsClaim(), "groups") {
			out = append(out, resource.OpensGroupsClaim+claim)
		}
	}
	return out
}
