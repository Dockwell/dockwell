// SPDX-License-Identifier: AGPL-3.0-only

package certificates

import (
	"context"
	"slices"
	"time"

	"github.com/dockwell/dockwell/internal/store"
)

// RenewDomainNow clears backoff and marks the DNS-01 certificate holder serving domain
// for immediate renewal on the next tick, waking the manager loop. Returns (false, nil)
// when no DNS-01 certificate currently serves domain.
func (m *Manager) RenewDomainNow(ctx context.Context, domain string) (bool, error) {
	wanted, _, err := m.wants(ctx)
	if err != nil {
		return false, err
	}
	var holders []Holder
	for h, w := range wanted {
		for _, served := range w.Serves {
			if slices.Contains(served, domain) {
				holders = append(holders, h)
				break
			}
		}
	}
	if len(holders) == 0 {
		return false, nil
	}
	err = m.DB.Update(ctx, func(tx *store.Tx) error {
		st, err := states(ctx, tx)
		if err != nil {
			return err
		}
		for _, h := range holders {
			s := st[h]
			if s == nil {
				s = &held{}
				st[h] = s
			}
			s.Next = time.Time{}
			s.RenewAt = time.Time{}
			s.LastOrder = time.Time{}
			s.Rekey = true
		}
		return putJSON(ctx, tx, stateSetting, st)
	})
	if err != nil {
		return true, err
	}
	m.Wake()
	return true, nil
}
