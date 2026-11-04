// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"crypto/tls"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestResolveSites(t *testing.T) {
	sites := Sites{Domain: "panel.acme.dev"}
	for _, tt := range []struct {
		name, host, remote string
		tls                bool
		want               Site
		misdirected        bool
	}{
		{
			name: "the domain over HTTPS", host: "panel.acme.dev", remote: "198.51.100.23:4000", tls: true,
			want: Site{RPID: "panel.acme.dev", Origin: "https://panel.acme.dev", Secure: true},
		},
		{
			name: "the domain on another port", host: "Panel.Acme.Dev.:8443", remote: "198.51.100.23:4000", tls: true,
			want: Site{RPID: "panel.acme.dev", Origin: "https://panel.acme.dev:8443", Secure: true},
		},
		{
			name: "localhost through a tunnel", host: "localhost:7600", remote: "127.0.0.1:5000",
			want: Site{RPID: "localhost", Origin: "http://localhost:7600"},
		},
		{
			name: "localhost on another local port", host: "localhost:8000", remote: "[::1]:5000",
			want: Site{RPID: "localhost", Origin: "http://localhost:8000"},
		},
		{
			name: "a loopback IP", host: "127.0.0.1:7600", remote: "127.0.0.1:5000",
			want: Site{Origin: "http://127.0.0.1:7600"},
		},
		{
			name: "IPv6 loopback", host: "[::1]:7600", remote: "[::1]:5000",
			want: Site{Origin: "http://[::1]:7600"},
		},
		{name: "another name over HTTPS", host: "evil.test", remote: "198.51.100.23:4000", tls: true, misdirected: true},
		{name: "the domain over plain HTTP", host: "panel.acme.dev", remote: "127.0.0.1:5000", misdirected: true},
		{name: "DNS rebinding to loopback", host: "evil.test:7600", remote: "127.0.0.1:5000", misdirected: true},
		{name: "localhost from the network", host: "localhost:7600", remote: "198.51.100.23:4000", misdirected: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
			r.Host, r.RemoteAddr = tt.host, tt.remote
			if tt.tls {
				r.TLS = &tls.ConnectionState{}
			}
			got, err := sites.Resolve(r)
			if tt.misdirected {
				if !errors.Is(err, ErrMisdirected) || !strings.Contains(err.Error(), "https://panel.acme.dev") {
					t.Errorf("Resolve = %+v, %v; want ErrMisdirected naming the domain", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("Resolve = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
	r, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
	r.Host, r.RemoteAddr, r.TLS = "panel.acme.dev", "198.51.100.23:4000", &tls.ConnectionState{}
	if _, err := (Sites{}).Resolve(r); !errors.Is(err, ErrMisdirected) {
		t.Errorf("HTTPS without a domain = %v, want ErrMisdirected", err)
	}
}

func TestSessionCookies(t *testing.T) {
	secure := Site{RPID: "panel.acme.dev", Origin: "https://panel.acme.dev", Secure: true}
	c, err := secure.SessionCookie("tok", SessionMax)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"__Host-dockwell_session=tok", "Path=/", "Max-Age=604800", "HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(c.String(), want) {
			t.Errorf("secure cookie %q lacks %q", c, want)
		}
	}
	if strings.Contains(c.String(), "Domain=") {
		t.Errorf("secure cookie %q names a domain", c)
	}

	local := Site{RPID: "localhost", Origin: "http://localhost:7600"}
	c, err = local.SessionCookie("tok", SessionMax)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.String(), "dockwell_session=tok") || strings.Contains(c.String(), "Secure") ||
		!strings.Contains(c.String(), "HttpOnly") || !strings.Contains(c.String(), "SameSite=Strict") {
		t.Errorf("localhost cookie = %q", c)
	}
	if !strings.Contains(local.ClearCookie().String(), "Max-Age=0") {
		t.Errorf("clearing cookie = %q", local.ClearCookie())
	}

	for _, plain := range []Site{{Origin: "http://127.0.0.1:7600"}, {RPID: "panel.acme.dev", Origin: "http://panel.acme.dev"}} {
		if _, err := plain.SessionCookie("tok", time.Hour); err == nil {
			t.Errorf("a session cookie over plain HTTP on %s", plain.Origin)
		}
	}
}

func TestValidDomain(t *testing.T) {
	for _, ok := range []string{"panel.acme.dev", "a.b", "x-1.example.co.uk"} {
		if err := ValidDomain(ok); err != nil {
			t.Errorf("ValidDomain(%q) = %v", ok, err)
		}
	}
	for bad, mentions := range map[string]string{
		"203.0.113.7":    "IP address",
		"[2001:db8::1]":  "IP address",
		"localhost":      "needs a dot",
		"Panel.acme.dev": "lowercase",
		"panel.acme.":    "trailing dot",
		"-x.acme.dev":    "valid hostname",
		"pa_nel.acme.de": "valid hostname",
		"a..b":           "valid hostname",
	} {
		if err := ValidDomain(bad); err == nil || !strings.Contains(err.Error(), mentions) {
			t.Errorf("ValidDomain(%q) = %v, want it to mention %q", bad, err, mentions)
		}
	}
}
