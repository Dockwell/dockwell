// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

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
	// Fresh reads the server's tunnels now, past the 30 s they're kept, as a person
	// pressing Check again has just started cloudflared (the review's open question 3).
	Fresh bool
}

// cache keeps what a check reads that changes slowly, for about 30 seconds, so a
// person typing reads only the name's records each time (ADR 0104, N12, and the
// review's M3). Once Cloudflare says too many requests, checks read nothing for a
// minute, leaving the token's rate to renewals and plans.
type cache struct {
	mu   sync.Mutex
	data map[string]cached
	slow time.Time
}

// slowFor is how long checks wait once Cloudflare says too many requests.
const slowFor = time.Minute

// waiting is a check while Cloudflare asks Dockwell to wait, with when checks read
// it again, or nil when they read it now (the re-check's L11).
func (p *Publisher) waiting(provider string) *dockwellv1.CloudflareCheck {
	now := p.now()
	p.cache.mu.Lock()
	until := p.cache.slow
	p.cache.mu.Unlock()
	if !now.Before(until) {
		return nil
	}
	return &dockwellv1.CloudflareCheck{Provider: provider, State: dockwellv1.CloudflareName_CLOUDFLARE_NAME_WAITING, ChecksAgain: timestamppb.New(until)}
}

// slowOn marks a 429 from Cloudflare, and gives err back.
func (c *cache) slowOn(err error, now time.Time) error {
	var api *dnsproviders.APIError
	if errors.As(err, &api) && api.Status == http.StatusTooManyRequests {
		c.mu.Lock()
		c.slow = now.Add(slowFor)
		c.mu.Unlock()
	}
	return err
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

// forgetUnder drops what's kept of every key that starts with prefix.
func (c *cache) forgetUnder(prefix string) {
	c.mu.Lock()
	for k := range c.data {
		if strings.HasPrefix(k, prefix) {
			delete(c.data, k)
		}
	}
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
		return nil, c.slowOn(err, now)
	}
	c.mu.Lock()
	c.data[key] = cached{value: v, at: now}
	c.mu.Unlock()
	return v, nil
}

// readFor reads through the cache under key where t is a live check's, and fresh
// otherwise: a plan proves what it writes against what Cloudflare says now. The one
// exception is a token's zone list, which target reads through the cache for plans
// and applies too: zone IDs don't change, so a stale list can only name a zone that's
// gone, and Cloudflare refuses a write there (the re-check's L12).
func readFor[T any](p *Publisher, t *target, key string, read func() (T, error)) (T, error) {
	if !t.cached {
		v, err := read()
		return v, p.cache.slowOn(err, p.now())
	}
	v, err := p.cache.get(key, p.now(), func() (any, error) { return read() })
	if err != nil {
		var zero T
		return zero, err
	}
	return v.(T), nil //nolint:forcetypeassert // only read puts a value under key
}

// providerFor finds the most specific connected zone that serves the project, or a
// matching zone outside its reach. With no project, an admin's new website can
// use any provider's zone and add its project in the website's plan.
func (p *Publisher) providerFor(ctx context.Context, project, name string) (provider, zone string, outside bool, err error) {
	all, err := p.Providers.All(ctx)
	if err != nil {
		return "", "", false, err
	}
	for _, saved := range all {
		unreached := project != "" && !dnsprovider.Serves(saved.Spec, project)
		if provider != "" && !outside && unreached {
			continue
		}
		for _, z := range saved.Spec.GetZones() {
			if dnsprovider.Covers(z, name) && (outside && !unreached || len(z) > len(zone)) {
				provider, zone, outside = saved.Name, z, unreached
			}
		}
	}
	return provider, zone, outside, nil
}

// Check says what a connected Cloudflare zone and the server's tunnels say of a name,
// and each way in, with the one the check would take. For zones outside the project
// it reads nothing at Cloudflare and returns only the reach state below an admin.
func (p *Publisher) Check(ctx context.Context, a CheckAsk) (out *dockwellv1.CloudflareCheck, err error) {
	name := strings.ToLower(a.Name)
	if a.Project == "" && !a.Admin {
		return nil, nil
	}
	provider, zone, outside, err := p.providerFor(ctx, a.Project, name)
	if err != nil || provider == "" {
		return nil, err
	}
	if outside {
		out := &dockwellv1.CloudflareCheck{State: dockwellv1.CloudflareName_CLOUDFLARE_NAME_OUTSIDE_PROJECT}
		if a.Admin {
			out.Provider, out.Zone = provider, zone
		}
		return out, nil
	}
	if w := p.waiting(provider); w != nil {
		return w, nil
	}
	// A 429 met on the way says so as a state, as the checks after it do. Any other
	// failure says the provider that couldn't be read, with no state, so the panel
	// says so with Check again rather than planning as if the zone weren't Cloudflare's
	// (the review's B-M1).
	defer func() {
		if w := p.waiting(provider); err != nil && w != nil {
			out, err = w, nil
		}
		if err != nil && out == nil {
			out = &dockwellv1.CloudflareCheck{Provider: provider}
		}
	}()
	t, err := p.target(ctx, provider, name)
	if err != nil {
		return nil, err
	}
	t.cached = true
	out = &dockwellv1.CloudflareCheck{Zone: t.zone.Name, Provider: provider, State: dockwellv1.CloudflareName_CLOUDFLARE_NAME_FREE}
	now := p.now()
	if mode, err := p.cache.get("ssl:"+t.zone.ID, now, func() (any, error) {
		m, _, err := t.cf.SSLMode(ctx, t.zone)
		return m, err
	}); err == nil {
		out.SslMode, _ = mode.(string)
	}
	there, err := t.cf.RecordsAt(ctx, t.zone, name)
	if err != nil {
		return nil, p.cache.slowOn(err, now)
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
	out.Ways, out.Way, out.HandRule = p.ways(ctx, a, t, name, facts, past)
	return out, nil
}

// ZoneRecordsAt is what a connected zone holds at name, read in one listing of the
// zone kept for 30 s, so a page of domains costs a listing per zone, never a check
// per name (ADR 0104, the review's B-M2). inZone says a connected zone holds it.
func (p *Publisher) ZoneRecordsAt(ctx context.Context, name string) (recs []dnsproviders.Record, inZone bool, err error) {
	name = strings.ToLower(name)
	all, err := p.Providers.All(ctx)
	if err != nil {
		return nil, false, err
	}
	provider, zone := "", ""
	for i := range all {
		for _, z := range all[i].Spec.GetZones() {
			if dnsprovider.Covers(z, name) && len(z) > len(zone) {
				provider, zone = all[i].Name, z
			}
		}
	}
	if provider == "" {
		return nil, false, nil
	}
	if w := p.waiting(provider); w != nil {
		return nil, true, errors.New("cloudflare asked Dockwell to wait, so its zones are read again in a minute")
	}
	t, err := p.target(ctx, provider, name)
	if err != nil {
		return nil, true, err
	}
	listed, err := p.cache.get("zone:"+t.zone.ID, p.now(), func() (any, error) { return t.cf.ZoneRecords(ctx, t.zone) })
	if err != nil {
		return nil, true, err
	}
	zoneRecs, _ := listed.([]dnsproviders.Record)
	for i := range zoneRecs {
		if strings.EqualFold(zoneRecs[i].Name, name) && addressLike(zoneRecs[i].Type) {
			recs = append(recs, zoneRecs[i])
		}
	}
	return recs, true, nil
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
func (p *Publisher) ways(ctx context.Context, a CheckAsk, t *target, name string, facts Facts, past bool) ([]*dockwellv1.WayOption, resourcev1.RouteWay, *dockwellv1.HandRule) {
	var out []*dockwellv1.WayOption
	var hand *dockwellv1.HandRule
	if a.Fresh {
		p.cache.forget("tunnels:" + a.Server)
	}
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
	unread := false
	for _, f := range tunnels {
		runsCloudflared = true
		if f.GetTunnelId() == "" {
			unread = true
			continue
		}
		if seen[f.GetTunnelId()] {
			continue
		}
		seen[f.GetTunnelId()] = true
		opt := &dockwellv1.WayOption{Way: resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL, Tunnel: f.GetTunnelId(), TunnelName: f.GetTunnelId(), Reaches: f.GetReaches()}
		out = append(out, opt)
		if f.GetManagedInFile() {
			opt.Off = facts.Name + "'s tunnel is managed in a file, so Dockwell writes nothing at Cloudflare"
			continue
		}
		if past {
			opt.Off = name + " is past Cloudflare's free certificate"
			continue
		}
		account := cmp.Or(f.GetAccountTag(), t.zone.Account)
		if info, err := readFor(p, t, "info:"+f.GetTunnelId(), func() (dnsproviders.Tunnel, error) {
			return t.tunnels.TunnelByID(ctx, account, f.GetTunnelId())
		}); err == nil {
			opt.TunnelName = info.Name
		}
		// Below an admin, a refusal says the tunnel's name and a fixed reason, never its
		// rules, their services, other names or servers (ADR 0104, section 1, and the
		// review's M7).
		off := func(err error) string {
			if a.Admin {
				return offWords(err)
			}
			return "Dockwell can't publish through " + opt.TunnelName + " now. An admin can see why"
		}
		if _, err := p.proveTunnel(ctx, a.Server, t, f.GetTunnelId(), facts); err != nil {
			opt.Off = off(err)
			if denied(err) && a.Admin {
				needTunnelEdit(opt, p.tunnelCapabilities(ctx, t, account, a.Fresh).FixURL)
			}
			continue
		}
		doc, err := p.cache.get("doc:"+f.GetTunnelId(), p.now(), func() (any, error) {
			cfg, err := t.tunnels.ReadTunnelConfig(ctx, account, f.GetTunnelId())
			if err != nil {
				return nil, err
			}
			return Parse(cfg.Config)
		})
		if err != nil {
			opt.Off = off(err)
			if denied(err) && a.Admin {
				needTunnelEdit(opt, p.tunnelCapabilities(ctx, t, account, a.Fresh).FixURL)
			}
			continue
		}
		d, _ := doc.(*Document)
		// A rule made by hand that serves the name, for an admin to take on or move to
		// Dockwell from the panel (the review's B-M4).
		if a.Admin && hand == nil {
			hand = p.handRuleAt(ctx, d, f, opt.TunnelName, name)
		}
		rt, err := p.routing(ctx, t, f.GetTunnelId(), d, f.GetReaches(), name)
		if err != nil {
			opt.Off = off(err)
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
		case need.Refused != nil && !a.Admin && errors.Is(need.Refused, ErrServesPanel):
			opt.Off = opt.TunnelName + " serves the panel there"
		case need.Refused != nil && !a.Admin:
			opt.Off = opt.TunnelName + " would send another name elsewhere"
		case need.Refused != nil:
			opt.Off = need.Refused.Error()
		case !need.Nothing:
			if a.Admin {
				caps := p.tunnelCapabilities(ctx, t, account, a.Fresh)
				if caps.Tunnels == dnsproviders.Lacks {
					needTunnelEdit(opt, caps.FixURL)
					continue
				}
			}
			opt.AddsRule = need.Rule.Hostname
		}
		if opt.Off == "" && pick == resourcev1.RouteWay_ROUTE_WAY_UNSPECIFIED {
			pick = resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL
		}
	}
	if unread {
		out = append(out, &dockwellv1.WayOption{Way: resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL, Off: facts.Name + " runs cloudflared Dockwell can't read"})
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
		// No way in: the check says so as the way it takes, with what fixes it, so the
		// panel shows the reason and never a way it can't name.
		out = append(out, &dockwellv1.WayOption{Way: resourcev1.RouteWay_ROUTE_WAY_OWN_TUNNEL, Off: facts.Name + " has no public address and no tunnel", Action: "Add a tunnel"})
		pick = resourcev1.RouteWay_ROUTE_WAY_OWN_TUNNEL
	}
	if a.Way != resourcev1.RouteWay_ROUTE_WAY_UNSPECIFIED {
		pick = a.Way
	}
	return out, pick, hand
}

// tunnelCapabilities checks a write right only when a new rule needs it. A token
// that can't read its own rights can still prove the tunnel and offer a plan.
func (p *Publisher) tunnelCapabilities(ctx context.Context, t *target, account string, fresh bool) dnsproviders.Capabilities {
	key := fmt.Sprintf("tunnel-rights:%s:%x", account, sha256.Sum256([]byte(t.tunnels.Token)))
	if fresh {
		p.cache.forget(key)
	}
	caps, _ := readFor(p, t, key, func() (dnsproviders.Capabilities, error) {
		return t.tunnels.CheckCapabilities(ctx, []string{account}, false), nil
	})
	return caps
}

func needTunnelEdit(opt *dockwellv1.WayOption, url string) {
	opt.Off = "The token lacks Cloudflare Tunnel edit"
	opt.Action, opt.ActionUrl = "Add it to this token", url
}

// handRuleAt is the rule made by hand in a tunnel's document at name, where one is
// and Dockwell holds none there: where it sends, and whether that's the server's
// proxy already.
func (p *Publisher) handRuleAt(ctx context.Context, d *Document, f *dockwellv1.CloudflaredTunnel, tunnelName, name string) *dockwellv1.HandRule {
	if d == nil {
		return nil
	}
	i := d.Index(name, "")
	if i < 0 {
		return nil
	}
	held := false
	if err := p.DB.View(ctx, func(tx *store.Tx) error {
		_, err := tx.CloudflareRuleAt(ctx, f.GetTunnelId(), d.Rules[i].Hostname, "")
		held = err == nil
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}); err != nil || held {
		return nil
	}
	return &dockwellv1.HandRule{
		Tunnel: f.GetTunnelId(), TunnelName: tunnelName, Hostname: d.Rules[i].Hostname, Service: d.Rules[i].Service,
		ReachesProxy: SameService(d.Rules[i].Service, f.GetReaches()),
	}
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
