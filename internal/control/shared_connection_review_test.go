// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/revision"
)

// F1: Ghost as the testdata has it sets no key prefix, so it writes at the bucket's
// root, though its link says site/. Shared, uploads must not be offered to an app
// beside it: no prefix keeps their files apart.
func TestReviewSharedNotBesideAnAppThatIgnoresItsPrefix(t *testing.T) {
	t.Parallel()
	dir, raw := ghostWithStorage(t)
	_, _, c, cat, _ := recheckSetup(t, raw)
	apply(t, c, propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true))))
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing site ended %v: %s", done.GetState(), done.GetError())
	}
	prefixed := ghostKeepingToAPrefix(t, false)
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), prefixed, 0o600); err != nil {
		t.Fatal(err)
	}
	if o := storageOption(t, cat); o.GetWhy() == "" {
		t.Errorf("shared uploads is offered beside site, which keeps its files at the bucket's root: %v", o)
	}
}

// F2: an install planned while uploads was shared applies after an admin unshared it,
// so two apps use a connection that says it serves one.
func TestReviewUnshareThenAnInstallPlannedBefore(t *testing.T) {
	t.Parallel()
	raw := ghostKeepingToAPrefix(t, true)
	_, _, c, cat, _ := recheckSetup(t, raw)
	apply(t, c, propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true))))
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing site ended %v: %s", done.GetState(), done.GetError())
	}
	blog := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "blog"})
	apply(t, c, propose(t, c, "Unshare uploads", connectionEdit("uploads", uploadsSpec(false))))
	w := watchWithin(t, c, blog.GetId(), 20*time.Second)
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: blog.GetId()}); err != nil {
		t.Logf("refused at apply: %v", err)
		return
	}
	done := waitWatchWithin(t, w, 20*time.Second).last()
	t.Logf("blog ended %v: %s", done.GetState(), done.GetError())
	if done.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Error("blog's install, planned while uploads was shared, applied after it was unshared: two apps on a connection that serves one")
	}
}

// The other order holds: unsharing planned with one app, applied after a second
// joined, is refused at apply.
func TestReviewAnInstallThenUnsharePlannedBefore(t *testing.T) {
	t.Parallel()
	raw := ghostKeepingToAPrefix(t, true)
	_, _, c, cat, _ := recheckSetup(t, raw)
	apply(t, c, propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true))))
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing site ended %v: %s", done.GetState(), done.GetError())
	}
	unshare := propose(t, c, "Unshare uploads", connectionEdit("uploads", uploadsSpec(false)))
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "blog"})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing blog ended %v: %s", done.GetState(), done.GetError())
	}
	w := watchWithin(t, c, unshare.GetId(), 20*time.Second)
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: unshare.GetId()}); err != nil {
		t.Logf("refused at apply: %v", err)
		return
	}
	if done := waitWatchWithin(t, w, 20*time.Second).last(); done.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Error("unsharing uploads, planned with one app, applied with two")
	}
}

// A hand-made second app on an unshared connection is refused on every path (the
// decision), and shared in the same change, it plans (the review's M2).
func TestReviewASecondLinkByHandToAnUnsharedConnection(t *testing.T) {
	t.Parallel()
	raw := ghostKeepingToAPrefix(t, true)
	f, b, c, cat, _ := recheckSetup(t, raw)
	other := listedEnv(t, b)
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing site ended %v: %s", done.GetState(), done.GetError())
	}
	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Link notes", Edits: recheckApp(t, f, other, "notes", "notes/")}))
	if err == nil || !strings.Contains(err.Error(), "the connection uploads serves another app already, and isn't shared. Give this app a connection of its own, or have an admin share uploads") {
		t.Errorf("a second app's link to unshared uploads, by hand: %v", err)
	}
	// Shared in the same change, it plans.
	apply(t, c, propose(t, c, "Share uploads and link notes", append([]*dockwellv1.Edit{connectionEdit("uploads", uploadsSpec(true))}, recheckApp(t, f, other, "notes", "notes/")...)...))
}

// L: an undo of sharing, which changes nothing an app reads, restarts every app.
func TestReviewUndoingSharingRestartsNothing(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	_, _, c, cat, _ := recheckSetup(t, raw)
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing site ended %v: %s", done.GetState(), done.GetError())
	}
	spec := uploadsSpec(true)
	spec.S3.AccessKeyId, spec.S3.SecretKey = "", ""
	share := apply(t, c, propose(t, c, "Share uploads", connectionEdit("uploads", spec)))
	res, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: share.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
		if r.GetResource().GetKind() != "connection" && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
			t.Errorf("undoing the sharing plans %v for %s %s", r.GetAction(), r.GetResource().GetKind(), r.GetResource().GetName())
		}
		for _, f := range r.GetChanges() {
			if f.GetSensitive() {
				t.Errorf("undoing the sharing restates %s", f.GetPath())
			}
		}
	}
}

// F1 by Connect storage: a link that doesn't name the setting that tells its app the prefix, as Connect
// storage makes one when the person names none, keeps its app's files at the bucket's
// root, whatever prefix it says. Sharing says no other app can join it yet, the
// catalog offers it to none, and the engine refuses one beside it (the review's H1).
func TestReviewAConnectStorageLinkWithNoPrefixNameIsAtTheRoot(t *testing.T) {
	t.Parallel()
	raw := ghostKeepingToAPrefix(t, false)
	f, b, c, cat, _ := recheckSetup(t, raw)
	env := listedEnv(t, b)
	apply(t, c, propose(t, c, "Connect notes", recheckLink(env, "notes", "notes/")))
	share := propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true)))
	if notes := notesOf(share, revision.ConnectionSharedNote); len(notes) != 1 || notes[0] != "No other app can keep its files on Uploads yet, as default's link doesn't say which prefix its app keeps to" {
		t.Errorf("sharing uploads beside an app at its root says %v", notes)
	}
	apply(t, c, share)
	if o := storageOption(t, cat); o.GetWhy() != "another app's link doesn't say which prefix its app keeps to, so nothing keeps their files apart" {
		t.Errorf("shared uploads is offered beside an app at its root as %v", o)
	}
	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Link media", Edits: recheckApp(t, f, env, "media", "media/")}))
	if err == nil || !strings.Contains(err.Error(), "where another app's link doesn't say which prefix its app keeps to") {
		t.Errorf("a link beside an app at the root: %v", err)
	}
}
