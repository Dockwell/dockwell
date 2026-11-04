// SPDX-License-Identifier: AGPL-3.0-only

// Package services serves dockwell.v1.ServiceService: what each service
// runs, where, and how its latest deploy went, and the plans that act on one
// service as a whole. See docs/adr/0026-the-app-screens.md.
package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/paging"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// Page sizes.
const (
	DefaultPageSize         = 200
	MaxPageSize             = 1000
	DefaultRevisionPageSize = 20
	MaxRevisionPageSize     = 100
)

// Handler implements ServiceService.
type Handler struct {
	changes *changes.Handler
	engine  *revision.Engine
	db      *store.DB
	logger  *slog.Logger
}

var _ dockwellv1connect.ServiceServiceHandler = (*Handler)(nil)

// New returns a handler that plans through ch and engine, and reads db.
func New(ch *changes.Handler, engine *revision.Engine, db *store.DB, logger *slog.Logger) *Handler {
	return &Handler{changes: ch, engine: engine, db: db, logger: logger}
}

func (h *Handler) internal(ctx context.Context, what string, err error) error {
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, what, slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read its services; try again, and check its logs if it keeps failing"))
}

// ListServices returns a page of the services that exist now.
func (h *Handler) ListServices(ctx context.Context, req *connect.Request[dockwellv1.ListServicesRequest]) (*connect.Response[dockwellv1.ListServicesResponse], error) {
	m := req.Msg
	size, err := paging.Size(m.GetPageSize(), DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	after, _, err := paging.Read(m.GetPageToken(), "ListServices")
	if err != nil {
		return nil, err
	}
	scope := ""
	if m.GetProject() != "" || m.GetEnvironment() != "" {
		_, env, err := h.changes.Environment(ctx, m.GetProject(), m.GetEnvironment())
		if err != nil {
			return nil, err
		}
		scope = env.ID
	}
	res := &dockwellv1.ListServicesResponse{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		// One more than asked for says whether there is a next page.
		list, err := tx.PageResources(ctx, string(service.Kind), scope, after.ID, size+1)
		if err != nil {
			return err
		}
		if len(list) > size {
			list = list[:size]
			res.NextPageToken = paging.Token(paging.Cursor{ID: list[size-1].ID})
		}
		ids := make([]string, len(list))
		changeIDs := make([]string, len(list))
		for i := range list {
			ids[i], changeIDs[i] = list[i].ID, list[i].ChangeID
		}
		deployments, err := tx.LatestDeployments(ctx, ids)
		if err != nil {
			return err
		}
		origins, err := tx.ChangeOrigins(ctx, changeIDs)
		if err != nil {
			return err
		}
		names := serverNames{tx: tx, known: map[string]string{}}
		for i := range list {
			r := &list[i]
			info := &dockwellv1.ServiceInfo{
				Id:         r.ID,
				Resource:   resourcepb.Ref(resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}),
				Revision:   int64(r.Revision),
				UpdateTime: timestamppb.New(r.UpdatedAt),
				ChangeId:   r.ChangeID,
			}
			spec, err := h.spec(r.Spec)
			if err != nil {
				return err
			}
			info.Spec = Blanked(spec)
			if info.Deployments, err = names.deployments(ctx, deployments[r.ID]); err != nil {
				return err
			}
			if c := origins[r.ChangeID]; revision.IsRollback(c) {
				info.RolledBackChangeId = c.Reverts
			}
			res.Services = append(res.Services, info)
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "can't list services", err)
	}
	return connect.NewResponse(res), nil
}

// spec reads a service's stored spec.
func (h *Handler) spec(encoded []byte) (*resourcev1.Service, error) {
	m, err := h.engine.Kinds().Decode(service.Kind, encoded)
	if err != nil {
		return nil, err
	}
	s, ok := m.(*resourcev1.Service)
	if !ok {
		return nil, fmt.Errorf("a service's spec is a %T", m)
	}
	return s, nil
}

// ListServiceRevisions returns a page of a service's revisions, newest
// first.
func (h *Handler) ListServiceRevisions(ctx context.Context, req *connect.Request[dockwellv1.ListServiceRevisionsRequest]) (*connect.Response[dockwellv1.ListServiceRevisionsResponse], error) {
	m := req.Msg
	size, err := paging.Size(m.GetPageSize(), DefaultRevisionPageSize, MaxRevisionPageSize)
	if err != nil {
		return nil, err
	}
	after, _, err := paging.Read(m.GetPageToken(), "ListServiceRevisions")
	if err != nil {
		return nil, err
	}
	before := 0
	if after.ID != "" {
		if before, err = strconv.Atoi(after.ID); err != nil {
			return nil, authapi.Invalid(errors.New("page_token isn't one ListServiceRevisions gave. Pass next_page_token from the page before, or leave it out to start with the latest"))
		}
	}
	records, err := h.engine.History(ctx, m.GetId(), before, size+1)
	if err != nil {
		return nil, h.changes.Error(ctx, err)
	}
	res := &dockwellv1.ListServiceRevisionsResponse{}
	if len(records) > size {
		records = records[:size]
		res.NextPageToken = paging.Token(paging.Cursor{ID: strconv.Itoa(records[size-1].Revision.Revision)})
	}
	made := make([]*revision.Record, len(records))
	for i := range records {
		made[i] = records[i].Record
	}
	protos, err := h.changes.Protos(ctx, made...)
	if err != nil {
		return nil, h.internal(ctx, "can't name the actors of revisions", err)
	}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		names := serverNames{tx: tx, known: map[string]string{}}
		for i := range records {
			r := &records[i]
			rev := &dockwellv1.ServiceRevision{
				Number:     int64(r.Revision.Revision),
				CreateTime: timestamppb.New(r.CreatedAt),
				Deleted:    r.Spec == nil,
				Change:     protos[i],
				Changes:    resourcepb.Changes(r.Changes),
			}
			if r.Spec != nil {
				spec, err := h.spec(r.Spec)
				if err != nil {
					return err
				}
				rev.Image = spec.GetApp().GetImage()
			}
			var err error
			if rev.Deployments, err = names.deployments(ctx, r.Deployments); err != nil {
				return err
			}
			res.Revisions = append(res.Revisions, rev)
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "can't read revisions", err)
	}
	return connect.NewResponse(res), nil
}

// DeployService plans running another image.
func (h *Handler) DeployService(ctx context.Context, req *connect.Request[dockwellv1.DeployServiceRequest]) (*connect.Response[dockwellv1.DeployServiceResponse], error) {
	m := req.Msg
	if m.GetImage() == "" {
		return nil, authapi.Invalid(errors.New("the deploy names no image. Pass one in image, such as ghcr.io/acme/web:1.4.2"))
	}
	var edit *dockwellv1.Edit
	var name string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResourceByID(ctx, m.GetId())
		if err != nil {
			return err
		}
		name = r.Name
		if r.Kind != string(service.Kind) || r.Spec == nil {
			return store.ErrNotFound
		}
		spec, err := h.spec(r.Spec)
		if err != nil {
			return err
		}
		app := spec.GetApp()
		if app.GetImage() == m.GetImage() {
			return authapi.Invalid(fmt.Errorf("%s already runs %s. Name another image, such as a new tag", r.Name, m.GetImage()))
		}
		app.Image = m.GetImage()
		edit = &dockwellv1.Edit{
			Resource: resourcepb.Ref(resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}),
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: spec}},
		}
		return nil
	})
	if err != nil {
		return nil, h.readError(ctx, m.GetId(), err)
	}
	summary := m.GetSummary()
	if summary == "" {
		summary = fmt.Sprintf("Deploy %s to %s", m.GetImage(), name)
	}
	c, err := h.changes.Propose(ctx, summary, []*dockwellv1.Edit{edit}, nil)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.DeployServiceResponse{Change: c}), nil
}

// RollBackService plans going back to an earlier revision.
func (h *Handler) RollBackService(ctx context.Context, req *connect.Request[dockwellv1.RollBackServiceRequest]) (*connect.Response[dockwellv1.RollBackServiceResponse], error) {
	m := req.Msg
	if m.GetRevision() <= 0 {
		return nil, authapi.Invalid(errors.New("the rollback names no revision. Pass the number of the revision to go back to, which ListServiceRevisions lists"))
	}
	if !h.isService(ctx, m.GetId()) {
		return nil, h.readError(ctx, m.GetId(), store.ErrNotFound)
	}
	c, err := h.changes.Planned(ctx, func(who revision.By) (*revision.Plan, error) {
		return h.engine.RollBackTo(ctx, m.GetId(), int(m.GetRevision()), func(latest store.Resource) string {
			if m.GetSummary() != "" {
				return m.GetSummary()
			}
			return fmt.Sprintf("Roll back %s to r%d", latest.Name, m.GetRevision())
		}, who)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.RollBackServiceResponse{Change: c}), nil
}

func (h *Handler) isService(ctx context.Context, id string) bool {
	var kind string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResourceByID(ctx, id)
		kind = r.Kind
		return err
	})
	return err == nil && kind == string(service.Kind)
}

// readError says a service wasn't found, or passes on what the caller is
// told already.
func (h *Handler) readError(ctx context.Context, id string, err error) error {
	var said *connect.Error
	switch {
	case errors.As(err, &said):
		return said
	case errors.Is(err, store.ErrNotFound):
		return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no service %s. It may have been removed; list what exists with ListServices", id), "not_found")
	}
	return h.internal(ctx, "can't read a service", err)
}

// serverNames names servers, removed ones included, reading each once.
type serverNames struct {
	tx    *store.Tx
	known map[string]string
}

func (n *serverNames) name(ctx context.Context, id string) (string, error) {
	if name, ok := n.known[id]; ok {
		return name, nil
	}
	s, err := n.tx.GetServer(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		n.known[id] = id
		return id, nil
	case err != nil:
		return "", err
	}
	n.known[id] = s.Name
	return s.Name, nil
}

var deploymentStates = map[string]dockwellv1.DeploymentState{
	store.DeploymentPending:    dockwellv1.DeploymentState_DEPLOYMENT_STATE_PENDING,
	store.DeploymentLive:       dockwellv1.DeploymentState_DEPLOYMENT_STATE_LIVE,
	store.DeploymentFailed:     dockwellv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
	store.DeploymentSuperseded: dockwellv1.DeploymentState_DEPLOYMENT_STATE_SUPERSEDED,
}

func (n *serverNames) deployments(ctx context.Context, list []store.Deployment) ([]*dockwellv1.ServiceDeployment, error) {
	out := make([]*dockwellv1.ServiceDeployment, len(list))
	for i := range list {
		d := &list[i]
		name, err := n.name(ctx, d.Server)
		if err != nil {
			return nil, err
		}
		out[i] = &dockwellv1.ServiceDeployment{
			ServerId: d.Server, ServerName: name, State: deploymentStates[d.State], Error: d.Error,
			UpdateTime: timestamppb.New(d.UpdatedAt),
		}
	}
	return out, nil
}

// Blanked is a copy of a service's spec with each sensitive value blanked. A
// sensitive map keeps its keys, so an environment variable shows by name.
func Blanked(spec *resourcev1.Service) *resourcev1.Service {
	out := proto.CloneOf(spec)
	blank(out.ProtoReflect())
	return out
}

func blank(m protoreflect.Message) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case resource.IsSensitive(fd) && fd.IsMap():
			blankValues(fd, v.Map())
		case resource.IsSensitive(fd):
			m.Clear(fd)
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, e protoreflect.Value) bool {
					blank(e.Message())
					return true
				})
			}
		case fd.IsList():
			if fd.Message() != nil {
				for i := range v.List().Len() {
					blank(v.List().Get(i).Message())
				}
			}
		case fd.Message() != nil:
			blank(v.Message())
		}
		return true
	})
}

// blankValues keeps a map's keys and blanks its values.
func blankValues(fd protoreflect.FieldDescriptor, values protoreflect.Map) {
	var keys []protoreflect.MapKey
	values.Range(func(k protoreflect.MapKey, _ protoreflect.Value) bool {
		keys = append(keys, k)
		return true
	})
	for _, k := range keys {
		if fd.MapValue().Message() != nil {
			values.Set(k, values.NewValue())
			continue
		}
		values.Set(k, fd.MapValue().Default())
	}
}
