// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/dnsprovider"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// Facts are what publishing needs to know of a server.
type Facts struct {
	// Name is the server's name, as plans say it.
	Name string
	// Public are its public addresses, which a proxied or DNS-only record points at.
	Public []netip.Addr
	// WaysOut are the addresses it leaves from, as ADR 0034 knows them, which a
	// tunnel's connections come from where no connector ID can be read.
	WaysOut []string
	// SharesWith are the other servers of the fleet that leave from one of the same
	// addresses, which addresses alone can't tell from this one (ADR 0104, section 3).
	SharesWith []string
}

// Publisher plans and takes a change's steps at Cloudflare (ADR 0104).
type Publisher struct {
	DB        *store.DB
	Providers *dnsproviders.Providers
	// Install is this control plane's install ID, which every record's comment
	// names: "dockwell <install-id> <route-id>".
	Install string
	// Tunnels asks a server's agent which tunnels cloudflared runs there now.
	Tunnels func(ctx context.Context, server string) ([]*dockwellv1.CloudflaredTunnel, error)
	// Server says what publishing needs of a server.
	Server func(ctx context.Context, server string) (Facts, error)
	// Panel says every name the panel answers to, and its own listener as a
	// tunnel's rule would name it.
	Panel func(ctx context.Context) (names []string, service string)
	// ProjectOf says the project an environment is in.
	ProjectOf func(ctx context.Context, environment string) (string, error)
	// Routes are every route as it stands, by resource ID, to tell when a zone's
	// rule is the last route's.
	Routes func(ctx context.Context) (map[string]*resourcev1.Route, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// LockWait is how long a step waits for another writer of a tunnel; zero waits
	// a minute.
	LockWait time.Duration

	// cache keeps what a live check reads that changes slowly.
	cache cache
}

var _ revision.Publisher = (*Publisher)(nil)

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Comment is what a record Dockwell writes says of itself, so a step run again
// finds it, and a control plane restored from a backup tells its records from those
// made by hand (ADR 0104, section 5).
func (p *Publisher) Comment(routeID string) string {
	return "dockwell " + p.Install + " " + routeID
}

// target is where a route publishes: its provider, its token's API, and its zone.
type target struct {
	provider string
	spec     *resourcev1.DnsProvider
	cf       *dnsproviders.Cloudflare
	tunnels  *dnsproviders.Cloudflare
	zone     dnsproviders.Zone
}

func (p *Publisher) target(ctx context.Context, provider, name string) (*target, error) {
	saved, err := p.Providers.Named(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("%s names the DNS provider %s, which isn't connected", name, provider)
	}
	api, err := p.Providers.APIFor(saved.Spec.GetProvider(), saved.Spec.GetApiToken())
	if err != nil {
		return nil, err
	}
	cf, ok := api.(*dnsproviders.Cloudflare)
	if !ok {
		return nil, errors.New("only a Cloudflare provider publishes domains")
	}
	t := &target{provider: provider, spec: saved.Spec, cf: cf, tunnels: cf}
	if second := saved.Spec.GetTunnelApiToken(); second != "" {
		if api, err := p.Providers.APIFor(saved.Spec.GetProvider(), second); err == nil {
			if c, ok := api.(*dnsproviders.Cloudflare); ok {
				t.tunnels = c
			}
		}
	}
	zoneName := ""
	for _, z := range saved.Spec.GetZones() {
		if dnsprovider.Covers(z, name) && len(z) > len(zoneName) {
			zoneName = z
		}
	}
	if zoneName == "" {
		return nil, fmt.Errorf("%s is in none of %s's zones, so Dockwell can't publish it there", name, provider)
	}
	zones, err := cf.Zones(ctx)
	if err != nil {
		return nil, fmt.Errorf("can't read %s's zones at Cloudflare: %w", provider, err)
	}
	for _, z := range zones {
		if z.Name == zoneName {
			t.zone = z
			return t, nil
		}
	}
	return nil, fmt.Errorf("%s's token doesn't reach %s at Cloudflare any more. Check the provider again under Integrations", provider, zoneName)
}

// desired is a record a route's publishing writes.
func desired(name, tunnel string, way resourcev1.RouteWay, facts Facts, comment string) ([]dnsproviders.Record, error) {
	switch way {
	case resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL:
		return []dnsproviders.Record{{Type: "CNAME", Name: name, Content: tunnel + ".cfargotunnel.com", Proxied: true, TTL: 1, Comment: comment}}, nil
	case resourcev1.RouteWay_ROUTE_WAY_PROXY, resourcev1.RouteWay_ROUTE_WAY_OWN_HTTPS:
		var out []dnsproviders.Record
		for _, a := range facts.Public {
			typ := "A"
			if a.Is6() && !a.Is4In6() {
				typ = "AAAA"
			}
			if slices.ContainsFunc(out, func(r dnsproviders.Record) bool { return r.Type == typ }) {
				continue
			}
			out = append(out, dnsproviders.Record{Type: typ, Name: name, Content: a.Unmap().String(), Proxied: way == resourcev1.RouteWay_ROUTE_WAY_PROXY, TTL: 1, Comment: comment})
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%s has no public address, so a record can't point at it. Publish %s through a tunnel", facts.Name, name)
		}
		return out, nil
	case resourcev1.RouteWay_ROUTE_WAY_FRONT, resourcev1.RouteWay_ROUTE_WAY_UNSPECIFIED, resourcev1.RouteWay_ROUTE_WAY_OWN_TUNNEL:
	}
	return nil, nil
}

// addressLike are the types a CNAME can't sit beside, nor each other beside one.
func addressLike(t string) bool { return t == "A" || t == "AAAA" || t == "CNAME" }

// pastCertificate says name is two labels or more below zone, where Cloudflare's
// free certificate doesn't reach (ADR 0104, Verified 2).
func pastCertificate(zone, name string) bool {
	rest := strings.TrimSuffix(name, "."+zone)
	return rest != name && strings.Contains(rest, ".")
}

func recordWords(r dnsproviders.Record) string {
	if r.Proxied {
		return r.Type + " " + r.Content + ", proxied"
	}
	return r.Type + " " + r.Content
}

func step(route revision.PublishRoute, action resource.Action, p resource.ProviderStep, access ...resource.Access) resource.Step {
	return resource.Step{Ref: route.Ref, Decision: resource.Decision{Action: action, Provider: &p, HostAccess: access}}
}

// Plan works out each route's steps at Cloudflare: what it publishes after the
// change against what Dockwell wrote for it, what Cloudflare holds at its name, and
// what its tunnel's document needs.
func (p *Publisher) Plan(ctx context.Context, changeID string, routes []revision.PublishRoute) (map[resource.Ref][]resource.Step, error) {
	out := map[resource.Ref][]resource.Step{}
	rules := map[string]bool{} // rules a route of this change adds already, by tunnel|hostname
	inChange := map[string]*resourcev1.Route{}
	for _, r := range routes {
		inChange[r.ID] = r.After
	}
	for _, r := range routes {
		steps, err := p.planRoute(ctx, r, rules, inChange)
		if err != nil {
			return nil, err
		}
		out[r.Ref] = steps
	}
	return out, nil
}

func (p *Publisher) planRoute(ctx context.Context, r revision.PublishRoute, rules map[string]bool, inChange map[string]*resourcev1.Route) ([]resource.Step, error) {
	var written, kept []store.CloudflareRecord
	if err := p.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		if written, err = tx.CloudflareRecordsOf(ctx, r.ID); err != nil {
			return err
		}
		kept, err = tx.CloudflarePendingOf(ctx, r.ID)
		return err
	}); err != nil {
		return nil, err
	}
	var steps []resource.Step
	keep := map[string]bool{} // written records this plan keeps, by ID
	if pub := r.After.GetPublish(); pub != nil && pub.GetWay() != resourcev1.RouteWay_ROUTE_WAY_FRONT {
		pre, kept, err := p.planWrites(ctx, r, pub, written, rules)
		if err != nil {
			return nil, err
		}
		steps = append(steps, pre...)
		for id := range kept {
			keep[id] = true
		}
	}
	// A record made by hand stays replaced only while the route's publish consents to
	// taking it over, by its ID: consent taken away, as by undo, gives it back (the
	// review's H1).
	for i := range written {
		w := &written[i]
		var old dnsproviders.Record
		if !keep[w.ID] || w.Replaced == "" || json.Unmarshal([]byte(w.Replaced), &old) != nil || old.ID == "" {
			continue
		}
		if !slices.ContainsFunc(r.After.GetPublish().GetTakesOver(), func(t *resourcev1.TakeOver) bool { return t.GetRecordId() == old.ID }) {
			delete(keep, w.ID)
		}
	}
	// What Dockwell wrote that this route doesn't publish any more comes off, after
	// the route is gone from its server, and what it replaced goes back.
	for i := range written {
		w := &written[i]
		if keep[w.ID] {
			continue
		}
		rec := &resource.ProviderRecord{ID: w.ID, Type: w.Type, Name: w.Name, Content: w.Content, Proxied: w.Proxied, TTL: w.TTL, Comment: w.Comment}
		words := recordWords(dnsproviders.Record{Type: w.Type, Content: w.Content, Proxied: w.Proxied})
		if w.TakenOnAt != nil {
			words += ", made in Cloudflare and taken on " + w.TakenOnAt.Format("2 January")
		}
		steps = append(steps, step(r, resource.ActionDelete, resource.ProviderStep{
			Provider: w.Provider, Action: resource.ProviderDeleteRecord, Target: w.Zone, Name: w.Name, Was: words, ZoneID: w.ZoneID, Record: rec,
		}))
		if w.Replaced != "" {
			var old dnsproviders.Record
			if json.Unmarshal([]byte(w.Replaced), &old) == nil {
				steps = append(steps, step(r, resource.ActionDelete, resource.ProviderStep{
					Provider: w.Provider, Action: resource.ProviderRestoreRecord, Target: w.Zone, Name: w.Name, To: recordWords(old), ZoneID: w.ZoneID,
					Record: &resource.ProviderRecord{Type: old.Type, Name: old.Name, Content: old.Content, Proxied: old.Proxied, TTL: old.TTL, Comment: old.Comment},
				}))
			}
		}
	}
	// What a replace kept of a record made by hand, before a delete that landed without
	// its write, goes back where the route doesn't publish that name any more. Where it
	// does, the write takes it on (createRecord) (the review's H1).
	publishes := strings.ToLower(r.After.GetDomain())
	if pub := r.After.GetPublish(); pub == nil || pub.GetWay() == resourcev1.RouteWay_ROUTE_WAY_FRONT {
		publishes = ""
	}
	for i := range kept {
		k := &kept[i]
		var old dnsproviders.Record
		if strings.EqualFold(k.Name, publishes) || json.Unmarshal([]byte(k.Replaced), &old) != nil {
			continue
		}
		steps = append(steps, step(r, resource.ActionDelete, resource.ProviderStep{
			Provider: k.Provider, Action: resource.ProviderRestoreRecord, Target: k.Zone, Name: k.Name, To: recordWords(old), ZoneID: k.ZoneID, Pending: k.ID,
			Record: &resource.ProviderRecord{Type: old.Type, Name: old.Name, Content: old.Content, Proxied: old.Proxied, TTL: old.TTL, Comment: old.Comment},
		}))
	}
	removal, err := p.planRuleRemoval(ctx, r, inChange)
	if err != nil {
		return nil, err
	}
	return append(steps, removal...), nil
}

// planWrites plans what a route publishes: its records, and its tunnel's rule.
func (p *Publisher) planWrites(ctx context.Context, r revision.PublishRoute, pub *resourcev1.RoutePublish, written []store.CloudflareRecord, rules map[string]bool) ([]resource.Step, map[string]bool, error) {
	name := strings.ToLower(r.After.GetDomain())
	project, err := p.ProjectOf(ctx, r.Environment)
	if err != nil {
		return nil, nil, err
	}
	t, err := p.target(ctx, pub.GetProvider(), name)
	if err != nil {
		return nil, nil, err
	}
	// Writing at Cloudflare follows the provider's reach (ADR 0104, section 5).
	if !dnsprovider.Serves(t.spec, project) {
		return nil, nil, fmt.Errorf("the DNS provider %s doesn't serve this route's project, so %s can't be published through it. An admin adds the project to the provider", t.provider, name)
	}
	way := pub.GetWay()
	if pastCertificate(t.zone.Name, name) && way != resourcev1.RouteWay_ROUTE_WAY_OWN_HTTPS {
		return nil, nil, fmt.Errorf("%s is past Cloudflare's free certificate, which covers one level below %s. Pick a name one level down, such as %s", name, t.zone.Name,
			strings.ReplaceAll(strings.TrimSuffix(name, "."+t.zone.Name), ".", "-")+"."+t.zone.Name)
	}
	facts, err := p.Server(ctx, r.Server)
	if err != nil {
		return nil, nil, err
	}
	comment := p.Comment(r.ID)
	want, err := desired(name, pub.GetTunnel(), way, facts, comment)
	if err != nil {
		return nil, nil, err
	}
	var steps []resource.Step
	if way == resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL {
		rule, err := p.planRule(ctx, r, t, pub.GetTunnel(), name, facts, rules)
		if err != nil {
			return nil, nil, err
		}
		steps = append(steps, rule...)
	}
	there, err := t.cf.RecordsAt(ctx, t.zone, name)
	if err != nil {
		return nil, nil, fmt.Errorf("can't read what Cloudflare holds at %s: %w", name, err)
	}
	keep := map[string]bool{}
	ours := func(id string) *store.CloudflareRecord {
		i := slices.IndexFunc(written, func(w store.CloudflareRecord) bool { return w.ID == id })
		if i < 0 {
			return nil
		}
		return &written[i]
	}
	consented := func(id string) bool {
		return slices.ContainsFunc(pub.GetTakesOver(), func(t *resourcev1.TakeOver) bool { return t.GetRecordId() == id })
	}
	for _, d := range want {
		base := resource.ProviderStep{
			Provider: t.provider, Target: t.zone.Name, Name: name, To: recordWords(d), ZoneID: t.zone.ID,
			Record: &resource.ProviderRecord{Type: d.Type, Name: d.Name, Content: d.Content, Proxied: d.Proxied, TTL: d.TTL, Comment: d.Comment},
		}
		// Ours, as wanted: nothing to do.
		if i := slices.IndexFunc(there, func(x dnsproviders.Record) bool {
			return ours(x.ID) != nil && x.Type == d.Type && x.Content == d.Content && x.Proxied == d.Proxied
		}); i >= 0 {
			keep[there[i].ID] = true
			continue
		}
		var conflict *dnsproviders.Record
		for i := range there {
			x := there[i]
			if !addressLike(x.Type) || x.Type != d.Type && x.Type != "CNAME" && d.Type != "CNAME" {
				continue
			}
			conflict = &x
			break
		}
		switch {
		case conflict == nil || conflict.Comment == comment && conflict.Type == d.Type && conflict.Content == d.Content:
			// Free, or written for this route by a step a crash cut short, which
			// writing finds and keeps. A record of Dockwell's of this type that's gone
			// from there is written again, so it isn't removed as well.
			base.Action = resource.ProviderCreateRecord
			steps = append(steps, step(r, resource.ActionCreate, base))
			for i := range written {
				if written[i].Name == name && written[i].Type == d.Type {
					keep[written[i].ID] = true
				}
			}
		case ours(conflict.ID) != nil || conflict.Comment == comment:
			// Ours, written another way, as when the way in changed: replaced, and
			// what it replaced carried on, not put back.
			base.Action, base.Was = resource.ProviderReplaceRecord, recordWords(*conflict)
			base.Record.ID = conflict.ID
			keep[conflict.ID] = true
			steps = append(steps, step(r, resource.ActionUpdate, base))
		case conflict.Type == d.Type && conflict.Content == d.Content:
			// Made by hand, and pointing where Dockwell would: taken on, writing
			// nothing, with consent.
			if !consented(conflict.ID) {
				return nil, nil, fmt.Errorf("%s has a record at Cloudflare made by hand that points here already. An admin can take it on, so Dockwell manages it", name)
			}
			base.Action, base.Was = resource.ProviderTakeOnRecord, recordWords(*conflict)
			base.Record.ID = conflict.ID
			steps = append(steps, step(r, resource.ActionCreate, base, resource.Access{Kind: resource.AccessCloudflareTakeover, Path: name, Detail: recordWords(*conflict)}))
		default:
			if !consented(conflict.ID) {
				return nil, nil, fmt.Errorf("%s is taken at Cloudflare by a record Dockwell didn't make. An admin can replace it, or pick another name", name)
			}
			base.Action, base.Was = resource.ProviderReplaceRecord, recordWords(*conflict)+", made in Cloudflare"
			base.Record.ID = conflict.ID
			steps = append(steps, step(r, resource.ActionUpdate, base, resource.Access{Kind: resource.AccessCloudflareTakeover, Path: name, Detail: recordWords(*conflict)}))
		}
	}
	return steps, keep, nil
}

// planRule plans the rule a route's tunnel needs, once for its zone, or its apex.
func (p *Publisher) planRule(ctx context.Context, r revision.PublishRoute, t *target, tunnel, name string, facts Facts, rules map[string]bool) ([]resource.Step, error) {
	found, err := p.proveTunnel(ctx, r.Server, t, tunnel, facts)
	if err != nil {
		return nil, err
	}
	account := cmp.Or(found.GetAccountTag(), t.zone.Account)
	info, err := t.tunnels.TunnelByID(ctx, account, tunnel)
	if err != nil {
		return nil, fmt.Errorf("can't read the tunnel at Cloudflare: %w", err)
	}
	cfg, err := t.tunnels.ReadTunnelConfig(ctx, account, tunnel)
	if err != nil {
		return nil, fmt.Errorf("can't read %s's routes at Cloudflare: %w", info.Name, err)
	}
	if cfg.Source == "local" {
		return nil, fmt.Errorf("%s's tunnel is managed in a file, so Dockwell writes nothing at Cloudflare. Publish %s with something else in front", facts.Name, name)
	}
	doc, err := Parse(cfg.Config)
	if err != nil {
		return nil, err
	}
	routing, err := p.routing(ctx, t, tunnel, doc, found.GetReaches(), name)
	if err != nil {
		return nil, err
	}
	need := NeedFor(doc, routing, t.zone.Name, name)
	switch {
	case need.Refused != nil:
		return nil, fmt.Errorf("can't publish %s through %s: %w", name, info.Name, need.Refused)
	case need.Nothing:
		return nil, nil
	}
	key := tunnel + "|" + need.Rule.Hostname
	if rules[key] {
		return nil, nil
	}
	rules[key] = true
	words := routing.Unless(info.Name, doc.Rules)
	return []resource.Step{step(r, resource.ActionCreate, resource.ProviderStep{
		Provider: t.provider, Action: resource.ProviderAddRule, Target: info.Name, Name: need.Rule.Hostname, To: need.Rule.Service, Routing: words,
		Account: account, Tunnel: tunnel, ZoneID: t.zone.ID, Rule: &resource.ProviderRule{Hostname: need.Rule.Hostname, Service: need.Rule.Service},
	}, resource.Access{Kind: resource.AccessTunnelRule, Path: need.Rule.Hostname, Detail: info.Name, Address: need.Rule.Service, Type: words})}, nil
}

// routing gathers what the routing check keeps where it goes: the panel's names,
// the tunnel's own, and the zone's names whose DNS points at the tunnel.
func (p *Publisher) routing(ctx context.Context, t *target, tunnel string, doc *Document, reaches, name string) (Routing, error) {
	rt := Routing{Proxy: cmp.Or(reaches, "http://127.0.0.1:80"), Ours: []string{name}}
	if p.Panel != nil {
		rt.Panel, rt.PanelService = p.Panel(ctx)
	}
	for _, r := range doc.Rules {
		if r.Hostname != "" && !strings.Contains(r.Hostname, "*") {
			rt.Names = append(rt.Names, r.Hostname)
		}
	}
	pointing, err := t.cf.RecordsPointingAt(ctx, t.zone, tunnel+".cfargotunnel.com")
	if err != nil {
		return Routing{}, fmt.Errorf("can't read which names point at the tunnel: %w", err)
	}
	for _, rec := range pointing {
		if !strings.EqualFold(rec.Name, name) {
			rt.Names = append(rt.Names, rec.Name)
		}
	}
	return rt, nil
}

// proveTunnel finds the tunnel on the route's server, as its agent reads it now, and
// proves it runs there and nowhere else: every connector Cloudflare lists is one of
// this server's runs of cloudflared, by its connector ID, or, where none could be
// read, comes from one of the server's ways out (ADR 0104, section 3).
func (p *Publisher) proveTunnel(ctx context.Context, server string, t *target, tunnel string, facts Facts) (*dockwellv1.CloudflaredTunnel, error) {
	if p.Tunnels == nil {
		return nil, errors.New("this control plane can't read a server's tunnels")
	}
	if tunnel == "" {
		return nil, fmt.Errorf("%s runs cloudflared Dockwell can't read. Pick a way in under Change", facts.Name)
	}
	found, err := p.Tunnels(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("can't read %s's tunnels: %w", facts.Name, err)
	}
	var here []*dockwellv1.CloudflaredTunnel
	for _, f := range found {
		if f.GetTunnelId() == tunnel {
			here = append(here, f)
		}
	}
	if len(here) == 0 {
		return nil, fmt.Errorf("%s doesn't run that tunnel. Pick a way in under Change", facts.Name)
	}
	if slices.ContainsFunc(here, func(f *dockwellv1.CloudflaredTunnel) bool { return f.GetManagedInFile() }) {
		return nil, fmt.Errorf("%s's tunnel is managed in a file, so Dockwell writes nothing at Cloudflare", facts.Name)
	}
	// Each run of the tunnel here gets names from Cloudflare, so a rule's service has
	// to reach apps from every one of them: runs that reach apps differently, or say
	// different accounts, leave no one service to write (the review's M2).
	for _, f := range here[1:] {
		if f.GetReaches() != here[0].GetReaches() || f.GetNetworkRange() != here[0].GetNetworkRange() || f.GetAccountTag() != here[0].GetAccountTag() {
			return nil, fmt.Errorf("the tunnel runs more than once on %s, and its runs reach apps differently, so Dockwell can't pick where its rule sends a name. Keep one of them", facts.Name)
		}
	}
	account := cmp.Or(here[0].GetAccountTag(), t.zone.Account)
	connectors, err := t.tunnels.TunnelConnectors(ctx, account, tunnel)
	if err != nil {
		return nil, fmt.Errorf("can't read the tunnel's connections at Cloudflare: %w", err)
	}
	if len(connectors) == 0 {
		return nil, fmt.Errorf("the tunnel isn't connected from %s now, so Dockwell can't tell it runs there", facts.Name)
	}
	ids := map[string]*dockwellv1.CloudflaredTunnel{}
	unread := false
	for _, f := range here {
		if f.GetConnectorId() != "" {
			ids[f.GetConnectorId()] = f
		} else {
			unread = true
		}
	}
	// Addresses stand in for connector IDs only where no run here gave one, and no
	// other server leaves from the same address (ADR 0104, section 3).
	byAddress := len(ids) == 0
	if byAddress && len(facts.SharesWith) > 0 {
		return nil, fmt.Errorf("%s and %s share an address, so Dockwell can't tell which runs the tunnel", facts.Name, strings.Join(facts.SharesWith, ", "))
	}
	if !byAddress && unread {
		return nil, fmt.Errorf("the tunnel runs more than once on %s, and Dockwell can't read one of its runs, so it can't tell which is real. Keep one of them", facts.Name)
	}
	var proved *dockwellv1.CloudflaredTunnel
	for _, c := range connectors {
		if f, ok := ids[c.ID]; ok {
			proved = cmp.Or(proved, f)
			continue
		}
		if byAddress && len(c.Connections) > 0 && !slices.ContainsFunc(c.Connections, func(conn dnsproviders.Connection) bool { return !slices.Contains(facts.WaysOut, conn.OriginIP) }) {
			continue
		}
		return nil, fmt.Errorf("the tunnel runs on %s and somewhere else too, so Dockwell can't send a name to one of them", facts.Name)
	}
	if byAddress {
		return here[0], nil
	}
	if proved == nil {
		return nil, fmt.Errorf("the tunnel isn't connected from %s now, so Dockwell can't tell it runs there", facts.Name)
	}
	return proved, nil
}

// planRuleRemoval takes the zone's rule out of a tunnel once no route publishes
// through it in the zone after the change.
func (p *Publisher) planRuleRemoval(ctx context.Context, r revision.PublishRoute, inChange map[string]*resourcev1.Route) ([]resource.Step, error) {
	was := r.Before.GetPublish()
	if was.GetWay() != resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL {
		return nil, nil
	}
	if is := r.After.GetPublish(); is.GetWay() == was.GetWay() && is.GetTunnel() == was.GetTunnel() && strings.EqualFold(r.After.GetDomain(), r.Before.GetDomain()) {
		return nil, nil
	}
	var held []store.CloudflareRule
	if err := p.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		held, err = tx.CloudflareRulesOf(ctx, was.GetTunnel())
		return err
	}); err != nil {
		return nil, err
	}
	name := strings.ToLower(r.Before.GetDomain())
	all, err := p.Routes(ctx)
	if err != nil {
		return nil, err
	}
	for id, spec := range inChange {
		all[id] = spec
	}
	var steps []resource.Step
	for i := range held {
		rule := &held[i]
		if rule.TakenOnAt != nil || !HostMatches(rule.Hostname, name) {
			continue
		}
		stillUsed := false
		for id, other := range all {
			pub := other.GetPublish()
			if id == r.ID && r.After.GetPublish() == nil || other == nil || pub.GetWay() != resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL || pub.GetTunnel() != rule.TunnelID {
				continue
			}
			if HostMatches(rule.Hostname, strings.ToLower(other.GetDomain())) {
				stillUsed = true
				break
			}
		}
		if stillUsed {
			continue
		}
		tunnelName := rule.TunnelID
		if t, err := p.target(ctx, rule.Provider, name); err == nil {
			if info, err := t.tunnels.TunnelByID(ctx, rule.Account, rule.TunnelID); err == nil {
				tunnelName = info.Name
			}
		}
		steps = append(steps, step(r, resource.ActionDelete, resource.ProviderStep{
			Provider: rule.Provider, Action: resource.ProviderRemoveRule, Target: tunnelName, Name: rule.Hostname, Was: rule.Service,
			Account: rule.Account, Tunnel: rule.TunnelID, Rule: &resource.ProviderRule{Hostname: rule.Hostname, Path: rule.Path, Service: rule.Service},
		}))
	}
	return steps, nil
}
