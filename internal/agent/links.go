// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
)

// linkJoins has the agent's containers join, as each is made, the link networks of
// other environments that name its service, from what the agent keeps of each
// network: a database shared with an app stays on the app's link network through a
// rollback, an undo, or a remake while the control plane is away (ADR 0084).
func linkJoins(drivers *resource.Registry, kept Desired) {
	if kept == nil {
		return
	}
	d, err := drivers.Driver(container.Kind)
	if err != nil {
		return
	}
	cd, ok := d.(*container.Driver)
	if !ok {
		return
	}
	rt, ok := cd.Runtime.(*container.Docker)
	if !ok {
		return
	}
	rt.Joins = func(t container.Target) []container.Join {
		var out []container.Join
		for _, ref := range kept.Refs() {
			if ref.Kind != network.Kind {
				continue
			}
			r, ok, err := kept.Read(ref)
			if err != nil || !ok || r.Spec == nil {
				continue
			}
			var n resourcev1.Network
			if proto.Unmarshal(r.Spec, &n) != nil {
				continue
			}
			for _, m := range n.GetMembers() {
				if project, svc := network.MemberTarget(m); project == t.Project && svc == t.Service {
					out = append(out, container.Join{Network: network.TargetOf(ref).Name, Alias: m.GetAlias()})
				}
			}
		}
		return out
	}
}
