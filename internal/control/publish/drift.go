// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"context"
	"encoding/json"
	"log/slog"
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
)

// Actions are the actions this package writes on Activity.
var Actions = []string{ActionWrite, ActionGone}

// told is what an event of this package keeps: never a token, nor a record's
// content.
type told struct {
	Provider string `json:"provider"`
	Target   string `json:"target"`
	Name     string `json:"name"`
	Action   int    `json:"action"`
	Drift    bool   `json:"drift,omitempty"`
	Version  int    `json:"version,omitempty"`
}

// ReadEvent is an event of this package as the API says it: only this package writes
// these actions, so what's read is what it wrote.
func ReadEvent(e *store.Event) (*dockwellv1.EventCloudflare, bool) {
	if e.Action != ActionWrite && e.Action != ActionGone {
		return nil, false
	}
	var t told
	if err := json.Unmarshal([]byte(e.Detail), &t); err != nil {
		return nil, false
	}
	return &dockwellv1.EventCloudflare{
		Provider: t.Provider, Target: t.Target, Name: t.Name, Action: dockwellv1.ProviderAction(t.Action), //nolint:gosec // a small enum
		Drift: t.Drift, Version: int32(min(t.Version, 1<<30)), //nolint:gosec // bounded
	}, true
}

// note puts one thing at Cloudflare on Activity, for a route by its ID where there's
// one.
func (p *Publisher) note(ctx context.Context, action, subject string, t told) {
	detail, err := json.Marshal(t)
	if err != nil {
		return
	}
	_ = p.DB.Update(context.WithoutCancel(ctx), func(tx *store.Tx) error {
		return tx.RecordEvent(ctx, store.Event{ID: id.New(id.Event), At: p.now(), Actor: "dockwell", Action: action, Subject: subject, Detail: string(detail)})
	})
}

// noteWrite puts a step's write on Activity.
func (p *Publisher) noteWrite(ctx context.Context, routeID string, s resource.ProviderStep, version int) {
	p.note(ctx, ActionWrite, routeID, told{Provider: s.Provider, Target: s.Target, Name: s.Name, Action: int(s.Action), Version: version})
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
	byTunnel := map[string][]store.CloudflareRule{}
	for i := range rules {
		byTunnel[rules[i].TunnelID] = append(byTunnel[rules[i].TunnelID], rules[i])
	}
	for tunnel, held := range byTunnel {
		if err := p.driftTunnel(ctx, tunnel, held); err != nil {
			return err
		}
	}
	byZone := map[string][]store.CloudflareRecord{}
	for i := range records {
		if records[i].Type != "pending" {
			byZone[records[i].ZoneID] = append(byZone[records[i].ZoneID], records[i])
		}
	}
	for _, held := range byZone {
		if err := p.driftZone(ctx, held); err != nil {
			return err
		}
	}
	return nil
}

func (p *Publisher) driftTunnel(ctx context.Context, tunnel string, held []store.CloudflareRule) error {
	first := &held[0]
	t, err := p.target(ctx, first.Provider, strings.TrimPrefix(first.Hostname, "*."))
	if err != nil {
		return err
	}
	cfg, err := t.tunnels.ReadTunnelConfig(ctx, first.Account, tunnel)
	if err != nil {
		return err
	}
	var seen store.CloudflareTunnel
	_ = p.DB.View(ctx, func(tx *store.Tx) error {
		seen, _ = tx.CloudflareTunnelOf(ctx, tunnel)
		return nil
	})
	unchanged := seen.SeenVersion == cfg.Version
	doc, err := Parse(cfg.Config)
	if err != nil {
		return err
	}
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
			state := store.CloudflarePublished
			switch at := doc.Index(r.Hostname, r.Path); {
			case at < 0:
				state = store.CloudflareGone
			case !SameService(doc.Rules[at].Service, r.Service):
				state = store.CloudflareChanged
			}
			if unchanged && state == r.State {
				continue
			}
			if state != store.CloudflarePublished && r.State == store.CloudflarePublished {
				gone = append(gone, told{Provider: r.Provider, Target: r.Zone, Name: r.Hostname, Action: int(resource.ProviderAddRule), Drift: true, Version: cfg.Version})
			}
			if err := tx.MarkCloudflareRule(ctx, r.TunnelID, r.Hostname, r.Path, state, now); err != nil {
				return err
			}
		}
		return tx.SawTunnel(ctx, tunnel, first.Account, cfg.Version, now)
	})
}

func (p *Publisher) driftZone(ctx context.Context, held []store.CloudflareRecord) error {
	first := &held[0]
	t, err := p.target(ctx, first.Provider, first.Name)
	if err != nil {
		return err
	}
	there, err := t.cf.RecordsCommented(ctx, t.zone, "dockwell "+p.Install)
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
