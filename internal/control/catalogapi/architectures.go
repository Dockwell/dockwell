// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
)

// checkArchitectures refuses to put an entry on a server whose CPU its images don't
// run on, as its source says (Entry.architectures, ADR 0085): servers, by ID, or the
// only server when none is named. A server that hasn't said its architecture yet is
// let through, as pulling the image says it then.
func (h *Handler) checkArchitectures(ctx context.Context, e *catalogv1.Entry, servers []string) error {
	runs := e.GetArchitectures()
	if len(runs) == 0 || h.db == nil {
		return nil
	}
	var reports map[string]store.ServerReport
	var names map[string]string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if reports, err = tx.ServerReports(ctx); err != nil {
			return err
		}
		list, err := tx.ListServers(ctx)
		if err != nil {
			return err
		}
		names = map[string]string{}
		for i := range list {
			if list[i].RevokedAt == nil {
				names[list[i].ID] = list[i].Name
			}
		}
		return nil
	})
	if err != nil {
		return h.internal(ctx, err)
	}
	on := servers
	if len(on) == 0 && len(names) == 1 {
		on = slices.Collect(maps.Keys(names))
	}
	for _, id := range on {
		var inv dockwellv1.ServerInventory
		if proto.Unmarshal(reports[id].Inventory, &inv) != nil {
			continue
		}
		if arch := inv.GetArchitecture(); arch != "" && !slices.Contains(runs, arch) {
			return authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("%s runs on %s servers alone, as its source says, and %s is %s. Install it on an %s server", e.GetName(), strings.Join(runs, " and "), serverName(names[id], id), arch, runs[0]),
				"architecture")
		}
	}
	return nil
}

// serverName is a server's name, or its ID when it has none.
func serverName(name, id string) string {
	if name != "" {
		return name
	}
	return id
}
