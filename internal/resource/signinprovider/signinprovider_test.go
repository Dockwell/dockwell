// SPDX-License-Identifier: AGPL-3.0-only

package signinprovider_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/signinprovider"
)

func aProvider() *resourcev1.SigninProvider {
	return &resourcev1.SigninProvider{Issuer: "https://auth.example.com/application/o/dockwell/", ClientId: "dockwell", ClientSecret: "s3cret-from-the-provider"}
}

// A provider is written the way it's kept: its issuer HTTPS, a host alone read as
// one, its display name its host where none is given, and the scopes sign-in always
// asks for, and repeats, left out.
func TestAProviderIsWrittenAsItsKept(t *testing.T) {
	p := &resourcev1.SigninProvider{Issuer: " Auth.Example.com ", ClientId: " dockwell ", Scopes: []string{"openid", "groups", "groups", " email ", "offline_access"}}
	if err := (signinprovider.Type{}).Validate(p); err != nil {
		t.Fatal(err)
	}
	if p.GetIssuer() != "https://auth.example.com" || p.GetDisplayName() != "auth.example.com" || p.GetClientId() != "dockwell" {
		t.Errorf("it reads %v", p)
	}
	if got := strings.Join(p.GetScopes(), " "); got != "groups offline_access" {
		t.Errorf("its scopes read %q", got)
	}
	// An issuer's path, and its trailing slash, are its own.
	q := aProvider()
	if err := (signinprovider.Type{}).Validate(q); err != nil || q.GetIssuer() != "https://auth.example.com/application/o/dockwell/" {
		t.Errorf("an issuer with a path reads %q, %v", q.GetIssuer(), err)
	}
}

// Each thing that can't sign anyone in is refused on its own, saying what to give,
// and never with the secret in the words.
func TestAProviderThatCantSignAnyoneInIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*resourcev1.SigninProvider)
		want string
	}{
		{"no issuer", func(p *resourcev1.SigninProvider) { p.Issuer = "" }, "needs its issuer"},
		{"plain HTTP", func(p *resourcev1.SigninProvider) { p.Issuer = "http://auth.example.com" }, "isn't HTTPS"},
		{"a user in it", func(p *resourcev1.SigninProvider) { p.Issuer = "https://admin:pw@auth.example.com" }, "no user or password"},
		{"a query", func(p *resourcev1.SigninProvider) { p.Issuer = "https://auth.example.com/?tenant=1" }, "query or a fragment"},
		{"a fragment", func(p *resourcev1.SigninProvider) { p.Issuer = "https://auth.example.com/#x" }, "query or a fragment"},
		{"no client", func(p *resourcev1.SigninProvider) { p.ClientId = " " }, "needs the client ID"},
		{"a client with a space", func(p *resourcev1.SigninProvider) { p.ClientId = "dock well" }, "printable characters with no spaces"},
		{"a secret on two lines", func(p *resourcev1.SigninProvider) { p.ClientSecret = "line\nline" }, "one line"},
		{"a scope with a quote", func(p *resourcev1.SigninProvider) { p.Scopes = []string{`gr"oups`} }, "isn't a scope"},
		{"a claim with a space", func(p *resourcev1.SigninProvider) { p.GroupsClaim = "my groups" }, "a claim's name"},
		{"a long display name", func(p *resourcev1.SigninProvider) { p.DisplayName = strings.Repeat("a", 65) }, "64 characters"},
		{"a display name on two lines", func(p *resourcev1.SigninProvider) { p.DisplayName = "Our\nAuthentik" }, "one line"},
		{"any account at Google", func(p *resourcev1.SigninProvider) { p.Issuer, p.AnyAccount = "https://accounts.google.com", true }, "anyone can have an account at accounts.google.com"},
		{"any account at Entra's common", func(p *resourcev1.SigninProvider) {
			p.Issuer, p.AnyAccount = "https://login.microsoftonline.com/common/v2.0", true
		}, "anyone can have an account"},
		{"a project by name", func(p *resourcev1.SigninProvider) { p.Projects = []string{"web"} }, "isn't a project's ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := aProvider()
			tc.edit(p)
			err := (signinprovider.Type{}).Validate(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("it reads %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("its words hold the secret: %v", err)
			}
		})
	}
	// Any account is fine where accounts are your team's, as at a tenant of your own.
	for _, issuer := range []string{"https://auth.example.com/application/o/dockwell/", "https://login.microsoftonline.com/2f1c4a3e-0000-4000-8000-000000000001/v2.0"} {
		p := aProvider()
		p.Issuer, p.AnyAccount = issuer, true
		if err := (signinprovider.Type{}).Validate(p); err != nil {
			t.Errorf("any account at %s: %v", issuer, err)
		}
	}
}

// An update that leaves the secret out keeps it while it goes where it went: the
// same issuer and client. Moved, it keeps nothing, as Dockwell sends what it kept
// nowhere new; the update is then a public client's.
func TestAnUpdateKeepsTheSecretOnlyWhereItWent(t *testing.T) {
	was := aProvider()
	same := proto.Clone(was).(*resourcev1.SigninProvider)
	same.ClientSecret, same.Scopes = "", []string{"groups"}
	kept, err := (signinprovider.Type{}).Keep(was, same)
	if err != nil || same.GetClientSecret() != was.GetClientSecret() || kept.Reason == "" {
		t.Errorf("the same client keeps %q, %+v, %v", same.GetClientSecret(), kept, err)
	}
	for name, edit := range map[string]func(*resourcev1.SigninProvider){
		"another issuer": func(p *resourcev1.SigninProvider) { p.Issuer = "https://other.example.com" },
		"another client": func(p *resourcev1.SigninProvider) { p.ClientId = "another" },
	} {
		moved := proto.Clone(was).(*resourcev1.SigninProvider)
		moved.ClientSecret = ""
		edit(moved)
		if kept, err := (signinprovider.Type{}).Keep(was, moved); err != nil || moved.GetClientSecret() != "" || kept.Reason != "" {
			t.Errorf("%s keeps %q, %+v, %v", name, moved.GetClientSecret(), kept, err)
		}
	}
	given := proto.Clone(was).(*resourcev1.SigninProvider)
	given.ClientSecret = "a-new-one"
	if _, err := (signinprovider.Type{}).Keep(was, given); err != nil || given.GetClientSecret() != "a-new-one" {
		t.Errorf("a new secret reads %q, %v", given.GetClientSecret(), err)
	}
}
