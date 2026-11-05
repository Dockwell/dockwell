// SPDX-License-Identifier: AGPL-3.0-only

// Package composeapi serves dockwell.v1.ComposeService: a Compose file
// planned into a project of its own as a change, and an environment, or one
// service, exported as a Compose file. See
// docs/adr/0021-the-docker-driver.md and docs/adr/0026-the-app-screens.md.
package composeapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/services"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Handler implements ComposeService.
type Handler struct {
	changes *changes.Handler
	engine  *revision.Engine
	db      *store.DB
	keeper  *secrets.Keeper
	logger  *slog.Logger
}

var _ dockwellv1connect.ComposeServiceHandler = (*Handler)(nil)

// New returns a handler that plans through ch and engine, and reads specs from
// db, opening the sealed ones with keeper.
func New(ch *changes.Handler, engine *revision.Engine, db *store.DB, keeper *secrets.Keeper, logger *slog.Logger) *Handler {
	return &Handler{changes: ch, engine: engine, db: db, keeper: keeper, logger: logger}
}

func invalid(err error) error {
	return authapi.ReasonError(connect.CodeInvalidArgument, errors.New(compose.Message(err)), "invalid")
}

// ImportCompose plans a Compose file into a project of its own.
func (h *Handler) ImportCompose(ctx context.Context, req *connect.Request[dockwellv1.ImportComposeRequest]) (*connect.Response[dockwellv1.ImportComposeResponse], error) {
	m := req.Msg
	if strings.TrimSpace(m.GetCompose()) == "" {
		return nil, invalid(errors.New("the request has no Compose file. Pass one in compose, such as --compose \"$(cat compose.yaml)\""))
	}
	stack, err := compose.Parse(ctx, []byte(m.GetCompose()), m.GetVariables())
	if err != nil {
		switch {
		case errors.Is(err, compose.ErrInvalid):
			return nil, invalid(err)
		case errors.Is(err, compose.ErrBusy):
			return nil, connect.NewError(connect.CodeResourceExhausted, err)
		}
		return nil, err
	}
	place, err := h.destination(ctx, m.GetProject(), m.GetEnvironment(), stack.Name)
	if err != nil {
		return nil, err
	}
	env := place.environment
	if err := h.useVariables(ctx, env, stack); err != nil {
		return nil, h.internal(ctx, err)
	}
	var edits []*dockwellv1.Edit
	keep := map[string]bool{}
	add := func(kind resource.Kind, name string, spec *resourcev1.Spec) {
		keep[string(kind)+"/"+name] = true
		edits = append(edits, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(kind), Scope: env, Name: name},
			Spec:     spec, Servers: m.GetServers(),
		})
	}
	for _, n := range stack.Networks {
		add(network.Kind, n.Name, &resourcev1.Spec{Kind: &resourcev1.Spec_Network{Network: n.Spec}})
	}
	for _, v := range stack.Volumes {
		add(volume.Kind, v.Name, &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: v.Spec}})
	}
	for _, s := range stack.Services {
		add(service.Kind, s.Name, &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: s.Spec}})
	}
	if m.GetRemoveOrphans() {
		// Deletions run last, in reverse: services go before the networks
		// they were on. Volumes stay, as docker compose keeps them.
		for _, kind := range []resource.Kind{network.Kind, service.Kind} {
			var orphans []string
			err := h.db.View(ctx, func(tx *store.Tx) error {
				list, err := tx.ListResources(ctx, env, string(kind))
				for _, r := range list {
					if !keep[string(kind)+"/"+r.Name] {
						orphans = append(orphans, r.Name)
					}
				}
				return err
			})
			if err != nil {
				return nil, h.internal(ctx, err)
			}
			slices.Sort(orphans)
			for _, name := range orphans {
				edits = append(edits, &dockwellv1.Edit{
					Resource: &dockwellv1.ResourceRef{Kind: string(kind), Scope: env, Name: name}, Delete: true,
				})
			}
		}
	}
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		summary = "Run a Compose file"
		if place.project != scope.DefaultProject {
			summary = fmt.Sprintf("Run %s from a Compose file", place.project)
		}
	}
	c, err := h.changes.Propose(ctx, summary, edits, func(choice *scope.ChoiceError, _ string) string {
		if choice.Level == scope.ServerLevel {
			return "servers"
		}
		return ""
	}, changes.Also{Edits: place.made, Notes: exposedNotes(env, stack.Exposed)})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ImportComposeResponse{Change: c, ExposedVariables: exposedProtos(stack.Exposed)}), nil
}

// destination is where a Compose file goes.
type destination struct {
	project     string // its project's name
	environment string // its environment's ID, which may not exist yet
	made        []revision.Edit
}

// destination says where a file goes (ADR 0026): an environment named by ID;
// else the project a request or the file names, brought to life if it isn't
// there; else the default project, while that has no apps, as a file with
// nothing to say otherwise could change what is there.
func (h *Handler) destination(ctx context.Context, project, environment, fileName string) (destination, error) {
	var out destination
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		place := func(p scope.Project, e scope.Environment) {
			out.project, out.environment = p.Name, e.ID
		}
		named := project
		switch {
		case strings.HasPrefix(environment, id.Environment+"_"):
			p, e, err := tree.Resolve(project, environment)
			if err != nil {
				return err
			}
			place(p, e)
			return nil
		case named == "" && isExported(fileName):
			// The name an export gives a file is its environment's Compose
			// project, so docker compose finds what Dockwell runs.
			p, e, ok := exportedFrom(tree, fileName)
			if !ok {
				return invalid(fmt.Errorf("the Compose file's name, %s, is an environment's Compose project, and no environment here has it, so it was exported from elsewhere. Change name: in the file, or pass project, to say where it goes", fileName))
			}
			place(p, e)
			return nil
		case named == "" && fileName != "":
			if err := (scope.ProjectType{}).CheckName(fileName); err != nil {
				return invalid(fmt.Errorf("the Compose file names itself %s, which can't name a project: %w. Change name: in the file, or pass project", fileName, err))
			}
			named = fileName
		}
		if named == "" {
			return h.intoDefault(ctx, tx, tree, environment, &out)
		}
		known := slices.ContainsFunc(tree.Projects, func(p scope.Project) bool { return p.ID == named || p.Name == named })
		if known || strings.HasPrefix(named, id.Project+"_") {
			p, e, err := tree.Resolve(named, environment)
			if err != nil {
				return err
			}
			place(p, e)
			return nil
		}
		// The change makes the project, and what lives in it, in one go.
		made, env, err := h.engine.NewProject(ctx, tx, tree.Organization.ID, named, environment)
		if err != nil {
			return err
		}
		out = destination{project: named, environment: env, made: made}
		return nil
	})
	var said *connect.Error
	if errors.As(err, &said) {
		return out, said
	}
	if err != nil {
		var choice *scope.ChoiceError
		if errors.As(err, &choice) || errors.Is(err, scope.ErrNotFound) {
			// Environment says these with the request's field names.
			_, _, err = h.changes.Environment(ctx, project, environment)
			return out, err
		}
		return out, h.changes.Error(ctx, err)
	}
	return out, nil
}

// intoDefault puts a file that names no project in the default one, or refuses
// it once that holds apps a service of the same name would change.
func (h *Handler) intoDefault(ctx context.Context, tx *store.Tx, tree scope.Tree, environment string, out *destination) error {
	p, e, err := tree.Resolve(scope.DefaultProject, environment)
	if errors.Is(err, scope.ErrNotFound) && len(tree.Projects) == 1 {
		// The default was renamed: it is still the only one.
		p, e, err = tree.Resolve("", environment)
	}
	if errors.Is(err, scope.ErrNotFound) {
		return needsProject("the Compose file names no project, and there's no default project for it")
	}
	if err != nil {
		return err
	}
	var apps []string
	for _, env := range p.Environments {
		list, err := tx.ListResources(ctx, env.ID, string(service.Kind))
		if err != nil {
			return err
		}
		for i := range list {
			apps = append(apps, list[i].Name)
		}
	}
	if len(apps) > 0 {
		slices.Sort(apps)
		return needsProject(fmt.Sprintf("the Compose file names no project, and %s already holds %s, which a service of the same name would change", p.Name, human.List(apps)))
	}
	out.project, out.environment = p.Name, e.ID
	return nil
}

// needsProject refuses a file with nowhere of its own to go.
func needsProject(why string) error {
	return authapi.ReasonError(connect.CodeInvalidArgument,
		fmt.Errorf("%s. Give it a project of its own: pass project, or add name: to the file, such as name: shop. To change what is there on purpose, pass project %s", why, scope.DefaultProject),
		"needs_project")
}

// isExported reports the name an export gives a file: its environment's
// Compose project, which isn't a name a project can have.
func isExported(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), id.Environment+"_")
}

// exportedFrom finds the environment whose Compose project a name is.
func exportedFrom(tree scope.Tree, name string) (scope.Project, scope.Environment, bool) {
	for _, p := range tree.Projects {
		for _, e := range p.Environments {
			if docker.Project(e.ID) == strings.ToLower(name) {
				return p, e, true
			}
		}
	}
	return scope.Project{}, scope.Environment{}, false
}

// exposedProtos are where a file writes a value in plain sight, for the schema.
func exposedProtos(exposed []compose.Exposed) []*dockwellv1.ExposedVariable {
	out := make([]*dockwellv1.ExposedVariable, len(exposed))
	for i, e := range exposed {
		out[i] = &dockwellv1.ExposedVariable{Name: e.Variable, Service: e.Service, Field: e.Field}
	}
	return out
}

// exposedNotes are what a plan says of them: the fact, what it means, and how
// to keep the value hidden.
func exposedNotes(env string, exposed []compose.Exposed) []revision.Note {
	out := make([]revision.Note, len(exposed))
	for i, e := range exposed {
		field := e.Field
		if field == "healthcheck" {
			field = "health check"
		}
		out[i] = revision.Note{
			Reason: "exposed_variable",
			Message: fmt.Sprintf("%s's %s holds the value of %s, and anyone who can read this app can see it. To keep it hidden, pass it in environment, and use $$%s in the %s",
				e.Service, field, e.Variable, e.Variable, field),
			Ref:   resource.Ref{Kind: service.Kind, Scope: env, Name: e.Service},
			Names: []string{e.Variable, e.Field},
		}
	}
	return out
}

// CheckCompose reads a Compose file as ImportCompose would, and says what
// it would make or why it can't, with nothing planned or recorded.
func (h *Handler) CheckCompose(ctx context.Context, req *connect.Request[dockwellv1.CheckComposeRequest]) (*connect.Response[dockwellv1.CheckComposeResponse], error) {
	m := req.Msg
	res := &dockwellv1.CheckComposeResponse{}
	file := []byte(m.GetCompose())
	if strings.TrimSpace(m.GetCompose()) == "" {
		return connect.NewResponse(res), nil
	}
	// The guard reads a file first, so nothing here reads one made to cost too
	// much: it says nothing of itself, and only why it was stopped.
	seen, stack, err := compose.Check(ctx, file, m.GetVariables())
	if err != nil && !errors.Is(err, compose.ErrInvalid) {
		if errors.Is(err, compose.ErrBusy) {
			return nil, connect.NewError(connect.CodeResourceExhausted, err)
		}
		return nil, h.internal(ctx, err)
	}
	for _, v := range seen.Variables {
		res.Variables = append(res.Variables, &dockwellv1.ComposeVariable{Name: v.Name, Required: v.Required})
	}
	res.ExposedVariables = exposedProtos(seen.Exposed)
	name, nameErr := h.fileName(ctx, seen.Name)
	if nameErr != nil {
		return nil, h.internal(ctx, nameErr)
	}
	res.Name = name
	refuse := func(err error) (*connect.Response[dockwellv1.CheckComposeResponse], error) {
		res.Refusal = &dockwellv1.ComposeRefusal{Message: compose.Message(err), Line: int32(compose.Line(ctx, file, err))} //nolint:gosec // a line of a file under compose.MaxFile
		return connect.NewResponse(res), nil
	}
	if err != nil {
		return refuse(err)
	}
	// What a change's edits go through when they are proposed: the rules on
	// images, ports and server paths are the drivers', not the file's.
	for _, s := range stack.Services {
		if err := h.engine.Kinds().Validate(service.Kind, s.Spec); err != nil {
			return refuse(fmt.Errorf("%w: the service %s isn't valid: %w", compose.ErrInvalid, s.Name, err))
		}
	}
	for _, v := range stack.Volumes {
		if err := h.engine.Kinds().Validate(volume.Kind, v.Spec); err != nil {
			return refuse(fmt.Errorf("%w: the volume %s isn't valid: %w", compose.ErrInvalid, v.Name, err))
		}
		res.Volumes = append(res.Volumes, v.Name)
	}
	for _, n := range stack.Networks {
		if err := h.engine.Kinds().Validate(network.Kind, n.Spec); err != nil {
			return refuse(fmt.Errorf("%w: the network %s isn't valid: %w", compose.ErrInvalid, n.Name, err))
		}
		res.Networks = append(res.Networks, n.Name)
	}
	for _, s := range stack.Services {
		res.Services = append(res.Services, &dockwellv1.ServiceInfo{
			Resource: &dockwellv1.ResourceRef{Kind: string(service.Kind), Name: s.Name},
			Spec:     services.Blanked(s.Spec),
		})
	}
	return connect.NewResponse(res), nil
}

// fileName is the project a file's name: says it belongs to, for a form: an
// export's name is its environment's project, and one from elsewhere says
// nothing.
func (h *Handler) fileName(ctx context.Context, name string) (string, error) {
	if !isExported(name) {
		return name, nil
	}
	out := ""
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		if p, _, ok := exportedFrom(tree, name); ok {
			out = p.Name
		}
		return nil
	})
	return out, err
}

// ExportCompose writes an environment, or one service, as a Compose file.
func (h *Handler) ExportCompose(ctx context.Context, req *connect.Request[dockwellv1.ExportComposeRequest]) (*connect.Response[dockwellv1.ExportComposeResponse], error) {
	m := req.Msg
	_, env, err := h.changes.Environment(ctx, m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	stack := &compose.Stack{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if stack.Variables, err = h.variables(ctx, tx, env.ID); err != nil {
			return err
		}
		for _, kind := range []resource.Kind{service.Kind, volume.Kind, network.Kind} {
			list, err := tx.ListResources(ctx, env.ID, string(kind))
			if err != nil {
				return err
			}
			for _, r := range list {
				spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
				if err != nil {
					return err
				}
				switch s := spec.(type) {
				case *resourcev1.Service:
					stack.Services = append(stack.Services, compose.Item[*resourcev1.Service]{Name: r.Name, Spec: s})
				case *resourcev1.Volume:
					stack.Volumes = append(stack.Volumes, compose.Item[*resourcev1.Volume]{Name: r.Name, Spec: s})
				case *resourcev1.Network:
					stack.Networks = append(stack.Networks, compose.Item[*resourcev1.Network]{Name: r.Name, Spec: s})
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	if name := m.GetService(); name != "" {
		if stack, err = only(stack, name, env.Name); err != nil {
			return nil, authapi.ReasonError(connect.CodeNotFound, err, "not_found")
		}
	}
	out, err := compose.Export(docker.Project(env.ID), stack)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.ExportComposeResponse{Compose: string(out.File), Omitted: out.Omitted}), nil
}

// variables are the values of an environment's variables, by name. A
// variable's spec is never sealed, and nothing here reads a secret's.
func (h *Handler) variables(ctx context.Context, tx *store.Tx, envID string) (map[string]string, error) {
	list, err := tx.ListResources(ctx, envID, string(variable.VariableKind))
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list))
	for _, r := range list {
		spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, err
		}
		if v, ok := spec.(*resourcev1.Variable); ok {
			out[r.Name] = v.GetValue()
		}
	}
	return out, nil
}

// useVariables takes out of a file's environment the values an export
// wrote for the variables a service uses, which are the environment's to
// hold, as the service uses them by reference. A value that differs stays,
// so the driver refuses it as set twice rather than drop what the file
// says. A secret's is never compared: a file that gives one a value is
// refused the same way, whatever it is.
func (h *Handler) useVariables(ctx context.Context, envID string, stack *compose.Stack) error {
	if !slices.ContainsFunc(stack.Services, func(s compose.Item[*resourcev1.Service]) bool { return len(s.Spec.GetVariables()) > 0 }) {
		return nil
	}
	var values map[string]string
	err := h.db.View(ctx, func(tx *store.Tx) (err error) {
		values, err = h.variables(ctx, tx, envID)
		return err
	})
	if err != nil {
		return err
	}
	for _, s := range stack.Services {
		app := s.Spec.GetApp()
		for as, name := range s.Spec.GetVariables() {
			if v, ok := app.GetEnv()[as]; ok && values[name] == v {
				delete(app.Env, as)
			}
		}
		if len(app.GetEnv()) == 0 {
			app.Env = nil
		}
	}
	return nil
}

// only narrows a stack to one service, with the volumes and networks it
// uses.
func only(stack *compose.Stack, name, env string) (*compose.Stack, error) {
	i := slices.IndexFunc(stack.Services, func(s compose.Item[*resourcev1.Service]) bool { return s.Name == name })
	if i < 0 {
		names := make([]string, len(stack.Services))
		for j, s := range stack.Services {
			names[j] = s.Name
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("the environment %s has no services yet, so it has no service %s", env, name)
		}
		return nil, fmt.Errorf("the environment %s has no service %s. Its services are %s", env, name, strings.Join(names, ", "))
	}
	svc := stack.Services[i]
	app := svc.Spec.GetApp()
	out := &compose.Stack{Services: []compose.Item[*resourcev1.Service]{{Name: svc.Name, Spec: proto.CloneOf(svc.Spec)}}, Variables: stack.Variables}
	for _, v := range stack.Volumes {
		if slices.ContainsFunc(app.GetMounts(), func(m *resourcev1.Mount) bool { return m.GetVolume() == v.Name }) {
			out.Volumes = append(out.Volumes, v)
		}
	}
	for _, n := range stack.Networks {
		if slices.Contains(app.GetNetworks(), n.Name) {
			out.Networks = append(out.Networks, n)
		}
	}
	return out, nil
}

func (h *Handler) internal(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, "compose call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't complete the request; try again, and check its logs if it keeps failing"))
}
