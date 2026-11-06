// SPDX-License-Identifier: AGPL-3.0-only

// Package services serves dockwell.v1.ServiceService: what each service
// runs, where, and how its latest deploy went, and the plans that act on one
// service as a whole. See docs/adr/0026-the-app-screens.md.
package services

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

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
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets"
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
	keeper  *secrets.Keeper
	logger  *slog.Logger
}

var _ dockwellv1connect.ServiceServiceHandler = (*Handler)(nil)

// New returns a handler that plans through ch and engine, and reads db, opening
// its sealed specs with keeper.
func New(ch *changes.Handler, engine *revision.Engine, db *store.DB, keeper *secrets.Keeper, logger *slog.Logger) *Handler {
	return &Handler{changes: ch, engine: engine, db: db, keeper: keeper, logger: logger}
}

func (h *Handler) internal(ctx context.Context, what string, err error) error {
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, "a service call failed", slog.String("doing", what), slog.String("error", err.Error()))
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
	in := ""
	if m.GetProject() != "" || m.GetEnvironment() != "" {
		_, env, err := h.changes.Environment(ctx, m.GetProject(), m.GetEnvironment())
		if err != nil {
			return nil, err
		}
		in = env.ID
	}
	res := &dockwellv1.ListServicesResponse{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		// One more than asked for says whether there is a next page.
		list, err := tx.PageResources(ctx, string(service.Kind), in, after.ID, size+1)
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
		specs := make([]store.StoredSpec, len(list))
		for i := range list {
			specs[i] = store.StoredSpec{At: store.InRevision(list[i].ID, list[i].Kind, list[i].Revision), Spec: list[i].Spec}
		}
		decoded, unreadable, err := h.keeper.DecodeEach(ctx, tx, specs)
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
			// One row that can't be read costs that row, which says so, and not the list,
			// as every other app still has to be seen, and to be put right.
			if why := unreadable[i]; why != nil {
				h.logger.ErrorContext(ctx, "a service's spec can't be read", slog.String("service", r.ID), slog.String("error", why.Error()))
				info.SpecUnreadable = why.Error()
			} else {
				spec, err := asService(decoded[i])
				if err != nil {
					return err
				}
				info.Spec = Blanked(spec)
			}
			var err error
			if info.Deployments, err = names.deployments(ctx, deployments[r.ID]); err != nil {
				return err
			}
			c := origins[r.ChangeID]
			info.ChangeState = changes.State(c.State)
			if revision.IsRollback(c) {
				info.RolledBackChangeId = c.Reverts
			}
			res.Services = append(res.Services, info)
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "listing services", err)
	}
	return connect.NewResponse(res), nil
}

// asService is a service's spec, read.
func asService(m proto.Message) (*resourcev1.Service, error) {
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
	target, err := h.find(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	records, err := h.engine.History(ctx, target.ID, before, size+1)
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
		return nil, h.internal(ctx, "naming the actors of revisions", err)
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

				MatchesLatest: r.MatchesLatest,
				RolledBackTo:  int64(r.RolledBackTo),
			}
			if r.Decoded != nil {
				spec, err := asService(r.Decoded)
				if err != nil {
					return err
				}
				rev.Image = spec.GetApp().GetImage()
			}
			var err error
			if rev.Deployments, err = names.deployments(ctx, r.Deployments); err != nil {
				return err
			}
			if rev.ServersAdded, err = names.names(ctx, r.ServersAdded); err != nil {
				return err
			}
			if rev.ServersLeft, err = names.names(ctx, r.ServersLeft); err != nil {
				return err
			}
			res.Revisions = append(res.Revisions, rev)
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "reading revisions", err)
	}
	return connect.NewResponse(res), nil
}

// DeployService plans running another image.
func (h *Handler) DeployService(ctx context.Context, req *connect.Request[dockwellv1.DeployServiceRequest]) (*connect.Response[dockwellv1.DeployServiceResponse], error) {
	m := req.Msg
	if m.GetImage() == "" {
		return nil, authapi.Invalid(errors.New("the deploy names no image. Pass one in image, such as ghcr.io/acme/web:1.4.2"))
	}
	intent := &revision.Intent{Kind: revision.IntentDeploy, Image: m.GetImage()}
	c, err := h.edit(ctx, m.GetId(), m.GetProject(), m.GetEnvironment(), intent, func(name string, app *resourcev1.Container) (editing, error) {
		// A tag can move, so naming the one it has means "whatever that is now". Only
		// a digest can't, and the record can name one no server runs, which deploying
		// again puts right, so only what the servers say refuses it.
		running := app.GetImage()
		again := resource.BareImage(running) == m.GetImage() && resource.BareImage(m.GetImage()) == m.GetImage()
		// Off a pinned digest this replaces it whatever the pull finds, so it's a
		// deploy, not the pull that may find nothing.
		if again && running == m.GetImage() {
			intent.Kind = revision.IntentRepull
		}
		app.Image = m.GetImage()
		did := editing{
			summary:   cmp.Or(m.GetSummary(), fmt.Sprintf("Deploy %s to %s", m.GetImage(), name)),
			ifNothing: fault.New(revision.ErrInvalid, "%s already runs %s, which can't change, as a digest names one image. Name another image, such as a new tag", name, m.GetImage()),
			pull:      again,
			run:       true,
		}
		switch {
		case again && running == m.GetImage():
			did.note = revision.Note{Reason: "pulls_again", Message: fmt.Sprintf("Pulls %s again, and replaces %s only if that's a newer image", m.GetImage(), name)}
		case again:
			// The container names the digest, and Docker can't change that in place,
			// so this replaces it whatever the pull finds.
			did.note = revision.Note{Reason: "leaves_pin", Message: fmt.Sprintf("Pulls %s again, and replaces %s with what that names now, in place of the digest it's pinned to", m.GetImage(), name)}
		}
		return did, nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.DeployServiceResponse{Change: c}), nil
}

// AddServicePort plans publishing one more port.
func (h *Handler) AddServicePort(ctx context.Context, req *connect.Request[dockwellv1.AddServicePortRequest]) (*connect.Response[dockwellv1.AddServicePortResponse], error) {
	m := req.Msg
	if m.GetHostPort() == 0 || m.GetContainerPort() == 0 {
		return nil, authapi.Invalid(errors.New("the port needs a number on the server and one in the app. Pass host_port and container_port, such as 8080 and 80"))
	}
	protocol := m.GetProtocol()
	if protocol == resourcev1.Protocol_PROTOCOL_UNSPECIFIED {
		protocol = resourcev1.Protocol_PROTOCOL_TCP
	}
	intent := &revision.Intent{Kind: revision.IntentPublishPort, Port: int(m.GetHostPort())}
	c, err := h.edit(ctx, m.GetId(), m.GetProject(), m.GetEnvironment(), intent, func(name string, app *resourcev1.Container) (editing, error) {
		for _, p := range app.GetPorts() {
			if p.GetHostPort() == m.GetHostPort() && p.GetProtocol() == protocol && p.GetHostIp() == m.GetHostIp() {
				return editing{}, authapi.Invalid(fmt.Errorf("%s already publishes port %d. Choose another port on the server", name, m.GetHostPort()))
			}
		}
		app.Ports = append(app.Ports, &resourcev1.PortMapping{
			HostPort: m.GetHostPort(), ContainerPort: m.GetContainerPort(), Protocol: protocol, HostIp: m.GetHostIp(),
		})
		return editing{summary: cmp.Or(m.GetSummary(), fmt.Sprintf("Publish port %d on %s", m.GetHostPort(), name))}, nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.AddServicePortResponse{Change: c}), nil
}

// editing says what an edit of a service is for, and what to say if its plan
// would do nothing.
type editing struct {
	summary   string
	ifNothing error
	pull      bool
	run       bool
	note      revision.Note
}

// edit plans a change to one service's container, read as it stands on the
// control plane and proposed as any edit, with the rest of its spec as it is.
// So anyone may make it without being able to read a secret back, and what
// the spec holds that no response carries survives. It says what the change is
// for, and refuses what it can't do. A change that lands after the read makes
// it stale, as proposing what was read would quietly undo that change. intent is
// what the plan says it comes to.
func (h *Handler) edit(ctx context.Context, named, project, environment string, intent *revision.Intent, change func(name string, app *resourcev1.Container) (editing, error)) (*dockwellv1.Change, error) {
	r, err := h.find(ctx, named, project, environment)
	if err != nil {
		return nil, err
	}
	var spec *resourcev1.Service
	err = h.db.View(ctx, func(tx *store.Tx) error {
		m, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		spec, err = asService(m)
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "reading a service", err)
	}
	app := spec.GetApp()
	if app == nil {
		return nil, authapi.Invalid(fmt.Errorf("%s doesn't run an image, so it has nothing to change here", r.Name))
	}
	did, err := change(r.Name, app)
	if err != nil {
		return nil, err
	}
	ref := resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}
	var notes []revision.Note
	if did.note.Reason != "" {
		did.note.Ref = ref
		notes = []revision.Note{did.note}
	}
	intent.Resource = r.ID
	return h.changes.Propose(ctx, did.summary, []*dockwellv1.Edit{{
		Resource: resourcepb.Ref(ref),
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: spec}},
	}}, nil, changes.Also{Base: r.Revision, IfNothing: did.ifNothing, Pull: did.pull, Run: did.run, Notes: notes, Intent: intent})
}

// RollBackService plans going back to an earlier revision.
func (h *Handler) RollBackService(ctx context.Context, req *connect.Request[dockwellv1.RollBackServiceRequest]) (*connect.Response[dockwellv1.RollBackServiceResponse], error) {
	m := req.Msg
	if m.GetRevision() <= 0 {
		return nil, authapi.Invalid(errors.New("the rollback names no revision. Pass the number of the revision to go back to, which ListServiceRevisions lists"))
	}
	target, err := h.find(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	c, err := h.changes.Planned(ctx, func(who revision.By) (*revision.Plan, error) {
		return h.engine.RollBackTo(ctx, target.ID, int(m.GetRevision()), func(latest store.Resource, spec proto.Message) (string, []revision.Note) {
			summary := cmp.Or(m.GetSummary(), fmt.Sprintf("Roll back %s to r%d", latest.Name, m.GetRevision()))
			return summary, latestValues(latest, spec)
		}, who)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.RollBackServiceResponse{Change: c}), nil
}

// latestValues is what a plan says of a rollback of a service that uses variables
// or secrets: it puts back which ones it uses, and each is at its latest value, as
// every change pins them (ADR 0026).
func latestValues(latest store.Resource, spec proto.Message) []revision.Note {
	s, ok := spec.(*resourcev1.Service)
	if !ok {
		return nil
	}
	var names []string
	for _, u := range binding.Uses(s) {
		names = append(names, u.As)
	}
	if len(names) == 0 {
		return nil
	}
	return []revision.Note{{
		Reason: "values_latest",
		Message: fmt.Sprintf("%s runs with the latest value of %s. A rollback puts back which variables and secrets it uses, not what they held then. To go back on a value, change it in Variables and secrets",
			latest.Name, human.List(names)),
		Ref:   resource.Ref{Kind: service.Kind, Scope: latest.Scope, Name: latest.Name},
		Names: names,
	}}
}

// find reads the service a request names, by ID or by name. A name may need
// its project or environment to say which, as two Compose files can each have
// a service called db.
func (h *Handler) find(ctx context.Context, named, project, environment string) (store.Resource, error) {
	var out store.Resource
	err := h.db.View(ctx, func(tx *store.Tx) error {
		if named == "" {
			return authapi.Invalid(errors.New("the request names no service. Pass its ID, or its name"))
		}
		if strings.HasPrefix(named, id.Service+"_") {
			r, err := tx.GetResourceByID(ctx, named)
			if err != nil {
				return err
			}
			if r.Kind != string(service.Kind) || r.Spec == nil {
				return store.ErrNotFound
			}
			out = r
			return nil
		}
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		var found []store.Resource
		var where []string
		for _, p := range tree.Projects {
			if project != "" && project != p.ID && project != p.Name {
				continue
			}
			for _, e := range p.Environments {
				if environment != "" && environment != e.ID && environment != e.Name {
					continue
				}
				r, err := tx.GetResource(ctx, string(service.Kind), e.ID, named)
				if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
					continue
				}
				if err != nil {
					return err
				}
				found = append(found, r)
				where = append(where, placeName(tree, p, e))
			}
		}
		switch len(found) {
		case 0:
			return store.ErrNotFound
		case 1:
			out = found[0]
			return nil
		}
		return authapi.ReasonError(connect.CodeInvalidArgument,
			fmt.Errorf("more than one service is named %s: in %s. Pass project or environment to say which, or use its ID", named, human.List(where)), "invalid")
	})
	if err != nil {
		return store.Resource{}, h.readError(ctx, named, err)
	}
	return out, nil
}

// placeName says where a service lives: its project, and its environment once
// its project has more than one to choose from.
func placeName(tree scope.Tree, p scope.Project, e scope.Environment) string {
	if tree.Shown(p.ID).Environment {
		return p.Name + "/" + e.Name
	}
	return p.Name
}

// readError says a service wasn't found, or passes on what the caller is
// told already.
func (h *Handler) readError(ctx context.Context, named string, err error) error {
	var said *connect.Error
	switch {
	case errors.As(err, &said):
		return said
	case errors.Is(err, store.ErrNotFound):
		return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no service %s. It may have been removed, or go by another name; check the list of services", named), "not_found")
	}
	return h.internal(ctx, "reading a service", err)
}

// serverNames names servers, removed ones included, reading each once.
type serverNames struct {
	tx    *store.Tx
	known map[string]string
}

func (n *serverNames) name(ctx context.Context, server string) (string, error) {
	if name, ok := n.known[server]; ok {
		return name, nil
	}
	s, err := n.tx.GetServer(ctx, server)
	switch {
	case errors.Is(err, store.ErrNotFound):
		n.known[server] = server
		return server, nil
	case err != nil:
		return "", err
	}
	n.known[server] = s.Name
	return s.Name, nil
}

func (n *serverNames) names(ctx context.Context, servers []string) ([]string, error) {
	out := make([]string, len(servers))
	for i, s := range servers {
		name, err := n.name(ctx, s)
		if err != nil {
			return nil, err
		}
		out[i] = name
	}
	return out, nil
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
			UpdateTime: timestamppb.New(d.UpdatedAt), Digest: d.Digest,
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
