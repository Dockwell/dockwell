// SPDX-License-Identifier: AGPL-3.0-only

// Package containers serves dockwell.v1.ContainerService: the containers on a
// server that Dockwell didn't make. Each server's agent lists what Docker has as it
// is asked, and only the control plane can tell what of that is its own, as only it
// knows what it runs. See docs/adr/0031-containers-dockwell-didnt-make.md.
package containers

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// callWait bounds a server's answer, as logs does: past it, a person reads why.
const callWait = 15 * time.Second

// Server is a server a request names.
type Server struct {
	ID, Name string
}

// Handler implements ContainerService.
type Handler struct {
	agents  *agentapi.Agents
	changes *changes.Handler
	db      *store.DB
	logger  *slog.Logger
}

var _ dockwellv1connect.ContainerServiceHandler = (*Handler)(nil)

// New returns a handler that lists containers through agents.
func New(agents *agentapi.Agents, ch *changes.Handler, db *store.DB, logger *slog.Logger) *Handler {
	return &Handler{agents: agents, changes: ch, db: db, logger: logger}
}

// AdoptContainer plans taking on a container as a service. It reads what the
// container runs with on its server, and proposes that as the service's spec, which
// keeps the environment in its sensitive field: sealed in the store, and only ever
// said to have changed. The plan goes through the engine like any other, so it is a
// revision, with an undo.
func (h *Handler) AdoptContainer(ctx context.Context, req *connect.Request[dockwellv1.AdoptContainerRequest]) (*connect.Response[dockwellv1.AdoptContainerResponse], error) {
	m := req.Msg
	if m.GetId() == "" {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, errors.New("name a container, by its name or its full ID"), "invalid")
	}
	s, c, err := h.Find(ctx, m.GetServer(), m.GetId())
	if err != nil {
		return nil, err
	}
	_, env, err := h.changes.Environment(ctx, m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	name := cmp.Or(m.GetService(), ServiceName(c))
	if err := (&service.Driver{}).CheckName(name); err != nil {
		return nil, authapi.Invalid(err)
	}
	// An edit of a service that's there would move it here, so a name is taken while
	// it runs anywhere.
	err = h.db.View(ctx, func(tx *store.Tx) error {
		res, err := tx.GetResource(ctx, string(service.Kind), env.ID, name)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err == nil && res.Spec != nil {
			return authapi.Invalid(fmt.Errorf("there's a service %s in this environment already. Choose another name for this one", name))
		}
		return err
	})
	var refusal *connect.Error
	if errors.As(err, &refusal) {
		return nil, err
	}
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	session, ok := h.agents.SessionPastMove(ctx, s.ID, agentapi.MoveGrace)
	if !ok {
		return nil, notConnected(s)
	}
	read, cancel := context.WithTimeout(ctx, callWait)
	defer cancel()
	spec, refused, err := session.Adoption(read, c.GetId(), resource.Ref{Kind: service.Kind, Scope: env.ID, Name: name})
	if err != nil {
		var se *resource.ServerError
		if errors.As(err, &se) {
			err = se.Err
		}
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't read %s now: %w", s.Name, c.GetName(), err), authapi.ReasonUnavailable)
	}
	if len(refused) > 0 {
		return nil, authapi.Invalid(fmt.Errorf("%s runs with %s, which a service can't hold yet, so changing its image would drop them without a word. It stays as it is, and nothing was taken on. Make it again without them, then take it on, or leave it outside Dockwell", c.GetName(), human.List(refused)))
	}
	svc, ok := spec.(*resourcev1.Service)
	if !ok {
		return nil, h.internal(ctx, fmt.Errorf("an agent read %s as %T", c.GetName(), spec))
	}
	edit := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(service.Kind), Scope: env.ID, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}},
		Servers:  []string{s.ID},
	}
	change, err := h.changes.Propose(ctx, fmt.Sprintf("Take on %s as %s", c.GetName(), name), []*dockwellv1.Edit{edit}, nil, changes.Also{
		Adopts: map[string]changes.Adoption{name: {ID: c.GetId(), Name: c.GetName()}},
		Notes: []revision.Note{{Reason: "takes_on", Message: fmt.Sprintf(
			"Takes on %s as it is: it keeps running, with no restart. Changing its image later replaces it, under a name of Dockwell's, and it keeps its volumes. What it wrote anywhere else goes with the old container", c.GetName())}},
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.AdoptContainerResponse{Change: change}), nil
}

// ServiceName is what a service that takes a container on is called unless someone
// says: its Compose service, or its own name, written as a service's name is.
func ServiceName(c *dockwellv1.ServerContainer) string {
	name := strings.ToLower(cmp.Or(c.GetComposeService(), c.GetName()))
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r == '_', r == '.':
			return '-'
		}
		return -1
	}, name)
	out = strings.Trim(out, "-")
	if out == "" || out[0] < 'a' {
		out = "app-" + out
	}
	return strings.TrimRight(out[:min(len(out), 63)], "-")
}

func notConnected(s Server) error {
	return authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("the agent on %s isn't connected, so what Docker runs there can't be read now. Its containers keep running. Check the server; this works again when its agent reconnects", s.Name), authapi.ReasonUnavailable)
}

// ListContainers returns the containers on a server that Dockwell didn't make.
func (h *Handler) ListContainers(ctx context.Context, req *connect.Request[dockwellv1.ListContainersRequest]) (*connect.Response[dockwellv1.ListContainersResponse], error) {
	s, err := h.server(ctx, req.Msg.GetServer())
	if err != nil {
		return nil, err
	}
	found, err := h.read(ctx, s, "")
	if err != nil {
		return nil, err
	}
	others, own, err := h.others(ctx, found.GetContainers())
	if err != nil {
		return nil, err
	}
	slices.SortFunc(others, func(a, b *dockwellv1.ServerContainer) int { return cmp.Compare(a.GetName(), b.GetName()) })
	return connect.NewResponse(&dockwellv1.ListContainersResponse{
		Containers: others, OwnContainers: own + found.GetIngress(), Unlisted: found.GetUnlisted(),
	}), nil
}

// Find returns a container Dockwell didn't make, by its server and its name or full
// ID. What Dockwell runs is read through its service, so a container of one is
// refused, saying so: this is the one way to a container that goes by no service.
func (h *Handler) Find(ctx context.Context, serverName, name string) (Server, *dockwellv1.ServerContainer, error) {
	s, err := h.server(ctx, serverName)
	if err != nil {
		return Server{}, nil, err
	}
	found, err := h.read(ctx, s, name)
	if err != nil {
		return Server{}, nil, err
	}
	if found.GetIngress() > 0 {
		return Server{}, nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("%s runs this server's ingress, which is Dockwell's own, so it isn't among the containers Dockwell didn't make. See what it prints with docker logs %s on the server", name, name), "not_found")
	}
	if len(found.GetContainers()) == 0 {
		return Server{}, nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no container %s on %s. See what's there with dockwell containers list --server %s", name, s.Name, s.Name), "not_found")
	}
	others, _, err := h.others(ctx, found.GetContainers())
	if err != nil {
		return Server{}, nil, err
	}
	if len(others) == 0 {
		c := found.GetContainers()[0]
		return Server{}, nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("%s is the container of %s, which Dockwell runs. Read its logs with dockwell logs read %s", c.GetName(), c.GetComposeService(), c.GetComposeService()), "not_found")
	}
	return s, others[0], nil
}

// server finds the server a request names, or the only one there is.
func (h *Handler) server(ctx context.Context, name string) (Server, error) {
	var servers []store.Server
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		servers, err = tx.ListServers(ctx)
		return err
	})
	if err != nil {
		return Server{}, h.internal(ctx, err)
	}
	if name == "" {
		switch len(servers) {
		case 0:
			return Server{}, authapi.ReasonError(connect.CodeNotFound, errors.New("there's no server yet. Add one first"), "not_found")
		case 1:
			return Server{ID: servers[0].ID, Name: servers[0].Name}, nil
		}
		options := make([]string, len(servers))
		for i := range servers {
			options[i] = servers[i].Name
		}
		choice := &scope.ChoiceError{Level: scope.ServerLevel, Options: options}
		cerr := authapi.ReasonError(connect.CodeInvalidArgument, choice, "choice")
		if d, err := connect.NewErrorDetail(&dockwellv1.Choice{Level: string(choice.Level), Options: options, Field: "server"}); err == nil {
			cerr.AddDetail(d)
		}
		return Server{}, cerr
	}
	for i := range servers {
		if servers[i].ID == name || servers[i].Name == name {
			return Server{ID: servers[i].ID, Name: servers[i].Name}, nil
		}
	}
	return Server{}, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no server %s in the fleet. Check its name with dockwell servers list", name), "not_found")
}

// read asks a server what Docker lists: every container, or the one named.
func (h *Handler) read(ctx context.Context, s Server, name string) (*agentv1.ContainersResult, error) {
	session, ok := h.agents.SessionPastMove(ctx, s.ID, agentapi.MoveGrace)
	if !ok {
		return nil, notConnected(s)
	}
	ctx, cancel := context.WithTimeout(ctx, callWait)
	defer cancel()
	found, err := session.Containers(ctx, name)
	if err != nil {
		var se *resource.ServerError
		if errors.As(err, &se) {
			err = se.Err
		}
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't list its containers now: %w", s.Name, err), authapi.ReasonUnavailable)
	}
	return found, nil
}

// others takes Dockwell's own out of a list, and says how many that was. A container
// is its own when a service or a container of that name was ever managed in the
// container's Compose project: then its service's page shows it, under the labels
// that make it that service's everywhere else in Dockwell. Going by the labels
// alone would hide what another Dockwell left behind, or what someone runs under a
// project of Dockwell's with a service it has never heard of. A one-off from docker
// compose run is no service's, whatever its project.
func (h *Handler) others(ctx context.Context, list []*dockwellv1.ServerContainer) ([]*dockwellv1.ServerContainer, int32, error) {
	if !slices.ContainsFunc(list, mayBeOwn) {
		return list, 0, nil
	}
	known := map[string]bool{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		for _, kind := range []resource.Kind{service.Kind, container.Kind} {
			names, err := tx.EverManaged(ctx, string(kind))
			if err != nil {
				return err
			}
			for _, n := range names {
				known[docker.Project(n.Scope)+"/"+n.Name] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, h.internal(ctx, err)
	}
	var own int32
	others := make([]*dockwellv1.ServerContainer, 0, len(list))
	for _, c := range list {
		if mayBeOwn(c) && known[c.GetComposeProject()+"/"+c.GetComposeService()] {
			own++
			continue
		}
		others = append(others, c)
	}
	return others, own, nil
}

// mayBeOwn says a container is a service of a Compose project, which is what
// Dockwell runs everything as.
func mayBeOwn(c *dockwellv1.ServerContainer) bool {
	return !c.GetOneOff() && c.GetComposeProject() != "" && c.GetComposeService() != ""
}

func (h *Handler) internal(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return connect.NewError(connect.CodeCanceled, ctx.Err())
	}
	h.logger.ErrorContext(ctx, "can't read which containers are Dockwell's own", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read its state; try again, and check its logs if it keeps failing"))
}
