// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// A tunnel's rule for one name, made by hand, is the route's to take on or move
// only with an admin's consent, by its tunnel and hostname (TakeOver), never as a
// side effect (ADR 0104, section 8):
//
//   - Taken on: a rule that already sends the name to the server's proxy is held as
//     Dockwell's, writing nothing, so removing the route takes it out, and undo puts
//     it back where it was.
//   - Moved (Move it to Dockwell): a rule that points at the app's own published
//     port is taken out where another rule, as the zone's, sends the name to the
//     proxy, and pointed at the proxy otherwise. Either way it's kept whole, so
//     removing the route, or undo, puts it back as it was, and lets it go.
//
// What's kept of a rule is its element as it sat in the document, and its index.

// Modes of the steps that take, move and put back a rule made by hand.
const (
	moveRemove = "remove" // a move that takes the rule out: another rule serves the name
	moveProxy  = "proxy"  // a move that points the rule at the server's proxy
	backHold   = "hold"   // put back where it was, and held again: a removal undone
	backDrop   = "drop"   // put back where it was, and let go: a move by removal undone
	backAgain  = "back"   // pointed back where it went, and let go: a move undone
	letGoRule  = "let_go" // held no more, as it is: consent to take it on taken away
)

// keptRule is a rule made by hand as Dockwell keeps it: whole, where it was, and
// whether Dockwell moved it.
type keptRule struct {
	Elem  json.RawMessage `json:"elem"`
	At    int             `json:"at"`
	Moved bool            `json:"moved,omitempty"`
	// Mode is how it was moved (moveRemove or moveProxy), for a rule moved.
	Mode string `json:"mode,omitempty"`
}

// original is the rule kept, as it read before Dockwell moved or took it out.
func (k keptRule) original() Rule {
	var r Rule
	_ = json.Unmarshal(k.Elem, &r)
	return r
}

// keepsName says the route still serves name after the change, publishing it at
// Cloudflare no more: what it took on there is let go, as it is, so the name goes on
// working (the review's H2).
func keepsName(r revision.PublishRoute, name string) bool {
	pub := r.After.GetPublish()
	return r.After != nil && strings.EqualFold(r.After.GetDomain(), name) && (r.After.GetPath() == "" || r.After.GetPath() == "/") &&
		(pub == nil || pub.GetWay() == resourcev1.RouteWay_ROUTE_WAY_FRONT)
}

// publishedElsewhere is the ID of another route of the change that publishes name
// after it, through tunnel where one's given, or "": a domain moved from one route to
// another, which hands over what's there (the review's M1).
func publishedElsewhere(inChange map[string]*resourcev1.Route, routeID, name, tunnel string) string {
	for id, rt := range inChange {
		pub := rt.GetPublish()
		if id == routeID || rt == nil || pub == nil || pub.GetWay() == resourcev1.RouteWay_ROUTE_WAY_FRONT || !strings.EqualFold(rt.GetDomain(), name) {
			continue
		}
		if tunnel == "" || pub.GetWay() == resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL && pub.GetTunnel() == tunnel {
			return id
		}
	}
	return ""
}

func readKept(s string) keptRule {
	var k keptRule
	_ = json.Unmarshal([]byte(s), &k)
	return k
}

func (k keptRule) String() string {
	b, _ := json.Marshal(k)
	return string(b)
}

// consentsRule says the route's publish takes over the rule for hostname in tunnel.
func consentsRule(pub *resourcev1.RoutePublish, tunnel, hostname string) bool {
	return slices.ContainsFunc(pub.GetTakesOver(), func(t *resourcev1.TakeOver) bool {
		return t.GetTunnel() == tunnel && strings.EqualFold(t.GetHostname(), hostname) && t.GetPath() == ""
	})
}

// heldRule reads what's recorded of the rule for hostname in tunnel, held or kept.
func (p *Publisher) heldRule(ctx context.Context, tunnel, hostname string) (store.CloudflareRule, bool, error) {
	var r store.CloudflareRule
	found := true
	err := p.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		r, err = tx.CloudflareRuleAt(ctx, tunnel, hostname, "")
		if errors.Is(err, store.ErrNotFound) {
			found = false
			return nil
		}
		return err
	})
	return r, found, err
}

// appsPort says service is one of the app's own published ports on its server, as
// a rule made by hand for it would name: on loopback, or the address the tunnel
// reaches the server's proxy at, and not the proxy itself.
func (p *Publisher) appsPort(ctx context.Context, r revision.PublishRoute, service, proxy string) bool {
	u, err := url.Parse(service)
	if err != nil || SameService(service, proxy) || u.Port() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	reach, _ := url.Parse(proxy)
	if !slices.Contains([]string{"127.0.0.1", "localhost", "::1", "host.docker.internal"}, host) && (reach == nil || host != strings.ToLower(reach.Hostname())) {
		return false
	}
	port, err := strconv.ParseUint(u.Port(), 10, 32)
	if err != nil {
		return false
	}
	if hp := r.After.GetHostPort(); hp != 0 && uint64(hp) == port {
		return true
	}
	if p.HostPorts == nil {
		return false
	}
	ports, err := p.HostPorts(ctx, r.Environment, r.After.GetService())
	return err == nil && slices.Contains(ports, uint32(port))
}

// planHandRule plans what a route does with the tunnel's rule for its own name made
// by hand: take it on, move it, put it back, or let it go. handled says the rule
// for the name is settled by its steps, and no zone rule is needed.
func (p *Publisher) planHandRule(ctx context.Context, r revision.PublishRoute, t *target, account, tunnel, tunnelName, name string, doc *Document, rt Routing, need Need) ([]resource.Step, bool, error) {
	held, found, err := p.heldRule(ctx, tunnel, name)
	if err != nil {
		return nil, false, err
	}
	consent := consentsRule(r.After.GetPublish(), tunnel, name)
	kept := readKept(held.Replaced)
	base := resource.ProviderStep{Provider: t.provider, Target: tunnelName, Name: name, Account: account, Tunnel: tunnel, ZoneID: t.zone.ID}
	takeover := resource.Access{Kind: resource.AccessCloudflareTakeover, Path: name, Detail: tunnelName}
	moved := func() error {
		return fmt.Errorf("%s's rule in %s, made in Cloudflare, was moved to Dockwell for this route. Keep the route's takeover of it, or stop publishing it through the tunnel, which puts the rule back", name, tunnelName)
	}
	switch {
	case found && held.State == store.CloudflareRuleMoving && consent:
		// A move cut short: planned again from what was kept, its step finishes it, or
		// makes it (the review's H1).
		was := kept.original()
		base.Action, base.Mode, base.Was, base.To = resource.ProviderMoveRule, kept.Mode, was.Service, held.Service
		base.Rule = &resource.ProviderRule{Hostname: was.Hostname, Path: was.Path, Service: was.Service}
		base.Routing = "Finishes moving it to Dockwell; Dockwell keeps it as it was, so undo puts it back"
		takeover.Address = was.Service
		return []resource.Step{step(r, resource.ActionUpdate, base, takeover)}, true, nil
	case found && held.State == store.CloudflareRuleMoving:
		return nil, true, moved()
	case found && held.State == store.CloudflareRuleKept && kept.Moved:
		if consent {
			// Moved out of the way: another rule serves the name, as planned.
			return nil, false, nil
		}
		return nil, true, moved()
	case found && held.State == store.CloudflareRuleKept:
		if !consent || doc.Index(name, "") >= 0 {
			return nil, false, nil
		}
		var rule Rule
		if err := json.Unmarshal(kept.Elem, &rule); err != nil {
			return nil, false, fmt.Errorf("the rule kept for %s can't be read: %w", name, err)
		}
		after, at, err := doc.InsertAt(kept.Elem, kept.At)
		if err != nil {
			return nil, false, err
		}
		if m := rt.Check(doc.Rules, after.Rules, Edit{At: at, Inserted: true}); len(m) > 0 {
			return nil, false, fmt.Errorf("can't put %s back in %s: %w", name, tunnelName, m[0])
		}
		base.Action, base.Mode, base.To = resource.ProviderRestoreRule, backHold, rule.Service
		base.Rule = &resource.ProviderRule{Hostname: rule.Hostname, Path: rule.Path, Service: rule.Service}
		base.Routing = "Puts back the rule made in Cloudflare that this route took on, where it was"
		takeover.Address = rule.Service
		return []resource.Step{step(r, resource.ActionCreate, base, takeover)}, true, nil
	case found && held.TakenOnAt != nil:
		if consent {
			return nil, false, nil
		}
		if kept.Moved {
			return nil, true, moved()
		}
		// Consent taken away, as by undo: held no more, and left as it is.
		base.Action, base.Mode, base.Was = resource.ProviderKeepRule, letGoRule, held.Service
		base.Rule = &resource.ProviderRule{Hostname: held.Hostname, Path: held.Path, Service: held.Service}
		base.Routing = "Dockwell lets go of it, and it stays in " + tunnelName + " as it is"
		return []resource.Step{step(r, resource.ActionNone, base)}, false, nil
	case found:
		// Dockwell's own, as an apex rule: the zone's rules decide.
		return nil, false, nil
	}
	if need.Nothing {
		j := FirstFor(doc.Rules, name)
		if j < 0 || !strings.EqualFold(doc.Rules[j].Hostname, name) || doc.Rules[j].Path != "" || !consent {
			return nil, false, nil
		}
		base.Action, base.Was = resource.ProviderTakeOnRule, doc.Rules[j].Service
		base.Rule = &resource.ProviderRule{Hostname: doc.Rules[j].Hostname, Service: doc.Rules[j].Service}
		base.Routing = "Takes on " + name + "'s rule, made in Cloudflare, writing nothing"
		takeover.Address = doc.Rules[j].Service
		return []resource.Step{step(r, resource.ActionCreate, base, takeover)}, true, nil
	}
	var caught Caught
	if !errors.As(need.Refused, &caught) || !strings.EqualFold(caught.Rule.Hostname, name) || caught.Rule.Path != "" || !p.appsPort(ctx, r, caught.Rule.Service, rt.Proxy) {
		return nil, false, nil
	}
	if !consent {
		return nil, true, fmt.Errorf("%s in %s points at the app's own port, %s, made in Cloudflare. An admin can move it to Dockwell, which sends it through the server's proxy", name, tunnelName, caught.Rule.Service)
	}
	j := doc.Index(name, "")
	without, err := doc.Remove(j)
	if err != nil {
		return nil, false, err
	}
	base.Action, base.Was, base.To = resource.ProviderMoveRule, caught.Rule.Service, rt.Proxy
	base.Rule = &resource.ProviderRule{Hostname: caught.Rule.Hostname, Service: caught.Rule.Service}
	takeover.Address = caught.Rule.Service
	if rt.Reaches(without.Rules, name) {
		if m := rt.Check(doc.Rules, without.Rules, Edit{At: j}); len(m) > 0 {
			return nil, false, fmt.Errorf("can't move %s to Dockwell in %s: %w", name, tunnelName, m[0])
		}
		base.Mode = moveRemove
		base.Routing = "Takes it out, as another rule in " + tunnelName + " sends " + name + " to the server's proxy; Dockwell keeps it, so undo puts it back"
		return []resource.Step{step(r, resource.ActionUpdate, base, takeover)}, true, nil
	}
	after, err := doc.WithService(j, rt.Proxy)
	if err != nil {
		return nil, false, err
	}
	if m := rt.Check(doc.Rules, after.Rules, Edit{At: -1}); len(m) > 0 {
		return nil, false, fmt.Errorf("can't move %s to Dockwell in %s: %w", name, tunnelName, m[0])
	}
	base.Mode = moveProxy
	base.Routing = "Points it at the server's proxy, keeping the rest of it; Dockwell keeps it as it was, so undo puts it back"
	return []resource.Step{step(r, resource.ActionUpdate, base, takeover)}, true, nil
}

// planHandRuleRemoval plans what a route that stops publishing name through tunnel
// does with the rule for it made by hand that it took on or moved. A rule moved goes
// back as it was, and is let go. A rule taken on comes out, and is kept so undo puts
// it back, but where the route still serves the name, or this undoes the change that
// took it on, it's let go as it is, writing nothing, so the name goes on working (the
// review's H2). Another route of the change that publishes the name through the
// tunnel keeps it (the review's M1).
func (p *Publisher) planHandRuleRemoval(ctx context.Context, r revision.PublishRoute, inChange map[string]*resourcev1.Route, tunnel, name string) ([]resource.Step, error) {
	held, found, err := p.heldRule(ctx, tunnel, name)
	if err != nil || !found || held.TakenOnAt == nil || publishedElsewhere(inChange, r.ID, name, tunnel) != "" {
		return nil, err
	}
	t, err := p.target(ctx, held.Provider, name)
	if err != nil {
		return nil, err
	}
	tunnelName := tunnel
	if info, err := t.tunnels.TunnelByID(ctx, held.Account, tunnel); err == nil {
		tunnelName = info.Name
	}
	kept := readKept(held.Replaced)
	rule := kept.original()
	base := resource.ProviderStep{
		Provider: held.Provider, Target: tunnelName, Name: name, Account: held.Account, Tunnel: tunnel, ZoneID: t.zone.ID,
		Rule: &resource.ProviderRule{Hostname: held.Hostname, Path: held.Path, Service: held.Service},
	}
	access := resource.Access{Kind: resource.AccessTunnelRule, Path: name, Detail: tunnelName, Address: rule.Service}
	undoing := r.Reverts != "" && r.Reverts == held.ChangeID
	switch {
	case kept.Moved && (held.State == store.CloudflareRuleKept || held.State == store.CloudflareRuleMoving && kept.Mode == moveRemove):
		base.Action, base.Mode, base.To = resource.ProviderRestoreRule, backDrop, rule.Service
		base.Routing = "Puts back the rule made in Cloudflare that Dockwell took out to move it, where it was"
	case held.State == store.CloudflareRuleKept:
		return nil, nil
	case kept.Moved:
		base.Action, base.Mode, base.Was, base.To = resource.ProviderRestoreRule, backAgain, held.Service, rule.Service
		base.Routing = "Points it back where it went before Move it to Dockwell"
	case keepsName(r, name) || undoing:
		base.Action, base.Mode, base.Was = resource.ProviderKeepRule, letGoRule, held.Service
		base.Routing = "Dockwell lets go of it, and it stays in " + tunnelName + " as it is"
		return []resource.Step{step(r, resource.ActionNone, base)}, nil
	default:
		base.Action, base.Was = resource.ProviderRemoveRule, held.Service+", made in Cloudflare and taken on "+held.TakenOnAt.Format("2 January")
		base.Routing = "Dockwell keeps it, so undo puts it back"
	}
	return []resource.Step{step(r, resource.ActionDelete, base, access)}, nil
}

// takeOnRule holds a rule made by hand as Dockwell's, as it reads now, writing
// nothing.
func (p *Publisher) takeOnRule(ctx context.Context, changeID, server string, s resource.ProviderStep) (string, error) {
	t, doc, version, err := p.readTunnel(ctx, s)
	if err != nil {
		return "", err
	}
	i := doc.Index(s.Rule.Hostname, s.Rule.Path)
	if i < 0 || !SameService(doc.Rules[i].Service, s.Rule.Service) {
		return "", fmt.Errorf("%s in %s changed since the change was planned. Plan it again", s.Name, s.Target)
	}
	now := p.now()
	return "", p.DB.Update(ctx, func(tx *store.Tx) error {
		return tx.PutCloudflareRule(ctx, store.CloudflareRule{
			TunnelID: s.Tunnel, Hostname: s.Rule.Hostname, Account: s.Account, Provider: s.Provider, Zone: t.zone.Name, Service: doc.Rules[i].Service,
			ReadDoc: string(doc.Raw()), WrittenDoc: string(doc.Raw()), Version: version, ChangeID: changeID, TakenOnAt: &now,
			Replaced: keptRule{Elem: doc.Elem(i), At: i}.String(), CreatedAt: now, ServerID: server,
		})
	})
}

// readTunnel reads the document of a step's tunnel, with its version.
func (p *Publisher) readTunnel(ctx context.Context, s resource.ProviderStep) (*target, *Document, int, error) {
	t, err := p.target(ctx, s.Provider, s.Name)
	if err != nil {
		return nil, nil, 0, err
	}
	read, err := t.tunnels.ReadTunnelConfig(ctx, s.Account, s.Tunnel)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("can't read %s's routes: %w", s.Target, err)
	}
	doc, err := Parse(read.Config)
	return t, doc, read.Version, err
}

// writeHandRule writes a tunnel's document with one rule made by hand moved or put
// back, after the routing check and the check that nothing else changed, reads it
// back, as adding a rule does, and says when an edit in Cloudflare between the read
// and the write was replaced.
func (p *Publisher) writeHandRule(ctx context.Context, changeID, routeID string, s resource.ProviderStep, t *target, readVersion int, after *Document, check func() error, back func(*Document) bool) (int, string, error) {
	if err := check(); err != nil {
		return 0, "", err
	}
	start := p.now()
	defer p.cache.forget("doc:" + s.Tunnel)
	wrote, err := t.tunnels.WriteTunnelConfig(ctx, s.Account, s.Tunnel, after.Raw())
	if err != nil {
		return 0, "", fmt.Errorf("can't write %s's routes: %w", s.Target, err)
	}
	read, err := t.tunnels.ReadTunnelConfig(ctx, s.Account, s.Tunnel)
	if err != nil {
		return 0, "", fmt.Errorf("wrote %s's routes and can't read them back: %w", s.Target, err)
	}
	if doc, err := Parse(read.Config); err != nil || !back(doc) {
		return 0, "", fmt.Errorf("%s's routes were written again in Cloudflare just as Dockwell wrote them, and %s isn't as Dockwell wrote it. Try again", s.Target, s.Name)
	}
	said := ""
	if wrote.Version > readVersion+1 {
		p.note(ctx, ActionReplaced, routeID, told{Provider: s.Provider, Target: s.Target, Name: s.Name, Action: int(s.Action), Version: wrote.Version, ReadVersion: readVersion, ChangeID: changeID})
		said = fmt.Sprintf("An edit made in Cloudflare between %s and %s was replaced. Dockwell kept what it read, and that's what's there now", start.Format("15:04:05"), p.now().Format("15:04:05"))
	}
	return wrote.Version, said, nil
}

// handRouting is the routing check for a step on a rule made by hand: every name
// kept where it goes but the rule's own, which the step means to move.
func (p *Publisher) handRouting(ctx context.Context, t *target, s resource.ProviderStep, doc *Document) (Routing, error) {
	rt, err := p.removalRouting(ctx, t, s.Tunnel, doc, "", s.Name)
	if err != nil {
		return Routing{}, err
	}
	rt.Names = slices.DeleteFunc(rt.Names, func(n string) bool { return strings.EqualFold(n, s.Name) })
	return rt, nil
}

// moveRule moves a rule made by hand to Dockwell, as its step says: taken out, or
// pointed at the server's proxy, kept whole either way. What's kept is written before
// the tunnel is, as a row moving, so a step cut short between the two finishes when
// it runs again, and a plan made again finds it (the review's H1).
func (p *Publisher) moveRule(ctx context.Context, changeID string, route revision.PublishRoute, s resource.ProviderStep) (string, error) {
	unlock, err := p.lock(ctx, s.Tunnel, "move:"+s.Tunnel+":"+s.Name)
	if err != nil {
		return "", err
	}
	defer unlock()
	t, doc, version, err := p.readTunnel(ctx, s)
	if err != nil {
		return "", err
	}
	held, found, err := p.heldRule(ctx, s.Tunnel, s.Name)
	if err != nil {
		return "", err
	}
	moving := found && held.State == store.CloudflareRuleMoving
	i := doc.Index(s.Rule.Hostname, s.Rule.Path)
	movedShape := func(d *Document) bool {
		j := d.Index(s.Rule.Hostname, s.Rule.Path)
		if s.Mode == moveRemove {
			return j < 0
		}
		return j >= 0 && SameService(d.Rules[j].Service, s.To)
	}
	now := p.now()
	finish := func(kept keptRule, written *Document, wrote int) error {
		row := store.CloudflareRule{
			TunnelID: s.Tunnel, Hostname: s.Rule.Hostname, Account: s.Account, Provider: s.Provider, Zone: t.zone.Name, Service: s.To,
			ReadDoc: string(doc.Raw()), WrittenDoc: string(written.Raw()), Version: wrote, ChangeID: changeID, TakenOnAt: &now,
			Replaced: kept.String(), CreatedAt: now, ServerID: route.Server,
		}
		if s.Mode == moveRemove {
			row.State = store.CloudflareRuleKept
		}
		return p.DB.Update(ctx, func(tx *store.Tx) error {
			if err := tx.PutCloudflareRule(ctx, row); err != nil {
				return err
			}
			return tx.SawTunnel(ctx, s.Tunnel, s.Account, wrote, now)
		})
	}
	if moving && movedShape(doc) {
		// Its write landed before the step was cut short: what was kept says the rest.
		return "", finish(readKept(held.Replaced), doc, version)
	}
	if i < 0 || !SameService(doc.Rules[i].Service, s.Rule.Service) {
		return "", fmt.Errorf("%s in %s changed since the change was planned. Plan it again", s.Name, s.Target)
	}
	rt, err := p.handRouting(ctx, t, s, doc)
	if err != nil {
		return "", err
	}
	rt.Proxy = s.To
	kept := keptRule{Elem: doc.Elem(i), At: i, Moved: true, Mode: s.Mode}
	if moving {
		kept = readKept(held.Replaced)
	}
	var after *Document
	var check func() error
	if s.Mode == moveRemove {
		if after, err = doc.Remove(i); err != nil {
			return "", err
		}
		check = func() error {
			if m := rt.Check(doc.Rules, after.Rules, Edit{At: i}); len(m) > 0 {
				return fmt.Errorf("can't move %s to Dockwell in %s: %w", s.Name, s.Target, m[0])
			}
			if !rt.Reaches(after.Rules, s.Name) {
				return fmt.Errorf("%s in %s changed since the change was planned, so no other rule sends it to the server's proxy. Plan it again", s.Name, s.Target)
			}
			return CheckSplice(doc.Raw(), after.Raw(), i, false)
		}
	} else {
		if after, err = doc.WithService(i, s.To); err != nil {
			return "", err
		}
		check = func() error {
			if rt.Serves(after.Rules[i]) {
				return ErrServesPanel
			}
			if m := rt.Check(doc.Rules, after.Rules, Edit{At: -1}); len(m) > 0 {
				return fmt.Errorf("can't move %s to Dockwell in %s: %w", s.Name, s.Target, m[0])
			}
			return CheckMoved(doc.Raw(), after.Raw(), i, s.To)
		}
	}
	if rt.Serves(doc.Rules[i]) {
		return "", ErrServesPanel
	}
	if !moving {
		if err := p.DB.Update(ctx, func(tx *store.Tx) error {
			return tx.PutCloudflareRule(ctx, store.CloudflareRule{
				TunnelID: s.Tunnel, Hostname: s.Rule.Hostname, Account: s.Account, Provider: s.Provider, Zone: t.zone.Name, Service: s.To,
				ReadDoc: string(doc.Raw()), WrittenDoc: string(doc.Raw()), Version: version, ChangeID: changeID, TakenOnAt: &now,
				Replaced: kept.String(), State: store.CloudflareRuleMoving, CreatedAt: now, ServerID: route.Server,
			})
		}); err != nil {
			return "", err
		}
	}
	wrote, said, err := p.writeHandRule(ctx, changeID, route.ID, s, t, version, after, check, movedShape)
	if err != nil {
		return "", err
	}
	return said, finish(kept, after, wrote)
}

// restoreRule puts back a rule made by hand as its step says: where it was, held
// again or let go, or pointed back where it went before Dockwell moved it. One
// changed in Cloudflare since, or made again there, stays as it is, and is let go.
func (p *Publisher) restoreRule(ctx context.Context, changeID string, route revision.PublishRoute, s resource.ProviderStep) (string, error) {
	unlock, err := p.lock(ctx, s.Tunnel, "restore:"+s.Tunnel+":"+s.Name)
	if err != nil {
		return "", err
	}
	defer unlock()
	held, found, err := p.heldRule(ctx, s.Tunnel, s.Name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", letGo{"Nothing of " + s.Name + " in " + s.Target + " was left to put back"}
	}
	kept := readKept(held.Replaced)
	rule := kept.original()
	t, doc, version, err := p.readTunnel(ctx, s)
	if err != nil {
		return "", err
	}
	forget := func() error {
		return p.DB.Update(ctx, func(tx *store.Tx) error { return tx.DeleteCloudflareRule(ctx, s.Tunnel, held.Hostname, held.Path) })
	}
	rt, err := p.handRouting(ctx, t, s, doc)
	if err != nil {
		return "", err
	}
	if rt.Serves(rule) {
		return "", ErrServesPanel
	}
	i := doc.Index(s.Name, rule.Path)
	var after *Document
	var check func() error
	var back func(*Document) bool
	switch s.Mode {
	case backAgain:
		switch {
		case i < 0:
			if err := forget(); err != nil {
				return "", err
			}
			return "", letGo{s.Name + " was gone from " + s.Target + " already"}
		case SameService(doc.Rules[i].Service, rule.Service):
			// Never moved, as a move cut short before its write leaves it.
			return "", forget()
		case !SameService(doc.Rules[i].Service, held.Service):
			if err := forget(); err != nil {
				return "", err
			}
			return "", letGo{s.Name + " in " + s.Target + " was changed in Cloudflare since, so it stays"}
		}
		if after, err = doc.WithService(i, rule.Service); err != nil {
			return "", err
		}
		check = func() error {
			if m := rt.Check(doc.Rules, after.Rules, Edit{At: -1}); len(m) > 0 {
				return fmt.Errorf("can't put %s back in %s: %w", s.Name, s.Target, m[0])
			}
			return CheckMoved(doc.Raw(), after.Raw(), i, rule.Service)
		}
		back = func(d *Document) bool {
			j := d.Index(s.Name, rule.Path)
			return j >= 0 && SameService(d.Rules[j].Service, rule.Service)
		}
	default:
		if i >= 0 {
			// There already: put back by a step a crash cut short, which is the rule
			// kept, or made again by hand since, which isn't Dockwell's to hold (the
			// review's M4).
			if s.Mode == backDrop || !SameService(doc.Rules[i].Service, rule.Service) {
				if err := forget(); err != nil {
					return "", err
				}
				if s.Mode == backDrop || SameService(doc.Rules[i].Service, rule.Service) {
					return "", nil
				}
				return "", letGo{s.Name + " in " + s.Target + " was made again in Cloudflare, so it stays as it is"}
			}
			held.State, held.Service, held.ChangeID = store.CloudflarePublished, doc.Rules[i].Service, changeID
			held.Replaced = keptRule{Elem: doc.Elem(i), At: i}.String()
			return "", p.DB.Update(ctx, func(tx *store.Tx) error { return tx.PutCloudflareRule(ctx, held) })
		}
		var at int
		if after, at, err = doc.InsertAt(kept.Elem, kept.At); err != nil {
			return "", err
		}
		check = func() error {
			if m := rt.Check(doc.Rules, after.Rules, Edit{At: at, Inserted: true}); len(m) > 0 {
				return fmt.Errorf("can't put %s back in %s: %w", s.Name, s.Target, m[0])
			}
			return CheckSplice(doc.Raw(), after.Raw(), at, true)
		}
		back = func(d *Document) bool { return d.Index(s.Name, rule.Path) >= 0 }
		kept.At = at
	}
	wrote, said, err := p.writeHandRule(ctx, changeID, route.ID, s, t, version, after, check, back)
	if err != nil {
		return "", err
	}
	now := p.now()
	return said, p.DB.Update(ctx, func(tx *store.Tx) error {
		if s.Mode == backHold {
			j := after.Index(s.Name, rule.Path)
			held.State, held.ChangeID, held.Version, held.WrittenDoc, held.ReadDoc = store.CloudflarePublished, changeID, wrote, string(after.Raw()), string(doc.Raw())
			held.Service, held.TakenOnAt, held.ServerID = after.Rules[j].Service, &now, route.Server
			held.Replaced = keptRule{Elem: after.Elem(j), At: j}.String()
			if err := tx.PutCloudflareRule(ctx, held); err != nil {
				return err
			}
		} else if err := tx.DeleteCloudflareRule(ctx, s.Tunnel, held.Hostname, held.Path); err != nil {
			return err
		}
		return tx.SawTunnel(ctx, s.Tunnel, s.Account, wrote, now)
	})
}

// letGoOfRule stops holding a rule made by hand, leaving it as it is.
func (p *Publisher) letGoOfRule(ctx context.Context, s resource.ProviderStep) (string, error) {
	if err := p.DB.Update(ctx, func(tx *store.Tx) error { return tx.DeleteCloudflareRule(ctx, s.Tunnel, s.Rule.Hostname, s.Rule.Path) }); err != nil {
		return "", err
	}
	return "", letGo{"Dockwell let go of " + s.Name + " in " + s.Target + ", which stays as it is"}
}
