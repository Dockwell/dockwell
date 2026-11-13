// SPDX-License-Identifier: AGPL-3.0-only

// Package dnsprovider is the "dns.provider" kind: what writes the records of DNS
// zones through a provider's API, so the control plane can prove a name by DNS-01
// and get its certificate. It lives in the organization, on the control plane
// alone: its token goes to no server. See
// docs/adr/0088-certificates-by-dns-01-and-wildcard-domains.md.
package dnsprovider

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the DNS provider kind.
const Kind resource.Kind = "dns.provider"

// Cloudflare is Cloudflare's API, the one provider for now.
const Cloudflare = "cloudflare"

// maxZones is how many zones one provider writes in.
const maxZones = 50

// Type is the kind.
type Type struct{}

var (
	_ resource.Type       = Type{}
	_ resource.Rotates    = Type{}
	_ resource.Keeper     = Type{}
	_ resource.NewChecker = Type{}
)

// Kind returns "dns.provider".
func (Type) Kind() resource.Kind { return Kind }

// Level is the organization: only an admin connects a provider.
func (Type) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "dnp".
func (Type) IDPrefix() string { return "dnp" }

// New returns an empty DnsProvider.
func (Type) New() proto.Message { return &resourcev1.DnsProvider{} }

// RotatesSensitive says setting the token always plans as a change, never
// compared with what's there, so a plan can't confirm a guess at it (ADR 0022).
func (Type) RotatesSensitive() {}

// CheckName takes what fits in a URL and on a command line.
func (Type) CheckName(name string) error {
	return resource.CheckLabel("DNS provider", name, "cloudflare")
}

// Zone writes a zone as DNS names it: lowercase, with no trailing dot. It's empty
// for what can't be a zone a certificate is had for: a wildcard, an address, one
// label, or a name under a reserved top-level domain.
func Zone(s string) string {
	z := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	labels := strings.Split(z, ".")
	if len(z) > 253 || len(labels) < 2 {
		return ""
	}
	for _, l := range labels {
		if !isLabel(l) {
			return ""
		}
	}
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" || reserved[last] {
		return ""
	}
	return z
}

// reserved are top-level domains no public certificate authority issues for, as
// a route's domain refuses them.
var reserved = map[string]bool{
	"localhost": true, "local": true, "internal": true, "test": true, "example": true,
	"invalid": true, "onion": true, "alt": true, "arpa": true,
}

// isLabel says whether l is a DNS label: up to 63 lowercase letters, digits and
// dashes, not starting or ending with a dash.
func isLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for _, r := range l {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// Covers says whether name is the zone or under it.
func Covers(zone, name string) bool {
	return name == zone || strings.HasSuffix(name, "."+zone)
}

// Validate refuses what can't write DNS, in words that hold nothing of the token.
func (Type) Validate(spec proto.Message) error {
	p := spec.(*resourcev1.DnsProvider) //nolint:forcetypeassert // the kind's own message
	switch p.GetProvider() {
	case Cloudflare:
	case "":
		return errors.New("a DNS provider needs its provider's API, cloudflare")
	default:
		return fmt.Errorf("a DNS provider writes through Cloudflare's API for now, and %q isn't it. Use cloudflare", p.GetProvider())
	}
	if len(p.GetZones()) == 0 {
		return errors.New("a DNS provider needs the zones it writes in, such as example.com. Check its token to list the zones it reaches")
	}
	if len(p.GetZones()) > maxZones {
		return fmt.Errorf("a DNS provider writes in up to %d zones, and this names %d. Connect another for the rest", maxZones, len(p.GetZones()))
	}
	zones := make([]string, 0, len(p.GetZones()))
	for _, z := range p.GetZones() {
		zone := Zone(z)
		switch {
		case strings.HasPrefix(strings.TrimSpace(z), "*"):
			return fmt.Errorf("%s is a wildcard, and a zone is the domain itself, such as example.com", z)
		case zone == "":
			return fmt.Errorf("%q isn't a zone a certificate can be had for. Write the domain the provider holds, such as example.com", z)
		case slices.Contains(zones, zone):
			return fmt.Errorf("%s is named twice. Name each zone once", zone)
		}
		zones = append(zones, zone)
	}
	slices.Sort(zones)
	p.Zones = zones
	if strings.ContainsAny(p.GetApiToken(), " \t\r\n") {
		return errors.New("the API token has a space or a line break in it. Paste the token alone")
	}
	if IsGlobalKey(p.GetApiToken()) {
		return ErrGlobalKey
	}
	for _, id := range p.GetProjects() {
		if !strings.HasPrefix(id, "prj_") {
			return fmt.Errorf("%q isn't a project's ID. A DNS provider names the projects it proves names for by ID, such as prj_…", id)
		}
	}
	return nil
}

// Serves says whether a provider proves names for project's routes: one that names
// no project proves none.
func Serves(p *resourcev1.DnsProvider, project string) bool {
	return slices.Contains(p.GetProjects(), project)
}

// ErrGlobalKey is a Cloudflare Global API Key, which can do anything the account can,
// offered where an API token goes.
var ErrGlobalKey = errors.New("that's Cloudflare's Global API Key, which can do anything the account can. Make an API token with Zone:Read and DNS:Edit for your zones, and use that")

// IsGlobalKey says whether s looks like a Cloudflare Global API Key: 37 lowercase hex
// digits, where an API token is 40 characters of base64url, or cfat_ and more for one
// an account owns.
func IsGlobalKey(s string) bool {
	if len(s) != 37 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// CheckNew asks a new provider for its token. An update can leave it out, which
// Keep fills in, so Validate can't.
func (Type) CheckNew(spec proto.Message) error {
	if spec.(*resourcev1.DnsProvider).GetApiToken() == "" { //nolint:forcetypeassert // Validate saw it
		return errors.New("a DNS provider needs its API token: for Cloudflare, an API token with Zone:Read and DNS:Edit, scoped to its zones")
	}
	return nil
}

// Keep holds on to the token when an update leaves it out, so a form can change
// a provider's zones without asking for it again, unless the update moves it to
// another provider's API, which never gets a token it wasn't saved for.
func (Type) Keep(current, desired proto.Message) (resource.Kept, error) {
	was, ok := current.(*resourcev1.DnsProvider)
	is, same := desired.(*resourcev1.DnsProvider)
	if !ok || !same || is.GetApiToken() != "" || was.GetApiToken() == "" {
		return resource.Kept{}, nil
	}
	if is.GetProvider() != was.GetProvider() {
		return resource.Kept{}, errors.New("the provider moves to another provider's API, so it needs a token for that one")
	}
	is.ApiToken = was.GetApiToken()
	return resource.Kept{Reason: "kept_token", Words: "keeps the token it was saved with, as the update left that out"}, nil
}
