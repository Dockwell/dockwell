// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"errors"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/backup/seal"
	"github.com/dockwell/dockwell/internal/backup/sftpstore/sftptest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestSFTPSweepPreservesBackupsMissingAfterControlRestore(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		const name, folder = "offsite", "env_example/data"
		const user, password, passphrase = "backups", "fixture-password", "fixture backup passphrase"
		ctx := t.Context()
		sftp, err := sftptest.ListenDisk(ctx, "127.0.0.1:0", sftptest.Login{User: user, Password: password})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sftp.Close)
		host, port, err := net.SplitHostPort(sftp.Address())
		if err != nil {
			t.Fatal(err)
		}
		portNum, err := strconv.ParseUint(port, 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		db, _ := storetest.Open(t, backend)
		kinds := resource.NewRegistry(backuppolicy.Destination{})
		keeper := secretstest.Keeper(db, kinds)
		old := time.Now().Add(-25 * time.Hour)
		spec, err := resource.Encode(&resourcev1.BackupDestination{Passphrase: passphrase, Sftp: &resourcev1.BackupDestinationSftp{
			Host: host, Port: uint32(portNum), User: user, Password: password, Path: root, HostKeyFingerprint: sftp.Fingerprint(),
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Revise(ctx, func(tx *store.RevisionTx) error {
			org := store.Organization{ID: id.New(id.Organization), Name: "example", CreatedAt: old}
			if err := tx.CreateOrganization(ctx, org); err != nil {
				return err
			}
			change := store.Change{
				ID: id.New(id.Change), Summary: "Add backup destination", Actor: "dockwell",
				State: store.ChangeApplying, CreatedAt: old, Plan: []byte("{}"), Fingerprint: "fixture",
			}
			if err := tx.CreateChange(ctx, change); err != nil {
				return err
			}
			r := store.NewRevision{
				Kind: string(backuppolicy.DestinationKind), Scope: org.ID, Name: name, ID: id.New("dst"),
				Number: 1, Change: change.ID, At: old,
			}
			r.Spec, err = keeper.Seal(ctx, tx.Tx, store.InRevision(r.ID, r.Kind, 1), spec)
			if err != nil {
				return err
			}
			_, err := tx.AddRevision(ctx, r)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		// The restored snapshot knows of a failed send and will sweep the folder.
		failed := store.Backup{
			ID: id.At(id.Backup, old, "failed"), EnvironmentID: "env_example", Volume: "data",
			Destination: name, StartedAt: old, State: store.BackupDone, CopyError: "the reply never arrived",
		}
		if err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.CreateBackup(ctx, failed); err != nil {
				return err
			}
			return tx.SetBackupCopy(ctx, failed)
		}); err != nil {
			t.Fatal(err)
		}

		// Put the control snapshot back after a newer backup made it off-site.
		snapshot := filepath.Join(t.TempDir(), "state.db")
		if _, err := db.SnapshotControl(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
		newID := id.At(id.Backup, old.Add(time.Minute), "later")
		key := path.Join(folder, newID+".tar.gz.sealed")
		row := store.Backup{
			ID: newID, EnvironmentID: "env_example", Volume: "data", Destination: name,
			StartedAt: old.Add(time.Minute), State: store.BackupDone, CopyKey: key, CopySealVersion: seal.ArchiveVersion,
		}
		if err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.CreateBackup(ctx, row); err != nil {
				return err
			}
			return tx.SetBackupCopy(ctx, row)
		}); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, folder), 0o700); err != nil {
			t.Fatal(err)
		}
		want := []string{path.Base(key), path.Base(key + ".json")}
		for _, filename := range want {
			file := filepath.Join(root, folder, filename)
			if err := os.WriteFile(file, []byte("off-site fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(file, row.StartedAt, row.StartedAt); err != nil {
				t.Fatal(err)
			}
		}
		restored, err := store.Open(ctx, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		if err := restored.View(ctx, func(tx *store.Tx) error {
			_, err := tx.GetBackup(ctx, newID)
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("restored snapshot should lack the new backup: %v", err)
		}

		h := &Handler{db: restored, keeper: secretstest.Keeper(restored, kinds)}
		values, err := h.copyDestination(ctx, name, folder)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(values)
		if values["sweep_failed_ids"] != failed.ID || values["sweep_failed_folder"] != folder || values["sweep_folder"] != "" {
			t.Fatal("the restored store did not request a failed-send-only sweep")
		}
		// Touch the folder the day after the backup. The trigger has no off-site file.
		trigger := id.New(id.Backup)
		ref := resource.Ref{Scope: "env_example", Name: "data"}
		runner := &backup.Runner{Root: t.TempDir()}
		if problem, err := runner.Delete(ctx, ref, trigger, folder+"/"+trigger+".tar.gz.sealed", backup.DestinationFrom(values)); problem != "" || err != nil {
			t.Fatalf("SFTP operation failed: %s, %v", problem, err)
		}
		items, err := os.ReadDir(filepath.Join(root, folder))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, item := range items {
			got = append(got, item.Name())
		}
		if !slices.Equal(got, want) {
			t.Fatalf("after the sweep got %v, want %v", got, want)
		}
	})
}
