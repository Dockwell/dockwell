// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/certificate"
	"github.com/dockwell/dockwell/internal/resource/dnsprovider"
	"github.com/dockwell/dockwell/internal/store"
)

// keysSetting is where the pairs of a server and a certificate an admin has let go
// there are kept (ADR 0088): a server's own certificate by the server's ID, a shared
// one by its own.
const keysSetting = "certificates.confirmed"

// KeysConfirmed are, for each server, the certificates an admin has let it have a
// key of: the server's own by its ID, a shared one by its own.
func KeysConfirmed(ctx context.Context, tx *store.Tx) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	b, err := tx.GetSetting(ctx, keysSetting)
	if errors.Is(err, store.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

// ConfirmKeys records that an admin let each server have the certificates pairs name.
func ConfirmKeys(ctx context.Context, tx *store.Tx, pairs map[string][]string) error {
	if len(pairs) == 0 {
		return nil
	}
	had, err := KeysConfirmed(ctx, tx)
	if err != nil {
		return err
	}
	for server, holders := range pairs {
		if had[server] == nil {
			had[server] = map[string]bool{}
		}
		for _, h := range holders {
			had[server][h] = true
		}
	}
	b, err := json.Marshal(had)
	if err != nil {
		return err
	}
	return tx.PutSetting(ctx, keysSetting, b)
}

// ForgetKeys drops what an admin let a server have, as it goes.
func ForgetKeys(ctx context.Context, tx *store.Tx, servers ...string) error {
	had, err := KeysConfirmed(ctx, tx)
	if err != nil {
		return err
	}
	for _, s := range servers {
		delete(had, s)
	}
	b, err := json.Marshal(had)
	if err != nil {
		return err
	}
	return tx.PutSetting(ctx, keysSetting, b)
}

// PanelDomains is what a route kind says of the panel's domains, as it's served and
// as it's saved.
type PanelDomains interface {
	PanelDomains() []string
}

// keyContext is what a plan reads once to say which certificate's key a route
// proven by DNS takes to its server (ADR 0088).
type keyContext struct {
	confirmed map[string]map[string]bool
	shared    []certificate.Shared
	providers map[string]*resourcev1.DnsProvider
	panels    []string
	projects  map[string]string
	// own counts the names each server's own certificate holds now.
	own map[string]map[string]bool
}

// keys reads what a plan needs to say which key goes where.
func (e *Engine) keys(ctx context.Context) *keyContext {
	k := &keyContext{confirmed: map[string]map[string]bool{}, providers: map[string]*resourcev1.DnsProvider{}, projects: map[string]string{}, own: map[string]map[string]bool{}}
	if t, err := e.kinds.Type("route"); err == nil {
		if p, ok := t.(PanelDomains); ok {
			k.panels = p.PanelDomains()
		}
	}
	_ = e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if k.confirmed, err = KeysConfirmed(ctx, tx); err != nil {
			return err
		}
		for _, kind := range []resource.Kind{certificate.Kind, dnsprovider.Kind} {
			stored, err := tx.ListKind(ctx, string(kind))
			if err != nil {
				return err
			}
			for i := range stored {
				r := &stored[i]
				if r.Spec == nil {
					continue
				}
				spec, err := e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
				if err != nil {
					return err
				}
				switch s := spec.(type) {
				case *resourcev1.DnsCertificate:
					k.shared = append(k.shared, certificate.Shared{ID: r.ID, Name: r.Name, Spec: s})
				case *resourcev1.DnsProvider:
					s.ApiToken = ""
					k.providers[r.Name] = s
				}
			}
		}
		slices.SortFunc(k.shared, func(a, b certificate.Shared) int { return strings.Compare(a.Name, b.Name) })
		return e.ownNames(ctx, tx, k)
	})
	return k
}

// proven says whether a provider proves domain for project.
func (k *keyContext) proven(domain, project string) bool {
	for _, p := range k.providers {
		if dnsprovider.Serves(p, project) && certificate.ZoneOf(p.GetZones(), domain) != "" {
			return true
		}
	}
	return false
}

// ownNames counts, for each server, the names its own certificate holds now: those
// of its routes proven by DNS that a provider proves and no shared certificate serves.
func (e *Engine) ownNames(ctx context.Context, tx *store.Tx, k *keyContext) error {
	routes, err := tx.ListKind(ctx, "route")
	if err != nil {
		return err
	}
	for i := range routes {
		r := &routes[i]
		if r.Spec == nil {
			continue
		}
		spec, err := e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		rt, ok := spec.(*resourcev1.Route)
		if !ok || rt.GetProof() != "dns" || rt.GetTls() == ingress.TLSOff {
			continue
		}
		project := ""
		if env, err := tx.GetResourceByID(ctx, r.Scope); err == nil {
			project = env.Scope
		}
		k.projects[r.Scope] = project
		if !k.proven(rt.GetDomain(), project) {
			continue
		}
		placed, err := tx.Placement(ctx, r.ID, r.Revision)
		if err != nil {
			return err
		}
		for _, server := range placed {
			if _, shared := certificate.Serving(k.shared, rt.GetDomain(), project, server, e.controlServer, k.panels); shared {
				continue
			}
			if k.own[server] == nil {
				k.own[server] = map[string]bool{}
			}
			k.own[server][rt.GetDomain()] = true
		}
	}
	return nil
}

// projectOf is the project an environment is in.
func (e *Engine) projectOf(ctx context.Context, k *keyContext, env string) string {
	if p, ok := k.projects[env]; ok {
		return p
	}
	p := ""
	_ = e.db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResourceByID(ctx, env)
		if err == nil {
			p = r.Scope
		}
		return err
	})
	k.projects[env] = p
	return p
}

// keyFor says what a route proven by DNS on server takes: the access an admin
// confirms, or a note where no key goes, or neither where the server has its key.
func (e *Engine) keyFor(ctx context.Context, k *keyContext, rt *resourcev1.Route, ref resource.Ref, server string) (*resource.Access, *Note) {
	domain := rt.GetDomain()
	project := e.projectOf(ctx, k, ref.Scope)
	holder, names := server, []string{domain}
	if c, ok := certificate.Serving(k.shared, domain, project, server, e.controlServer, k.panels); ok {
		holder, names = c.ID, c.Spec.GetDomains()
	} else {
		if !k.proven(domain, project) {
			return nil, &Note{
				Reason: "no_dns_provider", Ref: ref, Names: []string{domain},
				Message: domain + " is proven by DNS, and no DNS provider proves it for its project, so it gets no certificate until an admin connects one that does",
			}
		}
		if k.own[server] == nil {
			k.own[server] = map[string]bool{}
		}
		k.own[server][domain] = true
		if len(k.own[server]) > certificate.MaxNames {
			return nil, &Note{
				Reason: "too_many_dns_names", Ref: ref, Names: []string{domain},
				Message: domain + " is proven by DNS, and its server's certificate holds the 100 names a certificate can, so it gets none. Share a wildcard for its zone",
			}
		}
	}
	if k.confirmed[server][holder] {
		return nil, nil
	}
	a := resource.Access{Kind: resource.AccessCertificateKey, Path: domain, Address: holder}
	if holder != server {
		a.Detail = strings.Join(names, ", ")
	}
	return &a, nil
}

// checkCertificates refuses a shared certificate with a name none of its provider's
// zones holds, as its provider's token is used in those zones alone (ADR 0088). A
// provider in the same change counts as it will be.
func (e *Engine) checkCertificates(ctx context.Context, items []item) error {
	var k *keyContext
	for i := range items {
		c, ok := items[i].spec.(*resourcev1.DnsCertificate)
		if !ok {
			continue
		}
		if k == nil {
			k = e.keys(ctx)
			for j := range items {
				if p, ok := items[j].spec.(*resourcev1.DnsProvider); ok {
					k.providers[items[j].ref.Name] = p
				}
			}
		}
		p := k.providers[c.GetDnsProvider()]
		if p == nil {
			continue // the reference check names a provider that isn't there
		}
		if out := certificate.Outside(c.GetDomains(), p.GetZones()); out != "" {
			return fault.New(ErrInvalid, "the certificate %s names %s, which none of the DNS provider %s's zones holds, and its token writes in those alone. Add its zone to the provider, or name another",
				items[i].ref.Name, out, c.GetDnsProvider())
		}
	}
	return nil
}
