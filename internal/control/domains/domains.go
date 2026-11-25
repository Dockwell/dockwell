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
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/certificates"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/publish"
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
	// ServesRedirects says whether a server's agent serves a route's redirect, and
	// whether that's known, as it is while the server is connected (ADR 0070).
	ServesRedirects(server string) (serves, known bool)
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
	// dnsSeen keeps what public DNS said of each name, for Domains.
	dnsSeen dnsCache
	// DNS are the DNS providers an admin connected (ADR 0088); nil has none.
	DNS *dnsproviders.Providers
	// Certificates are the certificates by DNS (ADR 0088); nil has none.
	Certificates *certificates.Manager
	// Providers reads the sign-in providers, and checks an issuer. Nil keeps none.
	Providers *signinproviders.Providers
	// Cloudflare says what a connected Cloudflare zone holds at a name, and the ways
	// in (ADR 0104); nil says nothing of it.
	Cloudflare CloudflareChecker
	// Changes plans route edits for custom certificate upload/clear.
	Changes *changes.Handler
	// Agents reaches connected server agents for certificate renewal and download.
	Agents *agentapi.Agents
	// Auth verifies passkey step-ups when downloading a certificate's private key.
	Auth *auth.Service

	renewMu  sync.Mutex
	renewLog map[string][]time.Time
}

// CloudflareChecker says what a connected Cloudflare zone holds at a name, and the
// ways in.
type CloudflareChecker interface {
	Check(ctx context.Context, ask publish.CheckAsk) (*dockwellv1.CloudflareCheck, error)
	// Kept lists the records made by hand a provider keeps a copy of, with whether
	// each one's name is free now; their words only for an admin.
	Kept(ctx context.Context, provider string, admin bool) ([]*dockwellv1.KeptRecord, error)
	// ZoneRecordsAt is what a connected zone holds at name, from one listing of the
	// zone kept a while; inZone says a connected zone holds it.
	ZoneRecordsAt(ctx context.Context, name string) (recs []dnsproviders.Record, inZone bool, err error)
	// Managed says how many records Dockwell keeps at a provider, and when it last
	// read them, or nil before it has.
	Managed(ctx context.Context, provider string) (records int32, readAt *time.Time, err error)
	// Sync reads now what Dockwell keeps at a provider, joining a read already
	// running; while Cloudflare asks Dockwell to wait, a *publish.SlowDownError.
	Sync(ctx context.Context, provider string) error
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
	read := func(name string) *resource.DomainCheck {
		check := dns.Check(ctx, name)
		// What a name inside some network resolves to is no business of anyone who can
		// only reach this API, so a private answer is shown only when it's the server's
		// own.
		check.Found = slices.DeleteFunc(check.Found, func(s string) bool {
			return len(resource.PublicAddresses([]string{s})) == 0 && !slices.Contains(own, s)
		})
		return check
	}
	// Its companion is read in the same call, beside it, which counts once against
	// the budget.
	companion := Companion(domain)
	var other *resource.DomainCheck
	done := make(chan struct{})
	go func() {
		defer close(done)
		if companion != "" {
			other = read(companion)
		}
	}()
	check := read(domain)
	<-done
	out := &dockwellv1.CheckDomainResponse{Check: resourcepb.DomainCheck(check), Companion: companion}
	// What Cloudflare holds there, where a provider serving the project has the name,
	// read only when it's asked for: a project named, or a way to publish, as New
	// website asks for an admin, checked against every provider's zones as its plan
	// adds the project to the provider. Domains asks for neither, so it reads DNS
	// alone (the review's B-M2). A check that couldn't be made names the provider with
	// no state, beside what DNS says (ADR 0104).
	admin, _ := callerProjects(ctx)
	asked := req.Msg.GetProjectId() != "" || admin && req.Msg.GetPublish() != nil
	if h.Cloudflare != nil && asked {
		cf, err := h.Cloudflare.Check(ctx, publish.CheckAsk{
			Server: srv.GetId(), Project: req.Msg.GetProjectId(), Name: domain, Admin: admin, Way: req.Msg.GetPublish().GetWay(),
			// Check again reads the server's tunnels now (Cache-Control: no-cache).
			Fresh: strings.Contains(req.Header().Get("Cache-Control"), "no-cache"),
		})
		if err != nil {
			h.logger.WarnContext(ctx, "couldn't check a name at Cloudflare", slog.String("domain", domain), slog.String("error", err.Error()))
		}
		out.Cloudflare = cf
	}
	if companion != "" {
		if out.CompanionCheck, err = h.companionCheck(ctx, req.Msg.GetProjectId(), check, other); err != nil {
			return nil, err
		}
		out.CompanionCheck.Check = resourcepb.DomainCheck(other)
	}
	return connect.NewResponse(out), nil
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
	if req.Msg.GetReadDns() && h.servers != nil {
		h.readDNS(ctx, res.Domains)
	}
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
	if err := addPublished(ctx, tx, r.ID, rt, d); err != nil {
		return err
	}
	var users []string
	for _, u := range rt.GetAccess().GetUsers() {
		users = append(users, u.GetName())
	}
	d.Routes = append(d.Routes, &dockwellv1.DomainRoute{
		Id: r.ID, Name: r.Name, Path: rt.GetPath(), Service: rt.GetService(), Port: rt.GetPort(), Url: scheme + route.Where(rt),
		Tls: rt.GetTls(), HostPort: rt.GetHostPort(), PlainForAnyone: rt.GetPlainForAnyone(), Held: rt.GetHeld(), TrustedProxies: rt.GetTrustedProxies(), Redirect: rt.GetRedirect(),
		Allow: rt.GetAccess().GetAllow(), Users: users, SignIn: withoutProvider(rt.GetAccess().GetSignIn()),
		SetupUntil: setupUntil(rt), Publish: rt.GetPublish(), Proof: rt.GetProof(), Unwatched: rt.GetUnwatched(),
		RateLimit: rt.GetRateLimit(), CustomCert: rt.GetCustomCert() != nil,
	})
	placed, err := tx.Placement(ctx, r.ID, r.Revision)
	if err != nil {
		return err
	}
	for _, id := range placed {
		i := slices.IndexFunc(d.Servers, func(s *dockwellv1.DomainServer) bool { return s.GetServerId() == id })
		if i < 0 {
			srv, err := tx.GetServer(ctx, id)
			if err != nil {
				return err
			}
			if srv.RevokedAt != nil {
				continue
			}
			status, connected := h.reports.Domain(id, name)
			d.Servers = append(d.Servers, &dockwellv1.DomainServer{ServerId: id, ServerName: srv.Name, Status: status, Stale: !connected, Ingress: h.reports.Ingress(id)})
			i = len(d.Servers) - 1
		}
		// A redirect on a server whose agent doesn't serve one, as after a downgrade.
		if rt.GetRedirect() != nil {
			if serves, known := h.reports.ServesRedirects(id); known && !serves {
				d.Servers[i].RedirectUnserved = true
			}
		}
	}
	return nil
}

// addPublished says what Dockwell published at Cloudflare for a route, as the drift
// timer last read it: each record, and whether the tunnel's rule that served it is
// still there (ADR 0104).
func addPublished(ctx context.Context, tx *store.Tx, routeID string, rt *resourcev1.Route, d *dockwellv1.Domain) error {
	// A proxied record's content is the origin address public DNS hides, so below an
	// admin a row says only the record's type (ADR 0104, M12, and the review's M7).
	admin, _ := callerProjects(ctx)
	pub := rt.GetPublish()
	if pub == nil || pub.GetWay() == resourcev1.RouteWay_ROUTE_WAY_FRONT {
		return nil
	}
	records, err := tx.CloudflareRecordsOf(ctx, routeID)
	if err != nil {
		return err
	}
	ruleState := store.CloudflarePublished
	if pub.GetWay() == resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL {
		rules, err := tx.CloudflareRulesOf(ctx, pub.GetTunnel())
		if err != nil {
			return err
		}
		for i := range rules {
			if rules[i].State != store.CloudflarePublished && publish.HostMatches(rules[i].Hostname, rt.GetDomain()) {
				ruleState = rules[i].State
			}
		}
	}
	for i := range records {
		rec := &records[i]
		if rec.Type == "pending" {
			continue
		}
		words := rec.Type
		if admin {
			words += " " + rec.Content
		}
		if rec.Proxied {
			words += ", proxied"
		}
		p := &dockwellv1.DomainPublished{Name: rec.Name, Way: pub.GetWay(), Tunnel: pub.GetTunnel(), Record: words, State: dockwellv1.PublishedState_PUBLISHED_STATE_PUBLISHED}
		switch {
		case ruleState == store.CloudflareTunnelDeleted:
			p.State = dockwellv1.PublishedState_PUBLISHED_STATE_TUNNEL_DELETED
		case ruleState == store.CloudflareTunnelShared:
			p.State = dockwellv1.PublishedState_PUBLISHED_STATE_TUNNEL_SHARED
		case ruleState == store.CloudflareTunnelUnproven:
			p.State = dockwellv1.PublishedState_PUBLISHED_STATE_TUNNEL_UNPROVEN
		case ruleState == store.CloudflareUnreadable || rec.State == store.CloudflareUnreadable:
			p.State = dockwellv1.PublishedState_PUBLISHED_STATE_UNREADABLE
		case ruleState != store.CloudflarePublished:
			p.State = dockwellv1.PublishedState_PUBLISHED_STATE_GONE_FROM_TUNNEL
		case rec.State == store.CloudflareGone:
			p.State = dockwellv1.PublishedState_PUBLISHED_STATE_RECORD_GONE
		case rec.State == store.CloudflareChanged:
			p.State = dockwellv1.PublishedState_PUBLISHED_STATE_CHANGED
		}
		if rec.TakenOnAt != nil {
			p.TakenOn = timestamppb.New(*rec.TakenOnAt)
		}
		d.Published = append(d.Published, p)
	}
	return nil
}

// setupUntil is when a held route's setup link stops working, or 0: its end alone,
// never its key's hash (ADR 0105).
func setupUntil(rt *resourcev1.Route) int64 {
	if !rt.GetHeld() || rt.GetSetup().GetKeyHash() == "" {
		return 0
	}
	return rt.GetSetup().GetUntil()
}
