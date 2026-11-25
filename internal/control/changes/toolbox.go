// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// SetToolboxValues gives host service plans access to their environment values.
func (h *Handler) SetToolboxValues(k *secrets.Keeper) { h.toolboxValues = k }

// PlanHostToolbox records the reviewed host operation on its server's host.
func (h *Handler) PlanHostToolbox(ctx context.Context, req *connect.Request[v1.PlanHostToolboxRequest]) (*connect.Response[v1.PlanHostToolboxResponse], error) {
	op := proto.CloneOf(req.Msg.GetOperation())
	name := toolboxOperationName(op)
	if name == "" {
		return nil, authapi.Invalid(errors.New("choose a host toolbox operation"))
	}
	var serverID, serverName string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		set, err := h.targetsOf(ctx, tx, []string{req.Msg.GetServerId()}, "", nil, "change host settings on")
		if err != nil {
			return err
		}
		if len(set.servers) != 1 {
			return authapi.Invalid(errors.New("choose one server for this host operation"))
		}
		serverID, serverName = set.servers[0].ID, set.servers[0].Name
		bindings := op.GetService().GetEnvironment()
		if len(bindings) > 32 || op.GetService().GetRemove() && len(bindings) != 0 {
			return authapi.Invalid(errors.New("choose up to 32 environment values when creating a service"))
		}
		for _, b := range bindings {
			if b.GetName() == "" || b.GetRevision() != 0 || b.GetEnvironmentName() != "" || b.GetEnvironmentProtected() ||
				b.GetReviewedValue() != "" || len(b.GetValueDigest()) != 0 || len(b.GetSealedLiteral()) != 0 {
				return authapi.Invalid(errors.New("choose a Dockwell value or enter a value directly for each environment name"))
			}
			if h.toolboxValues == nil {
				return errors.New("the control plane can't read the service's environment values")
			}
			if b.LiteralValue != nil {
				if b.GetEnvironmentId() != "" || b.GetSource() != "" || b.GetSecret() {
					return authapi.Invalid(errors.New("choose either a Dockwell value or a value entered directly"))
				}
				if err := h.toolboxValues.SealHostToolboxLiteral(b); err != nil {
					return authapi.Invalid(err)
				}
				continue
			}
			if b.GetEnvironmentId() == "" || b.GetSource() == "" {
				return authapi.Invalid(errors.New("choose a Dockwell variable or secret for each environment binding"))
			}
			if err := h.toolboxValues.PinHostToolboxValue(ctx, tx, b); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return authapi.Invalid(fmt.Errorf("there's no value %s in that environment", b.GetSource()))
				}
				return err
			}
		}
		return nil
	})
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return nil, cerr
	}
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}

	change, err := h.Planned(ctx, func(who revision.By) (*revision.Plan, error) {
		return h.engine.Propose(ctx, revision.Proposal{
			Summary: fmt.Sprintf("%s on %s", name, serverName), Actor: who.Actor, Via: who.Via,
			Edits: []revision.Edit{{Ref: resource.Ref{Kind: host.Kind, Scope: serverID, Name: host.Name}, Toolbox: op}},
		})
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.PlanHostToolboxResponse{Change: change}), nil
}

func toolboxOperationName(op *v1.HostToolboxOperation) string {
	switch v := op.GetOperation().(type) {
	case *v1.HostToolboxOperation_Swap:
		if v.Swap.GetSizeMib() == 0 {
			return "Remove swap"
		}
		return "Set swap size"
	case *v1.HostToolboxOperation_Swappiness:
		return "Set swappiness"
	case *v1.HostToolboxOperation_Hostname:
		return "Change hostname"
	case *v1.HostToolboxOperation_TimeZone:
		return "Change time zone"
	case *v1.HostToolboxOperation_Ntp:
		if v.Ntp {
			return "Enable time synchronization"
		}
		return "Disable time synchronization"
	case *v1.HostToolboxOperation_Dns:
		return "Change DNS servers"
	case *v1.HostToolboxOperation_Hosts:
		if v.Hosts.GetAddress() == "" {
			return "Remove hosts entry"
		}
		return "Set hosts entry"
	case *v1.HostToolboxOperation_Cleanup:
		return "Clean host files"
	case *v1.HostToolboxOperation_Service:
		if v.Service.GetRemove() {
			return "Remove background service"
		}
		return "Create background service"
	default:
		return ""
	}
}

// toolboxStepUpTarget includes display words from the stored plan. The ID and
// fingerprint on the first line still bind the proof to the exact change.
func toolboxStepUpTarget(plan *revision.Plan, id, fingerprint string) string {
	key := id + ":" + fingerprint
	for i := range plan.Resources {
		r := &plan.Resources[i]
		if r.Toolbox == nil {
			continue
		}
		op := r.Toolbox.Proto().GetOperation()
		operation := toolboxOperationName(op)
		if operation == "" {
			return key
		}
		verb, subject, _ := strings.Cut(operation, " ")
		gerund := map[string]string{
			"Create": "creating", "Remove": "removing", "Set": "setting", "Change": "changing",
			"Enable": "enabling", "Disable": "disabling", "Clean": "cleaning",
		}[verb]
		words := gerund + " " + subject
		if service := op.GetService(); service != nil {
			words += " " + service.GetName()
			if !service.GetRemove() {
				words += " (runs as " + service.GetUser() + ")"
			}
		}
		server, ok := strings.CutPrefix(plan.Summary, operation+" on ")
		if !ok {
			server = r.Ref.Scope
		}
		return key + "\n" + strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, words+" on "+server)
	}
	return key
}
