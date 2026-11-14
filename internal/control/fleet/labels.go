// SPDX-License-Identifier: AGPL-3.0-only

package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"maps"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/store"
)

// A server's labels are on its host, kind server.host, as its latest revision has
// them (ADR 0091). The hub keeps them beside what its agent says, so every read and
// the fleet's stream give them.

// loadLabels reads each server's labels from its host, by server ID. A host the hub
// can't read is logged and left out, so one bad record doesn't keep the fleet
// from loading.
func loadLabels(ctx context.Context, tx *store.Tx, logger *slog.Logger) (map[string]map[string]string, error) {
	hosts, err := tx.ListKind(ctx, string(host.Kind))
	if err != nil {
		return nil, fmt.Errorf("can't read servers' labels: %w", err)
	}
	out := make(map[string]map[string]string, len(hosts))
	for i := range hosts {
		r := &hosts[i]
		if r.Spec == nil {
			continue
		}
		h := &resourcev1.ServerHost{}
		if err := proto.Unmarshal(r.Spec, h); err != nil {
			logger.WarnContext(ctx, "can't read a server's labels, so it shows none", slog.String("server", r.Scope), slog.String("error", err.Error()))
			continue
		}
		if len(h.GetLabels()) > 0 {
			out[r.Scope] = h.GetLabels()
		}
	}
	return out, nil
}

// ReadLabels reads every server's labels again, and tells whoever watches the
// fleet of each server whose labels changed.
func (h *Hub) ReadLabels(ctx context.Context) error {
	var labels map[string]map[string]string
	if err := h.db.View(ctx, func(tx *store.Tx) (err error) {
		labels, err = loadLabels(ctx, tx, h.logger)
		return err
	}); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, l := range h.servers {
		if maps.Equal(l.labels, labels[id]) {
			continue
		}
		l.labels = labels[id]
		h.notifyLocked(id, false)
	}
	return nil
}
