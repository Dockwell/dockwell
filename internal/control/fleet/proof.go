// SPDX-License-Identifier: AGPL-3.0-only

package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

// proof is what a certificate showed of a domain (rule 4, ADR 0034): that the server
// answered at its record, as the record read when the certificate came. A record can
// move while the route and the certificate stay, so a later read can only take that
// away. The next certificate makes a new one, as getting it took the server answering
// at the record again.
//
// Proofs are kept in the store, unlike what else is heard from agents: an upgrade
// restarts the control plane, and it would otherwise take a moved record for new.
type proof struct {
	// Certificate is when the certificate expires, which tells it from its renewal.
	Certificate time.Time `json:"certificate"`
	// Record is the domain's addresses as they read when it came.
	Record []string `json:"record"`
	// WayOuts are the addresses the server went out from then. One that's still among
	// them says nothing new, however lately it was seen.
	WayOuts []string `json:"way_outs,omitempty"`
	// Dropped says something since showed the record no longer leads here.
	Dropped bool `json:"dropped,omitempty"`
}

// expiry is what tells a certificate from its renewal.
func expiry(c *dockwellv1.Certificate) time.Time {
	if c.GetExpireTime() == nil {
		return time.Time{}
	}
	return c.GetExpireTime().AsTime()
}

// proofOf is what proves a domain now, or nil: the proof of the certificate it has,
// and nothing since against it.
func (l *live) proofOf(status *dockwellv1.DomainStatus) *proof {
	p := l.proofs[status.GetDomain()]
	if p == nil || p.Dropped || !served(status.GetCertificate()) || !p.Certificate.Equal(expiry(status.GetCertificate())) {
		return nil
	}
	return p
}

// weighProofs takes what's been heard against what each certificate showed, and
// reports whether that changed any proof.
func (l *live) weighProofs(now time.Time) bool {
	changed := false
	for _, status := range l.domains {
		cert, dns := status.GetCertificate(), status.GetDns()
		if !served(cert) {
			continue
		}
		p, record := l.proofs[status.GetDomain()], slices.Sorted(slices.Values(dns.GetFound()))
		switch {
		case p == nil || !p.Certificate.Equal(expiry(cert)):
			if readRecord(dns) && fresh(dns, now) {
				l.proofs[status.GetDomain()] = &proof{Certificate: expiry(cert), Record: record, WayOuts: wayOuts(l), Dropped: !pointsHere(dns)}
				changed = true
			}
		case p.Dropped:
		case readRecord(dns) && !slices.Equal(record, p.Record), overtaken(l, p):
			p.Dropped, changed = true, true
		}
	}
	return changed
}

// readRecord reports a read that says what the record is, as a failed one doesn't.
func readRecord(dns *dockwellv1.DomainCheck) bool {
	return pointsHere(dns) || dns.GetState() == dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_ELSEWHERE ||
		dns.GetState() == dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_NO_RECORD
}

// wayOuts are the addresses a server is known to go out from.
func wayOuts(l *live) []string {
	var out []string
	for _, s := range []string{l.seen, l.outside.GetLookedUpAddress()} {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// overtaken reports a way out whose address has changed since the certificate came,
// to one the record doesn't have, on a home or office network. It goes by the address
// changing and not by when it was last seen, as one that always differed says nothing
// new. On a cloud they're allowed to differ, as a NAT gateway's way out does, and the
// address behind a home router is the router's, in and out.
func overtaken(l *live, p *proof) bool {
	if shapeOf(l) != dockwellv1.NetworkShape_NETWORK_SHAPE_LOCAL_NAT {
		return false
	}
	for _, out := range wayOuts(l) {
		family := slices.DeleteFunc(slices.Clone(p.Record), func(s string) bool { return isFour(s) != isFour(out) })
		if len(family) > 0 && !slices.Contains(family, out) && !slices.Contains(p.WayOuts, out) {
			return true
		}
	}
	return false
}

// saveProofs keeps a server's proofs. Taking them under saving means the last save
// holds the latest, whichever order two got here in. Failing costs what a restart would
// forget, so it's said and not stopped for.
func (h *Hub) saveProofs(server string) {
	h.saving.Lock()
	defer h.saving.Unlock()
	h.mu.Lock()
	l, ok := h.servers[server]
	var raw []byte
	var err error
	if ok {
		raw, err = json.Marshal(l.proofs)
	}
	h.mu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err == nil {
		err = h.db.Update(ctx, func(tx *store.Tx) error { return tx.PutSetting(ctx, store.SettingDomainProofs(server), raw) })
	}
	if err != nil {
		h.logger.Warn("can't keep what a server's certificates proved, so a restart reads each domain afresh", slog.String("server", server), slog.String("error", err.Error()))
	}
}

// loadProofs reads a server's proofs. A row that can't be read is logged and skipped,
// as with a person's address: it can't be what stops a control plane starting.
func loadProofs(ctx context.Context, tx *store.Tx, logger *slog.Logger, server string) (map[string]*proof, error) {
	proofs := map[string]*proof{}
	raw, err := tx.GetSetting(ctx, store.SettingDomainProofs(server))
	if errors.Is(err, store.ErrNotFound) {
		return proofs, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &proofs); err != nil || proofs == nil {
		logger.WarnContext(ctx, "can't read what a server's certificates proved, so each domain reads afresh", slog.String("server", server))
		return map[string]*proof{}, nil //nolint:nilerr // a bad row is logged, and reads as none
	}
	return proofs, nil
}
