// SPDX-License-Identifier: AGPL-3.0-only

// Package databases serves dockwell.v1.DatabaseService: the services that run a
// database Dockwell knows, told apart by their specs, with what each database
// says of itself on its servers, read in its own container by the agent there.
// See docs/adr/0038-databases.md.
package databases

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// askWait bounds what a server gets to answer for its databases, so a list never
// waits long on one that's slow: its rows say they couldn't be read.
const askWait = 10 * time.Second

// keepFor is how long a reading stands. Several readers in that time, as a page
// that refreshes beside the CLI, ask the database once.
const keepFor = 10 * time.Second

// Backups lists a volume's backups, newest first, as BackupService does.
type Backups interface {
	ListBackups(context.Context, *connect.Request[dockwellv1.ListBackupsRequest]) (*connect.Response[dockwellv1.ListBackupsResponse], error)
}

// Secrets opens a stored spec, as the revision store sealed it.
type Secrets interface {
	DecodeEach(ctx context.Context, tx *store.Tx, specs []store.StoredSpec) ([]proto.Message, []error, error)
}

// Handler implements DatabaseService.
type Handler struct {
	agents  *agentapi.Agents
	backups Backups
	db      *store.DB
	keeper  Secrets
	logger  *slog.Logger
	now     func() time.Time

	mu   sync.Mutex
	kept map[string]keptReading
}

var _ dockwellv1connect.DatabaseServiceHandler = (*Handler)(nil)

// keptReading is a reading, and when it was asked for.
type keptReading struct {
	reading *dockwellv1.DatabaseReading
	at      time.Time
}

// New returns a handler that reads the store through keeper, asks agents what each
// database says, and backups for what's kept of each.
func New(agents *agentapi.Agents, backups Backups, db *store.DB, keeper Secrets, logger *slog.Logger) *Handler {
	return &Handler{agents: agents, backups: backups, db: db, keeper: keeper, logger: logger, now: time.Now, kept: map[string]keptReading{}}
}

// placed is a database with where it runs.
type placed struct {
	db      *dockwellv1.Database
	ref     resource.Ref
	servers []store.Server
}

// ListDatabases returns every database.
func (h *Handler) ListDatabases(ctx context.Context, _ *connect.Request[dockwellv1.ListDatabasesRequest]) (*connect.Response[dockwellv1.ListDatabasesResponse], error) {
	list, err := h.list(ctx)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if err := h.complete(ctx, list); err != nil {
		return nil, h.fail(ctx, err)
	}
	out := &dockwellv1.ListDatabasesResponse{}
	for _, p := range list {
		out.Databases = append(out.Databases, p.db)
	}
	return connect.NewResponse(out), nil
}

// GetDatabase returns one database.
func (h *Handler) GetDatabase(ctx context.Context, req *connect.Request[dockwellv1.GetDatabaseRequest]) (*connect.Response[dockwellv1.GetDatabaseResponse], error) {
	m := req.Msg
	if m.GetId() == "" {
		return nil, authapi.Invalid(errors.New("the request names no database. Pass its service's ID, or its name"))
	}
	list, err := h.list(ctx)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	one, err := h.pick(ctx, list, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if err := h.complete(ctx, []*placed{one}); err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.GetDatabaseResponse{Database: one.db}), nil
}

// pick finds the database a request names, by its service's ID or its name.
func (h *Handler) pick(ctx context.Context, list []*placed, named, project, environment string) (*placed, error) {
	if strings.HasPrefix(named, id.Service+"_") {
		for _, p := range list {
			if p.db.GetId() == named {
				return p, nil
			}
		}
		return nil, notFound(named)
	}
	var tree scope.Tree
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		tree, err = scope.Load(ctx, tx)
		return err
	}); err != nil {
		return nil, err
	}
	var found []*placed
	var where []string
	for _, p := range tree.Projects {
		if project != "" && project != p.ID && project != p.Name {
			continue
		}
		for _, e := range p.Environments {
			if environment != "" && environment != e.ID && environment != e.Name {
				continue
			}
			for _, d := range list {
				if d.ref.Scope == e.ID && d.ref.Name == named {
					found = append(found, d)
					where = append(where, p.Name+"/"+e.Name)
				}
			}
		}
	}
	switch len(found) {
	case 0:
		return nil, notFound(named)
	case 1:
		return found[0], nil
	}
	return nil, authapi.Invalid(fmt.Errorf("there's a database called %s in %s. Say which with its project and environment, or pass its ID", named, human.List(where)))
}

func notFound(named string) error {
	return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no database %s. A database is a service that runs PostgreSQL, MySQL, MariaDB, Redis or Valkey from its official image", named), "not_found")
}

// list reads every service that runs a database, by name, with where each runs.
func (h *Handler) list(ctx context.Context) ([]*placed, error) {
	var out []*placed
	err := h.db.View(ctx, func(tx *store.Tx) error {
		services, err := tx.ListKind(ctx, string(service.Kind))
		if err != nil {
			return err
		}
		specs := make([]store.StoredSpec, len(services))
		ids := make([]string, len(services))
		for i := range services {
			r := &services[i]
			specs[i] = store.StoredSpec{At: store.InRevision(r.ID, r.Kind, r.Revision), Spec: r.Spec}
			ids[i] = r.ID
		}
		decoded, unreadable, err := h.keeper.DecodeEach(ctx, tx, specs)
		if err != nil {
			return err
		}
		deployments, err := tx.LatestDeployments(ctx, ids)
		if err != nil {
			return err
		}
		servers, err := tx.ListServers(ctx)
		if err != nil {
			return err
		}
		byID := map[string]store.Server{}
		for i := range servers {
			if servers[i].RevokedAt == nil {
				byID[servers[i].ID] = servers[i]
			}
		}
		inScope := map[string][]string{}
		for i := range services {
			inScope[services[i].Scope] = append(inScope[services[i].Scope], services[i].Name)
		}
		entries := map[string]string{}
		for i := range services {
			r := &services[i]
			if unreadable[i] != nil {
				// ListServices says which can't be read, and why.
				continue
			}
			svc, ok := decoded[i].(*resourcev1.Service)
			if !ok {
				continue
			}
			f, ok := database.FromSpec(svc)
			if !ok {
				continue
			}
			db := &dockwellv1.Database{
				Id:             r.ID,
				Resource:       resourcepb.Ref(resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}),
				Engine:         f.Engine.Kind,
				Image:          svc.GetApp().GetImage(),
				Version:        database.Version(svc.GetApp().GetImage()),
				Volume:         f.Volume,
				VolumeExternal: f.External,
				Port:           f.Port,
				Host:           r.Name,
				PasswordSecret: f.PasswordSecret,
			}
			for _, name := range inScope[r.Scope] {
				if name != r.Name {
					db.Beside = append(db.Beside, name)
				}
			}
			if f.Volume != "" && !f.External {
				v, err := tx.GetResource(ctx, "volume", r.Scope, f.Volume)
				switch {
				case err == nil && v.Spec != nil:
					db.VolumeId = v.ID
				case err != nil && !errors.Is(err, store.ErrNotFound):
					return err
				}
			}
			entry, ok := entries[r.Scope]
			if !ok {
				if entry, err = catalogEntry(ctx, tx, r.Scope); err != nil {
					return err
				}
				entries[r.Scope] = entry
			}
			db.CatalogEntry = entry
			p := &placed{db: db, ref: resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}}
			for _, d := range deployments[r.ID] { //nolint:gocritic // a few rows, read once
				if s, ok := byID[d.Server]; ok && d.State != store.DeploymentSuperseded {
					p.servers = append(p.servers, s)
				}
			}
			slices.SortFunc(p.servers, func(a, b store.Server) int { return cmp.Compare(a.Name, b.Name) })
			out = append(out, p)
		}
		return nil
	})
	slices.SortStableFunc(out, func(a, b *placed) int { return cmp.Compare(a.ref.Name, b.ref.Name) })
	return out, err
}

// catalogEntry is the catalog entry an environment's project was installed from,
// or "".
func catalogEntry(ctx context.Context, tx *store.Tx, environment string) (string, error) {
	env, err := tx.GetResourceByID(ctx, environment)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	project, err := tx.GetResourceByID(ctx, env.Scope)
	if errors.Is(err, store.ErrNotFound) || err == nil && project.Spec == nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// A project is never sealed (ADR 0022).
	spec := &resourcev1.Project{}
	if err := proto.Unmarshal(project.Spec, spec); err != nil {
		return "", fmt.Errorf("can't read the project %s: %w", project.Name, err)
	}
	return spec.GetCatalog().GetEntry(), nil
}

// complete adds what's not in the store: each database's newest backups, and what
// each says of itself on its servers.
func (h *Handler) complete(ctx context.Context, list []*placed) error {
	for _, p := range list {
		if p.db.GetVolumeId() == "" {
			continue
		}
		res, err := h.backups.ListBackups(ctx, connect.NewRequest(&dockwellv1.ListBackupsRequest{VolumeId: p.db.GetVolumeId(), PageSize: 20}))
		if err != nil {
			return err
		}
		for _, b := range res.Msg.GetBackups() {
			if b.GetState() == dockwellv1.BackupState_BACKUP_STATE_DONE {
				p.db.NewestBackup = b
				break
			}
			if p.db.LatestBackup == nil {
				p.db.LatestBackup = b
			}
		}
	}
	h.read(ctx, list)
	return nil
}

// read asks each server about its databases at once, all servers together, and
// keeps what each said for keepFor.
func (h *Handler) read(ctx context.Context, list []*placed) {
	type ask struct {
		server store.Server
		dbs    []*placed
	}
	byServer := map[string]*ask{}
	var order []string
	now := h.now()
	for _, p := range list {
		for i := range p.servers {
			s := p.servers[i]
			if r, ok := h.fresh(s.ID, p.db.GetId(), now); ok {
				p.db.Readings = append(p.db.Readings, r)
				continue
			}
			a := byServer[s.ID]
			if a == nil {
				a = &ask{server: s}
				byServer[s.ID] = a
				order = append(order, s.ID)
			}
			a.dbs = append(a.dbs, p)
		}
	}
	readings := make([][]*dockwellv1.DatabaseReading, len(order))
	var wg sync.WaitGroup
	for i, server := range order {
		a := byServer[server]
		wg.Go(func() { readings[i] = h.ask(ctx, a.server, a.dbs) })
	}
	wg.Wait()
	for i, server := range order {
		for j, p := range byServer[server].dbs {
			p.db.Readings = append(p.db.Readings, readings[i][j])
		}
	}
	for _, p := range list {
		slices.SortFunc(p.db.Readings, func(a, b *dockwellv1.DatabaseReading) int { return cmp.Compare(a.GetServerName(), b.GetServerName()) })
	}
}

// fresh is a reading taken a moment ago.
func (h *Handler) fresh(server, db string, now time.Time) (*dockwellv1.DatabaseReading, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k, ok := h.kept[server+"/"+db]
	if !ok || now.Sub(k.at) >= keepFor {
		return nil, false
	}
	return proto.CloneOf(k.reading), true
}

// ask asks one server about its databases, and says for each why it couldn't.
func (h *Handler) ask(ctx context.Context, server store.Server, dbs []*placed) []*dockwellv1.DatabaseReading {
	out := make([]*dockwellv1.DatabaseReading, len(dbs))
	call := &agentv1.DatabaseCall{}
	for _, p := range dbs {
		call.Databases = append(call.Databases, &agentv1.DatabaseAsked{Service: resourcepb.Ref(p.ref), Engine: p.db.GetEngine()})
	}
	at := h.now()
	why := ""
	// What a server that isn't connected says costs nothing to ask again, and it may
	// be back in a moment.
	keep := true
	var got *agentv1.DatabaseResult
	if s, ok := h.agents.Session(server.ID); !ok {
		why, keep = server.Name+" isn't connected, so it can't say how its databases are", false
	} else {
		ctx, cancel := context.WithTimeout(ctx, askWait)
		var err error
		got, err = s.Databases(ctx, call)
		cancel()
		switch {
		case errors.Is(err, resource.ErrNotConnected), errors.Is(err, resource.ErrLost):
			why, keep = server.Name+" isn't connected, so it can't say how its databases are", false
		case errors.Is(err, context.DeadlineExceeded):
			why = fmt.Sprintf("%s didn't answer within %s", server.Name, askWait)
		case err != nil:
			plain, _ := resource.Explain(err)
			why = fmt.Sprintf("%s can't read its databases: %s", server.Name, strings.TrimSuffix(plain, "."))
		}
	}
	// A reader that left took the answer with it.
	keep = keep && ctx.Err() == nil
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, p := range dbs {
		r := &dockwellv1.DatabaseReading{Unread: why}
		if why == "" {
			r = got.GetReadings()[i]
		}
		r.ServerId, r.ServerName, r.ReadTime = server.ID, server.Name, timestamppb.New(at)
		out[i] = r
		if keep {
			h.kept[server.ID+"/"+p.db.GetId()] = keptReading{reading: proto.CloneOf(r), at: at}
		}
	}
	for k, v := range h.kept {
		if at.Sub(v.at) >= keepFor {
			delete(h.kept, k)
		}
	}
	return out
}

// fail turns what went wrong into what the API says. An error made for the caller
// passes through as it is.
func (h *Handler) fail(ctx context.Context, err error) error {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, "a database call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read its databases; try again, and check its logs if it keeps failing"))
}
