// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"

	"google.golang.org/protobuf/proto"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
)

func storageGuardOf(ed *Edit, changeID string) *resource.DockerStorageGuard {
	if !ed.DockerStorageCreate {
		return resource.DockerStorageGuardFrom(proto.CloneOf(ed.DockerStorageGuard))
	}
	name := network.TargetOf(ed.Ref).Name
	if ed.Ref.Kind == volume.Kind {
		name = volume.TargetOf(ed.Ref).Name
	}
	sum := sha256.Sum256([]byte(changeID + ":" + ed.Ref.String()))
	return &resource.DockerStorageGuard{Create: &v1.DockerStorageCreateGuard{
		EngineName: name, CreationToken: hex.EncodeToString(sum[:]),
	}}
}

func dockerStorageGuardIn(plan *planRead, ref resource.Ref) *resource.DockerStorageGuard {
	if r := plan.first(ref, func(r *ResourcePlan) bool { return r.DockerStorageGuard != nil }); r != nil {
		return r.DockerStorageGuard
	}
	return nil
}

// SpecWithNetworkMembership edits the saved workload without exposing its values.
func (e *Engine) SpecWithNetworkMembership(ctx context.Context, tx *store.Tx, ref resource.Ref, base int, server, name string, external, connect bool) (proto.Message, error) {
	it, err := e.load(ctx, tx, ref)
	if err != nil {
		return nil, err
	}
	if base <= 0 || it.base != base || resource.Workload(it.current) == nil ||
		len(it.placed) != 1 || it.placed[0] != server {
		return nil, fault.New(ErrStale, "%s changed. Refresh the workload and review again", ref.Name)
	}
	out := proto.Clone(it.current)
	app := resource.Workload(out)
	if app.GetHostNetwork() ||
		slices.Contains(app.GetNetworks(), "host") || slices.Contains(app.GetNetworks(), "none") {
		return nil, fault.New(ErrInvalid, "this workload uses host networking or has networking disabled, so it cannot join this network. Change its network mode in its workload plan first")
	}
	if connect && len(app.GetNetworks())+len(app.GetExternalNetworks()) == 0 {
		if ref.Kind == container.Kind {
			app.Networks = []string{"bridge"}
		} else {
			app.ExternalNetworks = []string{"bridge"}
		}
	}
	names := &app.Networks
	if external && ref.Kind != container.Kind {
		names = &app.ExternalNetworks
	}
	present := slices.Contains(*names, name)
	switch {
	case present == connect:
		return nil, fault.New(ErrStale, "the saved network membership differs from the requested change")
	case connect:
		*names = append(*names, name)
	default:
		if len(app.GetNetworks())+len(app.GetExternalNetworks()) == 1 {
			return nil, fault.New(ErrInvalid, "this is the workload's last network")
		}
		*names = slices.DeleteFunc(*names, func(n string) bool { return n == name })
		app.NetworkAliases = slices.DeleteFunc(app.NetworkAliases, func(alias string) bool {
			return len(alias) > len(name) && alias[:len(name)+1] == name+"="
		})
	}
	return out, nil
}
