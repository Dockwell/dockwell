// SPDX-License-Identifier: AGPL-3.0-only

// Package catalogapi serves dockwell.v1.CatalogService: the apps Dockwell can
// install, and planning one as a project of its own. See
// docs/adr/0029-a-catalog-format-fed-from-other-catalogs.md.
package catalogapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"connectrpc.com/connect"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/composeapi"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Handler implements CatalogService.
type Handler struct {
	imports *composeapi.Handler
	db      *store.DB
	keeper  *secrets.Keeper
	commit  string
	logger  *slog.Logger
}

var _ dockwellv1connect.CatalogServiceHandler = (*Handler)(nil)

// New returns a handler that plans through imports, and reads specs from db,
// opening the sealed ones with keeper. commit is the build's.
func New(imports *composeapi.Handler, db *store.DB, keeper *secrets.Keeper, commit string, logger *slog.Logger) *Handler {
	return &Handler{imports: imports, db: db, keeper: keeper, commit: commit, logger: logger}
}

// ListCatalogEntries returns the catalog.
func (h *Handler) ListCatalogEntries(ctx context.Context, _ *connect.Request[dockwellv1.ListCatalogEntriesRequest]) (*connect.Response[dockwellv1.ListCatalogEntriesResponse], error) {
	c, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ListCatalogEntriesResponse{Entries: c.All()}), nil
}

// GetCatalogEntry returns one entry, with what installing it would use.
func (h *Handler) GetCatalogEntry(ctx context.Context, req *connect.Request[dockwellv1.GetCatalogEntryRequest]) (*connect.Response[dockwellv1.GetCatalogEntryResponse], error) {
	entry, err := h.entry(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	free, err := h.whatsFree(ctx, entry)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.GetCatalogEntryResponse{Entry: entry, Project: free.project, Values: free.values}), nil
}

// InstallCatalogEntry plans an entry as a project of its own.
func (h *Handler) InstallCatalogEntry(ctx context.Context, req *connect.Request[dockwellv1.InstallCatalogEntryRequest]) (*connect.Response[dockwellv1.InstallCatalogEntryResponse], error) {
	m := req.Msg
	entry, err := h.entry(ctx, m.GetId())
	if err != nil {
		return nil, err
	}
	free, err := h.whatsFree(ctx, entry)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	filled, err := catalog.Fill(entry, m.GetValues(), free.values)
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	project := cmp.Or(m.GetProject(), free.project)
	if err := (scope.ProjectType{}).CheckName(project); err != nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	change, err := h.imports.PlanInstall(ctx, composeapi.Install{
		Project:    project,
		Summary:    fmt.Sprintf("Install %s %s as %s", entry.GetName(), entry.GetVersion(), project),
		Name:       entry.GetName(),
		File:       []byte(entry.GetCompose()),
		Variables:  filled.Variables,
		Secrets:    filled.Secrets,
		Servers:    m.GetServers(),
		FirstSteps: entry.GetFirstSteps(),
		Pin:        pinTo(entry, filled),
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.InstallCatalogEntryResponse{Change: change}), nil
}

// ListCatalogInstalls says what's installed, and what each can move to.
func (h *Handler) ListCatalogInstalls(ctx context.Context, _ *connect.Request[dockwellv1.ListCatalogInstallsRequest]) (*connect.Response[dockwellv1.ListCatalogInstallsResponse], error) {
	c, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	var installs []*dockwellv1.CatalogInstall
	err = h.db.View(ctx, func(tx *store.Tx) (err error) {
		installs, err = h.installs(ctx, tx, c)
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.ListCatalogInstallsResponse{Installs: installs}), nil
}

// installs reads each project's pin, by project, and leaves out a project whose
// services have all gone, which isn't installed any more, though it stays.
func (h *Handler) installs(ctx context.Context, tx *store.Tx, c *catalog.Catalog) ([]*dockwellv1.CatalogInstall, error) {
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return nil, err
	}
	var out []*dockwellv1.CatalogInstall
	for _, p := range tree.Projects {
		pin, err := h.pinOf(ctx, tx, tree.Organization.ID, p.Name)
		if err != nil {
			return nil, err
		}
		if pin == nil {
			continue
		}
		install := &dockwellv1.CatalogInstall{ProjectId: p.ID, Project: p.Name, Pin: pin}
		entry, known := c.Entry(pin.GetEntry())
		if known {
			install.Name, install.FirstSteps = entry.GetName(), entry.GetFirstSteps()
			if entry.GetRelease() > pin.GetRelease() {
				install.Upgrade = &dockwellv1.CatalogUpgrade{Version: entry.GetVersion(), Release: entry.GetRelease()}
			}
		}
		for _, env := range p.Environments {
			list, err := tx.ListResources(ctx, env.ID, string(service.Kind))
			if err != nil {
				return nil, err
			}
			for i := range list {
				install.EnvironmentId = cmp.Or(install.EnvironmentId, env.ID)
				install.WebServiceId = cmp.Or(install.WebServiceId, list[i].ID)
				if known && list[i].Name == entry.GetWeb().GetService() {
					install.WebServiceId = list[i].ID
				}
			}
		}
		if install.GetEnvironmentId() != "" {
			out = append(out, install)
		}
	}
	slices.SortFunc(out, func(a, b *dockwellv1.CatalogInstall) int { return cmp.Compare(a.GetProject(), b.GetProject()) })
	return out, nil
}

// UpgradeCatalogInstall plans an install's newest release.
func (h *Handler) UpgradeCatalogInstall(ctx context.Context, req *connect.Request[dockwellv1.UpgradeCatalogInstallRequest]) (*connect.Response[dockwellv1.UpgradeCatalogInstallResponse], error) {
	m := req.Msg
	c, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	var install *dockwellv1.CatalogInstall
	var display string
	held := map[string]bool{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		all, err := h.installs(ctx, tx, c)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(all, func(in *dockwellv1.CatalogInstall) bool {
			return in.GetProject() == m.GetId() || in.GetProjectId() == m.GetId()
		})
		if i < 0 {
			return nil
		}
		install = all[i]
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			if p.ID == install.GetProjectId() {
				display = p.DisplayName
			}
		}
		list, err := tx.ListResources(ctx, install.GetEnvironmentId(), string(variable.SecretKind))
		for i := range list {
			held[list[i].Name] = true
		}
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	if install == nil {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("nothing installed from the catalog is called %s. Run dockwell catalog installs to see what is", m.GetId()), "not_found")
	}
	pin := install.GetPin()
	entry, ok := c.Entry(pin.GetEntry())
	switch {
	case !ok:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("the catalog no longer has %s, so %s stays on %s", pin.GetEntry(), install.GetProject(), pin.GetVersion()), "not_in_catalog")
	case entry.GetRelease() <= pin.GetRelease():
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s is up to date, on %s %s", install.GetProject(), entry.GetName(), pin.GetVersion()), "up_to_date")
	}
	free, err := h.whatsFree(ctx, entry)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	filled, err := catalog.Carry(entry, pin, m.GetValues(), free.values, held)
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	change, err := h.imports.PlanUpgrade(ctx, composeapi.Upgrade{
		Project:     install.GetProjectId(),
		Environment: install.GetEnvironmentId(),
		Summary:     fmt.Sprintf("Upgrade %s from %s to %s", install.GetProject(), pin.GetVersion(), entry.GetVersion()),
		Name:        display,
		File:        []byte(entry.GetCompose()),
		Variables:   filled.Variables,
		Secrets:     filled.Secrets,
		Stands:      filled.Stands(),
		Pin:         pinTo(entry, filled),
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.UpgradeCatalogInstallResponse{Change: change}), nil
}

// pinTo is what an install of an entry is pinned to.
func pinTo(e *catalogv1.Entry, filled *catalog.Filled) *resourcev1.CatalogPin {
	return &resourcev1.CatalogPin{
		Entry:      e.GetId(),
		Version:    e.GetVersion(),
		Release:    e.GetRelease(),
		Repository: e.GetSource().GetRepository(),
		Commit:     e.GetSource().GetCommit(),
		Values:     filled.Plain,
	}
}

// pinOf reads what a project is pinned to. Nil for one that isn't an install. A
// project's spec is never sealed, as it has nothing sensitive.
func (h *Handler) pinOf(ctx context.Context, tx *store.Tx, org, project string) (*resourcev1.CatalogPin, error) {
	r, err := tx.GetResource(ctx, string(scope.ProjectKind), org, project)
	if err != nil {
		return nil, err
	}
	spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return nil, err
	}
	p, _ := spec.(*resourcev1.Project)
	return p.GetCatalog(), nil
}

// free is what an install can take without getting in anything's way.
type free struct {
	project string
	values  map[string]string
}

// whatsFree finds a name for an entry's project, and ports nothing Dockwell runs
// has taken, on any server, so a default needn't depend on where it lands. What
// else is on a server only its agent knows, so the plan refuses that by name.
func (h *Handler) whatsFree(ctx context.Context, e *catalogv1.Entry) (free, error) {
	taken := map[uint32]bool{}
	var out free
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			for _, env := range p.Environments {
				if err := h.addPorts(ctx, tx, env.ID, taken); err != nil {
					return err
				}
			}
		}
		// A name used before isn't free, as PlanInstall says of it.
		out.project = e.GetId()
		for n := 2; ; n++ {
			_, err := tx.GetResource(ctx, string(scope.ProjectKind), tree.Organization.ID, out.project)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			out.project = e.GetId() + "-" + strconv.Itoa(n)
		}
	})
	out.values = catalog.Suggest(e, taken)
	return out, err
}

func (h *Handler) addPorts(ctx context.Context, tx *store.Tx, env string, taken map[uint32]bool) error {
	list, err := tx.ListResources(ctx, env, string(service.Kind))
	if err != nil {
		return err
	}
	for _, r := range list {
		spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		s, _ := spec.(*resourcev1.Service)
		for _, p := range s.GetApp().GetPorts() {
			taken[p.GetHostPort()] = true
		}
	}
	return nil
}

func (h *Handler) catalog(ctx context.Context) (*catalog.Catalog, error) {
	c, err := catalog.Load(h.commit)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return c, nil
}

func (h *Handler) entry(ctx context.Context, id string) (*catalogv1.Entry, error) {
	c, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	e, ok := c.Entry(id)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("the catalog has no app %s. Run dockwell catalog list to see what it has", id), "not_found")
	}
	return e, nil
}

func (h *Handler) internal(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, "catalog call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't complete the request; try again, and check its logs if it keeps failing"))
}
