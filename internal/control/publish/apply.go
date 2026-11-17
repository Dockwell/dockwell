// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// Apply takes one step at Cloudflare. Each step looks before it writes, so one run
// again after a control plane stopped midway finds what it wrote (ADR 0104, section
// 8).
func (p *Publisher) Apply(ctx context.Context, changeID string, route revision.PublishRoute, s resource.ProviderStep) (string, error) {
	// A live check reads which names point at a tunnel again after any write here,
	// whatever came of it.
	defer p.cache.forgetUnder("pointing:" + s.ZoneID)
	said, err := p.apply(ctx, changeID, route, s)
	if err == nil {
		p.noteWrite(ctx, route.ID, s, 0)
	}
	return said, err
}

func (p *Publisher) apply(ctx context.Context, changeID string, route revision.PublishRoute, s resource.ProviderStep) (string, error) {
	switch s.Action {
	case resource.ProviderCreateRecord, resource.ProviderRestoreRecord:
		return p.createRecord(ctx, changeID, route, s, "")
	case resource.ProviderDeleteRecord:
		return p.deleteRecord(ctx, s)
	case resource.ProviderReplaceRecord:
		return p.replaceRecord(ctx, changeID, route, s)
	case resource.ProviderTakeOnRecord:
		return p.takeOnRecord(ctx, changeID, route, s)
	case resource.ProviderAddRule:
		return p.addRule(ctx, changeID, route.ID, s)
	case resource.ProviderRemoveRule:
		return p.removeRule(ctx, s, route.Before.GetDomain())
	case resource.ProviderKeepRule:
		return "Kept " + s.Name + " in " + s.Target + ": " + s.Routing, nil
	case resource.ProviderTakeOnRule, resource.ProviderMoveRule:
	}
	return "", fmt.Errorf("a step of kind %d at Cloudflare isn't one this control plane takes yet. Upgrade it, then try again", s.Action)
}

// zoneOf reaches the zone a step names, with its provider's token.
func (p *Publisher) zoneOf(ctx context.Context, s resource.ProviderStep) (*target, error) {
	t, err := p.target(ctx, s.Provider, s.Name)
	if err != nil {
		return nil, err
	}
	if s.ZoneID != "" && t.zone.ID != s.ZoneID {
		return nil, fmt.Errorf("%s is in another zone at Cloudflare than when the change was planned. Plan it again", s.Name)
	}
	return t, nil
}

func asRecord(r *resource.ProviderRecord) dnsproviders.Record {
	return dnsproviders.Record{Type: r.Type, Name: r.Name, Content: r.Content, Proxied: r.Proxied, TTL: cmp.Or(r.TTL, 1), Comment: r.Comment}
}

// createRecord writes a step's record, or finds it written by a step a crash cut
// short, and keeps it as Dockwell's, with what it replaced, if anything.
func (p *Publisher) createRecord(ctx context.Context, changeID string, route revision.PublishRoute, s resource.ProviderStep, replaced string) (string, error) {
	t, err := p.zoneOf(ctx, s)
	if err != nil {
		return "", err
	}
	want := asRecord(s.Record)
	there, err := t.cf.RecordsAt(ctx, t.zone, want.Name)
	if err != nil {
		return "", fmt.Errorf("can't read what Cloudflare holds at %s: %w", want.Name, err)
	}
	var got dnsproviders.Record
	for _, x := range there {
		if x.Type == want.Type && x.Content == want.Content && x.Comment == want.Comment {
			got = x
		}
	}
	if got.ID == "" {
		got, err = t.cf.CreateRecord(ctx, t.zone, want)
		if err != nil {
			var api *dnsproviders.APIError
			if errors.As(err, &api) && api.Code == 81053 {
				if s.Action == resource.ProviderRestoreRecord {
					return "", fmt.Errorf("%s has been taken at Cloudflare since, so the record it had can't be put back", want.Name)
				}
				return "", fmt.Errorf("%s was taken at Cloudflare after the change was planned. Plan it again", want.Name)
			}
			return "", fmt.Errorf("can't write the DNS record for %s: %w", want.Name, err)
		}
	}
	if s.Action == resource.ProviderRestoreRecord {
		// Put back as it was, it's someone else's again, and what kept it goes.
		if s.Pending == "" {
			return "", nil
		}
		return "", p.DB.Update(ctx, func(tx *store.Tx) error { return tx.DeleteCloudflareRecord(ctx, s.Pending) })
	}
	return "", p.DB.Update(ctx, func(tx *store.Tx) error {
		// What a replace kept of a record made by hand at this name, before a delete
		// that landed without its write, stays with the record that's written now, so
		// removing it, or undo, puts that one back (the review's H1).
		kept, err := tx.CloudflarePendingOf(ctx, route.ID)
		if err != nil {
			return err
		}
		for i := range kept {
			if strings.EqualFold(kept[i].Name, got.Name) {
				replaced = cmp.Or(replaced, kept[i].Replaced)
				if err := tx.DeleteCloudflareRecord(ctx, kept[i].ID); err != nil {
					return err
				}
			}
		}
		// What was kept of a record of this route's at the name, of this type, that's
		// gone, goes: this one stands for it now, with what that one replaced.
		was, err := tx.CloudflareRecordsOf(ctx, route.ID)
		if err != nil {
			return err
		}
		for i := range was {
			if was[i].ID != got.ID && strings.EqualFold(was[i].Name, got.Name) && was[i].Type == got.Type {
				replaced = cmp.Or(replaced, was[i].Replaced)
				if err := tx.DeleteCloudflareRecord(ctx, was[i].ID); err != nil {
					return err
				}
			}
		}
		return tx.PutCloudflareRecord(ctx, store.CloudflareRecord{
			ID: got.ID, Provider: s.Provider, ZoneID: t.zone.ID, Zone: t.zone.Name, Name: got.Name, Type: got.Type, Content: got.Content,
			Proxied: got.Proxied, TTL: got.TTL, Comment: got.Comment, RouteID: route.ID, ChangeID: changeID, Replaced: replaced, CreatedAt: p.now(),
		})
	})
}

// deleteRecord removes a record Dockwell wrote, only while it reads as written: one
// changed in Cloudflare since stays, and is let go.
func (p *Publisher) deleteRecord(ctx context.Context, s resource.ProviderStep) (string, error) {
	t, err := p.zoneOf(ctx, s)
	if err != nil {
		return "", err
	}
	forget := func() error {
		return p.DB.Update(ctx, func(tx *store.Tx) error { return tx.DeleteCloudflareRecord(ctx, s.Record.ID) })
	}
	there, err := t.cf.RecordByID(ctx, t.zone, s.Record.ID)
	var api *dnsproviders.APIError
	switch {
	case errors.As(err, &api) && api.Code == 81044:
		return "", forget()
	case err != nil:
		return "", fmt.Errorf("can't read the DNS record for %s: %w", s.Name, err)
	case there.Type != s.Record.Type || there.Content != s.Record.Content || there.Proxied != s.Record.Proxied:
		if err := forget(); err != nil {
			return "", err
		}
		return s.Name + "'s record was changed in Cloudflare, so it stays", nil
	}
	if err := t.cf.Delete(ctx, t.zone, s.Record.ID); err != nil && (!errors.As(err, &api) || api.Code != 81044) {
		return "", fmt.Errorf("can't remove the DNS record for %s: %w", s.Name, err)
	}
	return "", forget()
}

// replaceRecord replaces the record at a name with the step's. Someone else's is kept
// whole first, so undo writes it back; Dockwell's own, written another way, isn't.
func (p *Publisher) replaceRecord(ctx context.Context, changeID string, route revision.PublishRoute, s resource.ProviderStep) (string, error) {
	t, err := p.zoneOf(ctx, s)
	if err != nil {
		return "", err
	}
	var api *dnsproviders.APIError
	old, err := t.cf.RecordByID(ctx, t.zone, s.Record.ID)
	if errors.As(err, &api) && api.Code == 81044 {
		// Gone already, as a step a crash cut short leaves it: what was kept of it stays,
		// and the write takes it on (createRecord).
		return p.createRecord(ctx, changeID, route, s, "")
	}
	if err != nil {
		return "", fmt.Errorf("can't read the DNS record at %s: %w", s.Name, err)
	}
	// Dockwell's own only where the store holds this very record for this route, or
	// its comment is this route's own: another record at the name, of the same type,
	// is someone else's, and kept (the review's H1).
	var mine bool
	replaced := ""
	if err := p.DB.View(ctx, func(tx *store.Tx) error {
		row, err := tx.CloudflareRecordByID(ctx, old.ID)
		mine = err == nil && row.RouteID == route.ID || old.Comment == p.Comment(route.ID)
		if mine {
			// Its own record, written another way: what that one replaced, the new one
			// stands for now.
			replaced = row.Replaced
		}
		return nil
	}); err != nil {
		return "", err
	}
	if !mine {
		// Its ID is kept too, as what the route's consent names (the review's H1).
		b, err := json.Marshal(dnsproviders.Record{ID: old.ID, Type: old.Type, Name: old.Name, Content: old.Content, Proxied: old.Proxied, TTL: old.TTL, Comment: old.Comment, Priority: old.Priority})
		if err != nil {
			return "", err
		}
		replaced = string(b)
		// Kept before it's deleted, so a crash between the two loses nothing.
		if err := p.DB.Update(ctx, func(tx *store.Tx) error {
			return tx.PutCloudflareRecord(ctx, store.CloudflareRecord{
				ID: pending(old.ID), Provider: s.Provider, ZoneID: t.zone.ID, Zone: t.zone.Name, Name: old.Name, Type: store.CloudflarePendingType,
				Content: old.Content, RouteID: route.ID, ChangeID: changeID, Replaced: replaced, State: "pending", CreatedAt: p.now(),
			})
		}); err != nil {
			return "", err
		}
	}
	if err := t.cf.Delete(ctx, t.zone, old.ID); err != nil && (!errors.As(err, &api) || api.Code != 81044) {
		return "", fmt.Errorf("can't remove the record at %s to replace it: %w", s.Name, err)
	}
	said, err := p.createRecord(ctx, changeID, route, s, replaced)
	if err != nil {
		return "", err
	}
	return said, p.DB.Update(ctx, func(tx *store.Tx) error {
		if err := tx.DeleteCloudflareRecord(ctx, pending(old.ID)); err != nil {
			return err
		}
		if mine {
			return tx.DeleteCloudflareRecord(ctx, old.ID)
		}
		return nil
	})
}

// pending is the store's ID for what a replace kept of a record before deleting it:
// never a record's ID at Cloudflare, nor sent there.
func pending(id string) string { return "pending:" + id }

// takeOnRecord keeps a record made by hand as Dockwell's, writing nothing.
func (p *Publisher) takeOnRecord(ctx context.Context, changeID string, route revision.PublishRoute, s resource.ProviderStep) (string, error) {
	t, err := p.zoneOf(ctx, s)
	if err != nil {
		return "", err
	}
	there, err := t.cf.RecordByID(ctx, t.zone, s.Record.ID)
	if err != nil {
		return "", fmt.Errorf("can't read the record at %s to take it on: %w", s.Name, err)
	}
	now := p.now()
	return "", p.DB.Update(ctx, func(tx *store.Tx) error {
		return tx.PutCloudflareRecord(ctx, store.CloudflareRecord{
			ID: there.ID, Provider: s.Provider, ZoneID: t.zone.ID, Zone: t.zone.Name, Name: there.Name, Type: there.Type, Content: there.Content,
			Proxied: there.Proxied, TTL: there.TTL, Comment: there.Comment, RouteID: route.ID, ChangeID: changeID, TakenOnAt: &now, CreatedAt: now,
		})
	})
}

// lock makes this change the one writer of a tunnel in Dockwell, waiting for another
// that holds it, and returns how to let it go.
func (p *Publisher) lock(ctx context.Context, tunnel, holder string) (func(), error) {
	wait := cmp.Or(p.LockWait, time.Minute)
	deadline := p.now().Add(wait)
	for {
		var held bool
		now := p.now()
		if err := p.DB.Update(ctx, func(tx *store.Tx) error {
			var err error
			held, err = tx.LockTunnel(ctx, tunnel, holder, now, now.Add(2*time.Minute))
			return err
		}); err != nil {
			return nil, err
		}
		if held {
			return func() {
				_ = p.DB.Update(context.WithoutCancel(ctx), func(tx *store.Tx) error { return tx.UnlockTunnel(ctx, tunnel, holder) })
			}, nil
		}
		if p.now().After(deadline) {
			return nil, errors.New("another change is writing the same tunnel's routes. Try again once it's done")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// addRule adds the zone's rule, or its apex's, to a tunnel, as the document reads
// now: spliced into what was read, checked before it's written, and read back.
func (p *Publisher) addRule(ctx context.Context, changeID, routeID string, s resource.ProviderStep) (string, error) {
	t, err := p.target(ctx, s.Provider, s.Name)
	if err != nil {
		// A rule's name is a wildcard; its zone is what follows the "*.".
		if t, err = p.target(ctx, s.Provider, strings.TrimPrefix(s.Name, "*.")); err != nil {
			return "", err
		}
	}
	unlock, err := p.lock(ctx, s.Tunnel, changeID)
	if err != nil {
		return "", err
	}
	defer unlock()
	read, err := t.tunnels.ReadTunnelConfig(ctx, s.Account, s.Tunnel)
	if err != nil {
		return "", fmt.Errorf("can't read %s's routes: %w", s.Target, err)
	}
	doc, err := Parse(read.Config)
	if err != nil {
		return "", err
	}
	name := strings.TrimPrefix(s.Name, "*.")
	if strings.HasPrefix(s.Name, "*.") {
		name = "dockwell-routing-check." + name
	}
	rt, err := p.routing(ctx, t, s.Tunnel, doc, s.Rule.Service, name)
	if err != nil {
		return "", err
	}
	need := NeedFor(doc, rt, t.zone.Name, name)
	record := func(written *Document, version int) error {
		return p.DB.Update(ctx, func(tx *store.Tx) error {
			if err := tx.PutCloudflareRule(ctx, store.CloudflareRule{
				TunnelID: s.Tunnel, Hostname: s.Rule.Hostname, Account: s.Account, Provider: s.Provider, Zone: t.zone.Name, Service: s.Rule.Service,
				ReadDoc: string(read.Config), WrittenDoc: string(written.Raw()), Version: version, ChangeID: changeID, CreatedAt: p.now(),
			}); err != nil {
				return err
			}
			return tx.SawTunnel(ctx, s.Tunnel, s.Account, version, p.now())
		})
	}
	switch {
	case need.Refused != nil:
		return "", fmt.Errorf("can't add %s to %s: %w", s.Name, s.Target, need.Refused)
	case need.Nothing:
		// There already, as a step run again finds its rule, or someone added one.
		if doc.Index(s.Rule.Hostname, "") >= 0 {
			return "", record(doc, read.Version)
		}
		return "", nil
	}
	if !strings.EqualFold(need.Rule.Hostname, s.Rule.Hostname) || !SameService(need.Rule.Service, s.Rule.Service) {
		return "", fmt.Errorf("%s's routes changed since the change was planned. Plan it again", s.Target)
	}
	if err := CheckSplice(read.Config, need.After.Raw(), need.Edit.At, true); err != nil {
		return "", err
	}
	start := p.now()
	defer p.cache.forget("doc:" + s.Tunnel)
	wrote, err := t.tunnels.WriteTunnelConfig(ctx, s.Account, s.Tunnel, need.After.Raw())
	if err != nil {
		return "", fmt.Errorf("can't write %s's routes: %w", s.Target, err)
	}
	back, err := t.tunnels.ReadTunnelConfig(ctx, s.Account, s.Tunnel)
	if err != nil {
		return "", fmt.Errorf("wrote %s's routes and can't read them back: %w", s.Target, err)
	}
	backDoc, err := Parse(back.Config)
	if err != nil {
		return "", err
	}
	if backDoc.Index(s.Rule.Hostname, "") < 0 {
		return "", fmt.Errorf("%s's routes were written again in Cloudflare just as Dockwell wrote them, and %s isn't in them. Try again", s.Target, s.Rule.Hostname)
	}
	if err := record(need.After, wrote.Version); err != nil {
		return "", err
	}
	// Each write that changes the document adds one to its version (Verified, 1): more
	// says another write landed between Dockwell's read and its own.
	if wrote.Version > read.Version+1 {
		p.note(ctx, ActionReplaced, routeID, told{Provider: s.Provider, Target: s.Target, Name: s.Name, Action: int(s.Action), Version: wrote.Version, ReadVersion: read.Version})
		return fmt.Sprintf("An edit made in Cloudflare between %s and %s was replaced. Dockwell kept what it read, and that's what's there now",
			start.Format("15:04:05"), p.now().Format("15:04:05")), nil
	}
	return "", nil
}

// removeRule takes a rule Dockwell added out of a tunnel, only while it reads as
// written: one changed in Cloudflare since stays, and is let go.
func (p *Publisher) removeRule(ctx context.Context, s resource.ProviderStep, leaving string) (string, error) {
	t, err := p.target(ctx, s.Provider, strings.TrimPrefix(s.Name, "*."))
	if err != nil {
		return "", err
	}
	forget := func() error {
		return p.DB.Update(ctx, func(tx *store.Tx) error { return tx.DeleteCloudflareRule(ctx, s.Tunnel, s.Rule.Hostname, s.Rule.Path) })
	}
	unlock, err := p.lock(ctx, s.Tunnel, "removal:"+s.Tunnel+":"+s.Rule.Hostname)
	if err != nil {
		return "", err
	}
	defer unlock()
	read, err := t.tunnels.ReadTunnelConfig(ctx, s.Account, s.Tunnel)
	if err != nil {
		return "", fmt.Errorf("can't read %s's routes: %w", s.Target, err)
	}
	doc, err := Parse(read.Config)
	if err != nil {
		return "", err
	}
	i := doc.Index(s.Rule.Hostname, s.Rule.Path)
	if i < 0 {
		return "", forget()
	}
	if !SameService(doc.Rules[i].Service, s.Rule.Service) {
		if err := forget(); err != nil {
			return "", err
		}
		return s.Rule.Hostname + " was changed in Cloudflare since Dockwell added it, so it stays", nil
	}
	rt, err := p.removalRouting(ctx, t, s.Tunnel, doc, s.Rule.Service, leaving)
	if err != nil {
		return "", err
	}
	if rt.Serves(doc.Rules[i]) {
		return "", ErrServesPanel
	}
	after, err := doc.Remove(i)
	if err != nil {
		return "", err
	}
	// A name made in Cloudflare since the plan that goes through the rule keeps it
	// there, held, rather than failing the change (the review's M4).
	if moved := rt.Check(doc.Rules, after.Rules, Edit{At: i}); len(moved) > 0 {
		return "Kept " + s.Rule.Hostname + " in " + s.Target + ": " + keptWords(moved), nil
	}
	if err := CheckSplice(read.Config, after.Raw(), i, false); err != nil {
		return "", err
	}
	defer p.cache.forget("doc:" + s.Tunnel)
	wrote, err := t.tunnels.WriteTunnelConfig(ctx, s.Account, s.Tunnel, after.Raw())
	if err != nil {
		return "", fmt.Errorf("can't write %s's routes: %w", s.Target, err)
	}
	if err := p.DB.Update(ctx, func(tx *store.Tx) error {
		if err := tx.DeleteCloudflareRule(ctx, s.Tunnel, s.Rule.Hostname, s.Rule.Path); err != nil {
			return err
		}
		return tx.SawTunnel(ctx, s.Tunnel, s.Account, wrote.Version, p.now())
	}); err != nil {
		return "", err
	}
	return "", nil
}
