// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// What Dockwell wrote at Cloudflare is kept as it wrote it, found again by its route,
// its zone and its name, and written again unchanged by a step run twice (ADR 0104).
func TestCloudflareRecordsAndRules(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
		record := store.CloudflareRecord{
			ID: "rec1", Provider: "cloudflare", ZoneID: "zone1", Zone: "example.com", Name: "Wiki.example.com", Type: "CNAME",
			Content: "327d9d41-b009-413d-8494-7eeeefb3ed2a.cfargotunnel.com", Proxied: true, TTL: 1,
			Comment: "dockwell ins_1 rt_wiki", RouteID: "rt_wiki", ChangeID: "chg_1", CreatedAt: now,
			Replaced: `{"type":"A","name":"wiki.example.com","content":"203.0.113.9","proxied":false,"ttl":300,"comment":"by hand"}`,
		}
		rule := store.CloudflareRule{
			TunnelID: "327d9d41-b009-413d-8494-7eeeefb3ed2a", Hostname: "*.example.com", Account: "acc", Provider: "cloudflare",
			Zone: "example.com", Service: "http://127.0.0.1:80", ReadDoc: `{"ingress":[]}`, WrittenDoc: `{"ingress":[1]}`,
			Version: 8, ChangeID: "chg_1", CreatedAt: now,
		}
		err := db.Update(ctx, func(tx *store.Tx) error {
			for range 2 { // a step run again after a crash writes the same rows
				if err := tx.PutCloudflareRecord(ctx, record); err != nil {
					return err
				}
				if err := tx.PutCloudflareRule(ctx, rule); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		err = db.View(ctx, func(tx *store.Tx) error {
			got, err := tx.CloudflareRecordsOf(ctx, "rt_wiki")
			if err != nil || len(got) != 1 {
				t.Fatalf("by route: %v %v", got, err)
			}
			r := got[0]
			if r.Name != "wiki.example.com" || !r.Proxied || r.TTL != 1 || r.Replaced != record.Replaced || r.State != store.CloudflarePublished || !r.CreatedAt.Equal(now) || r.TakenOnAt != nil {
				t.Errorf("read back %+v", r)
			}
			if at, err := tx.CloudflareRecordAt(ctx, "zone1", "WIKI.example.com", "CNAME"); err != nil || at.ID != "rec1" {
				t.Errorf("at its name: %+v %v", at, err)
			}
			if _, err := tx.CloudflareRecordAt(ctx, "zone1", "wiki.example.com", "A"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("another type: %v", err)
			}
			rules, err := tx.CloudflareRulesOf(ctx, rule.TunnelID)
			if err != nil || len(rules) != 1 || rules[0].Version != 8 || rules[0].WrittenDoc != rule.WrittenDoc {
				t.Errorf("rules: %+v %v", rules, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// One record of a type at a name: another ID there is a conflict.
		err = db.Update(ctx, func(tx *store.Tx) error {
			other := record
			other.ID = "rec2"
			return tx.PutCloudflareRecord(ctx, other)
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Errorf("a second record at the name: %v", err)
		}
		// The drift timer marks, and removal forgets.
		later := now.Add(5 * time.Minute)
		err = db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.MarkCloudflareRecord(ctx, "rec1", store.CloudflareGone, later); err != nil {
				return err
			}
			if err := tx.MarkCloudflareRule(ctx, rule.TunnelID, "*.EXAMPLE.com", "", store.CloudflareChanged, later); err != nil {
				return err
			}
			r, err := tx.CloudflareRecordAt(ctx, "zone1", "wiki.example.com", "CNAME")
			if err != nil || r.State != store.CloudflareGone || r.CheckedAt == nil || !r.CheckedAt.Equal(later) {
				t.Errorf("marked: %+v %v", r, err)
			}
			ru, err := tx.CloudflareRuleAt(ctx, rule.TunnelID, "*.example.com", "")
			if err != nil || ru.State != store.CloudflareChanged {
				t.Errorf("rule marked: %+v %v", ru, err)
			}
			if err := tx.DeleteCloudflareRecord(ctx, "rec1"); err != nil {
				return err
			}
			if err := tx.DeleteCloudflareRule(ctx, rule.TunnelID, "*.example.com", ""); err != nil {
				return err
			}
			all, _ := tx.CloudflareRecordsAll(ctx)
			rules, _ := tx.CloudflareRulesAll(ctx)
			if len(all) != 0 || len(rules) != 0 {
				t.Errorf("forgotten: %v %v", all, rules)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

// A tunnel has one writer in Dockwell at a time: a lock another holds stays theirs
// until it runs out, and its holder may take it again (ADR 0104, N6).
func TestATunnelHasOneWriter(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
		const tunnel = "327d9d41-b009-413d-8494-7eeeefb3ed2a"
		lock := func(holder string, at time.Time) bool {
			t.Helper()
			var held bool
			if err := db.Update(ctx, func(tx *store.Tx) error {
				var err error
				held, err = tx.LockTunnel(ctx, tunnel, holder, at, at.Add(time.Minute))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return held
		}
		if !lock("chg_a", now) {
			t.Fatal("the first writer should hold it")
		}
		if lock("chg_b", now.Add(10*time.Second)) {
			t.Fatal("a second writer took a lock that hadn't run out")
		}
		if !lock("chg_a", now.Add(20*time.Second)) {
			t.Fatal("its holder should take it again")
		}
		// The lease runs out, as a control plane that stopped mid-write leaves it.
		if !lock("chg_b", now.Add(2*time.Minute)) {
			t.Fatal("a lock that ran out should go to the next writer")
		}
		if err := db.Update(ctx, func(tx *store.Tx) error { return tx.UnlockTunnel(ctx, tunnel, "chg_a") }); err != nil {
			t.Fatal(err)
		}
		if lock("chg_c", now.Add(2*time.Minute+time.Second)) {
			t.Fatal("unlocking by someone who doesn't hold it let it go")
		}
		if err := db.Update(ctx, func(tx *store.Tx) error { return tx.UnlockTunnel(ctx, tunnel, "chg_b") }); err != nil {
			t.Fatal(err)
		}
		if !lock("chg_c", now.Add(2*time.Minute+2*time.Second)) {
			t.Fatal("a lock let go should go to the next writer")
		}
		// The version seen is kept beside the lock.
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.SawTunnel(ctx, tunnel, "acc", 9, now)
		}); err != nil {
			t.Fatal(err)
		}
		_ = db.View(ctx, func(tx *store.Tx) error {
			got, err := tx.CloudflareTunnelOf(ctx, tunnel)
			if err != nil || got.SeenVersion != 9 || got.Account != "acc" {
				t.Errorf("seen: %+v %v", got, err)
			}
			return nil
		})
		if !lock("chg_c", now.Add(3*time.Minute)) {
			t.Fatal("recording the version shouldn't touch the lock")
		}
	})
}
