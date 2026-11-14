// SPDX-License-Identifier: AGPL-3.0-only

// Package certificate is the "certificate" kind: a certificate the control plane
// proves by DNS-01 through a DNS provider, and renews, for a wildcard or for names
// the internet can't reach. Routes whose domains it covers, in the projects it
// serves, are served with it on any server. It lives in the organization: only an
// admin makes one, as DNS can prove a name that points anywhere. See
// docs/adr/0088-certificates-by-dns-01-and-wildcard-domains.md.
package certificate

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/dnsprovider"
)

// Kind is the certificate kind.
const Kind resource.Kind = "certificate"

// MaxNames is how many names one certificate covers, as Let's Encrypt allows.
const MaxNames = 100

// Type is the kind.
type Type struct{}

var (
	_ resource.Type     = Type{}
	_ resource.Referrer = Type{}
)

// Kind returns "certificate".
func (Type) Kind() resource.Kind { return Kind }

// Level is the organization: only an admin makes one.
func (Type) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "crt".
func (Type) IDPrefix() string { return "crt" }

// New returns an empty Certificate.
func (Type) New() proto.Message { return &resourcev1.DnsCertificate{} }

// CheckName takes what fits in a URL and on a command line.
func (Type) CheckName(name string) error {
	return resource.CheckLabel("certificate", name, "example-wildcard")
}

// Name writes one of a certificate's names as DNS does, lowercase with no trailing
// dot, with a wildcard only as its whole first label. It's empty for what can't be
// one.
func Name(s string) string {
	n := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	if rest, ok := strings.CutPrefix(n, "*."); ok {
		if dnsprovider.Zone(rest) != rest {
			return ""
		}
		return n
	}
	if strings.Contains(n, "*") || dnsprovider.Zone(n) != n {
		return ""
	}
	return n
}

// Covers says whether name, a certificate's, covers host, a route's domain: the
// same name, or a wildcard whose one label stands for host's first.
func Covers(name, host string) bool {
	if rest, ok := strings.CutPrefix(name, "*."); ok {
		first, under, found := strings.Cut(host, ".")
		return found && first != "" && under == rest
	}
	return name == host
}

// CoversAny says whether any of names covers host.
func CoversAny(names []string, host string) bool {
	return slices.ContainsFunc(names, func(n string) bool { return Covers(n, host) })
}

// Validate refuses what can't be a certificate, saying what to write.
func (Type) Validate(spec proto.Message) error {
	c := spec.(*resourcev1.DnsCertificate) //nolint:forcetypeassert // the kind's own message
	if len(c.GetDomains()) == 0 {
		return errors.New("a certificate needs the names it covers, such as *.example.com and example.com")
	}
	if len(c.GetDomains()) > MaxNames {
		return fmt.Errorf("a certificate covers up to %d names, as Let's Encrypt allows, and this names %d. Use a wildcard, or a second certificate", MaxNames, len(c.GetDomains()))
	}
	names := make([]string, 0, len(c.GetDomains()))
	for _, d := range c.GetDomains() {
		n := Name(d)
		switch {
		case n == "" && strings.Contains(d, "*"):
			return fmt.Errorf("%s isn't a wildcard a certificate authority issues: the * goes alone as the first label, as in *.example.com", d)
		case n == "":
			return fmt.Errorf("%q isn't a name a certificate can be had for. Write a public domain, such as app.example.com", d)
		case slices.Contains(names, n):
			return fmt.Errorf("%s is named twice. Name each once", n)
		}
		names = append(names, n)
	}
	slices.Sort(names)
	c.Domains = names
	if c.GetDnsProvider() == "" {
		return errors.New("a certificate needs the DNS provider that proves its names, by name. List them with dockwell dns-providers list")
	}
	if err := resource.CheckLabel("DNS provider", c.GetDnsProvider(), "cloudflare"); err != nil {
		return err
	}
	for _, p := range c.GetProjects() {
		if !strings.HasPrefix(p, "prj_") {
			return fmt.Errorf("%q isn't a project's ID. A certificate names the projects it serves by ID, such as prj_…", p)
		}
	}
	return nil
}

// References names the DNS provider, so it can't go while a certificate uses it.
func (Type) References(spec proto.Message) []resource.Reference {
	return []resource.Reference{{Kind: dnsprovider.Kind, Name: spec.(*resourcev1.DnsCertificate).GetDnsProvider(), Verb: "prove its names with", Apart: true}}
}

// ReferenceKinds is DNS providers.
func (Type) ReferenceKinds() []resource.Kind { return []resource.Kind{dnsprovider.Kind} }

// ZoneOf is the zone among zones a name is proven in: the longest that holds it,
// a wildcard's by what follows its first label. It's empty for none.
func ZoneOf(zones []string, name string) string {
	host := strings.TrimPrefix(name, "*.")
	best := ""
	for _, z := range zones {
		if dnsprovider.Covers(z, host) && len(z) > len(best) {
			best = z
		}
	}
	return best
}

// Serves says whether a certificate serves project's routes: one that names no
// project serves none.
func Serves(c *resourcev1.DnsCertificate, project string) bool {
	return slices.Contains(c.GetProjects(), project)
}

// Shared is a shared certificate, as saved.
type Shared struct {
	ID, Name string
	Spec     *resourcev1.DnsCertificate
}

// TouchesPanel says whether names reach any of the panel's domains: one covers it,
// as a wildcard over it does, or is it or a name under it. A key for such names is a
// key to the panel's passkeys, so it stays on the control server (ADR 0088).
func TouchesPanel(names, panels []string) bool {
	for _, p := range panels {
		if p == "" {
			continue
		}
		for _, n := range names {
			host := strings.TrimPrefix(n, "*.")
			if Covers(n, p) || host == p || strings.HasSuffix(host, "."+p) {
				return true
			}
		}
	}
	return false
}

// Serving is the shared certificate that serves domain, a route's, for project on
// server: one that covers it and serves the project, and, unless server is the
// control server, touches none of the panel's domains. False is the server's own.
func Serving(shared []Shared, domain, project, server, control string, panels []string) (Shared, bool) {
	for _, c := range shared {
		if !Serves(c.Spec, project) || !CoversAny(c.Spec.GetDomains(), domain) {
			continue
		}
		if server != control && TouchesPanel(c.Spec.GetDomains(), panels) {
			continue
		}
		return c, true
	}
	return Shared{}, false
}

// Outside is the first of names no zone holds, or "" when each is in one.
func Outside(names, zones []string) string {
	for _, n := range names {
		if ZoneOf(zones, n) == "" {
			return n
		}
	}
	return ""
}
