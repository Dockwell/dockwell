// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// dnsFor is how long what public DNS said of a name is kept for Domains.
const dnsFor = 30 * time.Second

// readsAtOnce is how many names Domains reads from public DNS at a time.
const readsAtOnce = 8

// dnsCache keeps what public DNS said of each name, by server, for dnsFor.
type dnsCache struct {
	mu   sync.Mutex
	seen map[string]dnsSeen
}

type dnsSeen struct {
	check *dockwellv1.DomainCheck
	at    time.Time
}

func (c *dnsCache) get(key string, now time.Time) *dockwellv1.DomainCheck {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.seen[key]; ok && now.Sub(s.at) < dnsFor {
		return s.check
	}
	return nil
}

func (c *dnsCache) put(key string, check *dockwellv1.DomainCheck, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]dnsSeen{}
	}
	c.seen[key] = dnsSeen{check: check, at: now}
}

// readDNS says what DNS says of each domain now, in Domain.dns: from its connected
// zone's records, which one listing per zone gives, or from public DNS for a name in
// no connected zone, each kept for 30 s, so a page that opens costs a listing per
// zone, never a check per domain, and none of a caller's live checks (ADR 0104, the
// review's B-M2).
func (h *Handler) readDNS(ctx context.Context, domains []*dockwellv1.Domain) {
	sem := make(chan struct{}, readsAtOnce)
	var wg sync.WaitGroup
	for _, d := range domains {
		if len(d.GetServers()) == 0 {
			continue
		}
		srv, err := h.servers.Get(d.GetServers()[0].GetServerId())
		if err != nil {
			continue
		}
		own := srv.GetInventory().GetAddresses()
		if h.Cloudflare != nil {
			if recs, inZone, err := h.Cloudflare.ZoneRecordsAt(ctx, d.GetName()); inZone && err == nil {
				d.Dns = zoneCheck(d.GetName(), recs, own)
				continue
			}
		}
		key := srv.GetId() + " " + d.GetName()
		if c := h.dnsSeen.get(key, time.Now()); c != nil {
			d.Dns = c
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			d.Dns = h.publicCheck(ctx, srv.GetId(), own, d.GetName())
			h.dnsSeen.put(key, d.Dns, time.Now())
		}()
	}
	wg.Wait()
}

// zoneCheck is what a connected zone's records at name say: proxied, which says
// nothing of the origin address public DNS hides; pointing at the server's own
// addresses; elsewhere; or no record.
func zoneCheck(name string, recs []dnsproviders.Record, own []string) *dockwellv1.DomainCheck {
	out := &dockwellv1.DomainCheck{Domain: name, State: dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_NO_RECORD}
	public := resource.PublicAddresses(own)
	for i := range recs {
		r := recs[i]
		if r.Proxied {
			// Cloudflare answers for it: its content is the origin, said to no one here.
			out.Proxied, out.Found = true, nil
			out.State = dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_ELSEWHERE
			return out
		}
		if r.Type == "CNAME" {
			out.State = dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_CANT_TELL
			continue
		}
		out.Found = append(out.Found, r.Content)
	}
	if len(out.Found) > 0 {
		out.State = dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_ELSEWHERE
		for _, a := range out.Found {
			if slices.Contains(public, a) || slices.Contains(own, a) {
				out.State = dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_POINTS_HERE
			}
		}
	}
	return out
}

// publicCheck reads public DNS for name against a server's addresses, as CheckDomain
// does, keeping only answers the network guard allows to be said: a private
// address that isn't the server's own is no one's business here.
func (h *Handler) publicCheck(ctx context.Context, server string, own []string, name string) *dockwellv1.DomainCheck {
	dns := &ingress.DNS{Lookup: h.Lookup, Addrs: func() ([]netip.Addr, error) {
		var public []netip.Addr
		for _, s := range resource.PublicAddresses(own) {
			public = append(public, netip.MustParseAddr(s).Unmap())
		}
		return public, nil
	}}
	dns.Know(h.servers.PublicAddress(server))
	check := dns.Check(ctx, name)
	check.Found = slices.DeleteFunc(check.Found, func(s string) bool {
		return netguard.Forbidden(s, netguard.Options{}) != "" && !slices.Contains(own, s)
	})
	return resourcepb.DomainCheck(check)
}
