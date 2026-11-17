// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// A DNS provider's own steps at Cloudflare, which belong to no route: setting a zone
// to Full (strict) for an admin, and back, and putting back records made by hand
// that Dockwell keeps a copy of. They're steps of the provider's change, run by the
// control plane under its own server's plan (ADR 0104, sections 2 and 8).

// strict is Cloudflare's name for Full (strict).
const strict = "strict"

// modeWords is a zone's SSL/TLS mode as the dashboard says it.
func modeWords(mode string) string {
	switch mode {
	case "off":
		return "Off"
	case "flexible":
		return "Flexible"
	case "full":
		return "Full"
	case strict:
		return "Full (strict)"
	}
	return mode
}

// planProvider plans a DNS provider's steps: each zone it adds to its strict zones
// set to Full (strict), each it takes off put back to the mode it had, and each kept
// record it names put back where its name is free.
func (p *Publisher) planProvider(ctx context.Context, pp revision.PublishProvider) ([]resource.Step, error) {
	before, after := pp.Before.GetStrictZones(), pp.After.GetStrictZones()
	name := pp.Ref.Name
	var steps []resource.Step
	add := func(action resource.Action, ps resource.ProviderStep, access ...resource.Access) {
		steps = append(steps, resource.Step{Ref: pp.Ref, Decision: resource.Decision{Action: action, Provider: &ps, HostAccess: access}})
	}
	if pp.After == nil {
		// A provider that goes can't put back what it set: its token goes with it.
		for _, zone := range before {
			if set, err := p.modeSet(ctx, name, zone); err != nil || set {
				return nil, errors.Join(err, fmt.Errorf("%s set %s to Full (strict) for Dockwell. Take it off the provider's strict zones first, so its mode goes back, then remove the provider", name, zone))
			}
		}
		return nil, nil
	}
	for _, zone := range after {
		if slices.Contains(before, zone) {
			continue
		}
		if pp.Before == nil {
			return nil, fmt.Errorf("%s isn't saved yet, so it can't set %s to Full (strict). Save it, then set its strict zones", name, zone)
		}
		t, err := p.target(ctx, name, zone)
		if err != nil {
			return nil, err
		}
		mode, _, err := t.cf.SSLMode(ctx, t.zone)
		if err != nil {
			return nil, fmt.Errorf("can't read %s's SSL mode at Cloudflare: %w", zone, err)
		}
		if mode == strict {
			continue
		}
		proxied, err := t.cf.RecordsProxied(ctx, t.zone)
		if err != nil {
			return nil, fmt.Errorf("can't read %s's proxied names at Cloudflare: %w", zone, err)
		}
		// Each name the mode changes, and those it can break, which an admin approves:
		// the panel's first, then the names Dockwell doesn't publish (the review's M3).
		all, atRisk := p.zoneModeNames(ctx, proxied)
		add(resource.ActionUpdate, resource.ProviderStep{
			Provider: name, Action: resource.ProviderSetZoneMode, Target: zone, Name: zone, Was: modeWords(mode), To: modeWords(strict),
			Routing: strictWords(zone, all), ZoneID: t.zone.ID, Mode: strict,
		}, resource.Access{Kind: resource.AccessZoneMode, Path: zone, Detail: strings.Join(atRisk, ", ")})
	}
	for _, zone := range before {
		if slices.Contains(after, zone) {
			continue
		}
		t, err := p.target(ctx, name, zone)
		if err != nil {
			return nil, err
		}
		var held store.CloudflareZoneMode
		found := true
		if err := p.DB.View(ctx, func(tx *store.Tx) error {
			var err error
			held, err = tx.CloudflareZoneModeOf(ctx, t.zone.ID)
			if errors.Is(err, store.ErrNotFound) {
				found = false
				return nil
			}
			return err
		}); err != nil {
			return nil, err
		}
		if !found {
			// Dockwell didn't change it, so there's nothing of its to put back.
			continue
		}
		proxied, err := t.cf.RecordsProxied(ctx, t.zone)
		if err != nil {
			return nil, fmt.Errorf("can't read %s's proxied names at Cloudflare: %w", zone, err)
		}
		all, _ := p.zoneModeNames(ctx, proxied)
		add(resource.ActionUpdate, resource.ProviderStep{
			Provider: name, Action: resource.ProviderSetZoneMode, Target: zone, Name: zone, Was: modeWords(strict), To: modeWords(held.Was),
			Routing: putBackWords(zone, held.Was, all), ZoneID: t.zone.ID, Mode: held.Was,
		})
	}
	for _, id := range pp.After.GetPutBack() {
		ps, access, err := p.planPutBack(ctx, name, id)
		if err != nil {
			return nil, err
		}
		action := resource.ActionCreate
		if ps.Skip {
			action = resource.ActionNone
		}
		add(action, ps, access...)
	}
	return steps, nil
}

// planPutBack plans putting back a kept record where its name is free at
// Cloudflare. Where the name holds a record, the step says so and writes nothing:
// putting it back never replaces anything. It writes someone's record, so it takes
// an admin with a fresh passkey (cloudflare_takeover).
func (p *Publisher) planPutBack(ctx context.Context, provider, id string) (resource.ProviderStep, []resource.Access, error) {
	var k store.CloudflareRecord
	if err := p.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		k, err = tx.CloudflareRecordByID(ctx, id)
		return err
	}); err != nil || k.Type != store.CloudflarePendingType || k.Provider != provider {
		return resource.ProviderStep{}, nil, fmt.Errorf("%s keeps no record %s. The provider's info lists the ones it keeps", provider, id)
	}
	var old dnsproviders.Record
	if err := json.Unmarshal([]byte(k.Replaced), &old); err != nil {
		return resource.ProviderStep{}, nil, fmt.Errorf("the record kept as %s can't be read: %w", id, err)
	}
	t, err := p.target(ctx, provider, k.Name)
	if err != nil {
		return resource.ProviderStep{}, nil, err
	}
	there, err := t.cf.RecordsAt(ctx, t.zone, k.Name)
	if err != nil {
		return resource.ProviderStep{}, nil, fmt.Errorf("can't read what Cloudflare holds at %s: %w", k.Name, err)
	}
	ps := resource.ProviderStep{
		Provider: provider, Action: resource.ProviderRestoreRecord, Target: k.Zone, Name: k.Name, To: recordWords(old), ZoneID: k.ZoneID, Pending: k.ID,
		Record: &resource.ProviderRecord{Type: old.Type, Name: old.Name, Content: old.Content, Proxied: old.Proxied, TTL: old.TTL, Comment: old.Comment},
	}
	if i := slices.IndexFunc(there, func(x dnsproviders.Record) bool { return addressLike(x.Type) }); i >= 0 {
		ps.Skip, ps.Was = true, recordWords(there[i])
		ps.Routing = k.Name + " holds " + recordWords(there[i]) + " now, so the record kept isn't put back. Free the name, then put it back"
		return ps, nil, nil
	}
	return ps, []resource.Access{{Kind: resource.AccessCloudflareTakeover, Path: k.Name, Detail: recordWords(old)}}, nil
}

// modeSet says Dockwell set zone's mode through provider, and keeps what it was.
func (p *Publisher) modeSet(ctx context.Context, provider, zone string) (bool, error) {
	t, err := p.target(ctx, provider, zone)
	if err != nil {
		return false, err
	}
	set := false
	err = p.DB.View(ctx, func(tx *store.Tx) error {
		_, err := tx.CloudflareZoneModeOf(ctx, t.zone.ID)
		set = err == nil
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
	return set, err
}

// zoneModeNames are the proxied names a zone's mode changes how Cloudflare reaches,
// leaving out names on a tunnel, which the mode doesn't touch, with the panel's
// first, then the names Dockwell doesn't publish, then its own. atRisk are those
// that can stop answering under Full (strict): the panel's and the ones Dockwell
// doesn't publish, whose server's certificate it can't vouch for.
func (p *Publisher) zoneModeNames(ctx context.Context, proxied []dnsproviders.Record) (all, atRisk []string) {
	var panel []string
	if p.Panel != nil {
		panel = p.Panel(ctx).Names()
	}
	ours := "dockwell " + p.Install + " "
	var first, theirs, own []string
	for _, r := range proxied {
		if strings.HasSuffix(r.Content, ".cfargotunnel.com") || slices.Contains(all, r.Name) {
			continue
		}
		all = append(all, r.Name)
		switch {
		case slices.ContainsFunc(panel, func(n string) bool { return strings.EqualFold(n, r.Name) }):
			first = append(first, r.Name)
		case !strings.HasPrefix(r.Comment, ours):
			theirs = append(theirs, r.Name)
		default:
			own = append(own, r.Name)
		}
	}
	slices.Sort(theirs)
	slices.Sort(own)
	atRisk = slices.Concat(first, theirs)
	return slices.Concat(first, theirs, own), atRisk
}

// strictWords says what Full (strict) does to names, every one named.
func strictWords(zone string, names []string) string {
	if len(names) == 0 {
		return "No name in " + zone + " is proxied outside a tunnel now"
	}
	return "Cloudflare reaches each over HTTPS with a certificate it checks, so a name whose server has none valid stops answering: " + strings.Join(names, ", ")
}

// putBackWords says what putting a zone's mode back does to names.
func putBackWords(zone, mode string, names []string) string {
	if len(names) == 0 {
		return "No name in " + zone + " is proxied outside a tunnel now"
	}
	return "Cloudflare reaches each as " + modeWords(mode) + " says again: " + strings.Join(names, ", ")
}

// setZoneMode sets a zone's mode as its step says. Setting Full (strict) keeps what
// the mode was first, so a crash between the two loses nothing, and taking it off
// puts that back, only while the zone still reads Full (strict): one changed in
// Cloudflare since stays, and is let go.
func (p *Publisher) setZoneMode(ctx context.Context, changeID string, s resource.ProviderStep) (string, error) {
	t, err := p.target(ctx, s.Provider, s.Name)
	if err != nil {
		return "", err
	}
	if s.ZoneID != "" && t.zone.ID != s.ZoneID {
		return "", fmt.Errorf("%s is another zone at Cloudflare than when the change was planned. Plan it again", s.Name)
	}
	defer p.cache.forget("ssl:" + t.zone.ID)
	mode, _, err := t.cf.SSLMode(ctx, t.zone)
	if err != nil {
		return "", fmt.Errorf("can't read %s's SSL mode at Cloudflare: %w", s.Name, err)
	}
	var held store.CloudflareZoneMode
	found := true
	if err := p.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		held, err = tx.CloudflareZoneModeOf(ctx, t.zone.ID)
		if errors.Is(err, store.ErrNotFound) {
			found = false
			return nil
		}
		return err
	}); err != nil {
		return "", err
	}
	write := func(to string) error {
		if err := t.cf.SetSSLMode(ctx, t.zone, to); err != nil {
			if denied(err) {
				return fmt.Errorf("can't set %s's SSL mode: the token lacks Zone Settings edit. Add it to this token at %s, then apply again", s.Name, dnsproviders.TokenPage(t.zone.Account))
			}
			return fmt.Errorf("can't set %s's SSL mode: %w", s.Name, err)
		}
		return nil
	}
	if s.Mode == strict {
		switch {
		case mode == strict && !found:
			return "", letGo{s.Name + " is on Full (strict) already"}
		case mode == strict:
			return "", nil
		}
		if !found {
			if err := p.DB.Update(ctx, func(tx *store.Tx) error {
				return tx.PutCloudflareZoneMode(ctx, store.CloudflareZoneMode{ZoneID: t.zone.ID, Provider: s.Provider, Zone: t.zone.Name, Was: mode, ChangeID: changeID, SetAt: p.now()})
			}); err != nil {
				return "", err
			}
		}
		return "", write(strict)
	}
	forget := func() error {
		return p.DB.Update(ctx, func(tx *store.Tx) error { return tx.DeleteCloudflareZoneMode(ctx, t.zone.ID) })
	}
	switch {
	case !found:
		return "", letGo{"Dockwell didn't set " + s.Name + "'s mode, so it stays " + modeWords(mode)}
	case mode == held.Was:
		// Put back already, by a step a crash cut short.
		return "", forget()
	case mode != strict:
		if err := forget(); err != nil {
			return "", err
		}
		return "", letGo{s.Name + "'s mode was changed in Cloudflare since, so it stays " + modeWords(mode)}
	}
	if err := write(held.Was); err != nil {
		return "", err
	}
	return "", forget()
}

// Kept lists the records made by hand that provider keeps a copy of, with whether
// each name is free at Cloudflare now: a record replaced by a route that was
// removed while its name held something else. Below an admin, a record's content
// isn't said, as a proxied record's is the origin address (ADR 0104, M12).
func (p *Publisher) Kept(ctx context.Context, provider string, admin bool) ([]*dockwellv1.KeptRecord, error) {
	var kept []store.CloudflareRecord
	if err := p.DB.View(ctx, func(tx *store.Tx) error {
		all, err := tx.CloudflarePendingAll(ctx)
		for i := range all {
			if all[i].Provider == provider {
				kept = append(kept, all[i])
			}
		}
		return err
	}); err != nil {
		return nil, err
	}
	// Whether each name is free is read through the check's 30 s cache, and the list
	// waits on Cloudflare for a few seconds at most.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out := make([]*dockwellv1.KeptRecord, 0, len(kept))
	for i := range kept {
		k := &kept[i]
		var old dnsproviders.Record
		_ = json.Unmarshal([]byte(k.Replaced), &old)
		rec := &dockwellv1.KeptRecord{Id: k.ID, Name: k.Name, KeptAt: timestamppb.New(k.CreatedAt)}
		if admin {
			rec.Record = recordWords(old)
		}
		if t, err := p.target(ctx, provider, k.Name); err == nil {
			t.cached = true
			if there, err := readFor(p, t, "at:"+t.zone.ID+":"+k.Name, func() ([]dnsproviders.Record, error) { return t.cf.RecordsAt(ctx, t.zone, k.Name) }); err == nil {
				rec.NameFree = !slices.ContainsFunc(there, func(x dnsproviders.Record) bool { return addressLike(x.Type) })
			}
		}
		out = append(out, rec)
	}
	return out, nil
}
