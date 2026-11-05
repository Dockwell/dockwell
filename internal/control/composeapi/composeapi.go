// SPDX-License-Identifier: AGPL-3.0-only

// Package composeapi serves dockwell.v1.ComposeService: a Compose file
// planned into an environment as a change, and an environment, or one
// service, exported as a Compose file. See
// docs/adr/0021-the-docker-driver.md.
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
	"github.com/dockwell/dockwell/internal/docker"
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
	kinds   *resource.Registry
	db      *store.DB
	keeper  *secrets.Keeper
	logger  *slog.Logger
}

var _ dockwellv1connect.ComposeServiceHandler = (*Handler)(nil)

// New returns a handler that plans through changes, and reads specs from
// db with kinds, opening the sealed ones with keeper.
func New(ch *changes.Handler, kinds *resource.Registry, db *store.DB, keeper *secrets.Keeper, logger *slog.Logger) *Handler {
	return &Handler{changes: ch, kinds: kinds, db: db, keeper: keeper, logger: logger}
}

func invalid(err error) error {
	return authapi.ReasonError(connect.CodeInvalidArgument, errors.New(compose.Message(err)), "invalid")
}

// ImportCompose plans a Compose file into an environment.
func (h *Handler) ImportCompose(ctx context.Context, req *connect.Request[dockwellv1.ImportComposeRequest]) (*connect.Response[dockwellv1.ImportComposeResponse], error) {
	m := req.Msg
	if strings.TrimSpace(m.GetCompose()) == "" {
		return nil, invalid(errors.New("the request has no Compose file. Pass one in compose, such as --compose \"$(cat compose.yaml)\""))
	}
	p, env, err := h.changes.Environment(ctx, m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	stack, err := compose.Read(ctx, compose.Input{File: []byte(m.GetCompose()), Variables: m.GetVariables(), Env: m.GetEnv(), Directory: m.GetDirectory()})
	if err != nil {
		switch {
		case errors.Is(err, compose.ErrInvalid):
			return nil, invalid(err)
		case errors.Is(err, compose.ErrBusy):
			return nil, connect.NewError(connect.CodeResourceExhausted, err)
		}
		return nil, err
	}
	if err := h.useVariables(ctx, env.ID, stack); err != nil {
		return nil, h.internal(ctx, err)
	}
	var edits []*dockwellv1.Edit
	keep := map[string]bool{}
	add := func(kind resource.Kind, name string, spec *resourcev1.Spec) {
		keep[string(kind)+"/"+name] = true
		edits = append(edits, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(kind), Scope: env.ID, Name: name},
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
				list, err := tx.ListResources(ctx, env.ID, string(kind))
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
					Resource: &dockwellv1.ResourceRef{Kind: string(kind), Scope: env.ID, Name: name}, Delete: true,
				})
			}
		}
	}
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		summary = "Run a Compose file in " + env.Name
		if p.Name != "" {
			summary = fmt.Sprintf("Run a Compose file in %s of %s", env.Name, p.Name)
		}
	}
	c, err := h.changes.Propose(ctx, summary, edits, func(choice *scope.ChoiceError, _ string) string {
		if choice.Level == scope.ServerLevel {
			return "servers"
		}
		return ""
	}, changes.Also{Notes: slices.Concat(planNotes(env.ID, stack.Notices), exposedNotes(env.ID, stack.Exposed, stack.ExposedMore, m.GetVariables()))})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ImportComposeResponse{Change: c}), nil
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
		out = append(out, revision.Note{
			Reason:  "exposed_variable",
			Message: exposedMessage(e, given),
			Ref:     resource.Ref{Kind: resource.Kind(e.Kind), Scope: env, Name: e.Owner},
			Names:   []string{e.Variable, e.Field},
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

// exposedMessage says where a value lands. How to hide one depends on the field:
// only what runs can read it from the environment, and then only through a shell.
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
	case "healthcheck":
		field = "health check"
	case "driver_opts":
		field = "driver options"
	case "environment":
		field = "environment variable names"
	}
	fact := fmt.Sprintf("%s from %s is in %s %s, so anyone who can read this change or export this environment can read it", e.Variable, from, owner, field)
	switch e.Field {
	case "command", "entrypoint":
		return fmt.Sprintf("%s. To keep it hidden, set it in environment and have the %s read it there, with sh -c, if the image has a shell", fact, e.Field)
	case "healthcheck":
		return fact + ". To keep it hidden, set it in environment and have the health check read it there, with CMD-SHELL, if the image has a shell"
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
