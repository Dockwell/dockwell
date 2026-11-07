// SPDX-License-Identifier: AGPL-3.0-only

// Package websites serves dockwell.v1.WebsiteService: the websites, and planning
// making one, changing what it serves and deleting it, each as a change. A website
// is a project of its own, and what it serves is written from Project.website. See
// docs/adr/0037-websites-as-projects-of-their-own.md.
package websites

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/website"
)

// Handler implements WebsiteService.
type Handler struct {
	changes *changes.Handler
	engine  *revision.Engine
	db      *store.DB
	keeper  *secrets.Keeper
	logger  *slog.Logger
}

var _ dockwellv1connect.WebsiteServiceHandler = (*Handler)(nil)

// New returns a handler that plans through ch and engine, and reads specs from db,
// opening the sealed ones with keeper.
func New(ch *changes.Handler, engine *revision.Engine, db *store.DB, keeper *secrets.Keeper, logger *slog.Logger) *Handler {
	return &Handler{changes: ch, engine: engine, db: db, keeper: keeper, logger: logger}
}

// site is a website as the store has it.
type site struct {
	org     string
	project scope.Project
	spec    *resourcev1.Project
	env     string
	// route is the one that gives it its domain, and routes all of its own.
	route    *store.Resource
	rt       *resourcev1.Route
	routes   []store.Resource
	services []store.Resource
	updated  time.Time
}

func (s *site) info() *dockwellv1.WebsiteInfo {
	out := &dockwellv1.WebsiteInfo{Id: s.project.ID, Name: s.project.Name, Spec: s.spec.GetWebsite(), EnvironmentId: s.env}
	if s.route != nil {
		out.Domain, out.RouteId = s.rt.GetDomain(), s.route.ID
	}
	for i := range s.services {
		if s.services[i].Name == website.WebService {
			out.ServiceId = s.services[i].ID
		}
	}
	if !s.updated.IsZero() {
		out.UpdateTime = timestamppb.New(s.updated)
	}
	return out
}

// ListWebsites returns every website, by domain.
func (h *Handler) ListWebsites(ctx context.Context, _ *connect.Request[dockwellv1.ListWebsitesRequest]) (*connect.Response[dockwellv1.ListWebsitesResponse], error) {
	res := &dockwellv1.ListWebsitesResponse{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		all, err := h.sites(ctx, tx)
		if err != nil {
			return err
		}
		for i := range all {
			info := all[i].info()
			if all[i].route != nil {
				placed, err := tx.Placement(ctx, all[i].route.ID, all[i].route.Revision)
				if err != nil {
					return err
				}
				if len(placed) > 0 {
					srv, err := tx.GetServer(ctx, placed[0])
					if err != nil {
						return err
					}
					info.ServerId, info.ServerName = srv.ID, srv.Name
				}
			}
			res.Websites = append(res.Websites, info)
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	slices.SortFunc(res.Websites, func(a, b *dockwellv1.WebsiteInfo) int {
		return cmp.Or(cmp.Compare(a.GetDomain(), b.GetDomain()), cmp.Compare(a.GetName(), b.GetName()))
	})
	return connect.NewResponse(res), nil
}

// sites reads every project made as a website, with what's in its environment.
func (h *Handler) sites(ctx context.Context, tx *store.Tx) ([]site, error) {
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return nil, err
	}
	var out []site
	for _, p := range tree.Projects {
		r, err := tx.GetResource(ctx, string(scope.ProjectKind), tree.Organization.ID, p.Name)
		if err != nil {
			return nil, err
		}
		spec, err := h.decode(ctx, tx, r)
		if err != nil {
			return nil, err
		}
		project, _ := spec.(*resourcev1.Project)
		if project.GetWebsite() == nil || len(p.Environments) == 0 {
			continue
		}
		s := site{org: tree.Organization.ID, project: p, spec: project, env: p.Environments[0].ID}
		if s.routes, err = tx.ListResources(ctx, s.env, string(route.Kind)); err != nil {
			return nil, err
		}
		if s.services, err = tx.ListResources(ctx, s.env, string(service.Kind)); err != nil {
			return nil, err
		}
		for i := range s.routes {
			if s.routes[i].Name != website.RouteName && s.route != nil {
				continue
			}
			spec, err := h.decode(ctx, tx, s.routes[i])
			if err != nil {
				return nil, err
			}
			s.route, s.rt = &s.routes[i], spec.(*resourcev1.Route)
		}
		live, err := tx.PageResources(ctx, "", s.env, "", 1000)
		if err != nil {
			return nil, err
		}
		for _, l := range live {
			if l.UpdatedAt.After(s.updated) {
				s.updated = l.UpdatedAt
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// find is the website a request names, by its project's name or ID.
func (h *Handler) find(ctx context.Context, name string) (*site, error) {
	var out *site
	err := h.db.View(ctx, func(tx *store.Tx) error {
		all, err := h.sites(ctx, tx)
		if err != nil {
			return err
		}
		for i := range all {
			if all[i].project.Name == name || all[i].project.ID == name || (all[i].rt != nil && all[i].rt.GetDomain() == name) {
				out = &all[i]
			}
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	if out == nil {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no website %s. Run dockwell websites list to see them", name), "not_found")
	}
	return out, nil
}

// CreateWebsite plans a new website as a project of its own.
func (h *Handler) CreateWebsite(ctx context.Context, req *connect.Request[dockwellv1.CreateWebsiteRequest]) (*connect.Response[dockwellv1.CreateWebsiteResponse], error) {
	m := req.Msg
	domain := strings.ToLower(strings.TrimSpace(m.GetDomain()))
	if err := route.CheckDomain(domain); err != nil {
		return nil, authapi.Invalid(err)
	}
	spec := proto.CloneOf(m.GetWebsite())
	if err := website.Check(spec); err != nil {
		return nil, authapi.Invalid(err)
	}
	var made []revision.Edit
	var env, name string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		if name, err = h.freeName(ctx, tx, tree.Organization.ID, website.ProjectName(domain)); err != nil {
			return err
		}
		made, env, err = h.engine.NewProject(ctx, tx, tree.Organization.ID, name, "")
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	made[0].Spec = &resourcev1.Project{DisplayName: domain, Website: spec}
	serving := website.Resources(spec, domain, website.Front{TLS: m.GetTls(), TrustedProxies: m.GetTrustedProxies(), PlainForAnyone: m.GetPlainForAnyone()})
	edits := puts(env, serving, m.GetServers())
	summary := cmp.Or(strings.TrimSpace(m.GetSummary()), "Add the website "+domain)
	c, err := h.changes.Propose(ctx, summary, edits, serversField, changes.Also{Edits: made})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.CreateWebsiteResponse{Change: c}), nil
}

// freeName is a project's name that nothing has, nor had: a project deleted before
// keeps its ID, and would bring back what it had.
func (h *Handler) freeName(ctx context.Context, tx *store.Tx, org, want string) (string, error) {
	name := want
	for n := 2; ; n++ {
		_, err := tx.GetResource(ctx, string(scope.ProjectKind), org, name)
		if errors.Is(err, store.ErrNotFound) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
		name = want + "-" + strconv.Itoa(n)
	}
}

// UpdateWebsite plans changing what a website serves, keeping its domain.
func (h *Handler) UpdateWebsite(ctx context.Context, req *connect.Request[dockwellv1.UpdateWebsiteRequest]) (*connect.Response[dockwellv1.UpdateWebsiteResponse], error) {
	m := req.Msg
	spec := proto.CloneOf(m.GetWebsite())
	if err := website.Check(spec); err != nil {
		return nil, authapi.Invalid(err)
	}
	s, err := h.find(ctx, m.GetId())
	if err != nil {
		return nil, err
	}
	if s.route == nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s has no route left to give it a domain, so there's nowhere to serve it. Delete the website, and add it again", s.project.Name), "no_route")
	}
	domain := s.rt.GetDomain()
	serving := website.Resources(spec, domain, website.FrontOf(s.rt))
	edits := []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: string(scope.ProjectKind), Scope: s.org, Name: s.project.Name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: s.spec.GetDisplayName(), Catalog: s.spec.GetCatalog(), Website: spec}}},
	}}
	// It stays where it runs, which an edit with no servers keeps.
	edits = append(edits, puts(s.env, serving, nil)...)
	for _, r := range s.services {
		if serving.Services[r.Name] == nil {
			edits = append(edits, &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(service.Kind), Scope: s.env, Name: r.Name}, Delete: true})
		}
	}
	summary := cmp.Or(strings.TrimSpace(m.GetSummary()), "Change what "+domain+" serves")
	c, err := h.changes.Propose(ctx, summary, edits, serversField, changes.Also{
		IfNothing: fault.New(revision.ErrInvalid, "%s serves that already, so there's nothing to change", domain),
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.UpdateWebsiteResponse{Change: c}), nil
}

// DeleteWebsite plans deleting a website: its routes, its services and its project.
func (h *Handler) DeleteWebsite(ctx context.Context, req *connect.Request[dockwellv1.DeleteWebsiteRequest]) (*connect.Response[dockwellv1.DeleteWebsiteResponse], error) {
	s, err := h.find(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	// Deletions run in reverse, so this order has the routes go first, then what
	// they sent to, then the project, with its environment.
	edits := []*dockwellv1.Edit{{Resource: &dockwellv1.ResourceRef{Kind: string(scope.ProjectKind), Scope: s.org, Name: s.project.Name}, Delete: true}}
	for _, r := range s.services {
		edits = append(edits, &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(service.Kind), Scope: s.env, Name: r.Name}, Delete: true})
	}
	for _, r := range s.routes {
		edits = append(edits, &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: s.env, Name: r.Name}, Delete: true})
	}
	name := cmp.Or(s.spec.GetDisplayName(), s.project.Name)
	summary := cmp.Or(strings.TrimSpace(req.Msg.GetSummary()), "Delete the website "+name)
	c, err := h.changes.Propose(ctx, summary, edits, serversField, changes.Also{})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.DeleteWebsiteResponse{Change: c}), nil
}

// puts are the edits that make an environment hold what serves a website: its
// services first, as the route sends to them.
func puts(env string, serving website.Serving, servers []string) []*dockwellv1.Edit {
	var out []*dockwellv1.Edit
	for _, name := range slices.Sorted(maps.Keys(serving.Services)) {
		out = append(out, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(service.Kind), Scope: env, Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: serving.Services[name]}},
			Servers:  servers,
		})
	}
	return append(out, &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: website.RouteName},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: serving.Route}},
		Servers:  servers,
	})
}

// serversField is the request's field for a choice of server.
func serversField(choice *scope.ChoiceError, _ string) string {
	if choice.Level == scope.ServerLevel {
		return "servers"
	}
	return ""
}

func (h *Handler) decode(ctx context.Context, tx *store.Tx, r store.Resource) (proto.Message, error) {
	return h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
}

func (h *Handler) internal(ctx context.Context, err error) error {
	var said *connect.Error
	if errors.As(err, &said) {
		return said
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, "website call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't complete the request; try again, and check its logs if it keeps failing"))
}
