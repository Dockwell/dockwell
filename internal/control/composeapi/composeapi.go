// SPDX-License-Identifier: AGPL-3.0-only

// Package composeapi serves dockwell.v1.ComposeService: a Compose file
// planned into a project of its own as a change, and an environment, or one
// service, exported as a Compose file. See
// docs/adr/0021-the-docker-driver.md and docs/adr/0026-the-app-screens.md.
package composeapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	"github.com/dockwell/dockwell/internal/control/containers"
	places "github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/control/services"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
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
	changes    *changes.Handler
	containers *containers.Handler
	engine     *revision.Engine
	db         *store.DB
	keeper     *secrets.Keeper
	logger     *slog.Logger
}

var _ dockwellv1connect.ComposeServiceHandler = (*Handler)(nil)

// New returns a handler that plans through ch and engine, and reads specs from
// db, opening the sealed ones with keeper.
func New(ch *changes.Handler, ct *containers.Handler, engine *revision.Engine, db *store.DB, keeper *secrets.Keeper, logger *slog.Logger) *Handler {
	return &Handler{changes: ch, containers: ct, engine: engine, db: db, keeper: keeper, logger: logger}
}

func invalid(err error) error {
	return authapi.ReasonError(connect.CodeInvalidArgument, errors.New(compose.Message(err)), "invalid")
}

// ImportCompose plans a Compose file into a project of its own.
func (h *Handler) ImportCompose(ctx context.Context, req *connect.Request[dockwellv1.ImportComposeRequest]) (*connect.Response[dockwellv1.ImportComposeResponse], error) {
	m := req.Msg
	if strings.TrimSpace(m.GetCompose()) == "" {
		return nil, invalid(errors.New("the request has no Compose file. Pass one in compose, such as --compose @compose.yaml"))
	}
	stack, err := h.read(ctx, compose.Input{File: []byte(m.GetCompose()), Variables: m.GetVariables(), Env: m.GetEnv(), Directory: m.GetDirectory()})
	if err != nil {
		return nil, err
	}
	place, err := h.destination(ctx, m.GetProject(), m.GetEnvironment(), stack.Name)
	if err != nil {
		return nil, err
	}
	c, err := h.plan(ctx, m, stack, place, planning{})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ImportComposeResponse{Change: c, ExposedVariables: exposedProtos(stack.Exposed), MoreExposed: stack.ExposedMore}), nil
}

// read reads a file, with the API's errors for what's wrong with it.
func (h *Handler) read(ctx context.Context, in compose.Input) (*compose.Stack, error) {
	stack, err := compose.Read(ctx, in)
	switch {
	case errors.Is(err, compose.ErrInvalid):
		return nil, invalid(err)
	case errors.Is(err, compose.ErrBusy):
		return nil, connect.NewError(connect.CodeResourceExhausted, err)
	}
	return stack, err
}

// Install is a catalog entry to plan: a Compose file, with what an import has no
// way to say.
type Install struct {
	Project string
	Summary string
	Name    string // how the project shows, such as "PostgreSQL"
	File    []byte
	// Variables are values for what the file uses, secrets among them.
	Variables map[string]string
	// Secrets are those to keep as the environment's secrets, by name. A service
	// whose environment takes one whole uses it by reference.
	Secrets map[string]string
	Pin     *resourcev1.CatalogPin
	Servers []string
	// FirstSteps is what a person needs to know, which the plan says before they
	// apply it.
	FirstSteps string
	// Limits are what a person set for a service, by its name in the file, in place
	// of what the file gives it.
	Limits map[string]Limits
}

// Limits are what a service may use, and when Docker restarts it. An empty field
// keeps the file's.
type Limits struct {
	Memory, CPUs string
	Restart      resourcev1.RestartPolicy
}

// limit sets what a person gave each service of a stack, and refuses a service the
// file doesn't have.
func limit(stack *compose.Stack, limits map[string]Limits) error {
	for _, name := range slices.Sorted(maps.Keys(limits)) {
		i := slices.IndexFunc(stack.Services, func(s compose.Item[*resourcev1.Service]) bool { return s.Name == name })
		if i < 0 {
			var names []string
			for _, s := range stack.Services {
				names = append(names, s.Name)
			}
			return authapi.ReasonError(connect.CodeInvalidArgument,
				fmt.Errorf("the file has no service called %s, so nothing takes its limits. Name one of %s", name, strings.Join(names, ", ")), "invalid")
		}
		app, l := stack.Services[i].Spec.GetApp(), limits[name]
		if app == nil {
			continue
		}
		if l.Memory != "" || l.CPUs != "" {
			if app.Resources == nil {
				app.Resources = &resourcev1.Resources{}
			}
			app.Resources.Memory = cmp.Or(l.Memory, app.Resources.GetMemory())
			app.Resources.Cpus = cmp.Or(l.CPUs, app.Resources.GetCpus())
		}
		if l.Restart != resourcev1.RestartPolicy_RESTART_POLICY_UNSPECIFIED {
			app.Restart = l.Restart
			// Only on-failure restarts a set number of times, as the entry's may have.
			if l.Restart != resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE {
				app.RestartRetries = 0
			}
		}
	}
	return nil
}

// PlanInstall plans an entry into a project of its own, as an import does, with
// its secrets and its pin in the same change. So undoing it takes the whole
// install away, and it refuses a project that exists, as an install never changes
// what's there.
func (h *Handler) PlanInstall(ctx context.Context, in Install) (*dockwellv1.Change, error) {
	stack, err := h.read(ctx, compose.Input{File: in.File, Variables: in.Variables})
	if err != nil {
		return nil, err
	}
	place, err := h.destination(ctx, in.Project, "", "")
	if err != nil {
		return nil, err
	}
	if len(place.made) == 0 {
		return nil, authapi.ReasonError(connect.CodeAlreadyExists,
			fmt.Errorf("a project named %s is here already. Choose another name, so the install doesn't change it", place.project), "project_exists")
	}
	// A project deleted before keeps its IDs, so its volumes would come back, for 7
	// days, with data a password made now can't open.
	var before bool
	err = h.db.View(ctx, func(tx *store.Tx) error {
		_, err := tx.GetResource(ctx, string(scope.ProjectKind), place.organization, place.project)
		before = err == nil
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	if before {
		return nil, authapi.ReasonError(connect.CodeAlreadyExists,
			fmt.Errorf("a project named %s was here before, and its data may come back, which a new password can't open. Choose another name", place.project), "project_exists")
	}
	if err := limit(stack, in.Limits); err != nil {
		return nil, err
	}
	place.made[0].Spec = &resourcev1.Project{DisplayName: in.Name, Catalog: in.Pin}
	useSecrets(stack, in.Secrets)
	req := &dockwellv1.ImportComposeRequest{Summary: in.Summary, Servers: in.Servers, Variables: in.Variables}
	var notes []revision.Note
	if in.FirstSteps != "" {
		notes = []revision.Note{{
			Reason:  "first_steps",
			Message: in.FirstSteps,
			Ref:     resource.Ref{Kind: scope.ProjectKind, Scope: place.organization, Name: place.project},
		}}
	}
	return h.plan(ctx, req, stack, place, planning{secrets: in.Secrets, notes: notes})
}

// planning is what a plan adds to a file read for a place.
type planning struct {
	// secrets are new secrets to keep beside what the file makes, by name.
	secrets map[string]string
	// upgrade says a file's project is the file's own to change, so it needs no
	// word that it's already there.
	upgrade bool
	notes   []revision.Note
	// deletes are what goes with this, after everything else, in this order.
	deletes []resource.Ref
	// backUp are the volumes to back up before anything else.
	backUp []resource.Ref
}

// useSecrets has each service use by reference what a file's variables gave its
// environment from a secret, rather than hold the value itself.
func useSecrets(stack *compose.Stack, values map[string]string) {
	for _, s := range stack.Services {
		app := s.Spec.GetApp()
		for as, value := range app.GetEnv() {
			for name, secret := range values {
				if value == secret {
					delete(app.Env, as)
					binding.Use(s.Spec, resource.Use{As: as, Kind: variable.SecretKind, Name: name})
				}
			}
		}
		if len(app.GetEnv()) == 0 {
			app.Env = nil
		}
	}
}

// plan proposes a file read for a place.
func (h *Handler) plan(ctx context.Context, m *dockwellv1.ImportComposeRequest, stack *compose.Stack, place destination, also planning) (*dockwellv1.Change, error) {
	env := place.environment
	if err := h.useVariables(ctx, env, stack); err != nil {
		return nil, h.internal(ctx, err)
	}
	running, err := h.servicesIn(ctx, env)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	var deleted []string
	var edits []*dockwellv1.Edit
	keep := map[string]bool{}
	add := func(kind resource.Kind, name string, spec *resourcev1.Spec) {
		keep[string(kind)+"/"+name] = true
		edits = append(edits, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(kind), Scope: env, Name: name},
			Spec:     spec, Servers: m.GetServers(),
		})
	}
	for _, name := range slices.Sorted(maps.Keys(also.secrets)) {
		add(variable.SecretKind, name, &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: also.secrets[name]}}})
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
			if kind == service.Kind {
				deleted = orphans
			}
			for _, name := range orphans {
				edits = append(edits, &dockwellv1.Edit{
					Resource: &dockwellv1.ResourceRef{Kind: string(kind), Scope: env, Name: name}, Delete: true,
				})
			}
		}
	}
	for _, ref := range also.deletes {
		edits = append(edits, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(ref.Kind), Scope: ref.Scope, Name: ref.Name}, Delete: true,
		})
	}
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		switch {
		case len(running) > 0:
			summary = fmt.Sprintf("Update %s from a Compose file", place.project)
		case place.project != scope.DefaultProject:
			summary = fmt.Sprintf("Run %s from a Compose file", place.project)
		default:
			summary = "Run a Compose file"
		}
	}
	if also.upgrade {
		running, deleted = nil, nil
	}
	updates := place.updates(running, deleted)
	for i := range updates {
		updates[i].Deleted, updates[i].Named = deleted, true // for a surface that says it in its own words
	}
	return h.changes.Propose(ctx, summary, edits, func(choice *scope.ChoiceError, _ string) string {
		if choice.Level == scope.ServerLevel {
			return "servers"
		}
		return ""
	}, changes.Also{Edits: place.made, BackUp: also.backUp, Notes: slices.Concat(also.notes, updates, planNotes(env, stack.Notices), exposedNotes(env, stack.Exposed, stack.ExposedMore, m.GetVariables()), rotationNotes(env, stack.Rotations))})
}

// AdoptCompose plans taking on a Compose project made elsewhere: each container as a
// service of a project of its own, in one change. What stops one stops them all, and
// a refusal says everything at once, as a project is taken on whole or not at all.
func (h *Handler) AdoptCompose(ctx context.Context, req *connect.Request[dockwellv1.AdoptComposeRequest]) (*connect.Response[dockwellv1.AdoptComposeResponse], error) {
	m := req.Msg
	if m.GetName() == "" {
		return nil, invalid(errors.New("the request names no Compose project. Pass one in name, such as shop"))
	}
	t, err := h.ReadTaking(ctx, m.GetServer(), m.GetName(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	if len(t.Refusals) > 0 {
		return nil, invalid(fmt.Errorf("%s. It all stays as it is, and nothing was taken on. Deal with those, then take the project on, or leave it outside Dockwell", strings.Join(t.Refusals, ". ")))
	}
	c, err := h.changes.Propose(ctx, "Take on "+m.GetName(), t.Edits, nil, changes.Also{Edits: t.Made, Adopts: t.Adopts, Notes: t.Notes})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.AdoptComposeResponse{Change: c}), nil
}

// Taking is what taking on a Compose project would plan, and what's in the way of
// it, as AdoptCompose reads it.
type Taking struct {
	// Project is the project it goes into.
	Project string
	// Edits make a service of each of its containers.
	Edits []*dockwellv1.Edit
	// Made makes the project and its environment, where they're new.
	Made []revision.Edit
	// Adopts says which container each edit takes on.
	Adopts map[resource.Ref]changes.Adoption
	// Notes are what its plan says of taking it on.
	Notes []revision.Note
	// Refusals are what's in the way of taking it on, each in words a person can act on.
	Refusals []string
}

// ReadTaking reads what taking on the Compose project name on server would plan,
// into project and environment, as AdoptCompose takes them. What's in the way is
// collected, whatever kind it is, so a person fixes it once rather than meeting it
// one thing at a time.
func (h *Handler) ReadTaking(ctx context.Context, server, name, project, environment string) (*Taking, error) {
	r, err := h.containers.ReadRunning(ctx, server)
	if err != nil {
		return nil, err
	}
	return h.ReadTakingIn(ctx, r, name, project, environment)
}

// ReadRunning reads what a server runs, once, for ReadTakingIn and TakenOn.
func (h *Handler) ReadRunning(ctx context.Context, server string) (*containers.Running, error) {
	return h.containers.ReadRunning(ctx, server)
}

// ReadTakingIn is ReadTaking, of what r read the server running.
func (h *Handler) ReadTakingIn(ctx context.Context, r *containers.Running, name, project, environment string) (*Taking, error) {
	s := r.Server
	list, err := r.InProject(name)
	if err != nil {
		return nil, err
	}
	byName := make(map[string][]*dockwellv1.ServerContainer, len(list))
	for _, c := range list {
		byName[containers.ServiceName(c)] = append(byName[containers.ServiceName(c)], c)
	}
	place, err := h.destination(ctx, project, environment, containers.Label(name))
	if err != nil {
		return nil, err
	}
	running, err := h.servicesIn(ctx, place.environment)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	t := &Taking{Project: place.project, Made: place.made, Adopts: make(map[resource.Ref]changes.Adoption, len(byName))}
	var manager, folder string
	for _, svcName := range slices.Sorted(maps.Keys(byName)) {
		c := byName[svcName][0]
		if len(byName[svcName]) > 1 {
			t.Refusals = append(t.Refusals, oneService(svcName, byName[svcName]))
		}
		if slices.Contains(running, svcName) {
			t.Refusals = append(t.Refusals, fmt.Sprintf("the project %s runs a service %s already, so it can't take on %s as well. Pass another project", place.project, svcName, c.GetName()))
		}
		ref := resource.Ref{Kind: service.Kind, Scope: place.environment, Name: svcName}
		svc, found, err := h.containers.ReadAs(ctx, s, c, ref)
		if err != nil {
			return nil, err
		}
		if why := containers.Refusal(c.GetName(), found); why != "" {
			t.Refusals = append(t.Refusals, why)
		}
		manager, folder = cmp.Or(manager, found.Manager), cmp.Or(folder, found.Folder)
		t.Adopts[ref] = changes.Adoption{ID: c.GetId(), Name: c.GetName()}
		t.Edits = append(t.Edits, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(service.Kind), Scope: place.environment, Name: svcName},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}},
			Servers:  []string{s.ID},
		})
	}
	names := make([]string, 0, len(list))
	for _, c := range list {
		names = append(names, c.GetName())
	}
	t.Notes = []revision.Note{
		{Reason: "takes_on", Message: fmt.Sprintf(
			"Takes on %s as it is: its containers keep running, with no restart, on the networks and volumes they have. Changing an image later replaces that container, under a name of Dockwell's, and it keeps its volumes. What it wrote anywhere else goes with the old container.", name)},
		revision.ManagedElsewhere(resource.Elsewhere{Manager: manager, Container: human.List(names), Project: name, Folder: folder}),
	}
	return t, nil
}

// TakenOn is the project of Dockwell's that runs the container called name, as r
// read the server, or "" where none does: one taken on keeps its name, and runs in
// its environment's Compose project.
func (h *Handler) TakenOn(ctx context.Context, r *containers.Running, name string) (string, error) {
	env := r.RunsAs(name)
	if env == "" {
		return "", nil
	}
	project := ""
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			for _, e := range p.Environments {
				if docker.Project(e.ID) == env {
					project = p.Name
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", h.internal(ctx, err)
	}
	return project, nil
}

// FreeProject is want, or, where a project has that name, the first of
// want-1panel, want-1panel-2 and on that none has: a project to take something on
// into that's no one else's.
func (h *Handler) FreeProject(ctx context.Context, want string) (string, error) {
	var names []string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		for _, p := range tree.Projects {
			names = append(names, p.Name)
		}
		return err
	})
	if err != nil {
		return "", h.internal(ctx, err)
	}
	for i := 0; ; i++ {
		name := want
		switch {
		case i == 1:
			name = want + "-1panel"
		case i > 1:
			name = fmt.Sprintf("%s-1panel-%d", want, i)
		}
		if !slices.Contains(names, name) {
			return name, nil
		}
	}
}

// oneService says why containers that come to one service's name can't be taken on:
// a service runs one container, and a name can't tell two services apart that differ
// in what it can't hold.
func oneService(name string, list []*dockwellv1.ServerContainer) string {
	names := make([]string, 0, len(list))
	scaled := true
	for _, c := range list {
		names = append(names, c.GetName())
		scaled = scaled && c.GetComposeService() == list[0].GetComposeService()
	}
	if scaled {
		return fmt.Sprintf("%s are the service %s, and a service runs one container. Scale it to one with docker compose up --scale %s=1",
			human.List(names), name, list[0].GetComposeService())
	}
	both := "all"
	if len(list) == 2 {
		both = "both"
	}
	return fmt.Sprintf("%s would %s be the service %s, as a service's name can't tell them apart. Rename one in the project's file", human.List(names), both, name)
}

// servicesIn names what an environment runs now.
func (h *Handler) servicesIn(ctx context.Context, env string) ([]string, error) {
	var out []string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		list, err := tx.ListResources(ctx, env, string(service.Kind))
		for i := range list {
			out = append(out, list[i].Name)
		}
		return err
	})
	slices.Sort(out)
	return out, err
}

// destination is where a Compose file goes.
type destination struct {
	organization string
	project      string // its project's name
	environment  string // its environment's ID, which may not exist yet
	made         []revision.Edit
}

// updates is what a plan says of a file going where services run already: to
// the API and the CLI an update otherwise reads as a new app, and a file's name:
// can match a project by chance.
func (d destination) updates(running, deleted []string) []revision.Note {
	if len(running) == 0 {
		return nil
	}
	message := fmt.Sprintf("%s already runs %s, so this changes what runs: a service of the same name in the file replaces it", d.project, human.List(running))
	if len(deleted) > 0 {
		message += fmt.Sprintf(", and %s, which the file doesn't name, would be deleted", human.List(deleted))
	}
	message += ". If that isn't what you meant, give the file a project of its own: pass project, or change name: in the file"
	return []revision.Note{{
		Reason:      "updates_project",
		Message:     message,
		Ref:         resource.Ref{Kind: scope.ProjectKind, Scope: d.organization, Name: d.project},
		Names:       append([]string{d.project}, running...),
		IfItChanges: true,
	}}
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
		out.organization = tree.Organization.ID
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
			p, e, ok := exportedFrom(ctx, tree, fileName)
			if !ok {
				return invalid(fmt.Errorf("the Compose file's name, %s, is an environment's Compose project, and no environment here has it, so it was exported from elsewhere. Change name: in the file, or pass project, to say where it goes", fileName))
			}
			if environment != "" {
				// Asked for another environment of the project it came from.
				project = p.ID
				var err error
				if p, e, err = tree.Resolve(p.ID, environment); err != nil {
					return err
				}
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
		out = destination{organization: tree.Organization.ID, project: named, environment: env, made: made}
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
func exportedFrom(ctx context.Context, tree scope.Tree, name string) (scope.Project, scope.Environment, bool) {
	// Below an admin, only an environment in the caller's places is found, so a
	// file names no project out of their reach (ADR 0049).
	caller, checked := places.CallerFrom(ctx)
	for _, p := range tree.Projects {
		for _, e := range p.Environments {
			if checked && places.Holds(caller.Grants, places.Place{Project: p.ID, Environment: e.ID}) == "" {
				continue
			}
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
		out[i] = &dockwellv1.ExposedVariable{Name: e.Variable, Resource: &dockwellv1.ResourceRef{Kind: e.Kind, Name: e.Owner}, Field: e.Field}
	}
	return out
}

// rotationNotes are what a plan says of a log rotation a file asked for: what it
// asked, what Dockwell does instead, and what that means for reading its logs.
func rotationNotes(env string, rotations []compose.Rotation) []revision.Note {
	out := make([]revision.Note, len(rotations))
	for i, r := range rotations {
		out[i] = revision.Note{
			Reason:  "log_rotation",
			Message: rotationWords(r),
			Ref:     resource.Ref{Kind: service.Kind, Scope: env, Name: r.Service},
			Names:   []string{r.Asked},
		}
	}
	return out
}

func rotationWords(r compose.Rotation) string {
	return fmt.Sprintf("%s asks for %s in its logging, and Dockwell keeps each container's log in 5 files of 10 MiB instead, so a log can't fill the disk. Older lines go sooner than the file says", r.Service, r.Asked)
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
	seen, stack, err := compose.Check(ctx, compose.Input{File: file, Variables: m.GetVariables(), Env: m.GetEnv(), Directory: m.GetDirectory()})
	if err != nil && !errors.Is(err, compose.ErrInvalid) {
		if errors.Is(err, compose.ErrBusy) {
			return nil, connect.NewError(connect.CodeResourceExhausted, err)
		}
		return nil, h.internal(ctx, err)
	}
	for _, v := range seen.Variables {
		res.Variables = append(res.Variables, &dockwellv1.ComposeVariable{Name: v.Name, Required: v.Required})
	}
	// What a form warns of before anyone types a value, and what a value typed so far
	// makes of it, as a command written as one string can't keep one of several words
	// by name, and only the value says that.
	exposed, more := seen.Exposed, seen.ExposedMore
	if stack != nil {
		for _, e := range stack.Exposed {
			if !slices.Contains(exposed, e) {
				exposed = append(exposed, e)
			}
		}
		more = more || stack.ExposedMore
	}
	res.ExposedVariables, res.MoreExposed = exposedProtos(exposed), more
	name, nameErr := h.fileName(ctx, seen.Name)
	if nameErr != nil {
		return nil, h.internal(ctx, nameErr)
	}
	res.Name = name
	refuse := func(err error) (*connect.Response[dockwellv1.CheckComposeResponse], error) {
		from, to := compose.Lines(ctx, file, err)
		res.Refusal = &dockwellv1.ComposeRefusal{Message: compose.Message(err), Line: int32(from), EndLine: int32(to)} //nolint:gosec // lines of a file under compose.MaxFile
		return connect.NewResponse(res), nil
	}
	if r := seen.Refused; r != nil {
		// What it would refuse whatever its variables hold comes before asking
		// for them, as one may be a secret.
		res.Refusal = &dockwellv1.ComposeRefusal{Message: r.Message, Line: int32(r.From), EndLine: int32(r.To)} //nolint:gosec // lines of a file under compose.MaxFile
		return connect.NewResponse(res), nil
	}
	if err != nil {
		return refuse(err)
	}
	for _, r := range stack.Rotations {
		res.Notes = append(res.Notes, rotationWords(r))
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
		if p, _, ok := exportedFrom(ctx, tree, name); ok {
			out = p.Name
		}
		return nil
	})
	return out, err
}

// planNotes are what a plan says of a file beside its steps.
func planNotes(env string, notices []compose.Notice) []revision.Note {
	out := make([]revision.Note, len(notices))
	for i, n := range notices {
		out[i] = revision.Note{
			Reason: n.Reason, Message: n.Message, Names: n.Names,
			Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: n.Service},
		}
	}
	return out
}

// exposedNotes are what a plan says of where a file writes a value in free text, by
// the variable and the field and never the value: the fact, what it means, and what
// to do. more says the file writes more than these. A variable in given came from the
// request, and any other from the .env beside the file.
func exposedNotes(env string, exposed []compose.Exposed, more bool, given map[string]string) []revision.Note {
	out := make([]revision.Note, 0, len(exposed)+1)
	for _, e := range exposed {
		from := "env_file"
		if _, ok := given[e.Variable]; ok {
			from = "variables"
		}
		out = append(out, revision.Note{
			Reason:  "exposed_variable",
			Message: exposedMessage(e, given),
			Ref:     resource.Ref{Kind: resource.Kind(e.Kind), Scope: env, Name: e.Owner},
			Names:   []string{e.Variable, e.Field},
			From:    from,
		})
	}
	if more {
		out = append(out, revision.Note{
			Reason:  "exposed_more",
			Message: fmt.Sprintf("The file writes more variables in plain sight than the %d listed here. Read it for the rest before you apply this", len(exposed)),
		})
	}
	return out
}

// exposedMessage says where a value lands. How to hide one depends on the field: what
// runs keeps a value by name, as long as it is one word.
func exposedMessage(e compose.Exposed, given map[string]string) string {
	from := "your .env"
	if _, ok := given[e.Variable]; ok {
		from = "the request's variables"
	}
	owner := e.Owner + "'s"
	if e.Kind != string(service.Kind) {
		owner = "the " + e.Kind + " " + e.Owner + "'s"
	}
	field := e.Field
	switch e.Field {
	case "driver_opts":
		field = "driver options"
	case "environment":
		field = "environment variable names"
	}
	fact := fmt.Sprintf("%s from %s is in %s %s, so anyone who can read this change or export this environment can read it", e.Variable, from, owner, field)
	switch e.Field {
	case "command", "entrypoint":
		if e.Whole {
			return fmt.Sprintf("%s. Another value on that line is what one name can't stand for, and Dockwell couldn't keep the rest of the line apart from it, so the whole line is text. To keep it hidden, write the %s as a list, with each word its own variable", fact, e.Field)
		}
		return fmt.Sprintf("%s. Its value has a space, a quote or another character Compose reads as part of the line, so one name can't stand for it. To keep it hidden, write the %s as a list, with each word its own variable", fact, e.Field)
	case "environment":
		return fact + ", as only a variable's value is hidden. Don't write a secret in a variable's name"
	}
	return fact + ". It can't be hidden there, so if it is a secret, set it in environment for the app to read instead"
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
					if err := logDriverOf(ctx, tx, r, stack); err != nil {
						return err
					}
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

// logDriverOf notes a service on a server whose Docker logs elsewhere than
// json-file by default, which stays that server's operator's choice.
func logDriverOf(ctx context.Context, tx *store.Tx, r store.Resource, stack *compose.Stack) error {
	on, err := tx.Placement(ctx, r.ID, r.Revision)
	if err != nil {
		return err
	}
	reports, err := tx.ServerReports(ctx)
	if err != nil {
		return err
	}
	for _, server := range on {
		var inv dockwellv1.ServerInventory
		if proto.Unmarshal(reports[server].Inventory, &inv) != nil {
			continue
		}
		if d := inv.GetDockerLogDriver(); d != "" && d != docker.LogDriverJSONFile {
			if stack.LogDrivers == nil {
				stack.LogDrivers = map[string]string{}
			}
			stack.LogDrivers[r.Name] = d
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
