// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"

	"github.com/dockwell/dockwell/internal/store"
)

// Grants returns what a principal may act with: a person's own grants, or for an
// API token, its maker's, as they stand now, so a token loses what its maker
// loses (ADR 0049).
func (s *Service) Grants(ctx context.Context, p Principal) ([]store.Grant, error) {
	var out []store.Grant
	err := s.db.View(ctx, func(tx *store.Tx) error {
		var err error
		out, err = tx.Grants(ctx, p.User.ID)
		return err
	})
	return out, err
}

// Highest is the highest role among grants, anywhere, or "" for none.
func Highest(grants []store.Grant) string {
	best := ""
	for _, g := range grants {
		if store.RoleRank(g.Role) > store.RoleRank(best) {
			best = g.Role
		}
	}
	return best
}
