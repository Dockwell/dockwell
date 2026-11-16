// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// A release that adds storage, met by a connection, says in its plan that the app
// keeps its files on that connection, never that a database of its own meets it
// (the slice 1 review, M2).
func TestNewNeedWordsNameTheConnection(t *testing.T) {
	e := &catalogv1.Entry{Name: "Ghost", Version: "6.73.0", Needs: []*catalogv1.Need{{Name: "storage", Label: "Storage", Kind: catalogv1.Kind_KIND_S3}}}
	w := catalog.Way{Need: e.GetNeeds()[0], Connection: &catalog.ConnectionWay{Name: "uploads", Label: "Uploads"}}
	if got, want := newNeedWords(e, "storage", w), "Ghost 6.73.0 needs storage, which it didn't before, so it keeps its files on the connection Uploads"; got != want {
		t.Errorf("the plan says %q, want %q", got, want)
	}
}
