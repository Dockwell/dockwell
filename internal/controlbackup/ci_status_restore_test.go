// SPDX-License-Identifier: AGPL-3.0-only

package controlbackup_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/controlbackup"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A control-plane restore marks pending CI deploy status outbox entries as
// superseded so a restored install does not replay stale statuses to GitHub
// or GitLab, while keeping already-delivered entries intact (L7).
func TestExtractSupersedesPendingCIStatuses(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, _ := storetest.Open(t, store.SQLite)
	now := time.Now().UTC().Truncate(time.Millisecond)
	changeID := id.New(id.Change)
	serviceID := id.New(id.Service)

	if err := db.Revise(ctx, func(tx *store.RevisionTx) error {
		if err := tx.CreateChange(ctx, store.Change{
			ID: changeID, Summary: "Deploy web", Actor: "tok_1", State: store.ChangeApplying,
			CreatedAt: now, Plan: []byte(`{}`), Fingerprint: "fp1",
		}); err != nil {
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
			Image:      "ghcr.io/acme/web:1.0",
		}, changeID); err != nil {
			return err
		}
		if _, err := tx.EnqueueCIStatusForChange(ctx, changeID, store.CIStatusQueued, store.CIStatusEnqueueOpts{
			ServiceName: "web", EnvironmentName: "production", AllowedRepository: "https://github.com/acme/web",
		}, now); err != nil {
			return err
		}
		_, err := tx.EnqueueCIStatusForChange(ctx, changeID, store.CIStatusApplying, store.CIStatusEnqueueOpts{}, now.Add(time.Second))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Mark transition 1 delivered; leave transition 2 pending at backup time.
	var entries []store.CIStatusEntry
	if err := db.View(ctx, func(tx *store.Tx) error {
		var err error
		entries, err = tx.CIStatusAllForChange(ctx, changeID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(ctx, func(tx *store.Tx) error {
		return tx.CIStatusMarkDelivered(ctx, entries[0].ID, changeID, entries[0].Transition, "700", "https://github.com/acme/web/runs/700", "github_app:gha_1/7", now)
	}); err != nil {
		t.Fatal(err)
	}

	// Snapshot, seal a bundle, and extract it into a fresh directory.
	phrase := controlbackup.GeneratePhrase()
	rec := controlbackup.NewRecovery(phrase, now)
	if err := db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.PutSetting(ctx, "install.id", []byte(id.New("ins"))); err != nil {
			return err
		}
		return tx.InitControlBackup(ctx, id.New("gen"), rec.ID, []byte("sealed"))
	}); err != nil {
		t.Fatal(err)
	}
	snapPath := filepath.Join(t.TempDir(), "snap.db")
	snap, err := db.SnapshotControl(ctx, snapPath)
	if err != nil {
		t.Fatal(err)
	}
	snapBytes, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	installKey := bytes.Repeat([]byte{0x42}, 32)
	var bundle bytes.Buffer
	header := controlbackup.Header{
		Install:      snap.Install,
		Bundle:       id.New("cbk"),
		Generation:   snap.Backup.Generation,
		GenerationID: snap.Backup.GenerationID,
		Schema:       snap.Schema,
		Version:      "v1.0.0",
		Taken:        now,
	}
	if err := controlbackup.SealBundle(&bundle, bytes.NewReader(snapBytes), int64(len(snapBytes)), installKey, header, rec); err != nil {
		t.Fatal(err)
	}

	restoredDir := filepath.Join(t.TempDir(), "restored")
	if _, err := controlbackup.Extract(ctx, bytes.NewReader(bundle.Bytes()), phrase, restoredDir); err != nil {
		t.Fatal(err)
	}
	restoredDB, err := store.Open(ctx, filepath.Join(restoredDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restoredDB.Close() })

	var restored []store.CIStatusEntry
	if err := restoredDB.View(ctx, func(tx *store.Tx) error {
		var err error
		restored, err = tx.CIStatusAllForChange(ctx, changeID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(restored) != 2 || restored[0].DeliveryState != store.CIStatusDeliveryDelivered || restored[1].DeliveryState != store.CIStatusDeliverySuperseded {
		t.Fatalf("restored CI statuses = %+v, want [delivered, superseded]", restored)
	}
}
