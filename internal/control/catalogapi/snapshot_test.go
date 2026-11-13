// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
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
