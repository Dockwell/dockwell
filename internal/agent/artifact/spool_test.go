// SPDX-License-Identifier: AGPL-3.0-only

package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeArchive(t testing.TB, extra []*tar.Header) ([]byte, string) {
	t.Helper()
	cfg := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	id := sha256.Sum256(cfg)
	name := hex.EncodeToString(id[:]) + ".json"
	data, err := json.Marshal([]manifest{{Config: name, RepoTags: []string{"fixture/example:original"}, Layers: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	put := func(name string, p []byte) {
		t.Helper()
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(p))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	put(name, cfg)
	put("manifest.json", data)
	for _, h := range extra {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), digest(id[:])
}

func TestArchiveRejectsAmbiguousAndUnsafeEntries(t *testing.T) {
	for name, entries := range map[string][]*tar.Header{
		"traversal":    {{Name: "../outside", Mode: 0o600}},
		"absolute":     {{Name: "/outside", Mode: 0o600}},
		"dot":          {{Name: "./entry"}},
		"double slash": {{Name: "a//b"}},
		"hardlink":     {{Name: "layer", Linkname: "target", Typeflag: tar.TypeLink}},
		"device":       {{Name: "layer", Typeflag: tar.TypeChar}},
		"pax":          {{Name: "layer", Format: tar.FormatPAX, PAXRecords: map[string]string{"custom.key": "value"}}},
		"duplicate":    {{Name: "manifest.json", Mode: 0o600}},
		"symlink":      {{Name: "layer", Linkname: "target", Typeflag: tar.TypeSymlink}},
		"fifo":         {{Name: "layer", Typeflag: tar.TypeFifo}},
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := makeArchive(t, entries)
			if _, err := untar(t.Context(), bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrArchive) {
				t.Fatalf("unsafe archive accepted: %v", err)
			}
		})
	}
	data, _ := makeArchive(t, nil)
	for _, bad := range [][]byte{data[:len(data)-512], append(bytes.Clone(data), 1), append(bytes.Clone(data), make([]byte, maxPadding+1)...)} {
		if _, err := untar(t.Context(), bytes.NewReader(bad), int64(len(bad))); !errors.Is(err, ErrArchive) {
			t.Fatalf("incomplete or trailing archive accepted: %v", err)
		}
	}
}

func TestArchiveSpoolRecoversCommittedPrefixAndNormalizesAliases(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, configID := makeArchive(t, nil)
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	data = compressed.Bytes()
	upload, err := s.Begin("person/session", Import, uint64(len(data)), "", true, "image.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	id := upload.ArtifactId
	if _, err := s.Status(id, "person/other"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("other session: %v", err)
	}
	n := len(data) / 2
	if _, err := s.Receive(ctx, id, "person/session", 0, uint64(n), testFrames(data[:n-1])); err == nil {
		t.Fatal("short segment accepted")
	}
	received, err := s.Receive(ctx, id, "person/session", 0, uint64(n), testFrames(data[:n]))
	if err != nil {
		t.Fatal(err)
	}
	part, err := os.OpenFile(filepath.Join(dir, id, "bytes"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data[n:]); err != nil {
		t.Fatal(err)
	}
	if err := part.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orphan"), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.Status(id, "person/session")
	if err != nil || after.GetReceivedBytes() != uint64(n) || after.GetCommittedPrefixSha256() != received.GetCommittedPrefixSha256() {
		t.Fatalf("wrong restored offset: %v %v", after, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "orphan")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphan survived")
	}
	if _, err := s.Receive(ctx, id, "person/session", uint64(n), uint64(len(data)-n), testFrames(data[n:])); err != nil {
		t.Fatal(err)
	}
	sealed, err := s.Seal(ctx, id, "person/session")
	if err != nil || sealed.GetState() != Ready || sealed.GetImages()[0].GetImageId() != configID {
		t.Fatalf("seal: %v %v", sealed, err)
	}
	sha := sha256.Sum256(data)
	if sealed.GetSha256() != hex.EncodeToString(sha[:]) {
		t.Fatal("digest changed")
	}
	var normalized bytes.Buffer
	err = s.WithArchive(ctx, id, sealed.Sha256, func(a *Archive) error { return a.WriteImage(ctx, &normalized, configID, nil) })
	if err != nil {
		t.Fatal(err)
	}
	checked, err := untar(ctx, bytes.NewReader(normalized.Bytes()), int64(normalized.Len()))
	if err != nil || len(checked.images[0].ProposedTags) != 0 {
		t.Fatalf("original tags survived: %v", err)
	}
	export, err := s.Begin("person/session", Export, 0, "", false, "")
	if err != nil {
		t.Fatal(err)
	}
	ready, err := s.Publish(ctx, export.ArtifactId, bytes.NewReader(normalized.Bytes()), []string{"fixture/example:reviewed"}, func(_ *Archive, config string) bool { return config == configID })
	if err != nil {
		t.Fatal(err)
	}
	var download bytes.Buffer
	for _, part := range [][2]uint64{{0, 512}, {512, ready.SizeBytes - 512}} {
		if err := s.Send(ctx, ready.ArtifactId, "person/session", part[0], part[1], &download); err != nil {
			t.Fatal(err)
		}
	}
	final, err := untar(ctx, bytes.NewReader(download.Bytes()), int64(download.Len()))
	if err != nil || final.images[0].GetProposedTags()[0] != "fixture/example:reviewed" {
		t.Fatalf("export contents: %v", err)
	}
	hash := sha256.Sum256(download.Bytes())
	if hex.EncodeToString(hash[:]) != ready.GetSha256() {
		t.Fatal("resumed archive hash differs")
	}
}

// testFrames hands one bounded frame at a time to Receive.
func testFrames(data []byte) func() ([]byte, error) {
	return func() ([]byte, error) {
		if len(data) == 0 {
			return nil, io.EOF
		}
		part := data[:min(len(data), Frame)]
		data = data[len(part):]
		return part, nil
	}
}

func FuzzUntar(f *testing.F) {
	image, _ := makeArchive(f, nil)
	// A zero byte introduces an offset and patch into a valid image, so small
	// seeds exercise later tar and manifest fields without shrinking a whole tar.
	f.Add([]byte{0, 0, 0})
	f.Add(append([]byte{0, 0, 0}, []byte("../")...))
	f.Add([]byte{0, 0, 156, tar.TypeSymlink})
	f.Add([]byte{0, 4, 0, 'm', 'a', 'n'})
	f.Add([]byte{})
	f.Add([]byte("manifest.json"))
	f.Add(image[:tarBlock])
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		if len(data) >= 3 && data[0] == 0 {
			offset := (int(data[1])<<8 | int(data[2])) % len(image)
			patched := bytes.Clone(image)
			copy(patched[offset:], data[3:])
			data = patched
		}
		_, _ = untar(t.Context(), bytes.NewReader(data), int64(len(data)))
	})
}

func TestQueuedArchiveSurvivesRestartAndExposesStatusDuringSeal(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	clock := func() time.Time { return now }
	s, err := OpenWithClock(dir, clock)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := makeArchive(t, nil)
	const owner = "person/session"
	lease, err := s.Begin(owner, Import, uint64(len(data)), "", false, "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	id := lease.GetArtifactId()
	if _, err := s.Receive(t.Context(), id, owner, 0, uint64(len(data)), testFrames(data)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.QueueSeal(id, owner); err != nil || got.GetState() != Checking {
		t.Fatalf("queued seal: %v %v", got, err)
	}
	now = now.Add(Lifetime + time.Minute)
	s.Prune(now)
	s, err = OpenWithClock(dir, clock)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	allow := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunSeals(ctx, func(ctx context.Context) (func(), error) {
			close(start)
			select {
			case <-allow:
				return func() {}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	}()
	defer func() { cancel(); <-done }()
	select {
	case <-start:
	case <-time.After(2 * time.Second):
		t.Fatal("queued job was not recovered")
	}
	l, err := s.locked(id, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(id, owner)
	l.mu.Unlock()
	if err != nil || status.GetState() != Checking {
		t.Fatalf("status with writer holding lock: %v %v", status, err)
	}
	close(allow)
	for deadline := time.Now().Add(2 * time.Second); ; {
		status, err := s.Status(id, owner)
		if err == nil && status.GetState() == Ready {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("recovered seal: %v %v", status, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestIdleLeaseReleasesReservationAndListsOnlyOwner(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.Begin("owner/a", Import, 4096, "", false, "archive.tar")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.List("owner/b")) != 0 || len(s.List("owner/a")) != 1 {
		t.Fatal("lease list did not enforce the session")
	}
	s.Prune(time.Now().Add(idleLifetime + time.Minute))
	if s.reserved != 0 || len(s.List("owner/a")) != 0 {
		t.Fatalf("idle lease was retained, reserved %d", s.reserved)
	}
	if _, err := s.Status(lease.GetArtifactId(), "owner/a"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("idle lease status: %v", err)
	}
}

func TestManifestAndConfigCollectionsAreBounded(t *testing.T) {
	for _, field := range []string{"Layers", "RepoTags"} {
		data, err := json.Marshal([]map[string]any{{"Config": "one", field: make([]string, 129)}})
		if err != nil {
			t.Fatal(err)
		}
		var entries []manifest
		if readManifest(data, &entries) {
			t.Fatalf("accepted too many %s", field)
		}
	}
	data, err := json.Marshal(make([]manifest, 65))
	if err != nil {
		t.Fatal(err)
	}
	var entries []manifest
	if readManifest(data, &entries) || len(entries) != 64 {
		t.Fatal("manifest count was not bounded")
	}
}

func TestExportTiesConfigToReviewedOCIManifest(t *testing.T) {
	original, config := makeArchive(t, nil)
	for name, wanted := range map[string]string{
		"reviewed":                     config,
		"same layers different config": digest(make([]byte, sha256.Size)),
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"schemaVersion":2,"config":{"digest":"` + wanted + `"}}`)
			sum := sha256.Sum256(body)
			var raw bytes.Buffer
			raw.Write(original[:len(original)-2*tarBlock])
			writer := tar.NewWriter(&raw)
			err := writer.WriteHeader(&tar.Header{Name: "blobs/sha256/" + hex.EncodeToString(sum[:]), Size: int64(len(body)), Mode: 0o600})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			lease, err := s.Begin("owner/session", Export, 0, "", false, "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Publish(t.Context(), lease.GetArtifactId(), &raw, nil,
				func(_ *Archive, got string) bool { return got == config }, digest(sum[:]))
			if wanted == config && err != nil || wanted != config && !errors.Is(err, ErrArchive) {
				t.Fatalf("export with config %s: %v", wanted, err)
			}
		})
	}
}

func TestArchiveGzipReservationsGrowAndShareSpace(t *testing.T) {
	data, _ := makeArchive(t, nil)
	var extended bytes.Buffer
	extended.Write(data[:len(data)-2*tarBlock])
	tarOut := tar.NewWriter(&extended)
	const paddingFile = 1 << 20
	if err := tarOut.WriteHeader(&tar.Header{Name: "unrelated", Mode: 0o600, Size: paddingFile}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarOut.Write(make([]byte, paddingFile)); err != nil {
		t.Fatal(err)
	}
	if err := tarOut.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(extended.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const owner = "actor/session"
	size := uint64(compressed.Len())
	first, err := s.Begin(owner, Import, size, "", true, "first.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Begin(owner, Import, size, "", true, "second.tar.gz")
	if err != nil {
		t.Fatalf("a second small gzip could not reserve room: %v", err)
	}
	if s.reserved != 2*(size+gzipEstimate(size)) || gzipEstimate(size) >= paddingFile {
		t.Fatalf("unexpected gzip estimate: %d", s.reserved)
	}
	if _, err := s.Receive(t.Context(), first.GetArtifactId(), owner, 0, size, testFrames(compressed.Bytes())); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Seal(t.Context(), first.GetArtifactId(), owner); err != nil || got.GetState() != Ready {
		t.Fatalf("expansion could not grow past its estimate: %v %v", got, err)
	}
	if s.reserved != size+gzipEstimate(size) {
		t.Fatalf("first expansion still reserves room: %d", s.reserved)
	}
	if _, err := s.Receive(t.Context(), other.GetArtifactId(), owner, 0, size, testFrames(compressed.Bytes())); err != nil {
		t.Fatal(err)
	}
	// Simulate the spool's aggregate allowance becoming full during the upload.
	s.reserved = 24 << 30
	_, err = s.Seal(t.Context(), other.GetArtifactId(), owner)
	var space *SpaceError
	if !errors.As(err, &space) || space.Needed <= space.Available {
		t.Fatalf("real expansion limit gave no usable space figures: %v", err)
	}
	if err := s.failSeal(other.GetArtifactId(), err); err != nil {
		t.Fatal(err)
	}
	failed, err := s.Status(other.GetArtifactId(), owner)
	if err != nil || failed.GetFailureSpace().GetRequiredBytes() != space.Needed || failed.GetFailureSpace().GetAvailableBytes() != space.Available {
		t.Fatalf("failed validation lost its space figures: %v %v", failed, err)
	}
	if _, err := s.Begin(owner, Import, size, "", true, "third.tar.gz"); !errors.As(err, &space) || space.Needed <= space.Available {
		t.Fatalf("Begin did not give the required and available bytes: %v", err)
	}
}
