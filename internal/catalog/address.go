// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"net"
	"regexp"
	"strings"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// An Address is where a browser opens an app (ADR 0103): the oldest domain routed
// to what web opens, or the panel's Open address, or web's port on loopback.
type Address struct {
	// Scheme is "https" for a domain and "http" otherwise.
	Scheme string
	// Host is the domain, or the host and port: "docs.example.com",
	// "abcdefghij.localhost:7600" or "localhost:8080".
	Host string
}

// DomainAddress is an app's address at a domain.
func DomainAddress(domain string) Address { return Address{Scheme: "https", Host: domain} }

// LocalAddress is an app's address on a name only the person's own machine reaches,
// as the panel's Open address and a tunnel to its port are.
func LocalAddress(host string) Address { return Address{Scheme: "http", Host: host} }

// ParseAddress reads an address as Origin writes it. The zero Address, and false,
// for anything else, as an install's pin from before it kept one.
func ParseAddress(origin string) (Address, bool) {
	scheme, host, ok := strings.Cut(origin, "://")
	if !ok || (scheme != "https" && scheme != "http") || host == "" || strings.ContainsAny(host, "/?#@ ") {
		return Address{}, false
	}
	return Address{Scheme: scheme, Host: host}, true
}

// Origin is the address as a browser opens it, such as "https://docs.example.com".
func (a Address) Origin() string {
	if a.Host == "" {
		return ""
	}
	return a.Scheme + "://" + a.Host
}

// Domain reports a domain: any name but localhost and the names under it, which
// only the person's own machine reaches.
func (a Address) Domain() bool {
	name := a.Host
	if h, _, err := net.SplitHostPort(a.Host); err == nil {
		name = h
	}
	return name != "" && name != "localhost" && !strings.HasSuffix(name, ".localhost")
}

// addressPart is what an address variable's value names of the address.
var addressPart = regexp.MustCompile(`\{(origin|host|scheme|domain)\}`)

// Unrendered reports a value that names a part of the address, as written in an
// entry, rather than the address itself: an older panel wrote {origin} as it found
// it, replacing {domain} alone. Such a value is Dockwell's to render, never a
// person's (#779's review, M2).
func Unrendered(value string) bool { return addressPart.MatchString(value) }

// AddressValue is what an address variable's value says at a. It's false where a
// isn't a domain for a value that names {domain}, or names nothing of the address,
// as Nextcloud's OVERWRITEPROTOCOL "https": such a value goes with a domain, and is
// written once the app has one alone.
func AddressValue(value string, a Address) (string, bool) {
	if a.Host == "" || !a.Domain() && (strings.Contains(value, "{domain}") || !followsAlways(value)) {
		return "", false
	}
	return addressPart.ReplaceAllStringFunc(value, func(part string) string {
		switch part {
		case "{origin}":
			return a.Origin()
		case "{scheme}":
			return a.Scheme
		}
		return a.Host
	}), true
}

// AddressValues are the values an entry's address variables take at a, by service
// and variable, leaving out each one a takes no value for.
func AddressValues(e *catalogv1.Entry, a Address) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, v := range e.GetAddressVariables() {
		value, ok := AddressValue(v.GetValue(), a)
		if !ok {
			continue
		}
		if out[v.GetService()] == nil {
			out[v.GetService()] = map[string]string{}
		}
		out[v.GetService()][v.GetVariable()] = value
	}
	return out
}

// followsAlways reports a value that follows the app's address at every move, as
// one that names {origin}, {host} or {scheme} does, and no {domain}.
func followsAlways(value string) bool {
	return !strings.Contains(value, "{domain}") && strings.Contains(value, "{")
}

// WebPort is the port on the server web's service publishes, as variables have it,
// the lowest where it publishes several: what a tunnel to the app reaches. "" where
// it publishes none over TCP.
func WebPort(e *catalogv1.Entry, variables map[string]string) string {
	for _, p := range Published(e, variables)[e.GetWeb().GetService()] {
		if !strings.Contains(p, "/") {
			return p
		}
	}
	return ""
}
