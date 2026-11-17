// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/resource/dnsprovider"
	"github.com/dockwell/dockwell/internal/store"
)

// CheckAsk is a name to check before any plan: the server it's meant to reach, the
// project it's for, and the way picked, if one was.
type CheckAsk struct {
	Server, Project, Name string
	// Admin says the caller may see what Cloudflare holds: a record's content is the
	// origin address public DNS hides, so below an admin the check says only that a
	// name is taken (ADR 0104, M12).
	Admin bool
	Way   resourcev1.RouteWay
}

// cache keeps what a check reads that changes slowly, for about 30 seconds, so a
// person typing reads only the name's records each time (ADR 0104, N12).
type cache struct {
	mu   sync.Mutex
	data map[string]cached
}

type cached struct {
	value any
	at    time.Time
}

const cacheFor = 30 * time.Second

// forget drops what's kept of key, as a write of Dockwell's own makes it old.
func (c *cache) forget(key string) {
	c.mu.Lock()
	delete(c.data, key)
	c.mu.Unlock()
}

func (c *cache) get(key string, now time.Time, read func() (any, error)) (any, error) {
	c.mu.Lock()
	if c.data == nil {
		c.data = map[string]cached{}
	}
	if v, ok := c.data[key]; ok && now.Sub(v.at) < cacheFor {
		c.mu.Unlock()
		return v.value, nil
	}
	c.mu.Unlock()
	v, err := read()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.data[key] = cached{value: v, at: now}
	c.mu.Unlock()
	return v, nil
}

// providerFor is the connected provider that serves project and holds name in a
// zone, or "".
func (p *Publisher) providerFor(ctx context.Context, project, name string) (string, error) {
	all, err := p.Providers.All(ctx)
	if err != nil {
		return "", err
	}
	for i := range all {
		spec := all[i].Spec
		if !dnsprovider.Serves(spec, project) {
			continue
		}
		for _, z := range spec.GetZones() {
			if dnsprovider.Covers(z, name) {
				return all[i].Name, nil
			}
		}
	}
	return "", nil
}

// Check says what a connected Cloudflare zone and the server's tunnels say of a name,
// and each way in, with the one the check would take: nil where no connected
// provider serving the project holds the name (ADR 0104, section 1).
func (p *Publisher) Check(ctx context.Context, a CheckAsk) (*dockwellv1.CloudflareCheck, error) {
	name := strings.ToLower(a.Name)
	provider, err := p.providerFor(ctx, a.Project, name)
	if err != nil || provider == "" {
		return nil, err
	}
	t, err := p.target(ctx, provider, name)
	if err != nil {
		return nil, err
	}
	out := &dockwellv1.CloudflareCheck{Zone: t.zone.Name, Provider: provider, State: dockwellv1.CloudflareName_CLOUDFLARE_NAME_FREE}
	now := p.now()
	if mode, err := p.cache.get("ssl:"+t.zone.ID, now, func() (any, error) {
		m, _, err := t.cf.SSLMode(ctx, t.zone)
		return m, err
	}); err == nil {
		out.SslMode, _ = mode.(string)
	}
	there, err := t.cf.RecordsAt(ctx, t.zone, name)
	if err != nil {
		return nil, err
	}
	var ours []store.CloudflareRecord
	if err := p.DB.View(ctx, func(tx *store.Tx) error {
		all, err := tx.CloudflareRecordsIn(ctx, t.zone.ID)
		for i := range all {
			if all[i].Name == name {
				ours = append(ours, all[i])
			}
		}
		return err
	}); err != nil {
		return nil, err
	}
	facts, err := p.Server(ctx, a.Server)
	if err != nil {
		return nil, err
	}
	for i := range there {
		x := there[i]
		if a.Admin {
			out.Types = append(out.Types, x.Type)
		}
		if !addressLike(x.Type) {
			continue
		}
		mine := false
		for j := range ours {
			if ours[j].ID == x.ID {
				mine = true
			}
		}
		switch {
		case mine:
			out.State = dockwellv1.CloudflareName_CLOUDFLARE_NAME_OURS
		case pointsHere(x, facts):
			out.State = dockwellv1.CloudflareName_CLOUDFLARE_NAME_TAKE_ON
		default:
			out.State = dockwellv1.CloudflareName_CLOUDFLARE_NAME_TAKEN
		}
		if a.Admin && !mine {
			out.Record, out.RecordId = recordWords(x), x.ID
		}
	}
	past := pastCertificate(t.zone.Name, name)
	if past && out.State == dockwellv1.CloudflareName_CLOUDFLARE_NAME_FREE {
		out.State = dockwellv1.CloudflareName_CLOUDFLARE_NAME_PAST_CERTIFICATE
	}
	out.Ways, out.Way = p.ways(ctx, a, t, name, facts, past)
	return out, nil
}

// pointsHere says a record made by hand points where Dockwell would: at a tunnel, or
// at one of the server's addresses.
func pointsHere(r dnsproviders.Record, facts Facts) bool {
	if r.Type == "CNAME" {
		return strings.HasSuffix(r.Content, ".cfargotunnel.com")
	}
	for _, a := range facts.Public {
		if a.Unmap().String() == r.Content {
			return true
		}
	}
	return false
}

// ways are the ways in for a name, each with why it's off, and the one to take:
// the server's tunnel; then, where cloudflared runs that Dockwell can't use,
// something else in front, never publishing the server's address; then Cloudflare's
// proxy; then the server's own HTTPS (ADR 0104, section 2).
func (p *Publisher) ways(ctx context.Context, a CheckAsk, t *target, name string, facts Facts, past bool) ([]*dockwellv1.WayOption, resourcev1.RouteWay) {
	var out []*dockwellv1.WayOption
	pick := resourcev1.RouteWay_ROUTE_WAY_UNSPECIFIED
	runsCloudflared := false
	found, err := p.cache.get("tunnels:"+a.Server, p.now(), func() (any, error) {
		if p.Tunnels == nil {
			return []*dockwellv1.CloudflaredTunnel(nil), nil
		}
		return p.Tunnels(ctx, a.Server)
	})
	tunnels, _ := found.([]*dockwellv1.CloudflaredTunnel)
	if err != nil {
		// Whether cloudflared runs there isn't known, so the server's address isn't
		// published by default.
		runsCloudflared = true
	}
	seen := map[string]bool{}
	for _, f := range tunnels {
		runsCloudflared = true
		if seen[f.GetTunnelId()] {
			continue
		}
		seen[f.GetTunnelId()] = true
		opt := &dockwellv1.WayOption{Way: resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL, Tunnel: f.GetTunnelId(), TunnelName: f.GetTunnelId()}
		out = append(out, opt)
		if f.GetManagedInFile() {
			opt.Off = facts.Name + "'s tunnel is managed in a file, so Dockwell writes nothing at Cloudflare"
			continue
		}
		if past {
			opt.Off = name + " is past Cloudflare's free certificate"
			continue
		}
		if _, err := p.proveTunnel(ctx, a.Server, t, f.GetTunnelId(), facts); err != nil {
			opt.Off = offWords(err)
			if denied(err) {
				opt.Action, opt.ActionUrl = "Add it to this token", dnsproviders.TokenPage(t.zone.Account)
			}
			continue
		}
		account := f.GetAccountTag()
		if info, err := t.tunnels.TunnelByID(ctx, account, f.GetTunnelId()); err == nil {
			opt.TunnelName = info.Name
		}
		doc, err := p.cache.get("doc:"+f.GetTunnelId(), p.now(), func() (any, error) {
			cfg, err := t.tunnels.ReadTunnelConfig(ctx, account, f.GetTunnelId())
			if err != nil {
				return nil, err
			}
			return Parse(cfg.Config)
		})
		if err != nil {
			opt.Off = offWords(err)
			continue
		}
		d, _ := doc.(*Document)
		rt, err := p.routing(ctx, t, f.GetTunnelId(), d, f.GetReaches(), name)
		if err != nil {
			opt.Off = offWords(err)
			continue
		}
		need := NeedFor(d, rt, t.zone.Name, name)
		var caught Caught
		switch {
		case errors.As(need.Refused, &caught):
			opt.Off = opt.TunnelName + " sends it elsewhere"
			if a.Admin {
				opt.CaughtBy, opt.Off = caught.Rule.Hostname+", to "+caught.Rule.Service, caught.Error()
			}
		case need.Refused != nil:
			opt.Off = need.Refused.Error()
		case !need.Nothing:
			opt.AddsRule = need.Rule.Hostname
		}
		if opt.Off == "" && pick == resourcev1.RouteWay_ROUTE_WAY_UNSPECIFIED {
			pick = resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL
		}
	}
	public := len(facts.Public) > 0
	proxy := &dockwellv1.WayOption{Way: resourcev1.RouteWay_ROUTE_WAY_PROXY}
	own := &dockwellv1.WayOption{Way: resourcev1.RouteWay_ROUTE_WAY_OWN_HTTPS}
	if !public {
		proxy.Off = facts.Name + " has no public address"
		own.Off = proxy.Off
	} else if past {
		proxy.Off = name + " is past Cloudflare's free certificate"
	}
	front := &dockwellv1.WayOption{Way: resourcev1.RouteWay_ROUTE_WAY_FRONT}
	out = append(out, proxy, own, front)
	switch {
	case pick != resourcev1.RouteWay_ROUTE_WAY_UNSPECIFIED:
	case runsCloudflared:
		// A tunnel means its address is kept off DNS: never published by default.
		pick = resourcev1.RouteWay_ROUTE_WAY_FRONT
	case public && !past:
		pick = resourcev1.RouteWay_ROUTE_WAY_PROXY
	case public:
		pick = resourcev1.RouteWay_ROUTE_WAY_OWN_HTTPS
	default:
		out = append(out, &dockwellv1.WayOption{Way: resourcev1.RouteWay_ROUTE_WAY_OWN_TUNNEL, Off: facts.Name + " has no public address and no tunnel", Action: "Add a tunnel"})
	}
	if a.Way != resourcev1.RouteWay_ROUTE_WAY_UNSPECIFIED {
		pick = a.Way
	}
	return out, pick
}

// offWords is a refusal as a way's state, in a line.
func offWords(err error) string {
	s := err.Error()
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i]
	}
	return strings.TrimSuffix(s, ".")
}

// denied says Cloudflare refused for want of a right.
func denied(err error) bool {
	var api *dnsproviders.APIError
	return errors.As(err, &api) && (api.Status == 403 || api.Code == 10000 || api.Code == 9109)
}
