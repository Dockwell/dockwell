// SPDX-License-Identifier: AGPL-3.0-only

// Package connections serves ConnectionService: the connections apps use, and the
// check of one from where an app is (ADR 0099). A connection is saved as a change,
// through ProposeChange, as every install-wide resource is; this reads them, and
// checks one through a server's agent, which runs the check in a container of its own
// binary and says itself what the check may reach.
package connections

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/control/registrylogins"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/probe"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Connections reads the saved connections.
type Connections struct {
	db     *store.DB
	keeper *secrets.Keeper
}

// NewConnections reads connections from db, opened with keeper.
func NewConnections(db *store.DB, keeper *secrets.Keeper) *Connections {
	return &Connections{db: db, keeper: keeper}
}

// Saved is a connection as saved, keys and all: they go no further than a call to an
// agent, sealed.
type Saved struct {
	ID, Name string
	Revision int
	Spec     *resourcev1.ConnectionSpec
}

// All reads every saved connection, sorted by name.
func (c *Connections) All(ctx context.Context) ([]Saved, error) {
	var out []Saved
	err := c.db.View(ctx, func(tx *store.Tx) error {
		stored, err := tx.ListKind(ctx, string(connection.Kind))
		if err != nil {
			return err
		}
		for i := range stored {
			r := &stored[i]
			if r.Spec == nil {
				continue
			}
			spec, err := c.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return fmt.Errorf("can't read the connection %s: %w", r.Name, err)
			}
			if s, ok := spec.(*resourcev1.ConnectionSpec); ok {
				out = append(out, Saved{ID: r.ID, Name: r.Name, Revision: r.Revision, Spec: s})
			}
		}
		return nil
	})
	slices.SortFunc(out, func(a, b Saved) int { return strings.Compare(a.Name, b.Name) })
	return out, err
}

// ProjectFor is the project a request names, by its environment's ID, or by the
// project's own ID or name. Empty for none.
func (c *Connections) ProjectFor(ctx context.Context, environment, project string) (string, error) {
	var out string
	err := c.db.View(ctx, func(tx *store.Tx) error {
		if strings.HasPrefix(environment, id.Environment+"_") {
			got, err := registrylogins.ProjectOf(ctx, tx, environment)
			out = got
			return err
		}
		if project == "" {
			return nil
		}
		got, err := place.Project(ctx, tx, project)
		out = got
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", authapi.ReasonError(connect.CodeNotFound, errors.New("there's no such project or environment. List them to find its name"), "not_found")
	}
	return out, err
}

// Service serves ConnectionService.
type Service struct {
	connections *Connections
	fleet       *fleet.Hub
	agents      *agentapi.Agents
	// changes plans ConnectStorage's change, and db reads the app it connects.
	changes Changes
	db      *store.DB
}

var _ dockwellv1connect.ConnectionServiceHandler = (*Service)(nil)

// New returns a Service that checks through the agents of hub's servers, and plans
// through changes.
func New(connections *Connections, hub *fleet.Hub, agents *agentapi.Agents, changes Changes, db *store.DB) *Service {
	return &Service{connections: connections, fleet: hub, agents: agents, changes: changes, db: db}
}

// ListConnections lists the connections: every one for an admin, with all but its
// keys; below an admin, those the caller's projects may use, which the API trims to
// what a named object shows.
func (s *Service) ListConnections(ctx context.Context, req *connect.Request[dockwellv1.ListConnectionsRequest]) (*connect.Response[dockwellv1.ListConnectionsResponse], error) {
	out := &dockwellv1.ListConnectionsResponse{}
	all, err := s.connections.All(ctx)
	if err != nil {
		return nil, err
	}
	project := ""
	narrowed := req.Msg.GetProject() != "" || req.Msg.GetEnvironment() != ""
	if narrowed {
		if project, err = s.connections.ProjectFor(ctx, req.Msg.GetEnvironment(), req.Msg.GetProject()); err != nil {
			return nil, err
		}
	}
	admin, mine := callerProjects(ctx)
	for _, c := range all {
		switch {
		case narrowed && !connection.Reaches(c.Spec, project):
			continue
		case !admin && !reachesAny(c.Spec, mine):
			continue
		}
		info := &dockwellv1.ConnectionInfo{Id: c.ID, Name: c.Name, DisplayName: c.Spec.GetDisplayName(), Kind: c.Spec.GetKind()}
		if admin {
			s3 := c.Spec.GetS3()
			info.Endpoint, info.Region, info.Bucket, info.PathStyle, info.PublicUrl = s3.GetEndpoint(), s3.GetRegion(), s3.GetBucket(), s3.GetPathStyle(), s3.GetPublicUrl()
			info.Reach, info.Projects = c.Spec.GetReach(), c.Spec.GetProjects()
		}
		out.Connections = append(out.Connections, info)
	}
	return connect.NewResponse(out), nil
}

// callerProjects says whether the caller is an admin, who sees every connection, and
// otherwise the projects they hold a grant on; nil for every project, from a grant
// on the whole install.
func callerProjects(ctx context.Context) (admin bool, projects map[string]bool) {
	c, ok := place.CallerFrom(ctx)
	if !ok {
		return true, nil
	}
	if store.RoleRank(place.Holds(c.Grants, place.Install)) >= store.RoleRank(store.RoleAdmin) {
		return true, nil
	}
	projects = map[string]bool{}
	for _, g := range c.Grants {
		if g.ProjectID == store.Install {
			return false, nil
		}
		projects[g.ProjectID] = true
	}
	return false, projects
}

// reachesAny says whether a connection reaches one of projects; nil projects is any
// project, for a grant on the whole install, which sees those reaching any.
func reachesAny(c *resourcev1.ConnectionSpec, projects map[string]bool) bool {
	if c.GetReach() == resourcev1.ConnectionReach_CONNECTION_REACH_EVERY {
		return true
	}
	if projects == nil {
		return c.GetReach() == resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS
	}
	for _, p := range c.GetProjects() {
		if projects[p] && c.GetReach() == resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS {
			return true
		}
	}
	return false
}

// CheckConnection checks a connection from a server's agent, as typed or saved, and
// saves nothing. The agent runs the check in a container of its own binary, on the
// app's networks or a network of the check's own, and says itself what it may reach.
// An admin's connection may reach a private address, such as a NAS on the server's
// network, as ADR 0084's connections may.
func (s *Service) CheckConnection(ctx context.Context, req *connect.Request[dockwellv1.CheckConnectionRequest]) (*connect.Response[dockwellv1.CheckConnectionResponse], error) {
	spec, err := s.toCheck(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	if (req.Msg.GetEnvironment() == "") != (req.Msg.GetApp() == "") {
		return nil, authapi.Invalid(errors.New("a check from an app names its environment and its service, or neither for a network of the check's own"))
	}
	session, srv, err := s.session(ctx, req.Msg.GetServer())
	if err != nil {
		return nil, err
	}
	s3 := spec.GetS3()
	got, err := session.ProbeConnection(ctx, req.Msg.GetEnvironment(), req.Msg.GetApp(), probe.Request{
		Kind: probe.KindS3, Private: true,
		S3: &probe.S3{
			Endpoint: s3.GetEndpoint(), Region: s3.GetRegion(), Bucket: s3.GetBucket(), VirtualHost: !s3.GetPathStyle(),
			Prefix: req.Msg.GetPrefix(), AccessKey: s3.GetAccessKeyId(), SecretKey: s3.GetSecretKey(), Presign: req.Msg.GetPresign(),
		},
	})
	switch {
	case errors.Is(err, agentapi.ErrOlderAgent):
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s can't check a connection: %w", srv.GetName(), err))
	case err != nil:
		var se *resource.ServerError
		if errors.As(err, &se) {
			err = se.Err
		}
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't check a connection now: %w", srv.GetName(), err), authapi.ReasonUnavailable)
	}
	return connect.NewResponse(&dockwellv1.CheckConnectionResponse{
		Passed: got.GetPassed(), Step: got.GetStep(), Code: got.GetCode(), Status: got.GetStatus(),
		Message: got.GetMessage(), Words: got.GetWords(), Region: got.GetRegion(), Server: srv.GetName(),
	}), nil
}

// toCheck is the connection a check reaches: as typed, or a saved one with what's
// typed in its place, whose keys go only where they were saved to go.
func (s *Service) toCheck(ctx context.Context, req *dockwellv1.CheckConnectionRequest) (*resourcev1.ConnectionSpec, error) {
	typed := &resourcev1.ConnectionSpec{Kind: resourcev1.LinkKind_LINK_KIND_S3, S3: req.GetS3()}
	if typed.GetS3() == nil {
		typed.S3 = &resourcev1.ConnectionS3{}
	}
	if req.GetName() == "" {
		if err := (connection.Type{}).Validate(typed); err != nil {
			return nil, authapi.Invalid(err)
		}
		if err := (connection.Type{}).CheckNew(typed); err != nil {
			return nil, authapi.Invalid(err)
		}
		return typed, nil
	}
	all, err := s.connections.All(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range all {
		if c.Name != req.GetName() {
			continue
		}
		was := c.Spec.GetS3()
		to := typed.GetS3()
		if to.GetEndpoint() == "" {
			to.Endpoint = was.GetEndpoint()
		}
		if to.GetBucket() == "" {
			to.Bucket = was.GetBucket()
		}
		if to.GetRegion() == "" {
			to.Region = was.GetRegion()
		}
		if req.GetS3() == nil {
			to.PathStyle = was.GetPathStyle()
		}
		if err := (connection.Type{}).Validate(typed); err != nil {
			return nil, authapi.Invalid(err)
		}
		// What's kept goes only where it was saved to go, as an update keeps it.
		if _, err := (connection.Type{}).Keep(c.Spec, typed); err != nil {
			return nil, authapi.Invalid(err)
		}
		return typed, nil
	}
	return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no connection %s. List them with ListConnections", req.GetName()), "not_found")
}

// session is the agent session of the server named, or of the only server.
func (s *Service) session(ctx context.Context, named string) (*agentapi.Session, *dockwellv1.Server, error) {
	srv, err := s.server(named)
	if err != nil {
		return nil, nil, err
	}
	var session *agentapi.Session
	if s.agents != nil && srv.GetConnection() == dockwellv1.Connection_CONNECTION_CONNECTED {
		session, _ = s.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	}
	if session == nil {
		return nil, srv, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf(
			"the agent on %s isn't connected, so it can't check a connection now. This works again when its agent reconnects", srv.GetName()), authapi.ReasonUnavailable)
	}
	return session, srv, nil
}

func (s *Service) server(named string) (*dockwellv1.Server, error) {
	if named != "" {
		if srv, err := s.fleet.Get(named); err == nil {
			return srv, nil
		}
		for _, srv := range s.fleet.List() {
			if srv.GetName() == named {
				return srv, nil
			}
		}
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", named))
	}
	all := s.fleet.List()
	switch len(all) {
	case 0:
		return nil, authapi.ReasonError(connect.CodeUnavailable, errors.New("there's no server to check a connection from yet. Add one first"), authapi.ReasonUnavailable)
	case 1:
		return all[0], nil
	}
	names := make([]string, 0, len(all))
	for _, srv := range all {
		names = append(names, srv.GetName())
	}
	slices.Sort(names)
	return nil, authapi.ChoiceError(&dockwellv1.Choice{Level: "server", Field: "server", Options: names},
		fmt.Errorf("there are %d servers, so say which one checks it: a connection is checked as that server reaches it", len(all)))
}
