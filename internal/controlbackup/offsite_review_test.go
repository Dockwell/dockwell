// SPDX-License-Identifier: AGPL-3.0-only

package controlbackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
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
			problem, previous := d.prune(t.Context(), prefix, newest, &retentionState{}, now)
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
	trusted, err := d.ensureMarker(ctx, h, selection, "", time.Now())
	if err != nil || trusted != selection {
		t.Fatalf("could not fall back when conditional PUT is unsupported: %q, %v", trusted, err)
	}
	key := "dockwell-control/" + h.Install + "/current.json"
	if err := d.bucket.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ensureMarker(ctx, h, selection, trusted, time.Now()); !errors.Is(err, errDestinationChanged) || fake.Objects()["/bucket/"+key] != nil {
		t.Fatal("a trusted marker disappeared and the destination was automatically adopted again")
	}
	other := id.New("cbs")
	data, _ := json.Marshal(generationMarker{Install: h.Install, Generation: 1, GenerationID: h.GenerationID, Nonce: other})
	fake.Set("/bucket/"+key, data)
	if _, err := d.ensureMarker(ctx, h, selection, trusted, time.Now()); !errors.Is(err, errDestinationChanged) {
		t.Fatalf("a different marker was accepted: %v", err)
	}
	if got, err := d.ensureMarker(ctx, h, id.New("cbs"), "", time.Now()); err != nil || got != other {
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

func TestRetentionKeepsHiddenCopiesInTheWindow(t *testing.T) {
	fake := s3test.New(t)
	fake.Versioning = "Enabled"
	d := destination{bucket: s3.Bucket{Endpoint: fake.URL(), Name: "bucket", AccessKey: "key", SecretKey: "secret", Client: fake.Client()}}
	now := time.Now().UTC()
	prefix := keep.Prefix(id.New("ins"), 1)
	phrase := id.New("phrase")
	var kept []string
	for age := 1; age <= 5; age++ {
		kept = append(kept, keep.Name(now.Add(-time.Duration(age)*time.Hour), id.New("cbk"), phrase))
	}
	// The hidden names count against the same window as the visible ones.
	for age := 6; age <= 24; age++ {
		name := keep.Name(now.Add(-time.Duration(age)*time.Hour), id.New("cbk"), phrase)
		fake.Set("/bucket/"+prefix+name, []byte("visible"))
	}
	retired := keep.Name(now.AddDate(0, 0, -20), id.New("cbk"), phrase)
	for _, name := range append(slices.Clone(kept), retired) {
		fake.Set("/bucket/"+prefix+name, []byte("hidden"))
		if err := d.bucket.Delete(t.Context(), prefix+name); err != nil {
			t.Fatal(err)
		}
	}
	newest := keep.Name(now, id.New("cbk"), phrase)
	fake.Set("/bucket/"+prefix+newest, []byte("new"))
	if problem, previous := d.prune(t.Context(), prefix, newest, &retentionState{}, now); problem != "" || previous {
		t.Fatalf("prune: %q, previous=%v", problem, previous)
	}
	for _, name := range kept {
		if fake.VersionCount("/bucket/"+prefix+name) != 2 {
			t.Fatal("retention changed a hidden copy that should be kept")
		}
	}
	if fake.VersionCount("/bucket/"+prefix+retired) != 0 {
		t.Fatal("retention kept a hidden copy beyond the retention window")
	}
}

func TestOffsiteRefreshesOnlyAnOldTrustedMarker(t *testing.T) {
	fake := s3test.New(t)
	d := destination{bucket: s3.Bucket{Endpoint: fake.URL(), Name: "bucket", AccessKey: "key", SecretKey: "secret", Client: fake.Client()}}
	h := Header{Install: id.New("ins"), Generation: 1, GenerationID: id.New("gen")}
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	nonce := id.New("cbs")
	key := "dockwell-control/" + h.Install + "/current.json"
	if _, err := d.ensureMarker(ctx, h, nonce, "", now); err != nil {
		t.Fatal(err)
	}
	original := fake.Objects()["/bucket/"+key]
	fake.SetModified("/bucket/"+key, now.Add(-25*time.Hour))
	if got, err := d.ensureMarker(ctx, h, nonce, nonce, now); err != nil || got != nonce {
		t.Fatalf("refresh: %q, %v", got, err)
	}
	wantTag := fmt.Sprintf(`"%x"`, sha256.Sum256(original))
	if !bytes.Equal(fake.Objects()["/bucket/"+key], original) || !slices.Equal(fake.IfMatches, []string{wantTag}) {
		t.Fatal("the marker refresh did not use If-Match or changed its body")
	}
	if _, err := d.ensureMarker(ctx, h, nonce, nonce, now); err != nil || len(fake.IfMatches) != 1 {
		t.Fatalf("refreshed a fresh marker: %v", err)
	}
	if err := d.bucket.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ensureMarker(ctx, h, nonce, nonce, now); !errors.Is(err, errDestinationChanged) {
		t.Fatalf("a missing trusted marker was not refused: %v", err)
	}
}

func TestRetentionWhenVersioningStatusIsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name, versioning, code string
		status                 int
		refuseList             bool
	}{
		{"unsupported", "", "", http.StatusNotImplemented, false},
		{"NotImplemented error", "", "NotImplemented", http.StatusBadRequest, false},
		{"versioning forbidden", "Enabled", "AccessDenied", http.StatusForbidden, false},
		{"both denied", "Enabled", "AccessDenied", http.StatusForbidden, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := s3test.New(t)
			fake.Versioning, fake.RefuseVersionList = tc.versioning, tc.refuseList
			fake.Refuse, fake.RefuseOnly, fake.RefuseQuery, fake.RefuseStatus = tc.code, http.MethodGet, "versioning", tc.status
			if fake.Refuse == "" {
				fake.Refuse = "Unsupported"
			}
			d := destination{bucket: s3.Bucket{Endpoint: fake.URL(), Name: "bucket", AccessKey: "key", SecretKey: "secret", Client: fake.Client()}}
			now := time.Now().UTC()
			prefix := keep.Prefix(id.New("ins"), 1)
			current := id.New("phrase")
			newest := keep.Name(now, id.New("cbk"), current)
			fake.Set("/bucket/"+prefix+newest, []byte("new"))
			for age := 1; age <= 24; age++ {
				name := keep.Name(now.Add(-time.Duration(age)*time.Second), id.New("cbk"), current)
				fake.Set("/bucket/"+prefix+name, []byte("recent"))
			}
			outdated := keep.Name(now.AddDate(0, 0, -20), id.New("cbk"), current)
			currentKey := "/bucket/" + prefix + outdated
			fake.Set(currentKey, []byte("outdated"))
			state := retentionState{Marker: id.New("cbs")}
			if problem, old := d.prune(t.Context(), prefix, newest, &state, now); problem != "" || old || fake.Objects()[currentKey] != nil {
				t.Fatalf("could not retire a current-phrase copy: %q, previous=%v", problem, old)
			}
			oldName := keep.Name(now.Add(-time.Hour), id.New("cbk"), id.New("phrase"))
			oldKey := "/bucket/" + prefix + oldName
			fake.Set(oldKey, []byte("old"))
			fake.Set(oldKey, []byte("old again"))
			problem, previous := d.prune(t.Context(), prefix, newest, &state, now)
			if tc.refuseList {
				if !previous || !strings.Contains(problem, "can't tell whether storage keeps older versions") || fake.Objects()[oldKey] != nil {
					t.Fatalf("could not distinguish an unchecked old-phrase version: %q, previous=%v", problem, previous)
				}
				resumed := readRetention(state.encode(), state.Marker, now)
				if _, previous := d.prune(t.Context(), prefix, newest, &resumed, now); !previous {
					t.Fatal("lost the previous-passphrase warning after the key became hidden")
				}
				return
			}
			if problem != "" || previous || fake.Objects()[oldKey] != nil || fake.VersionCount(oldKey) != 0 {
				t.Fatalf("did not retire a previous-phrase copy: %q, previous=%v", problem, previous)
			}
		})
	}
}

func TestLockedCurrentPhraseCopyIsQuietUntilExpiry(t *testing.T) {
	fake := s3test.New(t)
	fake.Versioning = "Enabled"
	d := destination{bucket: s3.Bucket{Endpoint: fake.URL(), Name: "bucket", AccessKey: "key", SecretKey: "secret", Client: fake.Client()}}
	now := time.Now().UTC()
	prefix := keep.Prefix(id.New("ins"), 1)
	phrase := id.New("phrase")
	for age := range 24 {
		name := keep.Name(now.Add(-time.Duration(age)*time.Minute), id.New("cbk"), phrase)
		fake.Set("/bucket/"+prefix+name, []byte("recent"))
	}
	newest := keep.Name(now.Add(time.Minute), id.New("cbk"), phrase)
	name := keep.Name(now.AddDate(0, 0, -20), id.New("cbk"), phrase)
	key := "/bucket/" + prefix + name
	fake.Set("/bucket/"+prefix+newest, []byte("newest"))
	expiry := now.Add(time.Hour)
	fake.SetLocked(key, []byte("locked"), expiry)
	state := retentionState{Marker: id.New("cbs")}
	if problem, previous := d.prune(t.Context(), prefix, newest, &state, now); problem != "" || previous || len(state.Locks[name]) != 1 {
		t.Fatalf("a current-phrase lock is not a problem: %q, previous=%v", problem, previous)
	}
	var deletes int
	for _, method := range fake.Methods {
		if method == http.MethodDelete {
			deletes++
		}
	}
	before := len(fake.Methods)
	resumed := readRetention(state.encode(), state.Marker, now.Add(time.Minute))
	if problem, previous := d.prune(t.Context(), prefix, newest, &resumed, now.Add(time.Minute)); problem != "" || previous || len(fake.Methods)-before != 3 {
		t.Fatalf("retried a locked version before expiry: %q, previous=%v, requests=%v", problem, previous, fake.Methods[before:])
	}
	if deletes == 0 || len(readRetention(state.encode(), state.Marker, expiry).Locks) != 0 {
		t.Fatal("the lock expiry did not control retries")
	}
}

func TestOffsiteUnsupportedConditionalRefreshKeepsTrustedMarker(t *testing.T) {
	fake := s3test.New(t)
	fake.Versioning = "Enabled"
	fake.RefuseConditionalMatch = true
	d := destination{bucket: s3.Bucket{Endpoint: fake.URL(), Name: "bucket", AccessKey: "key", SecretKey: "secret", Client: fake.Client()}}
	h := Header{Install: id.New("ins"), Generation: 1, GenerationID: id.New("gen")}
	ctx := t.Context()
	now := time.Now()
	nonce := id.New("cbs")
	key := "dockwell-control/" + h.Install + "/current.json"
	if _, err := d.ensureMarker(ctx, h, nonce, "", now); err != nil {
		t.Fatal(err)
	}
	fake.SetModified("/bucket/"+key, now.Add(-25*time.Hour))
	if got, err := d.ensureMarker(ctx, h, nonce, nonce, now); err != nil || got != nonce {
		t.Fatalf("refused a trusted marker: %q, %v", got, err)
	}
	if len(fake.IfMatches) != 1 || fake.VersionCount("/bucket/"+key) != 1 {
		t.Fatal("the store attempted to overwrite the marker without a precondition")
	}
}

func TestSFTPIsRefusedForControlBackups(t *testing.T) {
	manager := &Manager{cfg: Config{Destination: func(context.Context, *store.Tx, string) (string, map[string]string, error) {
		return "bds_example", map[string]string{"transport": "sftp"}, nil
	}}}
	_, err := manager.destination(t.Context(), nil, "offsite")
	if !errors.Is(err, errSFTPDestination) {
		t.Fatalf("the control plane's SFTP destination = %v", err)
	}
}
