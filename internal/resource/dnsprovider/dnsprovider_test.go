// SPDX-License-Identifier: AGPL-3.0-only

package dnsprovider

import (
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// A provider's zones are written as DNS names them, and what can't be a zone a
// certificate is had for is refused, saying what to write.
func TestAProvidersZones(t *testing.T) {
	p := &resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"Example.ORG.", " example.com"}, ApiToken: "t"}
	if err := (Type{}).Validate(p); err != nil || !slices.Equal(p.GetZones(), []string{"example.com", "example.org"}) {
		t.Errorf("zones = %q, %v", p.GetZones(), err)
	}
	for _, c := range []struct {
		p    *resourcev1.DnsProvider
		says string
	}{
		{&resourcev1.DnsProvider{Zones: []string{"example.com"}}, "cloudflare"},
		{&resourcev1.DnsProvider{Provider: "route53", Zones: []string{"example.com"}}, "Cloudflare's API for now"},
		{&resourcev1.DnsProvider{Provider: Cloudflare}, "needs the zones"},
		{&resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"*.example.com"}}, "wildcard"},
		{&resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"com"}}, "isn't a zone"},
		{&resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"home.local"}}, "isn't a zone"},
		{&resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"192.0.2.1"}}, "isn't a zone"},
		{&resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"ex ample.com"}}, "isn't a zone"},
		{&resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com", "EXAMPLE.com"}}, "named twice"},
		{&resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}, ApiToken: "abc\ndef"}, "line break"},
		{&resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}, ApiToken: "0123456789abcdef0123456789abcdef01234"}, "Global API Key"},
	} {
		if err := (Type{}).Validate(c.p); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("Validate(%v) = %v, want %q", c.p, err, c.says)
		}
	}
	var many []string
	for i := range 51 {
		many = append(many, "z"+strings.Repeat("a", i+1)+".com")
	}
	if err := (Type{}).Validate(&resourcev1.DnsProvider{Provider: Cloudflare, Zones: many}); err == nil {
		t.Error("a provider with 51 zones was taken")
	}
}

// A new provider needs its token, and an update that leaves it out keeps it, unless
// it moves the provider to another provider's API.
func TestAProvidersTokenIsKept(t *testing.T) {
	if err := (Type{}).CheckNew(&resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}}); err == nil {
		t.Error("a new provider with no token was taken")
	}
	was := &resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}, ApiToken: "s3cret-dns-token"}
	is := &resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com", "example.org"}}
	kept, err := (Type{}).Keep(was, is)
	if err != nil || is.GetApiToken() != "s3cret-dns-token" || kept.Reason == "" || strings.Contains(kept.Words, "s3cret") {
		t.Errorf("Keep = %+v, %v, token %q", kept, err, is.GetApiToken())
	}
	moved := &resourcev1.DnsProvider{Provider: "route53", Zones: []string{"example.com"}}
	if _, err := (Type{}).Keep(was, moved); err == nil || moved.GetApiToken() != "" {
		t.Errorf("a move to another provider's API kept the token: %v", err)
	}
	given := &resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}, ApiToken: "new-token"}
	if _, err := (Type{}).Keep(was, given); err != nil || given.GetApiToken() != "new-token" {
		t.Errorf("a new token was replaced: %q, %v", given.GetApiToken(), err)
	}
}

func TestCovers(t *testing.T) {
	for name, want := range map[string]bool{"example.com": true, "app.example.com": true, "a.b.example.com": true, "notexample.com": false, "example.org": false} {
		if got := Covers("example.com", name); got != want {
			t.Errorf("Covers(example.com, %s) = %v", name, got)
		}
	}
}
