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
	"log/slog"
	"slices"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
)

// Reports is what each server's agent last said of its routes' domains.
type Reports interface {
	Domain(server, domain string) (status *dockwellv1.DomainStatus, connected bool)
	Ingress(server string) *dockwellv1.IngressStatus
}

// Handler implements DomainService.
type Handler struct {
	kinds   *resource.Registry
	db      *store.DB
	reports Reports
	logger  *slog.Logger
}

var _ dockwellv1connect.DomainServiceHandler = (*Handler)(nil)

// New returns a handler that reads routes from db, with kinds, and what their
// servers say from reports.
func New(kinds *resource.Registry, db *store.DB, reports Reports, logger *slog.Logger) *Handler {
	return &Handler{kinds: kinds, db: db, reports: reports, logger: logger}
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
	spec, err := h.kinds.Decode(route.Kind, r.Spec)
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
	d.Routes = append(d.Routes, &dockwellv1.DomainRoute{
		Id: r.ID, Name: r.Name, Path: rt.GetPath(), Service: rt.GetService(), Port: rt.GetPort(), Url: scheme + route.Where(rt),
		Tls: rt.GetTls(), HostPort: rt.GetHostPort(), PlainForAnyone: rt.GetPlainForAnyone(),
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
