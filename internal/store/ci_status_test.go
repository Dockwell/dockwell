// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Outbox entries are written in the same transaction as the change state, carry
// monotonic transition numbers, propagate remote status IDs, enforce retryable
// states, refresh token repository bindings on requeue, and never appear for
// non-CI changes. Tested on both SQLite and Postgres.
func TestCIStatusOutboxLifecycle(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		db, _ := storetest.Open(t, backend)
		ctx := t.Context()
		now := time.Now().UTC().Truncate(time.Millisecond)
		ciChangeID := id.New(id.Change)
		plainChangeID := id.New(id.Change)
		supersededChangeID := id.New(id.Change)
		serviceID := id.New(id.Service)
		userID := id.New(id.User)
		tokID := id.New(id.Token)

		if err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.CreateUser(ctx, store.User{ID: userID, Name: "Mira", Role: store.RoleOwner, WebAuthnHandle: []byte("mira"), CreatedAt: now}); err != nil {
				return err
			}
			return tx.CreateAPIToken(ctx, store.APIToken{
				ID: tokID, TokenHash: []byte("hash-deploy-1"), UserID: userID, Name: "CI deploy",
				Hint: "dwt_aaaa…zzzz", Scope: store.ScopeReadWrite, CreatedAt: now,
				Projects: []string{"prj_1"}, Role: store.RoleDeveloper,
				DeployService: serviceID, DeployEnvironment: "env_1", DeployRepository: "",
			})
		}); err != nil {
			t.Fatal(err)
		}

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
			if err := tx.CreateChange(ctx, store.Change{
				ID: supersededChangeID, Summary: "Old planned deploy", Actor: "tok_1", State: store.ChangePlanned,
				CreatedAt: now, Plan: []byte(`{}`), Fingerprint: "fp3",
			}); err != nil {
				return err
			}
			if err := tx.RecordCIDeploy(ctx, store.CIDeployOrigin{
				CIDeployKey: store.CIDeployKey{
					Service: serviceID,
					Digest:  "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
					Commit:  "1111111111111111111111111111111111111111",
				},
				Provider:   1,
				Repository: "https://github.com/acme/web",
				Ref:        "refs/heads/main",
				RunURL:     "https://github.com/acme/web/actions/runs/0",
				Image:      "ghcr.io/acme/web:0.9",
			}, supersededChangeID); err != nil {
				return err
			}
			if err := tx.RecordCIDeploy(ctx, store.CIDeployOrigin{
				CIDeployKey: store.CIDeployKey{
					Service: serviceID,
					Digest:  "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					Commit:  "0123456789abcdef0123456789abcdef01234567",
				},
				Provider:   1,
				Repository: "https://github.com/acme/web",
				Ref:        "refs/heads/main",
				RunURL:     "https://github.com/acme/web/actions/runs/1",
				Image:      "ghcr.io/acme/web:1.0",
			}, ciChangeID); err != nil {
				return err
			}
			ok, err := tx.EnqueueCIStatusForChange(ctx, ciChangeID, store.CIStatusQueued, store.CIStatusEnqueueOpts{
				ServiceName:       "web",
				EnvironmentName:   "production",
				TokenID:           tokID,
				AllowedRepository: "",
				ChangeURL:         "https://panel.example.com/changes/" + ciChangeID,
				Summary:           "Deploy web",
			}, now)
			if err != nil || !ok {
				t.Fatalf("EnqueueCIStatusForChange(ci) = %v, %v", ok, err)
			}
			okPlain, err := tx.EnqueueCIStatusForChange(ctx, plainChangeID, store.CIStatusQueued, store.CIStatusEnqueueOpts{
				ServiceName: "web",
				Summary:     "Manual",
			}, now)
			if err != nil || okPlain {
				t.Fatalf("EnqueueCIStatusForChange(plain) = %v, %v; want false, nil", okPlain, err)
			}
			ok2, err := tx.EnqueueCIStatusForChange(ctx, ciChangeID, store.CIStatusApplying, store.CIStatusEnqueueOpts{
				Summary: "Deploy web",
			}, now.Add(time.Second))
			if err != nil || !ok2 {
				t.Fatalf("EnqueueCIStatusForChange(applying) = %v, %v", ok2, err)
			}
			sup, err := tx.CIStatusEnqueueDiscardedForPlanned(ctx, []string{serviceID}, ciChangeID, "Superseded by "+ciChangeID, func(id string) string {
				return "https://panel.example.com/changes/" + id
			}, now.Add(time.Second))
			if err != nil || !sup {
				t.Fatalf("CIStatusEnqueueDiscardedForPlanned = %v, %v", sup, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		// Non-CI change must have no entries; superseded planned change gets discarded.
		var plainEntries, all, supEntries []store.CIStatusEntry
		var due []store.CIStatusDue
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
			supEntries, err = tx.CIStatusAllForChange(ctx, supersededChangeID)
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
		if len(supEntries) != 1 || supEntries[0].State != store.CIStatusDiscarded {
			t.Fatalf("superseded planned change entries = %+v", supEntries)
		}
		if len(all) != 2 || all[0].Transition != 1 || all[1].Transition != 2 || all[0].State != store.CIStatusQueued || all[1].State != store.CIStatusApplying {
			t.Fatalf("unexpected transitions: %+v", all)
		}
		if all[1].ServiceName != "web" || all[1].EnvironmentName != "production" || all[1].TokenID != tokID || all[1].ChangeURL != "https://panel.example.com/changes/"+ciChangeID {
			t.Fatalf("transition 2 did not inherit metadata: %+v", all[1])
		}
		if !all[0].RetrySince.IsZero() {
			t.Fatalf("retry_since should be zero before first attempt, got %v", all[0].RetrySince)
		}
		if len(due) != 2 {
			t.Fatalf("CIStatusDueChanges = %+v, want 2 changes", due)
		}

		// Requeueing a pending entry is refused with ErrCIStatusNotRetryable (L6).
		if err := db.Update(ctx, func(tx *store.Tx) error {
			_, err := tx.CIStatusRequeueLatest(ctx, ciChangeID, "", now.Add(2*time.Second))
			return err
		}); !errors.Is(err, store.ErrCIStatusNotRetryable) {
			t.Fatalf("CIStatusRequeueLatest on pending = %v, want ErrCIStatusNotRetryable", err)
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

		// Mark retry sets retry_since on the first attempt and preserves it on subsequent retries.
		firstAttemptAt := now.Add(3 * time.Second)
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.CIStatusMarkRetry(ctx, pending[0].ID, "github_app:gha_1/7", store.CIStatusReasonGitHubNotReached, "503", firstAttemptAt.Add(5*time.Second), firstAttemptAt)
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.CIStatusMarkRetry(ctx, pending[0].ID, "github_app:gha_1/7", store.CIStatusReasonGitHubNotReached, "503", firstAttemptAt.Add(15*time.Second), firstAttemptAt.Add(6*time.Second))
		}); err != nil {
			t.Fatal(err)
		}
		var afterRetry map[string]store.CIStatusEntry
		_ = db.View(ctx, func(tx *store.Tx) error {
			var err error
			afterRetry, err = tx.CIStatusLatest(ctx, []string{ciChangeID})
			return err
		})
		if !afterRetry[ciChangeID].RetrySince.Equal(firstAttemptAt) {
			t.Fatalf("RetrySince = %v, want %v", afterRetry[ciChangeID].RetrySince, firstAttemptAt)
		}

		// Bind repository on the deploy token, fail transition 2, then requeue it:
		// requeue refreshes allowed_repository from api_tokens and resets retry_since.
		if err := db.Update(ctx, func(tx *store.Tx) error {
			if _, err := tx.SetAPITokenDeployRepository(ctx, userID, tokID, "https://github.com/acme/web", now.Add(4*time.Second)); err != nil {
				return err
			}
			return tx.CIStatusMarkFinal(ctx, pending[0].ID, store.CIStatusDeliveryFailed, "github_app:gha_1/7", store.CIStatusReasonNoTokenRepository, "boom", true, now.Add(4*time.Second))
		}); err != nil {
			t.Fatal(err)
		}
		var requeued store.CIStatusEntry
		if err := db.Update(ctx, func(tx *store.Tx) error {
			var err error
			requeued, err = tx.CIStatusRequeueLatest(ctx, ciChangeID, "", now.Add(5*time.Second))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if requeued.DeliveryState != store.CIStatusDeliveryPending || requeued.Attempts != 0 || requeued.LastError != "" ||
			requeued.ReasonCode != "" || requeued.AllowedRepository != "https://github.com/acme/web" || !requeued.RetrySince.IsZero() {
			t.Fatalf("requeued = %+v", requeued)
		}

		// Delivering transition 2 makes requeue return ErrCIStatusNotRetryable.
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.CIStatusMarkDelivered(ctx, requeued.ID, ciChangeID, requeued.Transition, "901", "https://github.com/acme/web/runs/901", "github_app:gha_1/7", now.Add(6*time.Second))
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.Update(ctx, func(tx *store.Tx) error {
			_, err := tx.CIStatusRequeueLatest(ctx, ciChangeID, "", now.Add(7*time.Second))
			return err
		}); !errors.Is(err, store.ErrCIStatusNotRetryable) {
			t.Fatalf("CIStatusRequeueLatest on delivered = %v, want ErrCIStatusNotRetryable", err)
		}
	})
}

func TestNormalizeDeployRepository(t *testing.T) {
	t.Parallel()
	got, err := store.NormalizeDeployRepository("https://GitHub.com/Acme/Web.git/")
	if err != nil || got != "https://github.com/Acme/Web" {
		t.Fatalf("NormalizeDeployRepository = %q, %v", got, err)
	}
	if !store.SameDeployRepository("https://github.com/acme/web", "https://GitHub.com/Acme/Web.git") {
		t.Fatal("SameDeployRepository should match canonical forms")
	}
	for _, bad := range []string{
		"http://github.com/acme/web",
		"https://user:pass@github.com/acme/web",
		"https://github.com/acme/web?foo=1",
		"https://github.com/acme/web#frag",
		"https://github.com/acme",
		"https://github.com/acme/../other",
		"https://github.com/acme/./web",
		"https://github.com/acme//web",
	} {
		if _, err := store.NormalizeDeployRepository(bad); err == nil {
			t.Errorf("NormalizeDeployRepository(%q) succeeded, want error", bad)
		}
	}
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
