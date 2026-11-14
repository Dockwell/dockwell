// SPDX-License-Identifier: AGPL-3.0-only

package certificate

import (
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// A certificate's names are written as DNS names them, with a wildcard only as a
// whole first label, and what no authority issues is refused, saying what to write.
func TestACertificatesNames(t *testing.T) {
	c := &resourcev1.DnsCertificate{Domains: []string{"Example.com.", "*.EXAMPLE.com"}, DnsProvider: "cloudflare"}
	if err := (Type{}).Validate(c); err != nil || !slices.Equal(c.GetDomains(), []string{"*.example.com", "example.com"}) {
		t.Errorf("domains = %q, %v", c.GetDomains(), err)
	}
	for _, c := range []struct {
		c    *resourcev1.DnsCertificate
		says string
	}{
		{&resourcev1.DnsCertificate{DnsProvider: "cloudflare"}, "needs the names"},
		{&resourcev1.DnsCertificate{Domains: []string{"example.com"}}, "needs the DNS provider"},
		{&resourcev1.DnsCertificate{Domains: []string{"a.*.example.com"}, DnsProvider: "cloudflare"}, "first label"},
		{&resourcev1.DnsCertificate{Domains: []string{"*example.com"}, DnsProvider: "cloudflare"}, "first label"},
		{&resourcev1.DnsCertificate{Domains: []string{"*.com"}, DnsProvider: "cloudflare"}, "first label"},
		{&resourcev1.DnsCertificate{Domains: []string{"nas.home.local"}, DnsProvider: "cloudflare"}, "public domain"},
		{&resourcev1.DnsCertificate{Domains: []string{"example.com", "EXAMPLE.COM"}, DnsProvider: "cloudflare"}, "named twice"},
		{&resourcev1.DnsCertificate{Domains: []string{"example.com"}, DnsProvider: "Cloud Flare"}, "DNS provider"},
		{&resourcev1.DnsCertificate{Domains: []string{"example.com"}, DnsProvider: "cloudflare", Projects: []string{"shop"}}, "project's ID"},
	} {
		if err := (Type{}).Validate(c.c); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("Validate(%v) = %v, want %q", c.c, err, c.says)
		}
	}
	var many []string
	for i := range MaxNames + 1 {
		many = append(many, "n"+strings.Repeat("a", i%60+1)+string(rune('a'+i/60))+".example.com")
	}
	if err := (Type{}).Validate(&resourcev1.DnsCertificate{Domains: many, DnsProvider: "cloudflare"}); err == nil {
		t.Error("a certificate past 100 names was taken")
	}
}

// A wildcard covers one label, as an authority issues it.
func TestWhatACertificateCovers(t *testing.T) {
	for _, c := range []struct {
		name, host string
		want       bool
	}{
		{"*.example.com", "app.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", "a.b.example.com", false},
		{"*.example.com", "app.example.org", false},
		{"example.com", "example.com", true},
		{"example.com", "app.example.com", false},
	} {
		if got := Covers(c.name, c.host); got != c.want {
			t.Errorf("Covers(%s, %s) = %v", c.name, c.host, got)
		}
	}
	if z := ZoneOf([]string{"example.com", "eu.example.com"}, "*.eu.example.com"); z != "eu.example.com" {
		t.Errorf("ZoneOf = %q", z)
	}
	if z := ZoneOf([]string{"example.org"}, "app.example.com"); z != "" {
		t.Errorf("ZoneOf a name in no zone = %q", z)
	}
}

// Names that reach the panel's domain, as served or as saved, keep a shared
// certificate on the control server: a wildcard over it, the domain itself, and a
// name under it, whose key would set the panel's cookies (ADR 0088).
func TestWhatTouchesThePanel(t *testing.T) {
	panels := []string{"panel.example.com", "", "admin.example.org"}
	for _, c := range []struct {
		names []string
		want  bool
	}{
		{[]string{"*.example.com"}, true},
		{[]string{"panel.example.com"}, true},
		{[]string{"eu.panel.example.com"}, true},
		{[]string{"*.panel.example.com"}, true},
		{[]string{"shop.example.org", "admin.example.org"}, true},
		{[]string{"*.eu.example.com"}, false},
		{[]string{"shop.example.com", "panelexample.com"}, false},
	} {
		if got := TouchesPanel(c.names, panels); got != c.want {
			t.Errorf("TouchesPanel(%v) = %v", c.names, got)
		}
	}
	shared := []Shared{
		{ID: "crt_saved", Spec: &resourcev1.DnsCertificate{Domains: []string{"*.example.org"}, Projects: []string{"prj_shop"}}},
		{ID: "crt_eu", Spec: &resourcev1.DnsCertificate{Domains: []string{"*.eu.example.com"}, Projects: []string{"prj_shop"}}},
	}
	if c, ok := Serving(shared, "shop.example.org", "prj_shop", "srv_1", "srv_control", panels); ok {
		t.Errorf("a wildcard over the panel's saved domain serves another server: %s", c.ID)
	}
	if c, ok := Serving(shared, "shop.example.org", "prj_shop", "srv_control", "srv_control", panels); !ok || c.ID != "crt_saved" {
		t.Errorf("the control server's route under it: %v %v", c.ID, ok)
	}
	if c, ok := Serving(shared, "app.eu.example.com", "prj_shop", "srv_1", "srv_control", panels); !ok || c.ID != "crt_eu" {
		t.Errorf("a wildcard away from the panel: %v %v", c.ID, ok)
	}
	if _, ok := Serving(shared, "app.eu.example.com", "prj_blog", "srv_1", "srv_control", panels); ok {
		t.Error("a shared certificate served a project it doesn't name")
	}
}

func TestANameOutsideTheZones(t *testing.T) {
	zones := []string{"example.com"}
	if n := Outside([]string{"*.example.com", "example.com"}, zones); n != "" {
		t.Errorf("Outside = %q", n)
	}
	if n := Outside([]string{"*.example.com", "app.example.net"}, zones); n != "app.example.net" {
		t.Errorf("Outside = %q, want the name in no zone", n)
	}
}
