// SPDX-License-Identifier: AGPL-3.0-only

// Package health serves dockwell.v1.HealthService: how services are, from what
// each server's agent last said, and tells the timeline when one turns unhealthy
// or stops running, and when it comes back. See
// docs/adr/0027-health-checks-rollouts-and-rollback.md.
package health

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/paging"
	"github.com/dockwell/dockwell/internal/docker"
	agenthealth "github.com/dockwell/dockwell/internal/health"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Page sizes for ListHealth.
const (
	DefaultPageSize = 200
	MaxPageSize     = 1000
)

// Reports is what each server's agent last said of its services.
type Reports interface {
	Health(server string) (report agenthealth.Report, connected bool)
}

// How old what an agent said may be, and still be said for it. An agent reports at
// least this often, whether or not anything changed, so a report older than this is
// from one that has stopped looking, however connected it is.
const (
	// reportGoodFor is three of an agent's reports.
	reportGoodFor = 3 * agenthealth.DefaultRefresh
	// resultGoodFor is for a state that doesn't say, from an agent that didn't.
	resultGoodFor = 5 * time.Minute
)

// Handler implements HealthService.
type Handler struct {
	db      *store.DB
	reports Reports
	logger  *slog.Logger
	// now is the time. Nil means time.Now.
	now func() time.Time
}

var _ dockwellv1connect.HealthServiceHandler = (*Handler)(nil)

// New returns a handler that reads services from db, and how they are from
// reports.
func New(db *store.DB, reports Reports, logger *slog.Logger) *Handler {
	return &Handler{db: db, reports: reports, logger: logger}
}

// ListHealth returns how each service is.
func (h *Handler) ListHealth(ctx context.Context, req *connect.Request[dockwellv1.ListHealthRequest]) (*connect.Response[dockwellv1.ListHealthResponse], error) {
	m := req.Msg
	size, err := paging.Size(m.GetPageSize(), DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	after, _, err := paging.Read(m.GetPageToken(), "ListHealth")
	if err != nil {
		return nil, err
	}
	res := &dockwellv1.ListHealthResponse{}
	var invalid error
	err = h.db.View(ctx, func(tx *store.Tx) error {
		env, serviceID, err := narrow(ctx, tx, m)
		if err != nil {
			invalid = err
			return nil //nolint:nilerr // the caller's mistake, which goes back to it as one, and not the store's
		}
		// One service, by ID or by its name in its environment.
		if serviceID != "" || m.GetService() != "" {
			var r store.Resource
			if serviceID != "" {
				r, err = tx.GetResourceByID(ctx, serviceID)
			} else {
				r, err = tx.GetResource(ctx, string(service.Kind), env, m.GetService())
			}
			switch {
			case errors.Is(err, store.ErrNotFound) || err == nil && (r.Spec == nil || r.Kind != string(service.Kind)):
				return nil
			case err != nil:
				return err
			}
			s, err := h.serviceHealth(ctx, tx, r.ID, resource.Ref{Kind: service.Kind, Scope: r.Scope, Name: r.Name}, r.Revision)
			if err != nil {
				return err
			}
			res.Services = append(res.Services, s)
			return nil
		}
		// One more than asked for says whether there is a next page.
		list, err := tx.PageResources(ctx, string(service.Kind), env, after.ID, size+1)
		if err != nil {
			return err
		}
		if len(list) > size {
			list = list[:size]
			res.NextPageToken = paging.Token(paging.Cursor{ID: list[size-1].ID})
		}
		for i := range list {
			r := &list[i]
			s, err := h.serviceHealth(ctx, tx, r.ID, resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}, r.Revision)
			if err != nil {
				return err
			}
			res.Services = append(res.Services, s)
		}
		return nil
	})
	if invalid != nil {
		var choice *scope.ChoiceError
		if errors.As(invalid, &choice) {
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, invalid, "choice")
		}
		return nil, authapi.Invalid(invalid)
	}
	if err != nil {
		h.logger.ErrorContext(ctx, "can't list services' health", slog.String("error", err.Error()))
		return nil, connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read its services; try again, and check its logs if it keeps failing"))
	}
	return connect.NewResponse(res), nil
}

// narrow reads which environment and service a request is about, by name or ID.
func narrow(ctx context.Context, tx *store.Tx, m *dockwellv1.ListHealthRequest) (env, serviceID string, err error) {
	if strings.HasPrefix(m.GetService(), id.Service+"_") {
		return "", m.GetService(), nil
	}
	if strings.HasPrefix(m.GetEnvironment(), id.Environment+"_") {
		return m.GetEnvironment(), "", nil
	}
	if m.GetEnvironment() == "" && m.GetProject() == "" && m.GetService() == "" {
		return "", "", nil
	}
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return "", "", err
	}
	_, e, err := tree.Resolve(m.GetProject(), m.GetEnvironment())
	if err != nil {
		return "", "", err
	}
	return e.ID, "", nil
}

// serviceHealth is how one service is, on every server it is placed on.
func (h *Handler) serviceHealth(ctx context.Context, tx *store.Tx, resourceID string, ref resource.Ref, revision int) (*dockwellv1.ServiceHealth, error) {
	out := &dockwellv1.ServiceHealth{Id: resourceID, Resource: resourcepb.Ref(ref)}
	placed, err := tx.Placement(ctx, resourceID, revision)
	if err != nil {
		return nil, err
	}
	project := docker.Project(ref.Scope)
	var states []dockwellv1.HealthState
	for _, server := range placed {
		srv, err := tx.GetServer(ctx, server)
		if err != nil {
			return nil, err
		}
		if srv.RevokedAt != nil {
			continue
		}
		s := h.onServer(server, srv.Name, project, ref.Name)
		out.Servers = append(out.Servers, s)
		states = append(states, s.GetState())
	}
	out.State = Worst(states)
	return out, nil
}

// onServer is how a service is on a server, as far as anyone can say. A connected
// agent isn't enough: stats keep its stream open whatever its monitor is doing, so
// what it said only stands for as long as it says it is good for, from when it was
// known, and until a report that is too old stops counting altogether.
func (h *Handler) onServer(server, name, project, serviceName string) *dockwellv1.ServerHealth {
	report, connected := h.reports.Health(server)
	s := &dockwellv1.ServerHealth{ServerId: server, ServerName: name, Stale: !connected}
	if !report.ScannedAt.IsZero() {
		s.ScannedAt = timestamppb.New(report.ScannedAt)
	}
	for _, i := range report.Instances {
		if i.GetProject() == project && i.GetService() == serviceName {
			s.Instances = append(s.Instances, i)
		}
	}
	now := time.Now()
	if h.now != nil {
		now = h.now()
	}
	fresh := report.ReceivedAt.IsZero() || now.Sub(report.ReceivedAt) <= reportGoodFor
	// An agent that can't read Docker says so, whether or not it ever has.
	if connected && fresh && !report.DockerUnreadableSince.IsZero() {
		s.DockerUnreadableSince = timestamppb.New(report.DockerUnreadableSince)
	}
	switch {
	case !connected || !report.Said:
		s.State = dockwellv1.HealthState_HEALTH_STATE_UNKNOWN
		return s
	case !fresh:
		s.State, s.Stale = dockwellv1.HealthState_HEALTH_STATE_UNKNOWN, true
		return s
	}
	switch {
	case len(s.Instances) == 0 && s.DockerUnreadableSince != nil:
		// What it can't read isn't that nothing runs.
		s.State = dockwellv1.HealthState_HEALTH_STATE_UNKNOWN
		return s
	case len(s.Instances) == 0:
		s.State = dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING
		return s
	}
	for k, i := range s.Instances {
		if tooOld(i, report, now) {
			gone := proto.CloneOf(i)
			gone.State = dockwellv1.HealthState_HEALTH_STATE_UNKNOWN
			s.Instances[k], s.Stale = gone, true
		}
	}
	s.State = Best(s.Instances)
	for _, i := range s.Instances {
		if i.GetState() == s.GetState() && (s.Since == nil || i.GetSince().AsTime().Before(s.GetSince().AsTime())) {
			s.Since = i.GetSince()
		}
	}
	return s
}

// tooOld reports a state last known too long ago to stand. Its age is how old it
// was by the server's clock when the server said it, and how long ago that was by
// this one's, so the two needn't agree. One that says nothing of when it was last
// known goes by its last result.
func tooOld(i *dockwellv1.InstanceHealth, report agenthealth.Report, now time.Time) bool {
	known := i.GetConfirmedAt()
	if known == nil {
		known = i.GetLast().GetTime()
	}
	if known == nil {
		return false
	}
	age := now.Sub(known.AsTime())
	if !report.ReportedAt.IsZero() && !report.ReceivedAt.IsZero() {
		age = report.ReportedAt.Sub(known.AsTime()) + now.Sub(report.ReceivedAt)
	}
	goodFor := resultGoodFor
	if g := i.GetGoodFor(); g != nil {
		goodFor = g.AsDuration()
	}
	return age > goodFor
}

// Best is how a service is on a server, from its containers: how the better of
// them is. Two are there while one takes over from the other, and what serves is
// the one that passed. So a deploy whose new container fails its check, while
// the old one goes on serving, doesn't read as the service being down, as the
// change says what failed.
func Best(instances []*dockwellv1.InstanceHealth) dockwellv1.HealthState {
	best := dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING
	for _, i := range instances {
		if bestFirst[i.GetState()] > bestFirst[best] {
			best = i.GetState()
		}
	}
	return best
}

var bestFirst = map[dockwellv1.HealthState]int{
	dockwellv1.HealthState_HEALTH_STATE_HEALTHY:     6,
	dockwellv1.HealthState_HEALTH_STATE_NO_CHECK:    5,
	dockwellv1.HealthState_HEALTH_STATE_STARTING:    4,
	dockwellv1.HealthState_HEALTH_STATE_UNKNOWN:     3,
	dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY:   2,
	dockwellv1.HealthState_HEALTH_STATE_PAUSED:      1,
	dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING: 0,
}

// Worst is how a service is as a whole, from its servers: a service is only as
// well as its worst, and with none, nobody can say.
func Worst(states []dockwellv1.HealthState) dockwellv1.HealthState {
	if len(states) == 0 {
		return dockwellv1.HealthState_HEALTH_STATE_UNKNOWN
	}
	worst := states[0]
	for _, s := range states[1:] {
		if worstFirst[s] > worstFirst[worst] {
			worst = s
		}
	}
	return worst
}

var worstFirst = map[dockwellv1.HealthState]int{
	dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY:   7,
	dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING: 6,
	dockwellv1.HealthState_HEALTH_STATE_PAUSED:      5,
	dockwellv1.HealthState_HEALTH_STATE_UNKNOWN:     4,
	dockwellv1.HealthState_HEALTH_STATE_STARTING:    3,
	dockwellv1.HealthState_HEALTH_STATE_NO_CHECK:    2,
	dockwellv1.HealthState_HEALTH_STATE_HEALTHY:     1,
}

// placedChunk bounds the services one read of their placements names, under what
// either database takes in one query.
const placedChunk = 500

// Placed returns how every service is on each server it's placed on, as ListHealth
// says it, reading the store a few times however many there are, for what watches
// them all, such as alerts.
func (h *Handler) Placed(ctx context.Context) ([]*dockwellv1.ServiceHealth, error) {
	var out []*dockwellv1.ServiceHealth
	err := h.db.View(ctx, func(tx *store.Tx) error {
		list, err := tx.ListKind(ctx, string(service.Kind))
		if err != nil {
			return err
		}
		servers, err := tx.ListServers(ctx)
		if err != nil {
			return err
		}
		byID := make(map[string]*store.Server, len(servers))
		for i := range servers {
			byID[servers[i].ID] = &servers[i]
		}
		for start := 0; start < len(list); start += placedChunk {
			chunk := list[start:min(start+placedChunk, len(list))]
			ids := make([]string, len(chunk))
			for i := range chunk {
				ids[i] = chunk[i].ID
			}
			placed, err := tx.LatestDeployments(ctx, ids)
			if err != nil {
				return err
			}
			for i := range chunk {
				r := &chunk[i]
				s := &dockwellv1.ServiceHealth{Id: r.ID, Resource: resourcepb.Ref(resource.Ref{Kind: service.Kind, Scope: r.Scope, Name: r.Name})}
				project := docker.Project(r.Scope)
				var states []dockwellv1.HealthState
				for j := range placed[r.ID] {
					d := &placed[r.ID][j]
					srv := byID[d.Server]
					if srv == nil || srv.RevokedAt != nil {
						continue
					}
					on := h.onServer(d.Server, srv.Name, project, r.Name)
					s.Servers = append(s.Servers, on)
					states = append(states, on.GetState())
				}
				s.State = Worst(states)
				out = append(out, s)
			}
		}
		return nil
	})
	return out, err
}
