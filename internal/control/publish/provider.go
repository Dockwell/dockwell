// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

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
		add(resource.ActionUpdate, resource.ProviderStep{
			Provider: name, Action: resource.ProviderSetZoneMode, Target: zone, Name: zone, Was: modeWords(mode), To: modeWords(strict),
			Routing: proxiedWords(zone, proxied), ZoneID: t.zone.ID, Mode: strict,
		})
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
		add(resource.ActionUpdate, resource.ProviderStep{
			Provider: name, Action: resource.ProviderSetZoneMode, Target: zone, Name: zone, Was: modeWords(strict), To: modeWords(held.Was),
			Routing: proxiedWords(zone, proxied), ZoneID: t.zone.ID, Mode: held.Was,
		})
	}
	return steps, nil
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

// proxiedWords names the proxied names in zone whose way in a mode changes.
func proxiedWords(zone string, records []dnsproviders.Record) string {
	var names []string
	for _, r := range records {
		if !slices.Contains(names, r.Name) {
			names = append(names, r.Name)
		}
	}
	slices.Sort(names)
	switch {
	case len(names) == 0:
		return "No name in " + zone + " is proxied now"
	case len(names) <= 3:
		return "Changes how Cloudflare reaches " + strings.Join(names, ", ")
	}
	return fmt.Sprintf("Changes how Cloudflare reaches %s and %d other proxied names in %s", strings.Join(names[:3], ", "), len(names)-3, zone)
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
