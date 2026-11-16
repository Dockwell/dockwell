// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"maps"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
)

// The catalog this build holds passes the whole check, each Compose file read as an
// install reads it and each image it names in its folder media, so a control plane
// that reads it without reading those files again lists the same (ADR 0077).
func TestTheSnapshotPassesTheWholeCheck(t *testing.T) {
	files, ok := readSnapshot()
	if !ok {
		t.Fatal("the build holds no snapshot of Dockwell/catalog")
	}
	whole := checkFiles(t.Context(), DefaultName, snapshotSpec(), files, claims{}, false)
	if len(whole.refused) != 0 {
		t.Fatalf("the snapshot fails the check: %v", whole.refused)
	}
	if len(whole.entries) != len(files.entries) || len(whole.entries) < 30 {
		t.Fatalf("the check keeps %d entries of %d files", len(whole.entries), len(files.entries))
	}
	proved := checkedSnapshot()
	if len(proved.refused) != 0 || len(proved.entries) != len(whole.entries) {
		t.Fatalf("read as proved, it keeps %d entries and refuses %v", len(proved.entries), proved.refused)
	}
	for i, e := range whole.entries {
		if !proto.Equal(e, proved.entries[i]) {
			t.Errorf("%s reads otherwise as proved", e.GetId())
		}
	}
}

// Only the snapshot this build holds is decoded without being checked again. A source
// read from a repository is checked whole each time, even Dockwell's own at another
// commit: an entry there that the check refuses is left out, saying why, though it
// decodes as the snapshot's did.
func TestAFetchedSourceIsCheckedWhole(t *testing.T) {
	files, ok := readSnapshot()
	if !ok {
		t.Fatal("the build holds no snapshot of Dockwell/catalog")
	}
	raw, ok := files.entries["mongo.yaml"]
	if !ok {
		t.Fatal("the snapshot has no mongo entry")
	}
	// Its image named by tag alone, which only a converted store's entries may.
	unpinned := strings.Replace(string(raw), "@sha256:", "-unpinned-", 1)
	if unpinned == string(raw) {
		t.Fatal("the mongo entry names no image by digest")
	}
	fetched := commitFiles{entries: maps.Clone(files.entries), media: files.media, mediaKept: true}
	fetched.entries["mongo.yaml"] = []byte(unpinned)
	spec := snapshotSpec()
	spec.Commit = strings.Repeat("a", 40)
	if atSnapshot(spec) {
		t.Fatal("a source at another commit reads as the snapshot")
	}
	got := checkSource(t.Context(), DefaultName, spec, fetched, claims{})
	refused := false
	for _, r := range got.refused {
		refused = refused || r.GetFile() == "mongo.yaml" && strings.Contains(r.GetReason(), "isn't pinned by digest")
	}
	if !refused {
		t.Errorf("a fetched entry the check refuses reads as %v", got.refused)
	}
	for _, e := range got.entries {
		if e.GetId() == "mongo" {
			t.Error("a fetched entry the check refuses is kept")
		}
	}
}
