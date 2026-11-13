// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"context"
	"errors"
	"net"
	"testing"
)

// A registry on the server's loopback, which a build's steps share, is one a step
// listening there would answer for, so a build is never given its login, in any form
// its host takes. (Security review of #591, H2.)
func TestARegistryOnTheLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost":                 true,
		"localhost:5000":            true,
		"LOCALHOST.:5000":           true,
		"registry.localhost":        true,
		"127.0.0.1:5000":            true,
		"127.8.9.10":                true,
		"0.0.0.0:5000":              true,
		"[::1]:5000":                true,
		"[::]:5000":                 true,
		"::1":                       true,
		"[::ffff:127.0.0.1]":        true,
		"ghcr.io":                   false,
		"registry.example.com:5000": false,
		"192.0.2.10:5000":           false,
		"[2001:db8::1]:5000":        false,
		"notlocalhost.example":      false,
	} {
		if got := OnLoopback(host); got != want {
			t.Errorf("OnLoopback(%q) = %v", host, got)
		}
	}
}

// A name is on the loopback where any of its addresses is, or where nothing says
// where it leads.
func TestANameThatResolvesToTheLoopback(t *testing.T) {
	names := map[string][]string{
		"registry.example.com": {"192.0.2.10"},
		"sneaky.example.com":   {"192.0.2.11", "127.0.0.1"},
		"zero.example.com":     {"0.0.0.0"},
		"empty.example.com":    {},
	}
	was := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = was })
	lookupIPAddr = func(_ context.Context, host string) ([]net.IPAddr, error) {
		ips, ok := names[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var out []net.IPAddr
		for _, ip := range ips {
			out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
		}
		return out, nil
	}
	for host, want := range map[string]bool{
		"registry.example.com:5000": false,
		"sneaky.example.com":        true,
		"zero.example.com":          true,
		"empty.example.com":         true,
		"unknown.example.com":       true,
		"localhost:5000":            true,
		"192.0.2.10":                false,
	} {
		if got := ResolvesToLoopback(t.Context(), host); got != want {
			t.Errorf("ResolvesToLoopback(%q) = %v", host, got)
		}
	}
}
