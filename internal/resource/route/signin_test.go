// SPDX-License-Identifier: AGPL-3.0-only

package route

import (
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

func signingIn(s *resourcev1.RouteSignIn) *resourcev1.Route {
	return private(&resourcev1.RouteAccess{SignIn: s})
}

// Who may sign in is written as a route keeps it: emails and domains in lower case,
// a domain without its "@", each name once; and sign-in alone keeps strangers out.
func TestSignInIsWrittenAsARouteKeepsIt(t *testing.T) {
	r := signingIn(&resourcev1.RouteSignIn{
		Provider: "our-authentik", Emails: []string{" Mira@Example.com", "mira@example.com"},
		EmailDomains: []string{"@Example.org", "example.org"}, Groups: []string{" ops ", "ops", "Domain Admins"},
	})
	if err := hashing().Validate(r); err != nil {
		t.Fatal(err)
	}
	s := r.GetAccess().GetSignIn()
	if strings.Join(s.GetEmails(), ",") != "mira@example.com" || strings.Join(s.GetEmailDomains(), ",") != "example.org" || strings.Join(s.GetGroups(), ",") != "ops,Domain Admins" {
		t.Errorf("it reads %v", s)
	}
	if !hashing().KeepsOut(r) {
		t.Error("a route that signs people in doesn't keep strangers out")
	}
}

// Each way sign-in can't keep a visitor out is refused, saying what to do instead.
func TestSignInThatCantKeepAVisitorOutIsRefused(t *testing.T) {
	many := make([]string, maxSignInNames+1)
	for i := range many {
		many[i] = "g" + strings.Repeat("x", i)
	}
	plain := signingIn(&resourcev1.RouteSignIn{Provider: "ours", EmailDomains: []string{"example.com"}})
	plain.Tls, plain.PlainForAnyone = "off", true
	for _, tc := range []struct {
		name string
		r    *resourcev1.Route
		want string
	}{
		{"no provider", signingIn(&resourcev1.RouteSignIn{Emails: []string{"mira@example.com"}}), "names"},
		{"not an email", signingIn(&resourcev1.RouteSignIn{Provider: "ours", Emails: []string{"mira"}}), "isn't an email address"},
		{"a name beside the email", signingIn(&resourcev1.RouteSignIn{Provider: "ours", Emails: []string{"Mira <mira@example.com>"}}), "isn't an email address"},
		{"not a domain", signingIn(&resourcev1.RouteSignIn{Provider: "ours", EmailDomains: []string{"example com"}}), "isn't an email domain"},
		{"a wildcard domain", signingIn(&resourcev1.RouteSignIn{Provider: "ours", EmailDomains: []string{"*.example.com"}}), "isn't an email domain"},
		// What lowers to another address or domain, as the Kelvin sign to k (#598's review, L1).
		{"an address outside ASCII", signingIn(&resourcev1.RouteSignIn{Provider: "ours", Emails: []string{"mira@\u212aiwi.example"}}), "outside ASCII"},
		{"a domain outside ASCII", signingIn(&resourcev1.RouteSignIn{Provider: "ours", EmailDomains: []string{"b\u00fccher.example"}}), "as punycode"},
		{"a dotted capital I", signingIn(&resourcev1.RouteSignIn{Provider: "ours", Emails: []string{"\u0130nfo@example.com"}}), "outside ASCII"},
		{"a group on two lines", signingIn(&resourcev1.RouteSignIn{Provider: "ours", Groups: []string{"ops\nadmins"}}), "isn't a group's name"},
		{"too many names", signingIn(&resourcev1.RouteSignIn{Provider: "ours", Groups: many}), "names 101 emails, domains and groups"},
		{"no one named", signingIn(&resourcev1.RouteSignIn{Provider: "ours"}), "names no one who may"},
		{"too long a sign-in", signingIn(&resourcev1.RouteSignIn{Provider: "ours", Groups: []string{"ops"}, SessionHours: 721}), "720 hours at most"},
		{"plain HTTP at the browser", plain, "needs HTTPS at the browser"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := hashing().Validate(tc.r); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("it reads %v, want %q", err, tc.want)
			}
		})
	}
	// No one named is fine where the provider says every account is the team's.
	anyone := signingIn(&resourcev1.RouteSignIn{Provider: "ours", Filled: &resourcev1.RouteSignInProvider{AnyAccount: true}})
	if err := hashing().Validate(anyone); err != nil {
		t.Errorf("any account at a provider of the team's reads %v", err)
	}
}

// Opens says each way sign-in lets in someone it kept out: taken off with nothing
// in its place, another provider, someone added by email, domain or group, and
// every account at the provider; keeping out more says nothing.
func TestSignInOpensSaysEachWayIn(t *testing.T) {
	was := signingIn(&resourcev1.RouteSignIn{Provider: "ours", Emails: []string{"mira@example.com"}, Groups: []string{"ops"}})
	opens := func(to *resourcev1.Route) []string { return hashing().Opens(was, to) }
	for name, tc := range map[string]struct {
		to   *resourcev1.Route
		want []string
	}{
		"taken off": {private(nil), []string{resource.OpensSignInOff}},
		"another provider": {
			signingIn(&resourcev1.RouteSignIn{Provider: "theirs", Emails: []string{"mira@example.com"}, Groups: []string{"ops"}}),
			[]string{resource.OpensProvider + "theirs"},
		},
		"more people": {
			signingIn(&resourcev1.RouteSignIn{Provider: "ours", Emails: []string{"mira@example.com", "eve@example.com"}, EmailDomains: []string{"example.org"}, Groups: []string{"ops", "Domain Admins"}}),
			[]string{resource.OpensEmail + "eve@example.com", resource.OpensEmailDomain + "example.org", resource.OpensGroup + "Domain%20Admins"},
		},
		"any account": {
			signingIn(&resourcev1.RouteSignIn{Provider: "ours", Filled: &resourcev1.RouteSignInProvider{AnyAccount: true}}),
			[]string{resource.OpensAnyAccount},
		},
		"fewer people":        {signingIn(&resourcev1.RouteSignIn{Provider: "ours", Groups: []string{"ops"}}), nil},
		"a list in its place": {private(&resourcev1.RouteAccess{Allow: []string{"203.0.113.0/24"}}), nil},
	} {
		if got := opens(tc.to); !slices.Equal(got, tc.want) {
			t.Errorf("%s opens %v, want %v", name, got, tc.want)
		}
	}
	// The same provider, edited: another issuer, client or groups claim says who its
	// accounts and their groups are, so each opens the route (#598's review, M3).
	at := func(issuer, client, claim string) *resourcev1.Route {
		return signingIn(&resourcev1.RouteSignIn{
			Provider: "ours", Groups: []string{"ops"},
			Filled: &resourcev1.RouteSignInProvider{Issuer: issuer, ClientId: client, GroupsClaim: claim},
		})
	}
	edited := at("https://auth.example.com/", "dockwell", "")
	for name, tc := range map[string]struct {
		to   *resourcev1.Route
		want []string
	}{
		"the same":             {at("https://auth.example.com/", "dockwell", "groups"), nil},
		"another issuer":       {at("https://sso.example.org/", "dockwell", ""), []string{resource.OpensIssuer + "https://sso.example.org/"}},
		"another client":       {at("https://auth.example.com/", "dockwell-2", ""), []string{resource.OpensClient + "dockwell-2"}},
		"another groups claim": {at("https://auth.example.com/", "dockwell", "roles"), []string{resource.OpensGroupsClaim + "roles"}},
	} {
		if got := hashing().Opens(edited, tc.to); !slices.Equal(got, tc.want) {
			t.Errorf("%s opens %v, want %v", name, got, tc.want)
		}
	}
	words := resource.AccessWords(resource.Access{Kind: resource.AccessOpened, Path: "app.example.com", Detail: resource.OpensGroup + "Domain%20Admins " + resource.OpensAnyAccount})
	if !strings.Contains(words, `lets in anyone in the group "Domain Admins"`) || !strings.Contains(words, "every account at its provider") {
		t.Errorf("its words read %q", words)
	}
}
