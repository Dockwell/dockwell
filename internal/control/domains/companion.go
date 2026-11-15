// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/certificates"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/certificate"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Companion is the other half of a domain a site is also expected to answer on
// (ADR 0070): www's name for a registered domain, by the public suffix list, so
// example.co.uk gets www.example.co.uk, or the registered domain for its www name.
// Any other name, such as blog.example.com, has none.
func Companion(domain string) string {
	registered, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		return ""
	}
	switch domain {
	case registered:
		return "www." + domain
	case "www." + registered:
		return registered
	}
	return ""
}

// companionCheck says where a domain's companion stands: what DNS says of it, as
// check said of the domain, who serves it already, and the wildcard that covers it.
func (h *Handler) companionCheck(ctx context.Context, project string, of, check *resource.DomainCheck) (*dockwellv1.CompanionCheck, error) {
	out := &dockwellv1.CompanionCheck{AnswersAlike: answersAlike(of, check)}
	admin, mine := callerProjects(ctx)
	reads := func(p string) bool { return admin || mine == nil || mine[p] }
	err := h.db.View(ctx, func(tx *store.Tx) error {
		env, err := h.servedIn(ctx, tx, check.Domain)
		if err != nil || env == "" {
			return err
		}
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		out.HeldBy = "another project"
		for _, p := range tree.Projects {
			if !slices.ContainsFunc(p.Environments, func(e scope.Environment) bool { return e.ID == env }) || !reads(p.ID) {
				continue
			}
			out.HeldByEnvironmentId, out.HeldBy = env, cmp.Or(p.DisplayName, p.Name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if h.Certificates == nil || project == "" || !reads(project) {
		return out, nil
	}
	list, err := h.Certificates.List(ctx)
	if err != nil {
		return nil, err
	}
	out.Wildcard = wildcardFor(list, project, check.Domain)
	return out, nil
}

// wildcardFor is the wildcard of a shared certificate that serves project and covers
// host, so host needs no certificate of its own (ADR 0088), or empty when none does.
// A server's own certificate is no one domain's to lean on.
func wildcardFor(list []certificates.Standing, project, host string) string {
	for i := range list {
		c := &list[i]
		if !strings.HasPrefix(c.Holder, "crt_") || !slices.Contains(c.Projects, project) {
			continue
		}
		for _, n := range c.Names {
			if strings.HasPrefix(n, "*.") && certificate.Covers(n, host) {
				return n
			}
		}
	}
	return ""
}

// servedIn is the environment a route of domain is in, by ID, or empty when no
// route serves it.
func (h *Handler) servedIn(ctx context.Context, tx *store.Tx, domain string) (string, error) {
	stored, err := tx.ListKind(ctx, string(route.Kind))
	if err != nil {
		return "", err
	}
	for i := range stored {
		r := &stored[i]
		plain, err := tx.Open(ctx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return "", err
		}
		spec, err := h.kinds.Decode(route.Kind, plain)
		if err != nil {
			return "", err
		}
		if spec.(*resourcev1.Route).GetDomain() == domain {
			return r.Scope, nil
		}
	}
	return "", nil
}

// answersAlike says whether two names reach the same place: both point at the
// server, or both at the same addresses elsewhere, or both at Cloudflare's proxy,
// whose addresses can differ by name.
func answersAlike(a, b *resource.DomainCheck) bool {
	switch {
	case len(a.Found) == 0 || len(b.Found) == 0:
		return false
	case a.State == resource.DomainHere && b.State == resource.DomainHere:
		return true
	case a.Proxied && b.Proxied:
		return true
	}
	x, y := slices.Sorted(slices.Values(a.Found)), slices.Sorted(slices.Values(b.Found))
	return slices.Equal(x, y)
}
