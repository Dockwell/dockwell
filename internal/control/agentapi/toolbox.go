// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

func (s *Session) checkHostToolbox() error {
	if !s.hostToolbox {
		return fault.New(ErrOlderAgent, "the agent on %s doesn't support the host toolbox. Update the agent and try again", s.name)
	}
	return nil
}

// HostToolbox reads the current host settings on this server.
func (s *Session) HostToolbox(ctx context.Context) (*v1.HostToolboxStatus, error) {
	if err := s.checkHostToolbox(); err != nil {
		return nil, err
	}
	call := &agentv1.Call{Call: &agentv1.Call_HostToolboxRead{HostToolboxRead: &agentv1.HostToolboxReadCall{}}}
	r, err := s.call(ctx, call, ObserveTimeout, (*Session).checkHostToolbox)
	if err != nil {
		if s.hostToolboxRecovery != "" {
			return nil, fmt.Errorf("%s couldn't restore its previous host settings: %s", s.name, s.hostToolboxRecovery)
		}
		return nil, err
	}
	if status := r.GetHostToolboxRead(); status != nil {
		if recovery := status.GetRecoveryError(); recovery != "" {
			return nil, fmt.Errorf("%s couldn't restore its previous host settings: %s", s.name, recovery)
		}
		return status, nil
	}
	return nil, errors.New("the agent didn't return its host settings")
}

type toolboxValues interface {
	HostToolboxValues(context.Context, []*v1.ToolboxBinding) (map[string]string, error)
}

func (s *Session) applyHostToolbox(ctx context.Context, step resource.Step) error {
	call := &agentv1.ApplyCall{
		Step:    resourcepb.Step(step, ""),
		Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(step.Ref), HostToolbox: step.Toolbox.Proto()},
		Deploy:  &agentv1.Deploy{ChangeId: resource.DeployOf(ctx).Change},
	}
	bindings := step.Toolbox.Proto().GetOperation().GetService().GetEnvironment()
	var values map[string]string
	if len(bindings) > 0 {
		s.agents.mu.Lock()
		provider, ok := s.agents.values.(toolboxValues)
		s.agents.mu.Unlock()
		if !ok {
			return errors.New("the control plane can't send the service's environment values")
		}
		var err error
		values, err = provider.HostToolboxValues(ctx, bindings)
		if err != nil {
			return err
		}
		defer clear(values)
	}
	_, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Apply{Apply: call}}, s.agents.applyTimeout, func(on *Session) error {
		if err := on.checkHostToolbox(); err != nil {
			return err
		}
		if len(values) == 0 {
			return nil
		}
		sealed, err := on.seal(step.Ref, values)
		if err != nil {
			return err
		}
		call.Values = sealed
		return nil
	})
	return err
}

// A server can report its own state. Only the control plane chooses which
// resource, operation and secret references go into the change.
func checkToolboxPlan(want resource.Desired, got resource.Step) error {
	if want.Toolbox == nil && got.Toolbox == nil {
		return nil
	}
	ask, plan := want.Toolbox, got.Toolbox.Proto()
	if ask == nil || plan == nil || got.Ref != want.Ref || got.Action != resource.ActionUpdate ||
		plan.GetChangeId() != ask.GetChangeId() || !proto.Equal(ask.GetOperation(), plan.GetOperation()) ||
		plan.GetKeep() != resource.ToolboxNeedsKeep(ask.GetOperation()) || len(plan.GetEvidence()) != 64 {
		return errors.New("the agent's toolbox plan differs from the operation requested")
	}
	return nil
}
