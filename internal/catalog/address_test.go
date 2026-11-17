// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"maps"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// An address variable's value says the app's address: {origin}, {host} and
// {scheme} at a domain, the panel's Open address and loopback alike, and {domain},
// or a value that names nothing of it, at a domain alone, as before (ADR 0103).
func TestAnAddressVariableSaysTheAppsAddress(t *testing.T) {
	domain := DomainAddress("docs.example.com")
	open := LocalAddress("abcdefghij.localhost:7600")
	loopback := LocalAddress("localhost:8080")
	for _, c := range []struct {
		value string
		at    Address
		want  string
		ok    bool
	}{
		{"{origin}", domain, "https://docs.example.com", true},
		{"{origin}/", open, "http://abcdefghij.localhost:7600/", true},
		{"{host}", loopback, "localhost:8080", true},
		{"{scheme}", domain, "https", true},
		{"{scheme}", open, "http", true},
		{"https://{domain}", domain, "https://docs.example.com", true},
		{"https://{domain}", open, "", false},
		{"https", loopback, "", false},
		{"https", domain, "https", true},
		{"{origin}", Address{}, "", false},
	} {
		if got, ok := AddressValue(c.value, c.at); got != c.want || ok != c.ok {
			t.Errorf("%q at %v = %q, %v, want %q, %v", c.value, c.at, got, ok, c.want, c.ok)
		}
	}
	e := &catalogv1.Entry{AddressVariables: []*catalogv1.AddressVariable{
		{Service: "web", Variable: "URL", Value: "{origin}"},
		{Service: "web", Variable: "PROTO", Value: "https"},
		{Service: "web", Variable: "PUBLIC", Value: "https://{domain}"},
	}}
	if got := AddressValues(e, open); !maps.Equal(got["web"], map[string]string{"URL": "http://abcdefghij.localhost:7600"}) {
		t.Errorf("at Open: %v", got)
	}
}

// An address reads back as it's written, and a domain is any name but localhost's.
func TestAnAddressReadsBack(t *testing.T) {
	for origin, domain := range map[string]bool{
		"https://docs.example.com":         true,
		"http://abcdefghij.localhost:7600": false,
		"http://localhost:8080":            false,
	} {
		a, ok := ParseAddress(origin)
		if !ok || a.Origin() != origin || a.Domain() != domain {
			t.Errorf("%s reads as %v, %v, domain %v", origin, a, ok, a.Domain())
		}
	}
	for _, bad := range []string{"", "localhost:8080", "ftp://x", "https://", "https://x/y"} {
		if _, ok := ParseAddress(bad); ok {
			t.Errorf("%q reads as an address", bad)
		}
	}
}
