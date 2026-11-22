// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"slices"

	"google.golang.org/protobuf/proto"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/dockerlog"
	"github.com/dockwell/dockwell/internal/store"
)

// DockerLoggingTarget identifies the instance from which rotation was reviewed.
type DockerLoggingTarget struct {
	Server, Container string
}

func dockerLoggingTargetIn(plan *planRead, ref resource.Ref) *DockerLoggingTarget {
	if row := plan.first(ref, func(r *ResourcePlan) bool { return r.DockerLoggingTarget != nil }); row != nil {
		return row.DockerLoggingTarget
	}
	return nil
}

func checkDockerLoggingTarget(ctx context.Context, agent resource.Agent, target *DockerLoggingTarget, server string) error {
	if target == nil || target.Server != server {
		return nil
	}
	checked, ok := agent.(interface {
		CheckDockerLoggingTarget(context.Context, string) (bool, error)
	})
	if !ok {
		return fault.New(ErrInvalid, "update the agent to review log rotation")
	}
	found, err := checked.CheckDockerLoggingTarget(ctx, target.Container)
	if err != nil {
		return err
	}
	if !found {
		return fault.New(ErrStale, "the reviewed container is gone. Open the current container and review log rotation again")
	}
	return nil
}

// SpecWithDockerLogging preserves the workload and edits only its logging policy.
func (e *Engine) SpecWithDockerLogging(ctx context.Context, tx *store.Tx, ref resource.Ref, base int, server string, logging *spec.ContainerLogging) (proto.Message, error) {
	if err := dockerlog.ValidateContainerLogging(logging); err != nil {
		return nil, fault.New(ErrInvalid, "%s", err)
	}
	it, err := e.load(ctx, tx, ref)
	if err != nil {
		return nil, err
	}
	if it.base != base || resource.Workload(it.current) == nil || !slices.Contains(it.placed, server) {
		return nil, fault.New(ErrStale, "the workload changed. Open it and review log rotation again")
	}
	out := proto.Clone(it.current)
	resource.Workload(out).Logging = proto.CloneOf(logging)
	return out, nil
}
