// SPDX-License-Identifier: AGPL-3.0-only

package dnsprovider

import (
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
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
		{&resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}, Projects: []string{"shop"}}, "project's ID"},
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

// A second token comes off only when an edit says so; with a token beside the
// clear, the edit is refused; and neither the clear nor records to put back are
// ever kept in the provider's revision (ADR 0104).
func TestASecondTokenIsCleared(t *testing.T) {
	was := &resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}, ApiToken: "dns-token", TunnelApiToken: "tunnel-token"}
	kept := &resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}}
	if _, err := (Type{}).Keep(was, kept); err != nil || kept.GetTunnelApiToken() != "tunnel-token" {
		t.Fatalf("leaving it out should keep it: %q, %v", kept.GetTunnelApiToken(), err)
	}
	cleared := &resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}, ClearTunnelApiToken: true}
	if _, err := (Type{}).Keep(was, cleared); err != nil || cleared.GetTunnelApiToken() != "" || cleared.GetApiToken() != "dns-token" || cleared.GetClearTunnelApiToken() {
		t.Fatalf("a clear: %+v, %v", cleared, err)
	}
	both := &resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}, TunnelApiToken: "another", ClearTunnelApiToken: true}
	if err := (Type{}).Validate(both); err == nil {
		t.Fatal("a token and a clear together were taken")
	}
	fresh := &resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}, ApiToken: "dns-token", ClearTunnelApiToken: true}
	(Type{}).DefaultNew(resource.Ref{}, fresh)
	if fresh.GetClearTunnelApiToken() {
		t.Fatal("a new provider kept the clear")
	}
	if err := (Type{}).Validate(&resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}, PutBack: []string{"rec-1"}}); err == nil {
		t.Fatal("a put back that names no kept record was taken")
	}
	if err := (Type{}).CheckNew(&resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}, ApiToken: "t", PutBack: []string{"pending:rec-1"}}); err == nil {
		t.Fatal("a new provider with records to put back was taken")
	}
	asked := &resourcev1.DnsProvider{Provider: Cloudflare, Zones: []string{"example.com"}, ApiToken: "t", PutBack: []string{"pending:rec-1"}}
	stored, ok := (Type{}).StripTransient(asked).(*resourcev1.DnsProvider)
	if !ok || len(stored.GetPutBack()) != 0 || len(asked.GetPutBack()) != 1 {
		t.Fatalf("the revision should keep no put back, and the change should: %v, %v", stored, asked)
	}
}

func TestCovers(t *testing.T) {
	for name, want := range map[string]bool{"example.com": true, "app.example.com": true, "a.b.example.com": true, "notexample.com": false, "example.org": false} {
		if got := Covers("example.com", name); got != want {
			t.Errorf("Covers(example.com, %s) = %v", name, got)
		}
	}
}

// A provider proves names for the projects it names, and none when it names none.
func TestAProviderServesTheProjectsItNames(t *testing.T) {
	if Serves(&resourcev1.DnsProvider{}, "prj_shop") {
		t.Error("a provider that names no project proves names for one")
	}
	if !Serves(&resourcev1.DnsProvider{Projects: []string{"prj_shop"}}, "prj_shop") || Serves(&resourcev1.DnsProvider{Projects: []string{"prj_shop"}}, "prj_blog") {
		t.Error("a provider proves names for projects it doesn't name, or not for one it does")
	}
}
