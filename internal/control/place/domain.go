// SPDX-License-Identifier: AGPL-3.0-only

package place

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
)

// Domain resolves the places of the live routes serving domain, matching
// case-insensitively as DomainService does. Routes are opened with the store's
// keeper because a route may hold sealed basic-auth password hashes or custom
// certificate private keys (ADR 0087). Returns store.ErrNotFound when no live
// route serves domain.
func Domain(ctx context.Context, tx *store.Tx, domain string) ([]Place, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil, store.ErrNotFound
	}
	routes, err := tx.ListKind(ctx, string(route.Kind))
	if err != nil {
		return nil, err
	}
	pl := NewPlacer(tx)
	var out []Place
	for i := range routes {
		r := routes[i]
		plain, err := tx.Open(ctx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, fmt.Errorf("can't read route %s: %w", r.ID, err)
		}
		var rt resourcev1.Route
		if err := proto.Unmarshal(plain, &rt); err != nil {
			return nil, fmt.Errorf("can't read route %s: %w", r.ID, err)
		}
		if !strings.EqualFold(rt.GetDomain(), domain) {
			continue
		}
		p, err := pl.resourcePlace(ctx, r)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, store.ErrNotFound
	}
	return out, nil
}
