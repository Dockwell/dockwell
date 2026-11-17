// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A store at 0046 with records and a kept copy in it upgrades to 0047, keeps them,
// and then takes a second kept copy at the same name, while records written at
// Cloudflare stay one per name and type, refused in words that say what to do.
func TestUpgradeFrom46KeepsRecordsAndTakesTwoCopiesAtOneName(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		ctx := t.Context()
		location := storetest.New(t, backend)
		old, err := store.OpenAt(ctx, location, 46)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Unix(1_794_000_000, 0)
		row := func(id, typ, content string) store.CloudflareRecord {
			return store.CloudflareRecord{
				ID: id, Provider: "cloudflare", ZoneID: "zone-example-com", Zone: "example.com", Name: "vpn.example.com", Type: typ, Content: content,
				TTL: 1, Comment: "dockwell ins_1 rte_1", RouteID: "rte_1", ChangeID: "chg_1", CreatedAt: at,
			}
		}
		written := row("rec-1", "CNAME", "tunnel.cfargotunnel.com")
		kept := row("pending:rec-hand-a", store.CloudflarePendingType, "203.0.113.9")
		kept.Replaced = `{"id":"rec-hand-a","type":"A","name":"vpn.example.com","content":"203.0.113.9"}`
		if err := old.Update(ctx, func(tx *store.Tx) error {
			if err := tx.PutCloudflareRecord(ctx, written); err != nil {
				return err
			}
			return tx.PutCloudflareRecord(ctx, kept)
		}); err != nil {
			t.Fatal(err)
		}
		if err := old.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(ctx, location)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		second := row("pending:rec-hand-aaaa", store.CloudflarePendingType, "2001:db8::9")
		second.Replaced = `{"id":"rec-hand-aaaa","type":"AAAA","name":"vpn.example.com","content":"2001:db8::9"}`
		if err := db.Update(ctx, func(tx *store.Tx) error { return tx.PutCloudflareRecord(ctx, second) }); err != nil {
			t.Fatalf("a second kept copy at the name: %v", err)
		}
		if err := db.View(ctx, func(tx *store.Tx) error {
			pending, err := tx.CloudflarePendingOf(ctx, "rte_1")
			if err != nil {
				return err
			}
			if len(pending) != 2 {
				t.Errorf("kept copies after the upgrade: %+v", pending)
			}
			written, err := tx.CloudflareRecordsOf(ctx, "rte_1")
			if err != nil {
				return err
			}
			if len(written) != 1 || written[0].ID != "rec-1" {
				t.Errorf("records after the upgrade: %+v", written)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		twin := row("rec-2", "CNAME", "tunnel.cfargotunnel.com")
		err = db.Update(ctx, func(tx *store.Tx) error { return tx.PutCloudflareRecord(ctx, twin) })
		if !errors.Is(err, store.ErrConflict) || err.Error() != "a CNAME record at vpn.example.com is kept already under another ID, so this one can't be kept beside it, and the change stops here. Plan it again, and if it stops the same way, report it: already exists" {
			t.Fatalf("a second written record of a type at a name: %v", err)
		}
	})
}
