// SPDX-License-Identifier: AGPL-3.0-only

package controlbackup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/backup/keep"
	"github.com/dockwell/dockwell/internal/backup/s3"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestOffsiteRetirementRemovesObjectVersions(t *testing.T) {
	for _, versioning := range []string{"Enabled", "Suspended"} {
		t.Run(versioning, func(t *testing.T) {
			fake := s3test.New(t)
			fake.Versioning = versioning
			d := destination{bucket: s3.Bucket{Endpoint: fake.URL(), Name: "bucket", AccessKey: "key", SecretKey: "secret", Client: fake.Client()}}
			now := time.Now().UTC()
			prefix := keep.Prefix(id.New("ins"), 1)
			oldPhrase, newPhrase := id.New("phrase"), id.New("phrase")
			retired := keep.Name(now.Add(-time.Hour), id.New("cbk"), oldPhrase)
			locked := keep.Name(now.Add(-time.Hour), id.New("cbk"), oldPhrase)
			newest := keep.Name(now, id.New("cbk"), newPhrase)
			path := "/bucket/" + prefix
			fake.Set(path+retired, []byte("old"))
			fake.Set(path+retired, []byte("also old"))
			if versioning == "Enabled" {
				// On a versioned bucket, a key that was already deleted can still open.
				if err := d.bucket.Delete(t.Context(), prefix+retired); err != nil {
					t.Fatal(err)
				}
				if fake.Objects()[path+retired] != nil || fake.VersionCount(path+retired) != 3 {
					t.Fatal("the delete marker must hide two noncurrent versions")
				}
			}
			expiry := now.Add(24 * time.Hour)
			fake.SetLocked(path+locked, []byte("locked"), expiry)
			fake.Set(path+newest, []byte("new"))
			problem, previous := d.prune(t.Context(), prefix, newest)
			if !previous || !strings.Contains(problem, locked) || !strings.Contains(problem, expiry.Format(time.DateOnly)) {
				t.Fatalf("wrong lock status: %q, previous=%v", problem, previous)
			}
			if fake.VersionCount(path+retired) != 0 || fake.VersionCount(path+newest) != 1 || fake.VersionCount(path+locked) != 1 {
				t.Fatal("retention did not remove all versions of the retired copy, and only those versions")
			}
			status := placeStatus(now, newPhrase, store.ControlBackupPlace{SentAt: now, PreviousPending: previous, RecoveryID: newPhrase})
			if !status.GetPreviousPassphrase() {
				t.Fatal("a complete delivery hid a retained old-passphrase version")
			}
		})
	}
}

func TestOffsiteMarkerRequiresApprovalAfterItChanges(t *testing.T) {
	fake := s3test.New(t)
	fake.RefuseConditionalPut = true
	d := destination{bucket: s3.Bucket{Endpoint: fake.URL(), Name: "bucket", AccessKey: "key", SecretKey: "secret", Client: fake.Client()}}
	ctx := t.Context()
	h := Header{Install: id.New("ins"), Generation: 1, GenerationID: id.New("gen")}
	selection := id.New("cbs")
	trusted, err := d.ensureMarker(ctx, h, selection, "")
	if err != nil || trusted != selection {
		t.Fatalf("could not fall back when conditional PUT is unsupported: %q, %v", trusted, err)
	}
	key := "dockwell-control/" + h.Install + "/current.json"
	if err := d.bucket.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ensureMarker(ctx, h, selection, trusted); !errors.Is(err, errDestinationChanged) || fake.Objects()["/bucket/"+key] != nil {
		t.Fatal("a trusted marker disappeared and the destination was automatically adopted again")
	}
	other := id.New("cbs")
	data, _ := json.Marshal(generationMarker{Install: h.Install, Generation: 1, GenerationID: h.GenerationID, Nonce: other})
	fake.Set("/bucket/"+key, data)
	if _, err := d.ensureMarker(ctx, h, selection, trusted); !errors.Is(err, errDestinationChanged) {
		t.Fatalf("a different marker was accepted: %v", err)
	}
	if got, err := d.ensureMarker(ctx, h, id.New("cbs"), ""); err != nil || got != other {
		t.Fatalf("explicit re-selection did not adopt the existing marker: %q, %v", got, err)
	}
}

func TestOffsitePinsAccessKeyIDButAllowsSecretRotation(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	fake := s3test.New(t)
	access, secret := "key-a", "secret-a"
	m := New(Config{
		DB: db, Logger: slog.New(slog.DiscardHandler), LocalS3: true,
		Destination: func(context.Context, *store.Tx, string) (string, map[string]string, error) {
			return "destination-id", map[string]string{"endpoint": fake.URL(), "bucket": "bucket", "access_key": access, "secret_key": secret}, nil
		},
	})
	ctx := t.Context()
	now := time.Now().UTC()
	var state store.ControlBackup
	selectDestination := func() {
		t.Helper()
		if err := db.Update(ctx, func(tx *store.Tx) error {
			d, err := m.destination(ctx, tx, "archive")
			if err != nil {
				return err
			}
			if err := tx.SetControlBackupDestination(ctx, "archive", d.pin, id.New("cbs"), now); err != nil {
				return err
			}
			state, err = tx.ControlBackup(ctx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	selectDestination()
	file, err := os.CreateTemp(t.TempDir(), "bundle")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	body := []byte("sealed backup")
	if _, err := file.Write(body); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	install, generation, phrase := id.New("ins"), id.New("gen"), id.New("phrase")
	prefix := "/bucket/" + keep.Prefix(install, 1)
	deliver := func(want bool) {
		t.Helper()
		now = now.Add(time.Hour)
		name := keep.Name(now, id.New("cbk"), phrase)
		b := takenBundle{
			header: Header{Install: install, Generation: 1, GenerationID: generation}, state: state, name: name,
			file: file, size: int64(len(body)), sum: sum[:],
		}
		m.deliverOffsite(ctx, state, b)
		if got := fake.Objects()[prefix+name] != nil; got != want {
			t.Fatalf("upload success = %v, want %v", got, want)
		}
	}
	deliver(true)
	secret = "secret-b"
	deliver(true)
	access = "key-b"
	deliver(false)
	if err := db.View(ctx, func(tx *store.Tx) error {
		places, err := tx.ControlBackupPlaces(ctx, 1)
		if err == nil && (len(places) != 1 || places[0].Problem != destinationChanged || places[0].SentAt.IsZero()) {
			t.Fatalf("changed credentials lost the prior copy or failed to flag the destination: %+v", places)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	originalPin := state.DestinationPin
	selectDestination()
	deliver(true)
	if err := db.Update(ctx, func(tx *store.Tx) error {
		places, err := tx.ControlBackupPlaces(ctx, 1)
		if err != nil {
			return err
		}
		if len(places) != 2 {
			t.Fatalf("destination selection lost earlier history: %+v", places)
		}
		if dismissed, err := tx.DismissControlBackupPlace(ctx, state.DestinationPin, now); err != nil || dismissed {
			t.Fatalf("active destination could be dismissed: %v, %v", dismissed, err)
		}
		if dismissed, err := tx.DismissControlBackupPlace(ctx, originalPin, now); err != nil || !dismissed {
			t.Fatalf("earlier destination could not be dismissed: %v, %v", dismissed, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
