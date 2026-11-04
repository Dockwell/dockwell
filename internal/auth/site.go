// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Session cookie names. On HTTPS the cookie takes the __Host- prefix, which
// browsers accept only with Secure, Path=/ and no Domain, so nothing on
// another subdomain or over plain HTTP can set or shadow it.
const (
	SecureCookieName   = "__Host-dockwell_session"
	LoopbackCookieName = "dockwell_session"
)

// Site is the address a request reached the panel at: what WebAuthn calls
// the relying party, and the origin the browser is on.
type Site struct {
	// RPID is "localhost" or the panel's domain. Empty for a loopback IP
	// address, where passkeys can't work.
	RPID string
	// Origin is scheme://host[:port] as the browser sees it, such as
	// "http://localhost:7600" or "https://panel.acme.dev".
	Origin string
	// Secure is true over HTTPS.
	Secure bool
}

// CanSignIn reports whether people can sign in at this address.
func (s Site) CanSignIn() bool { return s.RPID != "" }

// CookieName is the session cookie's name at this address.
func (s Site) CookieName() string {
	if s.Secure {
		return SecureCookieName
	}
	return LoopbackCookieName
}

// SessionCookie is the cookie that carries token for maxAge. It is HttpOnly
// and SameSite=Strict everywhere, and Secure with the __Host- prefix on
// HTTPS. It refuses to make a cookie for plain HTTP anywhere but localhost.
func (s Site) SessionCookie(token string, maxAge time.Duration) (*http.Cookie, error) {
	if !s.Secure && s.RPID != "localhost" {
		return nil, errors.New("session cookies are only set over HTTPS, or on localhost")
	}
	// Secure is false only on localhost, which browsers treat as a secure
	// context and which plain HTTP never leaves.
	return &http.Cookie{ //nolint:gosec // see above
		Name:     s.CookieName(),
		Value:    token,
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteStrictMode,
	}, nil
}

// ClearCookie removes the session cookie.
func (s Site) ClearCookie() *http.Cookie {
	return &http.Cookie{ //nolint:gosec // as SessionCookie
		Name:     s.CookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteStrictMode,
	}
}

// Sites says which addresses the panel answers on: localhost and loopback
// addresses over plain HTTP, and the domain, if there is one, over HTTPS.
type Sites struct {
	// Domain is the panel's public hostname, such as "panel.acme.dev". Empty
	// keeps the panel on loopback.
	Domain string
}

// Serves reports whether a passkey made for rpID signs in at the panel now:
// on localhost always, through the loopback listener and a tunnel, and on
// the domain while the panel has it. Passkeys for a domain the panel no
// longer answers on don't.
func (s Sites) Serves(rpID string) bool {
	return rpID == "localhost" || (s.Domain != "" && rpID == s.Domain)
}

// ErrMisdirected reports a request for a host the panel doesn't answer on,
// such as another name pointed at this server, or a DNS rebinding attack.
var ErrMisdirected = errors.New("misdirected request")

// Resolve returns the site a request is for, or an error wrapping
// ErrMisdirected that says where the panel does answer.
func (s Sites) Resolve(r *http.Request) (Site, error) {
	host, port := splitHost(r.Host)
	if r.TLS != nil {
		if s.Domain == "" || host != s.Domain {
			return Site{}, fmt.Errorf("%w: %s", ErrMisdirected, s.where())
		}
		origin := "https://" + host
		if port != "" && port != "443" {
			origin += ":" + port
		}
		return Site{RPID: s.Domain, Origin: origin, Secure: true}, nil
	}
	if !isLoopbackHost(host) || !fromLoopback(r.RemoteAddr) {
		return Site{}, fmt.Errorf("%w: %s", ErrMisdirected, s.where())
	}
	origin := "http://" + joinHost(host, port)
	if host == "localhost" {
		return Site{RPID: "localhost", Origin: origin}, nil
	}
	return Site{Origin: origin}, nil
}

func (s Sites) where() string {
	if s.Domain != "" {
		return "the panel answers at https://" + s.Domain
	}
	return "the panel answers only on localhost. Reach it through an SSH tunnel, or give it a domain with dockwell run --domain"
}

// splitHost splits a Host header into a lowercase hostname without a
// trailing dot, and the port if there is one.
func splitHost(hostport string) (host, port string) {
	host = hostport
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		host, port = h, p
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	return host, port
}

func joinHost(host, port string) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func fromLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ValidDomain checks that domain can be the panel's hostname and a WebAuthn
// relying party ID: a lowercase DNS name with at least two labels, not an
// IP address.
func ValidDomain(domain string) error {
	if net.ParseIP(strings.Trim(domain, "[]")) != nil {
		return fmt.Errorf("%s is an IP address. Passkeys need a hostname: point a DNS A record at this server and pass that name", domain)
	}
	if domain != strings.ToLower(domain) || strings.HasSuffix(domain, ".") || len(domain) > 253 {
		return fmt.Errorf("%s isn't a lowercase hostname without a trailing dot, such as panel.example.com", domain)
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%s needs a dot, such as panel.example.com. Let's Encrypt only issues certificates for public names", domain)
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return fmt.Errorf("%s isn't a valid hostname, such as panel.example.com", domain)
		}
		for _, r := range l {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return fmt.Errorf("%s isn't a valid hostname, such as panel.example.com", domain)
			}
		}
	}
	return nil
}
