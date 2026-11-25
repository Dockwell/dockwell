// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"slices"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

func toolboxIn(plan *planRead, ref resource.Ref) *v1.HostToolboxPlan {
	if r := plan.first(ref, func(r *ResourcePlan) bool { return r.Toolbox.Proto() != nil }); r != nil {
		return r.Toolbox.Proto()
	}
	return nil
}

// HasToolbox identifies reviewed host operations that require a fresh passkey.
func (p *Plan) HasToolbox() bool {
	return slices.ContainsFunc(p.Resources, func(r ResourcePlan) bool { return r.Toolbox.Proto() != nil })
}
