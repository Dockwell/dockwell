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
	"log/slog"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/control/registrylogins"
	"github.com/dockwell/dockwell/internal/control/updates"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/probe"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Connections reads the saved connections.
type Connections struct {
	db     *store.DB
	keeper *secrets.Keeper
	// Logger says which link Uses left out, as it couldn't read it. Nil says nothing.
	Logger *slog.Logger
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
	out, _, err := c.read(ctx, false)
	return out, err
}

// Revisions is each saved connection's ID, name and revision, sorted by name,
// with no spec: what the checker looks at each minute, which opens no secret.
func (c *Connections) Revisions(ctx context.Context) ([]Saved, error) {
	var out []Saved
	err := c.db.View(ctx, func(tx *store.Tx) error {
		stored, err := tx.ListKind(ctx, string(connection.Kind))
		if err != nil {
			return err
		}
		for i := range stored {
			out = append(out, Saved{ID: stored[i].ID, Name: stored[i].Name, Revision: stored[i].Revision})
		}
		return nil
	})
	slices.SortFunc(out, func(a, b Saved) int { return strings.Compare(a.Name, b.Name) })
	return out, err
}

// Readable is every saved connection the control plane can open, and the names of
// those it can't, as one saved under a secrets key it no longer holds: one of them
// leaves out that connection alone, not every connection for everyone.
func (c *Connections) Readable(ctx context.Context) ([]Saved, []string, error) {
	return c.read(ctx, true)
}

func (c *Connections) read(ctx context.Context, skip bool) ([]Saved, []string, error) {
	var out []Saved
	var unread []string
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
			if err != nil && skip {
				unread = append(unread, r.Name)
				continue
			}
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
	slices.Sort(unread)
	return out, unread, err
}

// KeptPrefix is where a use's app keeps its files in the bucket: its link's prefix,
// where the link sets the name that tells the app it, and otherwise the bucket's root,
// "" (link.Kept).
func KeptPrefix(u *dockwellv1.ConnectionUse) string {
	for _, part := range u.GetSets() {
		if part == resourcev1.LinkPart_LINK_PART_KEY_PREFIX {
			return u.GetPrefix()
		}
	}
	return ""
}

// Uses reads what reads each connection, by its name: each link to it, in its
// environment and project, with the apps that read the names the link sets (ADR
// 0099). A link names a connection of the install's one organization by name alone.
func (c *Connections) Uses(ctx context.Context) (map[string][]*dockwellv1.ConnectionUse, error) {
	out := map[string][]*dockwellv1.ConnectionUse{}
	err := c.db.View(ctx, func(tx *store.Tx) error {
		links, err := tx.ListKind(ctx, string(link.Kind))
		if err != nil {
			return err
		}
		projects := map[string]string{}
		for i := range links {
			r := &links[i]
			spec, err := c.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				// One link that can't be read leaves out what reads through it, not every
				// connection for everyone, and its name stays out of the answer, which
				// may go to someone of another project.
				if c.Logger != nil {
					c.Logger.Warn("can't read a link, so the connections list leaves out what reads through it. Check that the control plane runs with the secrets key it was saved with",
						"link", r.ID, "environment", r.Scope, "error", err.Error())
				}
				continue
			}
			l, ok := spec.(*resourcev1.Link)
			if !ok || l.GetConnection() == "" {
				continue
			}
			project, seen := projects[r.Scope]
			if !seen {
				if project, err = registrylogins.ProjectOf(ctx, tx, r.Scope); err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
				projects[r.Scope] = project
			}
			use := &dockwellv1.ConnectionUse{
				LinkId: r.ID, Link: r.Name, Environment: r.Scope, Project: project, Prefix: l.GetPrefix(),
				Sets: map[string]resourcev1.LinkPart{},
			}
			names := make([]string, 0, len(l.GetSets()))
			for _, set := range l.GetSets() {
				use.Sets[set.GetName()] = set.GetPart()
				names = append(names, set.GetName())
			}
			if len(names) > 0 {
				users, err := tx.UsersOf(ctx, r.Scope, "", names...)
				if err != nil {
					return err
				}
				for i := range users {
					if u := &users[i]; u.Kind == serviceKind && !slices.Contains(use.AppIds, u.ID) {
						use.AppIds = append(use.AppIds, u.ID)
						use.Apps = append(use.Apps, u.Name)
					}
				}
			}
			out[l.GetConnection()] = append(out[l.GetConnection()], use)
		}
		return nil
	})
	return out, err
}

// ServersOf are the servers, by ID, that run an app linked to the connection named:
// one that reads a name a link to it sets.
func (c *Connections) ServersOf(ctx context.Context, name string) (map[string]bool, error) {
	at, err := c.Targets(ctx, name)
	out := make(map[string]bool, len(at))
	for srv := range at {
		out[srv] = true
	}
	return out, err
}

// Target is where a connection is checked from on a server: an app linked to it
// there, by its environment's ID and its service's name, on that app's networks and
// under its link's prefix, as the app reaches it (#799's review, M2).
type Target struct {
	Server, Environment, App, Prefix string
}

// Targets are, by server ID, an app linked to the connection named on each server
// it runs on: of several, the first by environment and name, so a look picks the
// same one each time.
func (c *Connections) Targets(ctx context.Context, name string) (map[string]Target, error) {
	out := map[string]Target{}
	err := c.db.View(ctx, func(tx *store.Tx) error {
		links, err := tx.ListKind(ctx, string(link.Kind))
		if err != nil {
			return err
		}
		for i := range links {
			r := &links[i]
			spec, err := c.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				continue // one it can't read names nothing here; Uses logs it
			}
			l, ok := spec.(*resourcev1.Link)
			if !ok || l.GetConnection() != name || len(l.GetSets()) == 0 {
				continue
			}
			names := make([]string, 0, len(l.GetSets()))
			for _, set := range l.GetSets() {
				names = append(names, set.GetName())
			}
			users, err := tx.UsersOf(ctx, r.Scope, "", names...)
			if err != nil {
				return err
			}
			for j := range users {
				if users[j].Kind != serviceKind {
					continue
				}
				app, err := tx.GetResource(ctx, serviceKind, r.Scope, users[j].Name)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				placed, err := tx.Placement(ctx, app.ID, app.Revision)
				if err != nil {
					return err
				}
				t := Target{Environment: r.Scope, App: users[j].Name, Prefix: l.GetPrefix()}
				for _, srv := range placed {
					t.Server = srv
					if was, ok := out[srv]; !ok || t.Environment+"/"+t.App < was.Environment+"/"+was.App {
						out[srv] = t
					}
				}
			}
		}
		return nil
	})
	return out, err
}

// serviceKind is an app's kind, which reads a link's values.
const serviceKind = "service"

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
	// checks keeps each connection's last check, and checks each on a schedule.
	checks *Checker
}

var _ dockwellv1connect.ConnectionServiceHandler = (*Service)(nil)

// New returns a Service that checks through the agents of hub's servers, and plans
// through changes.
func New(connections *Connections, hub *fleet.Hub, agents *agentapi.Agents, changes Changes, db *store.DB) *Service {
	s := &Service{connections: connections, fleet: hub, agents: agents, changes: changes, db: db}
	s.checks = newChecker(s)
	return s
}

// Checks is what checks each connection on a schedule and keeps its last checks.
func (s *Service) Checks() *Checker { return s.checks }

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
	uses, err := s.connections.Uses(ctx)
	if err != nil {
		return nil, err
	}
	// Whether an app's own values hold each, as a link would: a yes or no, which
	// names no project, so it reads the same to anyone (slice 2 re-check, N2).
	values, err := s.connections.OwnValues(ctx, "")
	if err != nil {
		return nil, err
	}
	admin, mine := callerProjects(ctx)
	for _, c := range all {
		switch {
		case narrowed && !connection.Reaches(c.Spec, project):
			continue
		case !admin && !reachesAny(c.Spec, mine):
			continue
		}
		info := &dockwellv1.ConnectionInfo{Id: c.ID, Name: c.Name, DisplayName: c.Spec.GetDisplayName(), Kind: c.Spec.GetKind(), Held: Holds(values, c.Spec), Shared: c.Spec.GetShared()}
		switch {
		case admin:
			s3 := c.Spec.GetS3()
			info.Endpoint, info.Region, info.Bucket, info.PathStyle, info.PublicUrl = s3.GetEndpoint(), s3.GetRegion(), s3.GetBucket(), s3.GetPathStyle(), s3.GetPublicUrl()
			if m := c.Spec.GetSmtp(); m != nil {
				info.Host, info.Port, info.Security = m.GetHost(), uint32(connection.Port(m)), connection.Security(m) //nolint:gosec // a port
			}
			info.Reach, info.Projects = c.Spec.GetReach(), c.Spec.GetProjects()
		case mine != nil:
			// Below an admin, which of the caller's own projects may use it, so the
			// panel offers only their apps: never another project's.
			info.Reach = resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS
			for _, p := range slices.Sorted(maps.Keys(mine)) {
				if connection.Reaches(c.Spec, p) {
					info.Projects = append(info.Projects, p)
				}
			}
		default:
			// A grant on the whole install holds every project.
			info.Reach, info.Projects = c.Spec.GetReach(), c.Spec.GetProjects()
		}
		// Who its mail comes from is what an app's card says it sends as, to whoever
		// may see the connection; where it is stays an admin's.
		info.Sender, info.SenderName = c.Spec.GetSmtp().GetSender(), c.Spec.GetSmtp().GetSenderName()
		// Its last checks; below an admin, whether each passed and when, alone: where
		// it's checked from, and what its provider said, stay an admin's (#799).
		for _, check := range s.checks.Of(c.ID, c.Revision) {
			if !admin {
				check = trimmedCheck(check)
			}
			info.Checks = append(info.Checks, check)
		}
		// Below an admin, only what reads it in the caller's own projects: another
		// project's apps and links aren't theirs to see.
		for _, u := range uses[c.Name] {
			if admin || mine == nil || mine[u.GetProject()] {
				info.Uses = append(info.Uses, u)
			}
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
	got, srv, ref, err := s.check(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	// A check of a saved connection as it's saved, from a network of its own, is the
	// one the schedule makes, so it's kept as its last from that server (#799).
	m := req.Msg
	if m.GetName() != "" && m.GetS3() == nil && m.GetSmtp() == nil && m.GetEnvironment() == "" && m.GetPrefix() == "" && !m.GetPresign() {
		s.checks.record(ref, srv.GetId(), got)
	}
	return connect.NewResponse(got), nil
}

// check is CheckConnection's check, with the server that made it and the saved
// connection it checked, by ID and revision, none for one as typed.
func (s *Service) check(ctx context.Context, msg *dockwellv1.CheckConnectionRequest) (*dockwellv1.CheckConnectionResponse, *dockwellv1.Server, Saved, error) {
	// Read before the check, so a change saved while it runs leaves it an older
	// revision's, which is never shown as the newer's.
	savedID, revision := s.savedRef(ctx, msg.GetName())
	ref := Saved{ID: savedID, Name: msg.GetName(), Revision: revision}
	spec, err := s.toCheck(ctx, msg)
	if err != nil {
		return nil, nil, Saved{}, err
	}
	if (msg.GetEnvironment() == "") != (msg.GetApp() == "") {
		return nil, nil, Saved{}, authapi.Invalid(errors.New("a check from an app names its environment and its service, or neither for a network of the check's own"))
	}
	if err := link.CheckPrefix(msg.GetPrefix()); err != nil {
		return nil, nil, Saved{}, authapi.Invalid(err)
	}
	session, srv, err := s.session(ctx, msg.GetServer())
	if err != nil {
		return nil, nil, Saved{}, err
	}
	if err := s.mayHoldKeys(ctx, msg, srv); err != nil {
		return nil, nil, Saved{}, err
	}
	got, err := s.probe(ctx, session, srv, msg.GetEnvironment(), msg.GetApp(), probeOf(spec, msg.GetPrefix(), msg.GetPresign()))
	if err != nil {
		return nil, nil, Saved{}, err
	}
	return got, srv, ref, nil
}

// probe runs a check through a server's agent, and says what it found, at when it
// found it.
func (s *Service) probe(ctx context.Context, session *agentapi.Session, srv *dockwellv1.Server, env, app string, req probe.Request) (*dockwellv1.CheckConnectionResponse, error) {
	got, err := session.ProbeConnection(ctx, env, app, req)
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
	return &dockwellv1.CheckConnectionResponse{
		Passed: got.GetPassed(), Step: got.GetStep(), Code: got.GetCode(), Status: got.GetStatus(),
		Message: got.GetMessage(), Words: got.GetWords(), Region: got.GetRegion(), Server: srv.GetName(),
		Time: timestamppb.New(s.checks.clock()),
	}, nil
}

// checkSaved checks a saved connection as it's saved, from where the checker chose:
// a server that runs an app linked to it, on that app's networks and under its
// link's prefix, as the app reaches it, or the control server, on a network of the
// check's own, where no app is linked (#799's review, M2). Check's full check once
// it's saved or changed, and on its schedule one that writes nothing (ADR 0099):
// storage's one signed read, and a mail server's sign-in, which sends nothing.
func (s *Service) checkSaved(ctx context.Context, c Saved, at Target, full bool) (*dockwellv1.CheckConnectionResponse, error) {
	session, srv, err := s.session(ctx, at.Server)
	if err != nil {
		return nil, err
	}
	req := probeOf(c.Spec, at.Prefix, false)
	if !full {
		req = scheduledProbe(c.Spec, at.Prefix)
	}
	got, err := s.probe(ctx, session, srv, at.Environment, at.App, req)
	if err != nil && req.Kind == probe.KindS3Read && strings.Contains(err.Error(), "stopped before it finished") &&
		beforeReads(srv.GetAgentBuild().GetVersion()) {
		// An agent from before the scheduled read refuses its kind, and says only
		// that its check stopped: kept as that server's state, so the panel says to
		// update its agent, rather than show an older pass (#799's review, L5). Only
		// where its version says it's older, so a current agent's probe that stops,
		// as one its memory cap ends, stays an error to try again (re-check, N3).
		return &dockwellv1.CheckConnectionResponse{
			Step: probe.StepList, Code: CodeAgentUpdate, Server: srv.GetName(),
			Words: fmt.Sprintf("the agent on %s couldn't run the scheduled check. Update its agent from the server's page", srv.GetName()),
			Time:  timestamppb.New(s.checks.clock()),
		}, nil
	}
	return got, err
}

// trimmedCheck is a check as someone below an admin reads it: whether it passed and
// when, and of a check that's neither passed nor failed, which of the two it is, in
// Dockwell's own words at step "list": unconfirmed, where the keys may neither list
// nor read, and agent-update. No server's name, and nothing the provider said, so a
// member sees it as neutral, not as a red failure (#799's re-check, N1).
func trimmedCheck(c *dockwellv1.CheckConnectionResponse) *dockwellv1.CheckConnectionResponse {
	out := &dockwellv1.CheckConnectionResponse{Passed: c.GetPassed(), Time: c.GetTime()}
	switch {
	case c.GetPassed():
	case c.GetCode() == CodeAgentUpdate:
		out.Step, out.Code = probe.StepList, CodeAgentUpdate
	case c.GetStep() == probe.StepList && c.GetCode() == "AccessDenied":
		out.Step, out.Code = probe.StepList, CodeUnconfirmed
	}
	return out
}

// CodeUnconfirmed is, below an admin, a scheduled read that can't confirm the keys
// without writing, as AccessDenied answered both its reads.
const CodeUnconfirmed = "unconfirmed"

// CodeAgentUpdate is a scheduled check a server's agent couldn't run, as one from
// before the scheduled read: the agent needs an update.
const CodeAgentUpdate = "agent-update"

// ReadsSince is the first release whose agent runs the scheduled read, s3-read: the
// one after v0.3.0, the last release cut before it. Raise it if another is cut
// before this lands.
const ReadsSince = "v0.3.1"

// beforeReads says an agent's version is a release from before ReadsSince. A build
// between releases, whose version is a pre-release or a commit's, says nothing of
// whether it has the read, so it isn't taken for one that needs an update.
func beforeReads(version string) bool {
	v := strings.TrimPrefix(version, "v")
	if v == "" || strings.ContainsAny(v, "-+") || strings.Count(v, ".") != 2 {
		return false
	}
	return updates.CompareVersions("v"+v, ReadsSince) < 0
}

// scheduledProbe is a check on a schedule: storage's reads the bucket and writes
// nothing, as a write a versioned bucket keeps, or Object Lock refuses to delete,
// four times a day would; a mail server's is Check's own, which sends nothing.
func scheduledProbe(c *resourcev1.ConnectionSpec, prefix string) probe.Request {
	r := probeOf(c, prefix, false)
	if r.Kind == probe.KindS3 {
		r.Kind = probe.KindS3Read
	}
	return r
}

// mayHoldKeys refuses to send a saved connection's keys, where none are typed, to a
// server that runs no app linked to it: they go only with the values of an app
// that reads them, or for the check from where such an app is, and to the control
// server's own agent, which holds them already (ADR 0099, #710). Keys typed in the
// request are the caller's to send anywhere.
func (s *Service) mayHoldKeys(ctx context.Context, req *dockwellv1.CheckConnectionRequest, srv *dockwellv1.Server) error {
	if req.GetName() == "" || req.GetS3().GetSecretKey() != "" || req.GetSmtp().GetPassword() != "" || srv.GetControlServer() {
		return nil
	}
	runs, err := s.connections.ServersOf(ctx, req.GetName())
	if err != nil {
		return err
	}
	if runs[srv.GetId()] {
		return nil
	}
	return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
		"%s runs no app linked to %s, so Dockwell doesn't send it the saved keys. Check it from the control server or a server an app linked to it runs on, or type its keys",
		srv.GetName(), req.GetName()), "keys_stay")
}

// probeOf is the check of a connection: its bucket under prefix, and a presigned
// URL where asked. It reaches a private address beyond the app's own networks only
// where an install admin saved the connection (ADR 0099, #710).
func probeOf(c *resourcev1.ConnectionSpec, prefix string, presign bool) probe.Request {
	if m := c.GetSmtp(); m != nil {
		// A mail server's check signs in and sends nothing (ADR 0099).
		return probe.Request{
			Kind: probe.KindSMTP, Private: connection.Private(c),
			SMTP: &probe.SMTP{
				Host: m.GetHost(), Port: connection.Port(m), Security: connection.SecurityWord(connection.Security(m)),
				User: m.GetUser(), Password: m.GetPassword(),
			},
		}
	}
	s3 := c.GetS3()
	return probe.Request{
		Kind: probe.KindS3, Private: connection.Private(c),
		S3: &probe.S3{
			Endpoint: s3.GetEndpoint(), Region: s3.GetRegion(), Bucket: s3.GetBucket(), VirtualHost: !s3.GetPathStyle(),
			Prefix: prefix, AccessKey: s3.GetAccessKeyId(), SecretKey: s3.GetSecretKey(), Presign: presign,
		},
	}
}

// toCheck is the connection a check reaches: as typed, or a saved one with what's
// typed in its place, whose keys go only where they were saved to go.
func (s *Service) toCheck(ctx context.Context, req *dockwellv1.CheckConnectionRequest) (*resourcev1.ConnectionSpec, error) {
	if req.GetS3() != nil && req.GetSmtp() != nil {
		return nil, authapi.Invalid(errors.New("a check reaches S3 storage or a mail server, and this names both. Give one"))
	}
	if req.GetSmtp() != nil || req.GetName() != "" && req.GetS3() == nil && s.savedKind(ctx, req.GetName()) == resourcev1.LinkKind_LINK_KIND_SMTP {
		return s.toCheckMail(ctx, req)
	}
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
		// Checked as it's saved: where someone below an admin saved it, it reaches no
		// private address (#710).
		typed.SavedBelowAdmin = c.Spec.GetSavedBelowAdmin()
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

// savedRef is the ID and revision of the saved connection named, or none where
// there's none: from the names and revisions alone, which opens no secret, so a Check
// decodes the connections once, in toCheck (#799's review, L7).
func (s *Service) savedRef(ctx context.Context, name string) (string, int) {
	if name == "" {
		return "", 0
	}
	all, err := s.connections.Revisions(ctx)
	if err != nil {
		return "", 0
	}
	for _, c := range all {
		if c.Name == name {
			return c.ID, c.Revision
		}
	}
	return "", 0
}

// savedKind is the kind of the saved connection named, or none where there's none.
func (s *Service) savedKind(ctx context.Context, name string) resourcev1.LinkKind {
	all, err := s.connections.All(ctx)
	if err != nil {
		return resourcev1.LinkKind_LINK_KIND_UNSPECIFIED
	}
	for _, c := range all {
		if c.Name == name {
			return c.Spec.GetKind()
		}
	}
	return resourcev1.LinkKind_LINK_KIND_UNSPECIFIED
}

// toCheckMail is toCheck for a mail server: as typed, or a saved one with what's
// typed in its place, whose user and password go only where they were saved to go.
func (s *Service) toCheckMail(ctx context.Context, req *dockwellv1.CheckConnectionRequest) (*resourcev1.ConnectionSpec, error) {
	typed := &resourcev1.ConnectionSpec{Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Smtp: proto.CloneOf(req.GetSmtp())}
	if typed.GetSmtp() == nil {
		typed.Smtp = &resourcev1.ConnectionSmtp{}
	}
	if req.GetPrefix() != "" || req.GetPresign() {
		return nil, authapi.Invalid(errors.New("a mail server's check takes no prefix or presigned URL, which only storage's does"))
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
	i := slices.IndexFunc(all, func(c Saved) bool { return c.Name == req.GetName() })
	if i < 0 {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no connection %s. List them with ListConnections", req.GetName()), "not_found")
	}
	saved := all[i].Spec
	if saved.GetKind() != resourcev1.LinkKind_LINK_KIND_SMTP {
		return nil, authapi.Invalid(fmt.Errorf("the connection %s reaches S3 storage, and this checks a mail server. Give its bucket instead", req.GetName()))
	}
	was, to := saved.GetSmtp(), typed.GetSmtp()
	if req.GetSmtp() == nil {
		to = proto.CloneOf(was)
		to.User, to.Password = "", ""
		typed.Smtp = to
	}
	if to.GetHost() == "" {
		to.Host = was.GetHost()
	}
	if to.GetPort() == 0 && to.GetSecurity() == resourcev1.SmtpSecurity_SMTP_SECURITY_UNSPECIFIED {
		to.Port, to.Security = was.GetPort(), was.GetSecurity()
	}
	// Checked as it's saved: where someone below an admin saved it, it reaches no
	// private address (#710).
	typed.SavedBelowAdmin = saved.GetSavedBelowAdmin()
	if err := (connection.Type{}).Validate(typed); err != nil {
		return nil, authapi.Invalid(err)
	}
	// What's kept goes only where it was saved to go, as an update keeps it.
	if _, err := (connection.Type{}).Keep(saved, typed); err != nil {
		return nil, authapi.Invalid(err)
	}
	return typed, nil
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

// OwnValues are the plain values of each environment, by its ID, but those of
// project's own: an app that a Disconnect left on its own values, or one set up by
// hand, still keeps its files in the bucket they name (slice 2 review, M6). An app
// may go back onto the bucket its own values name, so project's don't count; "" counts
// every one.
func (c *Connections) OwnValues(ctx context.Context, project string) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	err := c.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		mine := map[string]bool{}
		for _, p := range tree.Projects {
			if p.ID == project {
				for _, e := range p.Environments {
					mine[e.ID] = true
				}
			}
		}
		stored, err := tx.ListKind(ctx, string(variable.VariableKind))
		if err != nil {
			return err
		}
		for i := range stored {
			r := &stored[i]
			if r.Spec == nil || mine[r.Scope] {
				continue
			}
			spec, err := c.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				continue // one that can't be read names no bucket here
			}
			if v, ok := spec.(*resourcev1.Variable); ok && v.GetFrom() == nil && v.GetValue() != "" {
				if out[r.Scope] == nil {
					out[r.Scope] = map[string]bool{}
				}
				out[r.Scope][v.GetValue()] = true
			}
		}
		return nil
	})
	return out, err
}

// Holds says some environment's own values name c's endpoint and bucket, so an app
// there keeps its files in that bucket, with no link to say so: the connection serves
// that app (slice 2 review, M6).
func Holds(values map[string]map[string]bool, spec *resourcev1.ConnectionSpec) bool {
	s3 := spec.GetS3()
	if s3.GetEndpoint() == "" || s3.GetBucket() == "" {
		return false
	}
	for _, env := range values {
		if env[s3.GetEndpoint()] && env[s3.GetBucket()] {
			return true
		}
	}
	return false
}
