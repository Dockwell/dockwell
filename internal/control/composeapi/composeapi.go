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
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Handler implements ComposeService.
type Handler struct {
	changes *changes.Handler
	kinds   *resource.Registry
	db      *store.DB
	logger  *slog.Logger
}

var _ dockwellv1connect.ComposeServiceHandler = (*Handler)(nil)

// New returns a handler that plans through changes, and reads specs from
// db with kinds.
func New(ch *changes.Handler, kinds *resource.Registry, db *store.DB, logger *slog.Logger) *Handler {
	return &Handler{changes: ch, kinds: kinds, db: db, logger: logger}
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
	stack, err := compose.Parse(ctx, []byte(m.GetCompose()), m.GetVariables())
	if err != nil {
		if errors.Is(err, compose.ErrInvalid) {
			return nil, invalid(err)
		}
		return nil, err
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
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ImportComposeResponse{Change: c}), nil
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
		for _, kind := range []resource.Kind{service.Kind, volume.Kind, network.Kind} {
			list, err := tx.ListResources(ctx, env.ID, string(kind))
			if err != nil {
				return err
			}
			for _, r := range list {
				spec, err := h.kinds.Decode(kind, r.Spec)
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
	out := &compose.Stack{Services: []compose.Item[*resourcev1.Service]{{Name: svc.Name, Spec: proto.CloneOf(svc.Spec)}}}
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
