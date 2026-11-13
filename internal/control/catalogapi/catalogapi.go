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
	"maps"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/catalog/folder"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/composeapi"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Handler implements CatalogService.
type Handler struct {
	imports *composeapi.Handler
	changes *changes.Handler
	db      *store.DB
	keeper  *secrets.Keeper
	sources *Sources
	commit  string
	logger  *slog.Logger
	// digests pins the images of an entry converted on the control plane when a
	// plan installs it. Nil can't plan installing one.
	digests Digests
	// follower moves the sources that follow a branch, once the control plane has
	// one (Follow).
	follower *Follower
	// shareable lists the database servers an app may share (ADR 0084). Nil offers
	// none.
	shareable Shareable
}

var _ dockwellv1connect.CatalogServiceHandler = (*Handler)(nil)

// New returns a handler that plans installs through imports and other changes
// through ch, and reads specs from db, opening the sealed ones with keeper. It reads
// catalog sources through sources, and nil reads them with nothing kept between
// runs. commit is the build's.
func New(imports *composeapi.Handler, ch *changes.Handler, db *store.DB, keeper *secrets.Keeper, sources *Sources, commit string, logger *slog.Logger) *Handler {
	if sources == nil {
		sources = &Sources{}
	}
	return &Handler{imports: imports, changes: ch, db: db, keeper: keeper, sources: sources, commit: commit, logger: logger}
}

// ListCatalogEntries returns the catalog.
func (h *Handler) ListCatalogEntries(ctx context.Context, _ *connect.Request[dockwellv1.ListCatalogEntriesRequest]) (*connect.Response[dockwellv1.ListCatalogEntriesResponse], error) {
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	also := s.alternatives()
	for _, a := range also {
		a.Entry = listed(a.GetEntry())
	}
	entries := make([]*catalogv1.Entry, 0, len(s.catalog.All()))
	for _, e := range s.catalog.All() {
		entries = append(entries, listed(e))
	}
	return connect.NewResponse(&dockwellv1.ListCatalogEntriesResponse{Entries: entries, Also: also}), nil
}

// listed is an entry as the catalog lists it: without its README, and with only the
// summary of what it says in other languages, which GetCatalogEntry has, as a
// store's READMEs alone come to a megabyte or more (ADR 0083). The catalog's own
// entry is left as it is.
func listed(e *catalogv1.Entry) *catalogv1.Entry {
	if e.GetReadme() == "" && len(e.GetTranslations()) == 0 {
		return e
	}
	out := proto.CloneOf(e)
	out.Readme = ""
	for tag, words := range out.GetTranslations() {
		if words.GetSummary() == "" {
			delete(out.Translations, tag)
			continue
		}
		words.Description, words.Readme = "", ""
	}
	return out
}

// GetCatalogEntry returns one entry, with what installing it would use.
func (h *Handler) GetCatalogEntry(ctx context.Context, req *connect.Request[dockwellv1.GetCatalogEntryRequest]) (*connect.Response[dockwellv1.GetCatalogEntryResponse], error) {
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	entry, _, err := s.entryIn(req.Msg.GetId(), req.Msg.GetCatalog())
	if err != nil {
		return nil, err
	}
	free, err := h.whatsFree(ctx, entry, req.Msg.GetProject())
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	// What it installs is something to read before the plan, which says it again, so
	// an entry this can't read still opens, and says nothing of its parts.
	parts, err := partsOf(ctx, catalog.Standing(entry), free.values)
	if err != nil {
		h.logger.WarnContext(ctx, "couldn't say what a catalog entry installs", slog.String("entry", entry.GetId()), slog.String("error", err.Error()))
		parts = nil
	}
	needs := needOptions(entry, catalog.WaysWith(entry, s.lookup(), h.candidates(ctx), ""))
	seen, err := h.seenProjects(ctx)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	if seen != nil {
		// Who else shares a server is named only where the person holds that project.
		for _, o := range needs {
			o.SharedWith = slices.DeleteFunc(o.SharedWith, func(p string) bool { return !seen[p] })
		}
	}
	return connect.NewResponse(&dockwellv1.GetCatalogEntryResponse{
		Entry: entry, Project: free.project, Values: free.values, Parts: parts, Catalogs: s.catalogsOf(entry.GetId()),
		Needs: needs,
	}), nil
}

// partsOf reads what an entry's Compose file runs, from the values an install starts
// with and a stand-in for what a person types or Dockwell generates, as nothing it
// says depends on those but a server path, which names the input it comes from. The
// service a person opens comes first, then the other apps, then the databases they
// keep their data in.
func partsOf(ctx context.Context, e *catalogv1.Entry, values map[string]string) ([]*dockwellv1.CatalogPart, error) {
	variables := maps.Clone(values)
	if variables == nil {
		variables = map[string]string{}
	}
	for _, in := range e.GetInputs() {
		if variables[in.GetName()] == "" {
			variables[in.GetName()] = folder.StandIn(in)
		}
	}
	stack, err := compose.Read(ctx, compose.Input{File: []byte(e.GetCompose()), Variables: variables})
	if err != nil {
		return nil, fmt.Errorf("reading the Compose file of the catalog entry %s: %w", e.GetId(), err)
	}
	out := make([]*dockwellv1.CatalogPart, 0, len(stack.Services))
	for _, s := range stack.Services {
		app := s.Spec.GetApp()
		part := &dockwellv1.CatalogPart{
			Service:    s.Name,
			Image:      tagged(app.GetImage()),
			Database:   slices.Contains(e.GetDatabases(), s.Name) || len(e.GetDatabases()) == 0 && isDatabase(app.GetImage()),
			Opens:      s.Name == e.GetWeb().GetService(),
			HostAccess: resourcepb.HostAccess(container.ServiceAccess(app)),
			Memory:     app.GetResources().GetMemory(),
			Cpus:       app.GetResources().GetCpus(),
			Restart:    app.GetRestart(),
		}
		if part.GetDatabase() {
			part.Engine = catalog.EngineOfImage(app.GetImage())
		}
		for _, m := range app.GetMounts() {
			if v := m.GetVolume(); v != "" && !m.GetExternal() {
				part.Volumes = append(part.Volumes, v)
			}
		}
		for _, a := range part.GetHostAccess() {
			for _, in := range e.GetInputs() {
				if a.GetPath() != "" && !catalog.Secret(in) && variables[in.GetName()] == a.GetPath() {
					if part.PathInputs == nil {
						part.PathInputs = map[string]string{}
					}
					part.PathInputs[a.GetPath()] = in.GetName()
				}
			}
		}
		out = append(out, part)
	}
	rank := func(p *dockwellv1.CatalogPart) int {
		switch {
		case p.GetOpens():
			return 0
		case !p.GetDatabase():
			return 1
		}
		return 2
	}
	slices.SortStableFunc(out, func(a, b *dockwellv1.CatalogPart) int { return cmp.Compare(rank(a), rank(b)) })
	return out, nil
}

// tagged is an image by repository and tag, without the digest that pins it.
func tagged(image string) string {
	repo, _, _ := strings.Cut(image, "@")
	return repo
}

// databases are the images of databases an app keeps its data in, by repository, for
// an entry that marks none of its services as one.
var databases = []string{"postgres", "mysql", "mariadb", "redis", "clickhouse/clickhouse-server"}

// isDatabase reports an image of a database, by its repository: one of these, or
// any engine Dockwell knows, such as MongoDB or Valkey.
func isDatabase(image string) bool {
	if catalog.EngineOfImage(image) != catalogv1.Engine_ENGINE_UNSPECIFIED {
		return true
	}
	repo := tagged(image)
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	repo = strings.TrimPrefix(strings.TrimPrefix(repo, "docker.io/"), "library/")
	return slices.Contains(databases, repo)
}

// InstallCatalogEntry plans an entry as a project of its own.
func (h *Handler) InstallCatalogEntry(ctx context.Context, req *connect.Request[dockwellv1.InstallCatalogEntryRequest]) (*connect.Response[dockwellv1.InstallCatalogEntryResponse], error) {
	m := req.Msg
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	entry, from, err := s.entryIn(m.GetId(), m.GetCatalog())
	if err != nil {
		return nil, err
	}
	// An entry another store's commit was converted into names its images by tag,
	// which the plan pins, through the agent of the server it goes on.
	var onServer string
	if len(m.GetServers()) > 0 {
		onServer = m.GetServers()[0]
	}
	entry, pinNotes, err := h.pinImages(ctx, entry, onServer, nil)
	if err != nil {
		return nil, err
	}
	// What meets each need, as asked, or each need's first way (ADR 0084).
	var candidates []catalog.Candidate
	if asksToShare(m.GetNeeds()) {
		candidates = h.candidates(ctx)
	}
	named, err := h.whatsFree(ctx, entry, "")
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	needed, needNotes, err := h.meetNeeds(ctx, s, entry, m.GetValues(), m.GetNeeds(), nil, candidates, cmp.Or(m.GetProject(), named.project), onServer)
	if err != nil {
		return nil, err
	}
	pinNotes = append(pinNotes, needNotes...)
	free, err := h.whatsFree(ctx, needed.Entry, "")
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	// What it installs is the entry without the companions left out.
	choice, err := catalog.Chosen(needed.Entry, m.GetCompanions())
	if err == nil {
		err = catalog.LeftOut(needed.Entry, choice, m.GetValues())
	}
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	chosen, err := catalog.Choose(needed.Entry, choice)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	filled, err := catalog.Fill(chosen, m.GetValues(), free.values)
	if err == nil {
		err = filled.Link(needed.Plain)
	}
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	written := writtenEdits(needed, filled)
	pin := pinTo(entry, from, filled, choice)
	pin.Needs = needed.Pinned
	shared, sharedOn, err := h.sharedEdits(ctx, needed, filled)
	if err != nil {
		return nil, err
	}
	servers := m.GetServers()
	if len(servers) == 0 {
		// An app that shares a server goes where it is.
		servers = sharedOn
	}
	project := cmp.Or(m.GetProject(), free.project)
	if err := (scope.ProjectType{}).CheckName(project); err != nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	change, err := h.imports.PlanInstall(ctx, composeapi.Install{
		Project:    project,
		Summary:    fmt.Sprintf("Install %s %s as %s", entry.GetName(), entry.GetVersion(), project),
		Name:       entry.GetName(),
		File:       []byte(chosen.GetCompose()),
		Variables:  filled.Variables,
		Secrets:    filled.Secrets,
		Stands:     filled.Stands(),
		Servers:    servers,
		FirstSteps: catalog.FirstWords(entry, choice),
		Pin:        pin,
		Limits:     limitsOf(m),
		Notes:      pinNotes,
		Needs:      composeapi.Needs{Variables: filled.Linked, Stands: filled.LinkStands, Links: needed.Links, Extra: slices.Concat(shared.Extra, userEdits(needed), written), Joins: shared.Joins},
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.InstallCatalogEntryResponse{Change: change}), nil
}

// limitsOf is what an install sets for each service, by its name.
func limitsOf(m *dockwellv1.InstallCatalogEntryRequest) map[string]composeapi.Limits {
	out := map[string]composeapi.Limits{}
	for name, memory := range m.GetMemory() {
		l := out[name]
		l.Memory = memory
		out[name] = l
	}
	for name, cpus := range m.GetCpus() {
		l := out[name]
		l.CPUs = cpus
		out[name] = l
	}
	for name, restart := range m.GetRestart() {
		l := out[name]
		l.Restart = restart
		out[name] = l
	}
	return out
}

// ListCatalogInstalls says what's installed, and what each can move to.
func (h *Handler) ListCatalogInstalls(ctx context.Context, _ *connect.Request[dockwellv1.ListCatalogInstallsRequest]) (*connect.Response[dockwellv1.ListCatalogInstallsResponse], error) {
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	var installs []*dockwellv1.CatalogInstall
	err = h.db.View(ctx, func(tx *store.Tx) (err error) {
		installs, err = h.installs(ctx, tx, s)
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.ListCatalogInstallsResponse{Installs: installs}), nil
}

// InstallUpgrade is what one catalog install can move to.
type InstallUpgrade struct {
	// FromVersion is the app's version it runs, and Release its entry's release.
	FromVersion string
	Release     uint32
	// ToVersion and ToRelease are its entry's newer release, empty and 0 when
	// it's up to date or its catalog no longer has its entry.
	ToVersion string
	ToRelease uint32
	// AppSource is the app's source repository, as its entry says, for its
	// release notes.
	AppSource string
}

// InstallUpgrades says, of each catalog install, by project ID, what it can
// move to, as ListCatalogInstalls does, for knowing an app's updates (ADR 0079).
func (h *Handler) InstallUpgrades(ctx context.Context) (map[string]InstallUpgrade, error) {
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]InstallUpgrade{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		installs, err := h.installs(ctx, tx, s)
		if err != nil {
			return err
		}
		for _, in := range installs {
			u := InstallUpgrade{FromVersion: in.GetPin().GetVersion(), Release: in.GetPin().GetRelease(), ToVersion: in.GetUpgrade().GetVersion(), ToRelease: in.GetUpgrade().GetRelease()}
			if entry, known := s.entryFor(in.GetPin()); known {
				u.AppSource = entry.GetAppSource()
			}
			out[in.GetProjectId()] = u
		}
		return nil
	})
	return out, err
}

// installs reads each project's pin, by project, and leaves out a project whose
// services have all gone, which isn't installed any more, though it stays. An
// install's entry is the catalog's only where it comes from the catalog the install
// came from: an entry of its ID from elsewhere is another catalog's app, with no
// upgrade for it.
func (h *Handler) installs(ctx context.Context, tx *store.Tx, s sourced) ([]*dockwellv1.CatalogInstall, error) {
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
		entry, known := s.entryFor(pin)
		if known {
			install.Companions = catalog.Resolve(entry, pin)
			install.Name, install.FirstSteps = entry.GetName(), catalog.FirstWords(entry, install.GetCompanions())
			if catalog.Upgrades(entry, pin) && len(pin.GetFile()) > 0 {
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
	change, err := h.upgrade(ctx, req.Msg, "", false)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.UpgradeCatalogInstallResponse{Change: change}), nil
}

// IsInstall says a project, by its ID, is an install from the catalog, for an update
// of one of its services, which upgrades the install (ADR 0080).
func (h *Handler) IsInstall(ctx context.Context, project string) (bool, error) {
	s, err := h.catalog(ctx)
	if err != nil {
		return false, err
	}
	var is bool
	err = h.db.View(ctx, func(tx *store.Tx) error {
		all, err := h.installs(ctx, tx, s)
		is = slices.ContainsFunc(all, func(in *dockwellv1.CatalogInstall) bool { return in.GetProjectId() == project })
		return err
	})
	if err != nil {
		return false, h.internal(ctx, err)
	}
	return is, nil
}

// UpgradeInstall plans upgrading the install a project is, by its ID, as
// UpgradeCatalogInstall does, for an update of one of its services (ADR 0080).
// Empty summary says what it upgrades. rebuild takes only the new build of each
// image it runs by a tag that moves, at the release it runs (ADR 0085).
func (h *Handler) UpgradeInstall(ctx context.Context, project string, skipBackup bool, summary string, rebuild bool) (*dockwellv1.Change, error) {
	return h.upgrade(ctx, &dockwellv1.UpgradeCatalogInstallRequest{Id: project, SkipBackup: skipBackup}, summary, rebuild)
}

// upgrade plans an install's upgrade, with summary for the timeline, or what it
// upgrades, from and to, where that's empty. rebuildOnly takes a new build alone.
func (h *Handler) upgrade(ctx context.Context, m *dockwellv1.UpgradeCatalogInstallRequest, summary string, rebuildOnly bool) (*dockwellv1.Change, error) {
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	found, err := h.find(ctx, s, m.GetId())
	if err != nil {
		return nil, err
	}
	install := found.install
	pin := install.GetPin()
	entry, ok := s.entryFor(pin)
	// A tag that moves, such as latest, names no version, so a new build of it is the
	// only way such an app moves on: with no newer release, it's what an upgrade takes
	// (ADR 0085).
	newer := ok && catalog.Upgrades(entry, pin)
	moving := catalog.MovingImages(entry.GetCompose())
	rebuild := ok && !newer && len(moving) > 0 && catalog.Unpin(string(pin.GetFile())) == catalog.Unpin(entry.GetCompose())
	switch {
	case !ok && s.origins[pin.GetEntry()] != (origin{}):
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s came from %s, which has no %s now, so it stays on %s. The %s in the catalog is %s's, another app: install it under another name to move to it", install.GetProject(), originOf(pin).words(), pin.GetEntry(), pin.GetVersion(), pin.GetEntry(), s.origins[pin.GetEntry()].words()), "not_in_catalog")
	case !ok:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s came from %s, which has no %s now, so it stays on %s", install.GetProject(), originOf(pin).words(), pin.GetEntry(), pin.GetVersion()), "not_in_catalog")
	case !newer && !rebuild:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s is up to date, on %s %s", install.GetProject(), entry.GetName(), pin.GetVersion()), "up_to_date")
	case len(pin.GetFile()) == 0:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s was installed before Dockwell kept what its entry's file gave, so an upgrade can't tell that from what you changed, and it stays on %s. To move on, install the new version under another name and bring its data across", install.GetProject(), pin.GetVersion()), "no_record")
	case rebuildOnly && newer:
		// A new build was offered at the release it runs, and its catalog has moved
		// on since: taking the build mustn't take the release, which a person may
		// have skipped, so the release stays an update of its own.
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s's catalog has moved on to %s %s, release %d, so a new build alone would leave that out. Take the release as its own update, which brings the newest builds too", install.GetProject(), entry.GetName(), entry.GetVersion(), entry.GetRelease()), "newer_release")
	case rebuildOnly && !rebuild:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s runs no image by a tag that moves at release %d, so it has no new build to take alone", install.GetProject(), pin.GetRelease()), "no_rebuild")
	}
	// An image whose tag the release keeps stays at the build the install runs, and
	// one it moves is pinned to the build its new tag points at now. A tag that moves
	// by itself is read again, for the build it points at now.
	had := catalog.PinnedDigests(string(pin.GetFile()))
	for _, image := range moving {
		delete(had, image)
	}
	entry, pinNotes, err := h.pinImages(ctx, entry, h.serverOf(ctx, install.GetEnvironmentId()), had)
	if err != nil {
		return nil, err
	}
	if rebuild {
		if maps.Equal(catalog.PinnedDigests(entry.GetCompose()), catalog.PinnedDigests(string(pin.GetFile()))) {
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s is up to date, on the newest build of %s", install.GetProject(), strings.Join(moving, " and ")), "up_to_date")
		}
		summary = cmp.Or(summary, fmt.Sprintf("Update %s to the newest build of %s", install.GetProject(), strings.Join(moving, " and ")))
	}
	// Its needs stay met as they are, and one the release adds takes its first way.
	server := h.serverOf(ctx, install.GetEnvironmentId())
	needed, needNotes, err := h.meetNeeds(ctx, s, entry, m.GetValues(), nil, pin.GetNeeds(), nil, install.GetProject(), server)
	if err != nil {
		return nil, err
	}
	pinNotes = append(pinNotes, needNotes...)
	free, err := h.whatsFree(ctx, needed.Entry, "")
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	// The companions it runs stay as they are, and one the release adds is as its entry says.
	choice := catalog.Resolve(needed.Entry, pin)
	chosen, err := catalog.Choose(needed.Entry, choice)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	filled, err := catalog.Carry(chosen, pin, m.GetValues(), free.values, found.held)
	if err == nil {
		err = filled.Link(needed.Plain)
	}
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	if err := h.keepEdited(ctx, install.GetEnvironmentId(), pin, filled); err != nil {
		return nil, h.internal(ctx, err)
	}
	old, err := ranFile(pin)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	oldVariables, oldStands, err := catalog.Given(old)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	oldLinked, err := catalog.StandFor(oldVariables, needed.Plain)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	written := writtenEdits(needed, filled)
	newPin := pinTo(entry, s.origins[entry.GetId()], filled, choice)
	newPin.Needs = needed.Pinned
	return h.imports.PlanUpgrade(ctx, composeapi.Upgrade{
		Project:     install.GetProjectId(),
		Environment: install.GetEnvironmentId(),
		Summary:     cmp.Or(summary, fmt.Sprintf("Upgrade %s from %s to %s", install.GetProject(), pin.GetVersion(), entry.GetVersion())),
		FromRelease: pin.GetVersion(),
		Release:     entry.GetVersion(),
		Name:        found.display,
		File:        []byte(chosen.GetCompose()),
		Variables:   filled.Variables,
		Secrets:     filled.Secrets,
		Stands:      filled.Stands(),
		Pin:         newPin,
		SkipBackup:  m.GetSkipBackup(),

		Old:          old.GetFile(),
		OldVariables: oldVariables,
		OldStands:    oldStands,
		Notes:        pinNotes,
		Needs:        composeapi.Needs{Variables: filled.Linked, Stands: filled.LinkStands, OldStands: oldLinked, Links: needed.Links, Extra: slices.Concat(userEdits(needed), written), Joins: linkJoins(needed)},
	})
}

// SetCatalogCompanion plans adding one of an install's companions, or taking it away,
// at the release it's on: a merge of its file as it runs now and as it would, as an
// upgrade is, with the pin's choice in the same change.
func (h *Handler) SetCatalogCompanion(ctx context.Context, req *connect.Request[dockwellv1.SetCatalogCompanionRequest]) (*connect.Response[dockwellv1.SetCatalogCompanionResponse], error) {
	m := req.Msg
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	found, err := h.find(ctx, s, m.GetId())
	if err != nil {
		return nil, err
	}
	install := found.install
	pin := install.GetPin()
	entry, ok := s.entryFor(pin)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s came from %s, which has no %s now, so its companions stay as they are", install.GetProject(), originOf(pin).words(), pin.GetEntry()), "not_in_catalog")
	}
	companion, ok := catalog.Companion(entry, m.GetService())
	switch {
	case !ok:
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, fmt.Errorf("%s has no companion %s. %s", entry.GetName(), m.GetService(), catalog.CompanionList(entry)), "invalid")
	case len(pin.GetFile()) == 0:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s was installed before Dockwell kept what its entry's file gave, so its companions stay as they are", install.GetProject()), "no_record")
	case !catalog.Runs(string(pin.GetFile()), m.GetService()):
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s runs %s %s, which has no %s. Upgrade it first", install.GetProject(), entry.GetName(), pin.GetVersion(), companion.GetLabel()), "upgrade_first")
	}
	choice := catalog.Resolve(entry, pin)
	if choice[m.GetService()] == m.GetOn() {
		state := "off"
		if m.GetOn() {
			state = "on"
		}
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s is %s already in %s", companion.GetLabel(), state, install.GetProject()), "unchanged")
	}
	values := maps.Clone(pin.GetValues())
	if values == nil {
		values = map[string]string{}
	}
	for _, in := range companion.GetInputs() {
		delete(values, in)
	}
	if m.GetOn() {
		free, err := h.whatsFree(ctx, entry, "")
		if err != nil {
			return nil, h.internal(ctx, err)
		}
		given, err := catalog.CompanionValues(entry, companion, m.GetValues(), free.values)
		if err != nil {
			return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
		}
		maps.Copy(values, given)
	} else if len(m.GetValues()) > 0 {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, fmt.Errorf("removing %s takes no values. Leave them out", companion.GetLabel()), "invalid")
	}
	next := maps.Clone(choice)
	next[m.GetService()] = m.GetOn()
	old, err := ranFile(pin)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	met, err := catalog.MetFile(string(pin.GetFile()), pin.GetNeeds())
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	file, err := catalog.Without(met, catalog.Off(next))
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	newPin := proto.CloneOf(pin)
	newPin.Values, newPin.Companions = values, next
	oldVariables, oldStands, err := catalog.Given(old)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	variables, stands, err := catalog.Given(&resourcev1.CatalogPin{File: []byte(file), Values: values})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	// What its needs set is used by reference on both sides, and stays as it is, by the
	// names the install ran, whatever the release in the catalog now names.
	plain := catalog.PinnedPlain(pin.GetNeeds(), entry)
	oldLinked, err := catalog.StandFor(oldVariables, plain)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	linked, err := catalog.StandFor(variables, plain)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	summary := fmt.Sprintf("Add %s to %s", companion.GetLabel(), install.GetProject())
	if !m.GetOn() {
		summary = fmt.Sprintf("Remove %s from %s", companion.GetLabel(), install.GetProject())
	}
	change, err := h.imports.PlanUpgrade(ctx, composeapi.Upgrade{
		Project:     install.GetProjectId(),
		Environment: install.GetEnvironmentId(),
		Summary:     summary,
		Name:        found.display,
		File:        []byte(file),
		Variables:   variables,
		Stands:      stands,
		Pin:         newPin,
		Companion:   true,

		Old:          old.GetFile(),
		OldVariables: oldVariables,
		OldStands:    oldStands,
		Needs:        composeapi.Needs{Stands: linked, OldStands: oldLinked, Joins: pinJoins(entry, pin)},
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SetCatalogCompanionResponse{Change: change}), nil
}

// serverOf is the server an environment's services run on, by ID: the first service's
// first, or "" for the only server when it can't tell.
func (h *Handler) serverOf(ctx context.Context, env string) string {
	var out string
	_ = h.db.View(ctx, func(tx *store.Tx) error {
		list, err := tx.ListResources(ctx, env, string(service.Kind))
		if err != nil {
			return err
		}
		for i := range list {
			servers, err := tx.Placement(ctx, list[i].ID, list[i].Revision)
			if err == nil && len(servers) > 0 {
				out = servers[0]
				return nil
			}
		}
		return nil
	})
	return out
}

// ranFile is the pin as what runs now read it: its file without the companions it
// left out, with the values it kept.
func ranFile(pin *resourcev1.CatalogPin) (*resourcev1.CatalogPin, error) {
	met, err := catalog.MetFile(string(pin.GetFile()), pin.GetNeeds())
	if err != nil {
		return nil, err
	}
	file, err := catalog.Without(met, catalog.Off(pin.GetCompanions()))
	if err != nil {
		return nil, err
	}
	return &resourcev1.CatalogPin{File: []byte(file), Values: pin.GetValues()}, nil
}

// found is an install, as an upgrade or a change to its companions needs it.
type found struct {
	install *dockwellv1.CatalogInstall
	// display is how its project shows.
	display string
	// held are the secrets it has, by name.
	held map[string]bool
}

// find reads an install by its project's name or ID, with the secrets it has.
func (h *Handler) find(ctx context.Context, s sourced, id string) (found, error) {
	out := found{held: map[string]bool{}}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		all, err := h.installs(ctx, tx, s)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(all, func(in *dockwellv1.CatalogInstall) bool {
			return in.GetProject() == id || in.GetProjectId() == id
		})
		if i < 0 {
			return nil
		}
		out.install = all[i]
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			if p.ID == out.install.GetProjectId() {
				out.display = p.DisplayName
			}
		}
		list, err := tx.ListResources(ctx, out.install.GetEnvironmentId(), string(variable.SecretKind))
		for i := range list {
			out.held[list[i].Name] = true
		}
		return err
	})
	if err != nil {
		return found{}, h.internal(ctx, err)
	}
	if out.install == nil {
		return found{}, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("nothing installed from the catalog is called %s. Run dockwell catalog installs to see what is", id), "not_found")
	}
	return out, nil
}

// pinTo is what an install of an entry is pinned to: the entry, and where it came
// from, as the control plane knows it.
func pinTo(e *catalogv1.Entry, from origin, filled *catalog.Filled, companions map[string]bool) *resourcev1.CatalogPin {
	return &resourcev1.CatalogPin{
		Companions: companions,
		Entry:      e.GetId(),
		Version:    e.GetVersion(),
		Release:    e.GetRelease(),
		Catalog:    from.catalog,
		Repository: from.repository,
		Path:       from.path,
		Commit:     from.commit,
		Values:     filled.Plain,
		File:       []byte(e.GetCompose()),
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

// whatsFree finds a name for an entry's project, from the one asked for or else the
// entry's ID, and ports nothing Dockwell runs has taken, on any server, so a default
// needn't depend on where it lands. What else is on a server only its agent knows, so
// the plan refuses that by name.
func (h *Handler) whatsFree(ctx context.Context, e *catalogv1.Entry, asked string) (free, error) {
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
		if (scope.ProjectType{}).CheckName(asked) != nil {
			asked = e.GetId()
		}
		stem, n := numbered(asked)
		for out.project = asked; ; n++ {
			_, err := tx.GetResource(ctx, string(scope.ProjectKind), tree.Organization.ID, out.project)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			out.project = stem + "-" + strconv.Itoa(n)
		}
	})
	out.values = catalog.Suggest(e, taken)
	return out, err
}

// numbered splits a name that ends in a number, such as mysql-2, into what comes
// before it and the number after, so the next to try is mysql-3. Any other name is a
// stem of its own, whose next is the name with -2.
func numbered(name string) (string, int) {
	if i := strings.LastIndex(name, "-"); i > 0 {
		if n, err := strconv.Atoi(name[i+1:]); err == nil && n >= 1 && name[i+1:] == strconv.Itoa(n) {
			return name[:i], n + 1
		}
	}
	return name, 2
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

// catalog is Dockwell's own catalog, then each source's entries, as ADR 0055 has it.
func (h *Handler) catalog(ctx context.Context) (sourced, error) {
	s, err := h.withSources(ctx, nil)
	if err != nil {
		return sourced{}, h.internal(ctx, err)
	}
	return s, nil
}

func (h *Handler) internal(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, "catalog call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't complete the request; try again, and check its logs if it keeps failing"))
}
