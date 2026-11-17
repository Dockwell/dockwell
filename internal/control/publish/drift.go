// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// What Activity keeps of Dockwell at Cloudflare: each write a change made there, and
// each thing the drift timer found gone (ADR 0104).
const (
	ActionWrite = "cloudflare.write"
	ActionGone  = "cloudflare.gone"
	// ActionReplaced is an edit made in Cloudflare that a write of Dockwell's
	// replaced, as it landed between Dockwell's read and its write: whoever made it
	// isn't watching the change, so it raises an alert (ADR 0104, section 4).
	ActionReplaced = "cloudflare.replaced"
)

// Actions are the actions this package writes on Activity.
var Actions = []string{ActionWrite, ActionGone, ActionReplaced}

// told is what an event of this package keeps: never a token, nor a record's
// content.
type told struct {
	Provider string `json:"provider"`
	Target   string `json:"target"`
	Name     string `json:"name"`
	Action   int    `json:"action"`
	Drift    bool   `json:"drift,omitempty"`
	Version  int    `json:"version,omitempty"`
	// ReadVersion is the version read before a write that replaced an edit.
	ReadVersion int `json:"read_version,omitempty"`
	// ChangeID is the change that wrote it; empty for the drift timer's.
	ChangeID string `json:"change_id,omitempty"`
}

// ReadEvent is an event of this package as the API says it: only this package writes
// these actions, so what's read is what it wrote.
func ReadEvent(e *store.Event) (*dockwellv1.EventCloudflare, bool) {
	if !slices.Contains(Actions, e.Action) {
		return nil, false
	}
	var t told
	if err := json.Unmarshal([]byte(e.Detail), &t); err != nil {
		return nil, false
	}
	return &dockwellv1.EventCloudflare{
		Provider: t.Provider, Target: t.Target, Name: t.Name, Action: dockwellv1.ProviderAction(t.Action), //nolint:gosec // a small enum
		Drift: t.Drift, Version: int32(min(t.Version, 1<<30)), ReadVersion: int32(min(t.ReadVersion, 1<<30)), //nolint:gosec // bounded
		ChangeId: t.ChangeID,
	}, true
}

// note puts one thing at Cloudflare on Activity, for a route by its ID where there's
// one. Its actor is the person whose change wrote it, where t names one, and Dockwell
// for what the drift timer found (the review's M9).
func (p *Publisher) note(ctx context.Context, action, subject string, t told) {
	detail, err := json.Marshal(t)
	if err != nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	_ = p.DB.Update(ctx, func(tx *store.Tx) error {
		actor := "dockwell"
		if t.ChangeID != "" {
			if c, err := tx.GetChange(ctx, t.ChangeID); err == nil {
				actor = cmp.Or(c.AppliedBy, c.ActsFor, c.Actor, actor)
			}
		}
		return tx.RecordEvent(ctx, store.Event{ID: id.New(id.Event), At: p.now(), Actor: actor, Action: action, Subject: subject, Detail: string(detail)})
	})
}

// noteWrite puts a step's write on Activity, by what it did. A rule kept writes
// nothing, so it isn't one.
func (p *Publisher) noteWrite(ctx context.Context, changeID, routeID string, s resource.ProviderStep, version int) {
	if s.Action == resource.ProviderKeepRule {
		return
	}
	p.note(ctx, ActionWrite, routeID, told{Provider: s.Provider, Target: s.Target, Name: s.Name, Action: int(s.Action), Version: version, ChangeID: changeID})
}

// DriftEvery is how often the drift timer reads what Dockwell published.
const DriftEvery = 5 * time.Minute

// Watch reads what Dockwell published at Cloudflare every DriftEvery, until ctx ends.
func (p *Publisher) Watch(ctx context.Context, logger *slog.Logger) {
	t := time.NewTicker(DriftEvery)
	defer t.Stop()
	for {
		if err := p.Drift(ctx); err != nil && ctx.Err() == nil && logger != nil {
			logger.WarnContext(ctx, "couldn't read what Dockwell published at Cloudflare", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Drift reads, once, each tunnel Dockwell wrote a rule in, where its version has
// moved, and each zone's records Dockwell wrote, in one listing by their comment, and
// marks what it finds: still published, gone, or changed. Nothing is fixed: a domain
// gone says so, with Put it back as a plan (ADR 0104, section 4).
func (p *Publisher) Drift(ctx context.Context) error {
	var rules []store.CloudflareRule
	var records []store.CloudflareRecord
	if err := p.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		if rules, err = tx.CloudflareRulesAll(ctx); err != nil {
			return err
		}
		records, err = tx.CloudflareRecordsAll(ctx)
		return err
	}); err != nil {
		return err
	}
	// Each tunnel and zone is read on its own: one that fails says so and the rest
	// are read. Once Cloudflare says too many requests, the tick ends, leaving the
	// token's rate to renewals and plans (the review's M6).
	var errs []error
	tooMany := func(err error) bool {
		var api *dnsproviders.APIError
		if errors.As(err, &api) && api.Status == http.StatusTooManyRequests {
			_ = p.cache.slowOn(err, p.now())
			return true
		}
		return false
	}
	byTunnel := map[string][]store.CloudflareRule{}
	for i := range rules {
		byTunnel[rules[i].TunnelID] = append(byTunnel[rules[i].TunnelID], rules[i])
	}
	for tunnel, held := range byTunnel {
		if err := p.driftTunnel(ctx, tunnel, held); err != nil {
			if tooMany(err) {
				return err
			}
			errs = append(errs, err)
		}
	}
	byZone := map[string][]store.CloudflareRecord{}
	for i := range records {
		if records[i].Type != store.CloudflarePendingType {
			byZone[records[i].ZoneID] = append(byZone[records[i].ZoneID], records[i])
		}
	}
	for _, held := range byZone {
		if err := p.driftZone(ctx, held); err != nil {
			if tooMany(err) {
				return err
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *Publisher) driftTunnel(ctx context.Context, tunnel string, held []store.CloudflareRule) error {
	first := &held[0]
	all := func(state string) func(*store.CloudflareRule) string {
		return func(*store.CloudflareRule) string { return state }
	}
	t, err := p.target(ctx, first.Provider, strings.TrimPrefix(first.Hostname, "*."))
	if errors.As(err, new(Unreadable)) {
		return p.markRules(ctx, tunnel, held, all(store.CloudflareUnreadable), -1)
	}
	if err != nil {
		return err
	}
	cfg, err := t.tunnels.ReadTunnelConfig(ctx, first.Account, tunnel)
	switch {
	case dnsproviders.TunnelGone(err):
		return p.markRules(ctx, tunnel, held, all(store.CloudflareTunnelDeleted), -1)
	case refused(err):
		return p.markRules(ctx, tunnel, held, all(store.CloudflareUnreadable), -1)
	case err != nil:
		return err
	}
	doc, err := Parse(cfg.Config)
	if err != nil {
		return err
	}
	return p.markRules(ctx, tunnel, held, func(r *store.CloudflareRule) string {
		switch at := doc.Index(r.Hostname, r.Path); {
		case at < 0:
			return store.CloudflareGone
		case !SameService(doc.Rules[at].Service, r.Service):
			return store.CloudflareChanged
		}
		return store.CloudflarePublished
	}, cfg.Version)
}

// markRules marks each of a tunnel's rules with what stateOf finds, putting each one
// newly not published on Activity. version is the document's, as read; -1 where it
// couldn't be read.
func (p *Publisher) markRules(ctx context.Context, tunnel string, held []store.CloudflareRule, stateOf func(*store.CloudflareRule) string, version int) error {
	first := &held[0]
	var seen store.CloudflareTunnel
	_ = p.DB.View(ctx, func(tx *store.Tx) error {
		seen, _ = tx.CloudflareTunnelOf(ctx, tunnel)
		return nil
	})
	unchanged := version >= 0 && seen.SeenVersion == version
	now := p.now()
	var gone []told
	defer func() {
		for _, g := range gone {
			p.note(ctx, ActionGone, tunnel, g)
		}
	}()
	return p.DB.Update(ctx, func(tx *store.Tx) error {
		gone = nil
		for i := range held {
			r := &held[i]
			state := stateOf(r)
			if unchanged && state == r.State || state == r.State && version < 0 {
				continue
			}
			if state != store.CloudflarePublished && r.State == store.CloudflarePublished {
				gone = append(gone, told{Provider: r.Provider, Target: r.Zone, Name: r.Hostname, Action: int(resource.ProviderAddRule), Drift: true, Version: max(version, 0)})
			}
			if err := tx.MarkCloudflareRule(ctx, r.TunnelID, r.Hostname, r.Path, state, now); err != nil {
				return err
			}
		}
		if version < 0 {
			return nil
		}
		return tx.SawTunnel(ctx, tunnel, first.Account, version, now)
	})
}

func (p *Publisher) driftZone(ctx context.Context, held []store.CloudflareRecord) error {
	first := &held[0]
	t, err := p.target(ctx, first.Provider, first.Name)
	if errors.As(err, new(Unreadable)) {
		return p.markRecords(ctx, held, nil, true)
	}
	if err != nil {
		return err
	}
	there, err := t.cf.RecordsCommented(ctx, t.zone, "dockwell "+p.Install)
	if refused(err) {
		return p.markRecords(ctx, held, nil, true)
	}
	if err != nil {
		return err
	}
	byID := map[string]dnsproviders.Record{}
	for i := range there {
		byID[there[i].ID] = there[i]
	}
	// A record taken on keeps the comment it was made with, so it's read by its ID.
	for i := range held {
		if held[i].TakenOnAt == nil {
			continue
		}
		if r, err := t.cf.RecordByID(ctx, t.zone, held[i].ID); err == nil {
			byID[r.ID] = r
		}
	}
	return p.markRecords(ctx, held, byID, false)
}

// markRecords marks each of a zone's records with what Cloudflare holds by its ID,
// or, for unreadable, as Dockwell can't read them, putting each one newly not
// published on Activity.
func (p *Publisher) markRecords(ctx context.Context, held []store.CloudflareRecord, byID map[string]dnsproviders.Record, unreadable bool) error {
	now := p.now()
	type goneRecord struct {
		route string
		t     told
	}
	var gone []goneRecord
	defer func() {
		for _, g := range gone {
			p.note(ctx, ActionGone, g.route, g.t)
		}
	}()
	return p.DB.Update(ctx, func(tx *store.Tx) error {
		gone = nil
		for i := range held {
			r := &held[i]
			state := store.CloudflarePublished
			got, ok := byID[r.ID]
			switch {
			case unreadable:
				state = store.CloudflareUnreadable
			case !ok:
				state = store.CloudflareGone
			case got.Type != r.Type || got.Content != r.Content || got.Proxied != r.Proxied:
				state = store.CloudflareChanged
			}
			if state == r.State {
				continue
			}
			if state != store.CloudflarePublished && r.State == store.CloudflarePublished {
				gone = append(gone, goneRecord{route: r.RouteID, t: told{Provider: r.Provider, Target: r.Zone, Name: r.Name, Action: int(resource.ProviderCreateRecord), Drift: true}})
			}
			if err := tx.MarkCloudflareRecord(ctx, r.ID, state, now); err != nil {
				return err
			}
		}
		return nil
	})
}
