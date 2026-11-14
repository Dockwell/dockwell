// SPDX-License-Identifier: AGPL-3.0-only

// Package domains serves dockwell.v1.DomainService: the domains of the
// routes that exist now, with where each one's certificate and DNS stand on
// its servers, as their agents say. See
// docs/adr/0024-the-caddy-ingress-driver.md.
package domains

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/certificates"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/signinproviders"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
)

// Reports is what each server's agent last said of its routes' domains.
type Reports interface {
	Domain(server, domain string) (status *dockwellv1.DomainStatus, connected bool)
	Ingress(server string) *dockwellv1.IngressStatus
}

// Servers is what's known of each server.
type Servers interface {
	Get(id string) (*dockwellv1.Server, error)
	PublicAddress(server string) *agentv1.PublicAddress
}

// Handler implements DomainService.
type Handler struct {
	kinds   *resource.Registry
	db      *store.DB
	reports Reports
	servers Servers
	logger  *slog.Logger
	// Lookup resolves a name for CheckDomain. Nil means the control server's resolver.
	Lookup  func(ctx context.Context, name string) ([]netip.Addr, error)
	callers *callers
	// DNS are the DNS providers an admin connected (ADR 0088); nil has none.
	DNS *dnsproviders.Providers
	// Certificates are the certificates by DNS (ADR 0088); nil has none.
	Certificates *certificates.Manager
	// Providers reads the sign-in providers, and checks an issuer. Nil keeps none.
	Providers *signinproviders.Providers
}

var _ dockwellv1connect.DomainServiceHandler = (*Handler)(nil)

// New returns a handler that reads routes from db, with kinds, what their servers
// say from reports, and what's known of a server from servers.
func New(kinds *resource.Registry, db *store.DB, reports Reports, servers Servers, logger *slog.Logger) *Handler {
	return &Handler{kinds: kinds, db: db, reports: reports, servers: servers, logger: logger, callers: newCallers(checksPerMinute)}
}

// CheckDomain reads DNS for a name against what's known of a server. It's a read a
// form makes as someone waits for a record, so it needs no route, and no agent: it
// goes by the control plane's resolver.
func (h *Handler) CheckDomain(ctx context.Context, req *connect.Request[dockwellv1.CheckDomainRequest]) (*connect.Response[dockwellv1.CheckDomainResponse], error) {
	domain := strings.ToLower(strings.TrimSpace(req.Msg.GetDomain()))
	if err := route.CheckDomain(domain); err != nil {
		return nil, authapi.Invalid(err)
	}
	if h.servers == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the control plane has no servers to check a domain against yet"))
	}
	srv, err := h.servers.Get(req.Msg.GetServerId())
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("there's no server "+req.Msg.GetServerId()+". List them with ListServers"))
	}
	if err != nil {
		return nil, err
	}
	caller := "someone"
	if p, ok := authapi.PrincipalFrom(ctx); ok {
		caller = p.Actor()
	}
	if !h.callers.allow(caller) {
		err := connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("you've checked %d domains in the last minute. Wait a minute, then check again", checksPerMinute))
		err.Meta().Set(authapi.ReasonKey, "too_many_requests")
		return nil, err
	}
	own := srv.GetInventory().GetAddresses()
	// Only what the internet can reach is where a record can lead to this server: a
	// private address, a bridge's or a TUN's on it never is.
	dns := &ingress.DNS{Lookup: h.Lookup, Addrs: func() ([]netip.Addr, error) {
		var public []netip.Addr
		for _, s := range resource.PublicAddresses(own) {
			public = append(public, netip.MustParseAddr(s).Unmap())
		}
		return public, nil
	}}
	dns.Know(h.servers.PublicAddress(srv.GetId()))
	check := dns.Check(ctx, domain)
	// What a name inside some network resolves to is no business of anyone who can only
	// reach this API, so a private answer is shown only when it's the server's own.
	check.Found = slices.DeleteFunc(check.Found, func(s string) bool {
		return len(resource.PublicAddresses([]string{s})) == 0 && !slices.Contains(own, s)
	})
	return connect.NewResponse(&dockwellv1.CheckDomainResponse{Check: resourcepb.DomainCheck(check)}), nil
}

// ListDomains returns the domains of the routes that exist now.
func (h *Handler) ListDomains(ctx context.Context, req *connect.Request[dockwellv1.ListDomainsRequest]) (*connect.Response[dockwellv1.ListDomainsResponse], error) {
	env, svc := req.Msg.GetEnvironment(), req.Msg.GetService()
	if svc != "" && env == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a service's name is unique only in its environment, so name the environment too"))
	}
	byName := map[string]*dockwellv1.Domain{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		stored, err := tx.ListKind(ctx, string(route.Kind))
		if err != nil {
			return err
		}
		for i := range stored {
			if err := h.add(ctx, tx, &stored[i], byName); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		h.logger.ErrorContext(ctx, "can't list domains", slog.String("error", err.Error()))
		return nil, connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read its routes; try again, and check its logs if it keeps failing"))
	}
	res := &dockwellv1.ListDomainsResponse{}
	for _, d := range byName {
		if env != "" && d.GetEnvironmentId() != env {
			continue
		}
		if svc != "" && !slices.ContainsFunc(d.GetRoutes(), func(r *dockwellv1.DomainRoute) bool { return r.GetService() == svc }) {
			continue
		}
		slices.SortFunc(d.Routes, func(a, b *dockwellv1.DomainRoute) int { return cmp.Compare(a.GetPath(), b.GetPath()) })
		slices.SortFunc(d.Servers, func(a, b *dockwellv1.DomainServer) int { return cmp.Compare(a.GetServerName(), b.GetServerName()) })
		res.Domains = append(res.Domains, d)
	}
	slices.SortFunc(res.Domains, func(a, b *dockwellv1.Domain) int { return cmp.Compare(a.GetName(), b.GetName()) })
	return connect.NewResponse(res), nil
}

// add puts one route in its domain.
func (h *Handler) add(ctx context.Context, tx *store.Tx, r *store.Resource, byName map[string]*dockwellv1.Domain) error {
	plain, err := tx.Open(ctx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return err
	}
	spec, err := h.kinds.Decode(route.Kind, plain)
	if err != nil {
		return err
	}
	rt := spec.(*resourcev1.Route)
	name := rt.GetDomain()
	d := byName[name]
	// What this server serves, which always answers: whatever ends TLS in front of
	// it may answer HTTPS too, and a private network may have nothing to.
	scheme := "https://"
	if rt.GetTls() == ingress.TLSOff {
		scheme = "http://"
	}
	if d == nil {
		d = &dockwellv1.Domain{Name: name, EnvironmentId: r.Scope, Url: scheme + name}
		byName[name] = d
	}
	var users []string
	for _, u := range rt.GetAccess().GetUsers() {
		users = append(users, u.GetName())
	}
	d.Routes = append(d.Routes, &dockwellv1.DomainRoute{
		Id: r.ID, Name: r.Name, Path: rt.GetPath(), Service: rt.GetService(), Port: rt.GetPort(), Url: scheme + route.Where(rt),
		Tls: rt.GetTls(), HostPort: rt.GetHostPort(), PlainForAnyone: rt.GetPlainForAnyone(), Held: rt.GetHeld(), TrustedProxies: rt.GetTrustedProxies(),
		Allow: rt.GetAccess().GetAllow(), Users: users, SignIn: withoutProvider(rt.GetAccess().GetSignIn()),
	})
	placed, err := tx.Placement(ctx, r.ID, r.Revision)
	if err != nil {
		return err
	}
	for _, id := range placed {
		if slices.ContainsFunc(d.Servers, func(s *dockwellv1.DomainServer) bool { return s.GetServerId() == id }) {
			continue
		}
		srv, err := tx.GetServer(ctx, id)
		if err != nil {
			return err
		}
		if srv.RevokedAt != nil {
			continue
		}
		status, connected := h.reports.Domain(id, name)
		d.Servers = append(d.Servers, &dockwellv1.DomainServer{ServerId: id, ServerName: srv.Name, Status: status, Stale: !connected, Ingress: h.reports.Ingress(id)})
	}
	return nil
}
