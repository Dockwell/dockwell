// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Outbox entries are written in the same transaction as the change state, carry
// monotonic transition numbers, propagate remote status IDs, and never appear
// for non-CI changes. Tested on both SQLite and Postgres.
func TestCIStatusOutboxLifecycle(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		db, _ := storetest.Open(t, backend)
		ctx := t.Context()
		now := time.Now().UTC().Truncate(time.Millisecond)
		ciChangeID := id.New(id.Change)
		plainChangeID := id.New(id.Change)
		serviceID := id.New(id.Service)

		err := db.Revise(ctx, func(tx *store.RevisionTx) error {
			if err := tx.CreateChange(ctx, store.Change{
				ID: ciChangeID, Summary: "Deploy web", Actor: "tok_1", State: store.ChangePlanned,
				CreatedAt: now, Plan: []byte(`{}`), Fingerprint: "fp1",
			}); err != nil {
				return err
			}
			if err := tx.CreateChange(ctx, store.Change{
				ID: plainChangeID, Summary: "Manual change", Actor: "usr_1", State: store.ChangePlanned,
				CreatedAt: now, Plan: []byte(`{}`), Fingerprint: "fp2",
			}); err != nil {
				return err
			}
			if err := tx.RecordCIDeploy(ctx, store.CIDeployOrigin{
				Service:    serviceID,
				Provider:   1,
				Repository: "https://github.com/acme/web",
				Commit:     "0123456789abcdef0123456789abcdef01234567",
				Ref:        "refs/heads/main",
				RunURL:     "https://github.com/acme/web/actions/runs/1",
				Image:      "ghcr.io/acme/web:1.0",
				Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}, ciChangeID); err != nil {
				return err
			}
			ok, err := tx.EnqueueCIStatusForChange(ctx, ciChangeID, store.CIStatusQueued, "web", "https://panel.example.com/changes/"+ciChangeID, "Deploy web", now)
			if err != nil || !ok {
				t.Fatalf("EnqueueCIStatusForChange(ci) = %v, %v", ok, err)
			}
			okPlain, err := tx.EnqueueCIStatusForChange(ctx, plainChangeID, store.CIStatusQueued, "web", "", "Manual", now)
			if err != nil || okPlain {
				t.Fatalf("EnqueueCIStatusForChange(plain) = %v, %v; want false, nil", okPlain, err)
			}
			ok2, err := tx.EnqueueCIStatusForChange(ctx, ciChangeID, store.CIStatusApplying, "", "", "Deploy web", now.Add(time.Second))
			if err != nil || !ok2 {
				t.Fatalf("EnqueueCIStatusForChange(applying) = %v, %v", ok2, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		// Non-CI change must have no entries.
		var plainEntries []store.CIStatusEntry
		var all []store.CIStatusEntry
		var due []string
		err = db.View(ctx, func(tx *store.Tx) error {
			var err error
			plainEntries, err = tx.CIStatusAllForChange(ctx, plainChangeID)
			if err != nil {
				return err
			}
			all, err = tx.CIStatusAllForChange(ctx, ciChangeID)
			if err != nil {
				return err
			}
			due, err = tx.CIStatusDueChanges(ctx, now.Add(2*time.Second), 10)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(plainEntries) != 0 {
			t.Fatalf("non-CI change got %d outbox entries", len(plainEntries))
		}
		if len(all) != 2 || all[0].Transition != 1 || all[1].Transition != 2 || all[0].State != store.CIStatusQueued || all[1].State != store.CIStatusApplying {
			t.Fatalf("unexpected transitions: %+v", all)
		}
		if all[1].ServiceName != "web" || all[1].ChangeURL != "https://panel.example.com/changes/"+ciChangeID {
			t.Fatalf("transition 2 did not inherit service_name/change_url: %+v", all[1])
		}
		if len(due) != 1 || due[0] != ciChangeID {
			t.Fatalf("CIStatusDueChanges = %v, want [%s]", due, ciChangeID)
		}

		// Delivering transition 1 propagates remote_id/remote_url/credential_ref to transition 2.
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.CIStatusMarkDelivered(ctx, all[0].ID, ciChangeID, all[0].Transition, "901", "https://github.com/acme/web/runs/901", "github_app:gha_1/7", now.Add(2*time.Second))
		}); err != nil {
			t.Fatal(err)
		}

		var pending []store.CIStatusEntry
		var maxDelivered int
		err = db.View(ctx, func(tx *store.Tx) error {
			var err error
			pending, maxDelivered, err = tx.CIStatusPendingForChange(ctx, ciChangeID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if maxDelivered != 1 || len(pending) != 1 || pending[0].RemoteID != "901" || pending[0].CredentialRef != "github_app:gha_1/7" {
			t.Fatalf("after delivering 1: maxDelivered=%d pending=%+v", maxDelivered, pending)
		}

		// Fail transition 2, then requeue it.
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.CIStatusMarkFinal(ctx, pending[0].ID, store.CIStatusDeliveryFailed, "github_app:gha_1/7", "boom", true, now.Add(3*time.Second))
		}); err != nil {
			t.Fatal(err)
		}
		var requeued store.CIStatusEntry
		if err := db.Update(ctx, func(tx *store.Tx) error {
			var err error
			requeued, err = tx.CIStatusRequeueLatest(ctx, ciChangeID, "", now.Add(4*time.Second))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if requeued.DeliveryState != store.CIStatusDeliveryPending || requeued.Attempts != 0 || requeued.LastError != "" {
			t.Fatalf("requeued = %+v", requeued)
		}
	})
}

func TestGitHubInstallationPermissionsRoundTrip(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		db, _ := storetest.Open(t, backend)
		ctx := t.Context()
		now := time.Now().UTC().Truncate(time.Millisecond)
		in := store.GitHubInstallation{
			ID:           42,
			AppID:        "gha_test",
			Account:      "acme",
			Organization: true,
			Selection:    "selected",
			Permissions:  map[string]string{"contents": "read", "metadata": "read", "checks": "write"},
			UpdatedAt:    now,
		}
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.SetGitHubInstallation(ctx, in)
		}); err != nil {
			t.Fatal(err)
		}
		var got store.GitHubInstallation
		if err := db.View(ctx, func(tx *store.Tx) error {
			var err error
			got, err = tx.GetGitHubInstallation(ctx, 42)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got.Permissions["checks"] != "write" || got.Permissions["contents"] != "read" {
			t.Fatalf("permissions = %+v", got.Permissions)
		}
	})
}
