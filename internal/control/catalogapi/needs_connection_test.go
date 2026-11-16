// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// checksFor stands in for the agent's check of a connection, answering as told and
// keeping what it was asked.
type checksFor struct {
	checked bool
	err     error
	asked   []string
}

func (c *checksFor) CheckBeforeLinking(_ context.Context, name, server, env, app, prefix string, presign bool) (bool, error) {
	c.asked = append(c.asked, fmt.Sprintf("%s %s %s %s %s %v", name, server, env, app, prefix, presign))
	return c.checked, c.err
}

// An install checks the connection it links to from the app's server, under the
// app's prefix, presigning where the need asks: a check that passes plans with no
// note, one that fails refuses, and one nothing can make plans with the note that
// says so (ADR 0099, slice 2).
func TestAnInstallChecksTheConnectionFromTheAppsServer(t *testing.T) {
	need := &catalogv1.Need{
		Name: "storage", Label: "Storage", Kind: catalogv1.Kind_KIND_S3,
		Accepts: []*catalogv1.NeedAccept{{Kind: catalogv1.Kind_KIND_S3, Features: []catalogv1.Feature{catalogv1.Feature_FEATURE_PRESIGNED_URLS}}},
	}
	e := &catalogv1.Entry{Name: "Ghost", Needs: []*catalogv1.Need{need}}
	w := catalog.Way{Need: need, Connection: &catalog.ConnectionWay{Name: "uploads", Label: "Uploads", Prefix: "site/"}}

	passes := &checksFor{checked: true}
	h := &Handler{checks: passes}
	if notes, err := h.checkConnection(t.Context(), e, w, "srv_1", ""); err != nil || len(notes) != 0 {
		t.Errorf("a check that passes plans with %v, %v", notes, err)
	}
	if !slices.Equal(passes.asked, []string{"uploads srv_1   site/ true"}) {
		t.Errorf("the check was asked %v", passes.asked)
	}
	fails := &checksFor{err: errors.New("the bucket refused the check")}
	if _, err := (&Handler{checks: fails}).checkConnection(t.Context(), e, w, "srv_1", ""); err == nil {
		t.Error("a check that fails plans")
	}
	for _, h := range []*Handler{{checks: &checksFor{}}, {}} {
		notes, err := h.checkConnection(t.Context(), e, w, "srv_1", "")
		if err != nil || len(notes) != 1 || notes[0].Reason != ConnectionUncheckedNote {
			t.Errorf("a check nothing can make plans with %v, %v", notes, err)
		}
	}
}
