// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/dockwell/dockwell/internal/control/place"
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
	Panel func(ctx context.Context) PanelNames
	// ProjectOf says the project an environment is in.
	ProjectOf func(ctx context.Context, environment string) (string, error)
	// Routes are every route as it stands, by resource ID, to tell when a zone's
	// rule is the last route's.
	Routes func(ctx context.Context) (map[string]*resourcev1.Route, error)
	// HostPorts are the ports a service publishes on its server, by its
	// environment and name: an app's own ports, which a tunnel's rule made by hand
	// may point at, for Move it to Dockwell.
	HostPorts func(ctx context.Context, environment, service string) ([]uint32, error)
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
	// cached says it's a live check's, whose reads may be up to 30 seconds old.
	cached bool
}

func (p *Publisher) target(ctx context.Context, provider, name string) (*target, error) {
	saved, err := p.Providers.Named(ctx, provider)
	if err != nil {
		return nil, Unreadable{fmt.Errorf("%s names the DNS provider %s, which isn't connected", name, provider)}
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
	// A token's zones change seldom; a new token reads them again.
	sum := sha256.Sum256([]byte(saved.Spec.GetApiToken()))
	zones, err := readFor(p, &target{cached: true}, "zones:"+provider+":"+hex.EncodeToString(sum[:8]), func() ([]dnsproviders.Zone, error) { return cf.Zones(ctx) })
	if err != nil {
		err = fmt.Errorf("can't read %s's zones at Cloudflare: %w", provider, err)
		if refused(err) {
			err = Unreadable{err}
		}
		return nil, err
	}
	for _, z := range zones {
		if z.Name == zoneName {
			t.zone = z
			return t, nil
		}
	}
	return nil, Unreadable{fmt.Errorf("%s's token doesn't reach %s at Cloudflare any more. Check the provider again under Integrations", provider, zoneName)}
}

// ErrShared is a tunnel connected from somewhere besides the server it's proved on,
// or from there no more, which Cloudflare sends a zone's names to.
var ErrShared = errors.New("its names go to whichever connection answers")

// ErrUnproven is a tunnel Dockwell can't prove runs on the server alone: run more
// than once there in ways that disagree, unread, or not connected.
var ErrUnproven = errors.New("Dockwell can't prove it runs on the server alone") //nolint:staticcheck // the product's name

// proofFailed is a tunnel's proof refused in words, of a kind (ErrShared or
// ErrUnproven) the drift timer marks.
type proofFailed struct {
	said string
	kind error
}

func (p proofFailed) Error() string        { return p.said }
func (p proofFailed) Is(target error) bool { return target == p.kind }

// handedOver says the record id is Dockwell's own, of another route of the change
// that publishes name no more, while route publishes it now.
func (p *Publisher) handedOver(ctx context.Context, id, route, name string, inChange map[string]*resourcev1.Route) bool {
	var row store.CloudflareRecord
	if err := p.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		row, err = tx.CloudflareRecordByID(ctx, id)
		return err
	}); err != nil || row.RouteID == route {
		return false
	}
	if _, ok := inChange[row.RouteID]; !ok {
		return false
	}
	return publishedElsewhere(inChange, row.RouteID, name, "") == route
}

// AdminCaller says the caller ctx carries is an admin of the install, or that it
// carries none, as the control plane's own work does.
func AdminCaller(ctx context.Context) bool {
	c, ok := place.CallerFrom(ctx)
	return !ok || store.RoleRank(place.Holds(c.Grants, place.Install)) >= store.RoleRank(store.RoleAdmin)
}

// Unreadable is a reason Dockwell can't read what it published at all, short of
// someone fixing it: its provider isn't connected any more, or Cloudflare refuses the
// token. Anything else may pass on its own.
type Unreadable struct{ error }

func (u Unreadable) Unwrap() error { return u.error }

// refused says Cloudflare refused the token, or a right it needs.
func refused(err error) bool {
	var api *dnsproviders.APIError
	return errors.As(err, &api) && (api.Refused() || denied(err))
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
func (p *Publisher) Plan(ctx context.Context, changeID string, routes []revision.PublishRoute, providers []revision.PublishProvider) (map[resource.Ref][]resource.Step, error) {
	out := map[resource.Ref][]resource.Step{}
	rules := map[string]bool{} // rules a route of this change adds already, by tunnel|hostname
	inChange := map[string]*resourcev1.Route{}
	for i := range routes {
		inChange[routes[i].ID] = routes[i].After
	}
	for i := range routes {
		steps, err := p.planRoute(ctx, routes[i], rules, inChange)
		if err != nil {
			return nil, err
		}
		out[routes[i].Ref] = steps
	}
	for _, pp := range providers {
		steps, err := p.planProvider(ctx, pp)
		if err != nil {
			return nil, err
		}
		if len(steps) > 0 {
			out[pp.Ref] = steps
		}
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
		pre, kept, err := p.planWrites(ctx, r, pub, written, rules, inChange)
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
	// The name this plan still writes or keeps a record of the route's at. Nothing is
	// put back there: what a record there replaced is carried on to it (the re-check's
	// H1a).
	published := ""
	for id := range keep {
		if i := slices.IndexFunc(written, func(w store.CloudflareRecord) bool { return w.ID == id }); i >= 0 {
			published = written[i].Name
		}
	}
	for i := range steps {
		if ps := steps[i].Provider; ps != nil && ps.Record != nil && published == "" {
			published = strings.ToLower(ps.Name)
		}
	}
	// What Dockwell wrote that this route doesn't publish any more comes off, after
	// the route is gone from its server, and what it replaced goes back where the route
	// writes nothing any more. Deleting it keeps that backup first (keepBackup), and
	// putting it back takes it from there.
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
		// A domain moved to another route of the change goes on with it, as it is (the
		// review's M1).
		if to := publishedElsewhere(inChange, r.ID, w.Name, ""); to != "" {
			steps = append(steps, step(r, resource.ActionNone, resource.ProviderStep{
				Provider: w.Provider, Action: resource.ProviderTakeOnRecord, Target: w.Zone, Name: w.Name, Was: words, ZoneID: w.ZoneID, Record: rec,
				HandOverTo: to, Routing: "It goes on with the route that publishes " + w.Name + " now, as it is",
			}))
			continue
		}
		// A name the route still serves keeps its record, let go as it is, unless this
		// undoes the change that wrote it; one taken on is let go by that undo too, as
		// it was there before (the review's H2).
		// Only what was taken on is let go. Dockwell's own record goes as before, unless
		// a rule made by hand that this lets go still sends the name through it (the
		// re-check's A-M3), so nothing of Dockwell's is left at Cloudflare for good.
		undoingIt := r.Reverts != "" && r.Reverts == w.ChangeID
		letGo := w.TakenOnAt != nil && (keepsName(r, w.Name) || undoingIt)
		if !letGo && w.TakenOnAt == nil && keepsName(r, w.Name) && !undoingIt {
			hand, err := p.handRuleLetGo(ctx, r, w.Name)
			if err != nil {
				return nil, err
			}
			letGo = hand
		}
		if letGo {
			steps = append(steps, step(r, resource.ActionNone, resource.ProviderStep{
				Provider: w.Provider, Action: resource.ProviderDeleteRecord, Target: w.Zone, Name: w.Name, Was: words, ZoneID: w.ZoneID, Record: rec,
				Mode: letGoRule, Routing: "Dockwell lets go of it, and it stays as it is",
			}))
			continue
		}
		steps = append(steps, step(r, resource.ActionDelete, resource.ProviderStep{
			Provider: w.Provider, Action: resource.ProviderDeleteRecord, Target: w.Zone, Name: w.Name, Was: words, ZoneID: w.ZoneID, Record: rec,
		}))
		if w.Replaced != "" && !strings.EqualFold(w.Name, published) {
			var old dnsproviders.Record
			if json.Unmarshal([]byte(w.Replaced), &old) == nil {
				steps = append(steps, step(r, resource.ActionDelete, resource.ProviderStep{
					Provider: w.Provider, Action: resource.ProviderRestoreRecord, Target: w.Zone, Name: w.Name, To: recordWords(old), ZoneID: w.ZoneID,
					Pending: pending(w.ID),
					Record:  &resource.ProviderRecord{Type: old.Type, Name: old.Name, Content: old.Content, Proxied: old.Proxied, TTL: old.TTL, Comment: old.Comment},
				}))
			}
		}
	}
	// What a replace kept of a record made by hand, before a delete that landed without
	// its write, goes back where the route doesn't publish that name any more. Where it
	// does, the write takes it on (createRecord) (the review's H1).
	for i := range kept {
		k := &kept[i]
		var old dnsproviders.Record
		if strings.EqualFold(k.Name, published) || json.Unmarshal([]byte(k.Replaced), &old) != nil {
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
func (p *Publisher) planWrites(ctx context.Context, r revision.PublishRoute, pub *resourcev1.RoutePublish, written []store.CloudflareRecord, rules map[string]bool, inChange map[string]*resourcev1.Route) ([]resource.Step, map[string]bool, error) {
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
		// Dockwell's own record of a route this change moves the name from goes on with
		// this one, as it is: the other route's step hands it over (the review's M1).
		if conflict != nil && p.handedOver(ctx, conflict.ID, r.ID, name, inChange) {
			if conflict.Type != d.Type || conflict.Content != d.Content || conflict.Proxied != d.Proxied {
				return nil, nil, fmt.Errorf("%s moves to this route in this change, published another way. Move it as it is, then change how it's published in another change", name)
			}
			continue
		}
		switch {
		case conflict == nil || conflict.Comment == comment && conflict.Type == d.Type && conflict.Content == d.Content:
			// Free, or written for this route by a step a crash cut short, which
			// writing finds and keeps. A record of Dockwell's of this type that's gone
			// from there is written again, so it isn't removed as well.
			base.Action = resource.ProviderCreateRecord
			steps = append(steps, step(r, resource.ActionCreate, base))
			// A row of this type, or of any type whose record is gone, as a replace
			// across types cut short leaves it, is what the write stands for: kept, so
			// what it replaced is carried on, never put back beside it (the re-check's
			// H1a).
			for i := range written {
				gone := !slices.ContainsFunc(there, func(x dnsproviders.Record) bool { return x.ID == written[i].ID })
				if written[i].Name == name && (written[i].Type == d.Type || gone) {
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
func (p *Publisher) planRule(ctx context.Context, r revision.PublishRoute, t *target, tunnel, name string, facts Facts, rules map[string]bool) (_ []resource.Step, err error) {
	// Below an admin, a refusal says the tunnel's name and a fixed reason, never its
	// rules, their services, other names or servers, as the live check does (ADR 0104,
	// section 1, and the re-check's M7a). Applying plans again as the control plane,
	// which is told why.
	tunnelName := tunnel
	defer func() {
		if err != nil && !AdminCaller(ctx) {
			err = fmt.Errorf("can't publish %s through %s now. An admin can see why", name, tunnelName)
		}
	}()
	found, err := p.proveTunnel(ctx, r.Server, t, tunnel, facts)
	if err != nil {
		return nil, err
	}
	account := cmp.Or(found.GetAccountTag(), t.zone.Account)
	info, err := t.tunnels.TunnelByID(ctx, account, tunnel)
	if err != nil {
		return nil, fmt.Errorf("can't read the tunnel at Cloudflare: %w", err)
	}
	tunnelName = info.Name
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
	// The rule for the name made by hand, taken on, moved, put back or let go, as the
	// route's consent says (ADR 0104, section 8).
	hand, handled, err := p.planHandRule(ctx, r, t, account, tunnel, info.Name, name, doc, routing, need)
	if err != nil || handled {
		if handKey := tunnel + "|" + name; err == nil && rules[handKey] {
			return nil, nil
		} else if err == nil {
			rules[handKey] = true
		}
		return hand, err
	}
	switch {
	case need.Refused != nil:
		return nil, fmt.Errorf("can't publish %s through %s: %w", name, info.Name, need.Refused)
	case need.Nothing:
		return hand, nil
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
		names := p.Panel(ctx)
		rt.Panel, rt.PanelService = names.Names(), names.Service
	}
	for _, r := range doc.Rules {
		if r.Hostname != "" && !strings.Contains(r.Hostname, "*") {
			rt.Names = append(rt.Names, r.Hostname)
		}
	}
	pointing, err := readFor(p, t, "pointing:"+t.zone.ID+":"+tunnel, func() ([]dnsproviders.Record, error) {
		return t.cf.RecordsPointingAt(ctx, t.zone, tunnel+".cfargotunnel.com")
	})
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
		return nil, proofFailed{facts.Name + " doesn't run that tunnel. Pick a way in under Change", ErrShared}
	}
	if slices.ContainsFunc(here, func(f *dockwellv1.CloudflaredTunnel) bool { return f.GetManagedInFile() }) {
		return nil, proofFailed{facts.Name + "'s tunnel is managed in a file, so Dockwell writes nothing at Cloudflare", ErrUnproven}
	}
	// Each run of the tunnel here gets names from Cloudflare, so a rule's service has
	// to reach apps from every one of them: runs that reach apps differently, or say
	// different accounts, leave no one service to write (the review's M2).
	for _, f := range here[1:] {
		if f.GetReaches() != here[0].GetReaches() || f.GetNetworkRange() != here[0].GetNetworkRange() || f.GetAccountTag() != here[0].GetAccountTag() {
			return nil, proofFailed{fmt.Sprintf("the tunnel runs more than once on %s, and its runs reach apps differently, so Dockwell can't pick where its rule sends a name. Keep one of them", facts.Name), ErrUnproven}
		}
	}
	account := cmp.Or(here[0].GetAccountTag(), t.zone.Account)
	connectors, err := readFor(p, t, "connectors:"+tunnel, func() ([]dnsproviders.Connector, error) {
		return t.tunnels.TunnelConnectors(ctx, account, tunnel)
	})
	if err != nil {
		return nil, fmt.Errorf("can't read the tunnel's connections at Cloudflare: %w", err)
	}
	if len(connectors) == 0 {
		return nil, proofFailed{fmt.Sprintf("the tunnel isn't connected from %s now, so Dockwell can't tell it runs there", facts.Name), ErrUnproven}
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
		return nil, proofFailed{fmt.Sprintf("%s and %s share an address, so Dockwell can't tell which runs the tunnel", facts.Name, strings.Join(facts.SharesWith, ", ")), ErrUnproven}
	}
	if !byAddress && unread {
		return nil, proofFailed{fmt.Sprintf("the tunnel runs more than once on %s, and Dockwell can't read one of its runs, so it can't tell which is real. Keep one of them", facts.Name), ErrUnproven}
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
		return nil, proofFailed{fmt.Sprintf("the tunnel runs on %s and somewhere else too, so Dockwell can't send a name to one of them", facts.Name), ErrShared}
	}
	if byAddress {
		return here[0], nil
	}
	if proved == nil {
		return nil, proofFailed{fmt.Sprintf("the tunnel isn't connected from %s now, so Dockwell can't tell it runs there", facts.Name), ErrUnproven}
	}
	return proved, nil
}

// removalRouting is what a rule's removal keeps where it goes: the panel's names,
// the tunnel's own, and the zone's names pointing at the tunnel, but for names
// Dockwell wrote, which go with their routes, and leaving, the route's own.
func (p *Publisher) removalRouting(ctx context.Context, t *target, tunnel string, doc *Document, service, leaving string) (Routing, error) {
	rt, err := p.routing(ctx, t, tunnel, doc, service, strings.ToLower(leaving))
	if err != nil {
		return Routing{}, err
	}
	var own []store.CloudflareRecord
	if err := p.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		own, err = tx.CloudflareRecordsIn(ctx, t.zone.ID)
		return err
	}); err != nil {
		return Routing{}, err
	}
	rt.Names = slices.DeleteFunc(rt.Names, func(n string) bool {
		return strings.EqualFold(n, leaving) || slices.ContainsFunc(own, func(r store.CloudflareRecord) bool { return strings.EqualFold(r.Name, n) })
	})
	return rt, nil
}

// removalMoves are the names removing rule would send elsewhere, read as Cloudflare
// has its tunnel now.
func (p *Publisher) removalMoves(ctx context.Context, t *target, rule *store.CloudflareRule, leaving string) ([]Moved, error) {
	read, err := t.tunnels.ReadTunnelConfig(ctx, rule.Account, rule.TunnelID)
	if err != nil {
		return nil, fmt.Errorf("can't read the tunnel's routes at Cloudflare: %w", err)
	}
	doc, err := Parse(read.Config)
	if err != nil {
		return nil, err
	}
	i := doc.Index(rule.Hostname, rule.Path)
	if i < 0 {
		return nil, nil
	}
	rt, err := p.removalRouting(ctx, t, rule.TunnelID, doc, rule.Service, leaving)
	if err != nil {
		return nil, err
	}
	after, err := doc.Remove(i)
	if err != nil {
		return nil, err
	}
	return rt.Check(doc.Rules, after.Rules, Edit{At: i}), nil
}

// keptWords says why a rule stays, naming what goes through it.
func keptWords(moved []Moved) string {
	if len(moved) == 1 {
		return moved[0].Name + " goes through it, so it stays"
	}
	return fmt.Sprintf("%s and %d other names go through it, so it stays", moved[0].Name, len(moved)-1)
}

// handRuleLetGo says a rule made by hand that the route took on for name is let go,
// as it still serves the name: the record that sends the name to it is still needed.
func (p *Publisher) handRuleLetGo(ctx context.Context, r revision.PublishRoute, name string) (bool, error) {
	was := r.Before.GetPublish()
	if was.GetWay() != resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL {
		return false, nil
	}
	held, found, err := p.heldRule(ctx, was.GetTunnel(), strings.ToLower(name))
	if err != nil || !found {
		return false, err
	}
	return held.TakenOnAt != nil && held.State != store.CloudflareRuleKept && !readKept(held.Replaced).Moved, nil
}

// recordLetGoAt says the route took on a record made by hand at name, which is let
// go as the route still serves the name: the zone's rule it goes through is still needed.
func (p *Publisher) recordLetGoAt(ctx context.Context, routeID, name string) (bool, error) {
	needed := false
	err := p.DB.View(ctx, func(tx *store.Tx) error {
		recs, err := tx.CloudflareRecordsOf(ctx, routeID)
		for i := range recs {
			if strings.EqualFold(recs[i].Name, name) && recs[i].TakenOnAt != nil && recs[i].Type != store.CloudflarePendingType {
				needed = true
			}
		}
		return err
	})
	return needed, err
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
	// The rule for the name made by hand that the route took on or moved comes out,
	// or goes back as it was (ADR 0104, section 8).
	hand, err := p.planHandRuleRemoval(ctx, r, inChange, was.GetTunnel(), name)
	if err != nil {
		return nil, err
	}
	all, err := p.Routes(ctx)
	if err != nil {
		return nil, err
	}
	for id, spec := range inChange {
		all[id] = spec
	}
	steps := hand
	for i := range held {
		rule := &held[i]
		// A rule made by hand is the route's own to settle, above. A route that still
		// serves the name, publishing it no more, keeps what brings it in, unless this
		// undoes the change that added it (the review's H2).
		if rule.TakenOnAt != nil || !HostMatches(rule.Hostname, name) {
			continue
		}
		// Dockwell's own zone rule goes as before, unless a record made by hand that
		// this lets go still points the name at the tunnel through it (the re-check's
		// A-M3).
		if keepsName(r, name) && r.Reverts != rule.ChangeID {
			needed, err := p.recordLetGoAt(ctx, r.ID, name)
			if err != nil {
				return nil, err
			}
			if needed {
				continue
			}
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
		t, err := p.target(ctx, rule.Provider, name)
		if err != nil {
			return nil, err
		}
		tunnelName := rule.TunnelID
		if info, err := t.tunnels.TunnelByID(ctx, rule.Account, rule.TunnelID); err == nil {
			tunnelName = info.Name
		}
		ps := resource.ProviderStep{
			Provider: rule.Provider, Action: resource.ProviderRemoveRule, Target: tunnelName, Name: rule.Hostname, Was: rule.Service,
			Account: rule.Account, Tunnel: rule.TunnelID, ZoneID: t.zone.ID, Rule: &resource.ProviderRule{Hostname: rule.Hostname, Path: rule.Path, Service: rule.Service},
		}
		// Names made in Cloudflare that go through the rule keep it, as removing it
		// would move them; the plan says which (the review's M4).
		moved, err := p.removalMoves(ctx, t, rule, name)
		if err != nil {
			return nil, err
		}
		action := resource.ActionDelete
		if len(moved) > 0 {
			action, ps.Action, ps.Routing = resource.ActionNone, resource.ProviderKeepRule, keptWords(moved)
		}
		steps = append(steps, step(r, action, ps))
	}
	return steps, nil
}
