// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// uploadsSpec is the connection recheckSetup adds, shared or not.
func uploadsSpec(shared bool) *resourcev1.ConnectionSpec {
	return &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY, Shared: shared,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "eu-1", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret, PathStyle: true},
	}
}

// ghostKeepingToAPrefixIn writes Ghost with a storage need that sets a key prefix, as an
// app that keeps to one does, and, where required, one it can't go without.
func ghostKeepingToAPrefixIn(t *testing.T, required bool) (string, []byte) {
	t.Helper()
	dir, raw := ghostWithStorage(t)
	s := strings.Replace(string(raw), "        part: PART_SECRET_KEY\n",
		"        part: PART_SECRET_KEY\n      - name: S3_PREFIX\n        part: PART_KEY_PREFIX\n", 1)
	s = strings.Replace(s, "        storage__s3__secretAccessKey: ${S3_SECRET_ACCESS_KEY}\n",
		"        storage__s3__secretAccessKey: ${S3_SECRET_ACCESS_KEY}\n        storage__s3__prefix: ${S3_PREFIX}\n", 1)
	if required {
		s = strings.Replace(s, "    kind: KIND_S3\n    optional: true\n", "    kind: KIND_S3\n", 1)
		s = regexp.MustCompile(`      - name: STORAGE_DRIVER\n        part: PART_MET\n        words:\n.*\n.*\n`).ReplaceAllString(s, "")
		s = strings.Replace(s, "${STORAGE_DRIVER}", "s3", 1)
	}
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, []byte(s)
}

// ghostKeepingToAPrefix is ghostKeepingToAPrefixIn's entry alone.
func ghostKeepingToAPrefix(t *testing.T, required bool) []byte {
	t.Helper()
	_, raw := ghostKeepingToAPrefixIn(t, required)
	return raw
}

// storageOption is how the catalog offers uploads for Ghost's storage, as a sheet
// for a new project reads it.
func storageOption(t *testing.T, cat dockwellv1connect.CatalogServiceClient) *dockwellv1.CatalogNeedOption {
	t.Helper()
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "ghost"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range got.Msg.GetNeeds() {
		if o.GetNeed() == "storage" && o.GetConnection() == "uploads" {
			return o
		}
	}
	t.Fatal("uploads isn't offered for Ghost's storage")
	return nil
}

// An admin marks a connection shared, with a plan that says what it allows: then a
// need that keeps to a prefix is offered it though another app keeps its files there,
// and takes it by default, under a prefix of its own. Unsharing it while two apps use
// it is refused, with the fix (ADR 0099).
func TestASharedConnectionMeetsASecondApp(t *testing.T) {
	t.Parallel()
	raw := ghostKeepingToAPrefix(t, true)
	_, b, c, cat, _ := recheckSetup(t, raw)
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing site ended %v: %s", done.GetState(), done.GetError())
	}

	// Unshared, it serves site alone.
	if o := storageOption(t, cat); o.GetWhy() != "it serves another app already, and isn't shared" || o.GetConnectionShared() {
		t.Errorf("unshared, uploads is offered as %v", o)
	}
	if _, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "blog"})); err == nil {
		t.Error("unshared, a second Ghost took uploads")
	}

	share := propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true)))
	if notes := notesOf(share, revision.ConnectionSharedNote); len(notes) != 1 || notes[0] != "Other apps may keep their files on Uploads beside site's, each under its own prefix" {
		t.Errorf("sharing uploads says %v", notes)
	}
	apply(t, c, share)
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	listed, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range listed.Msg.GetConnections() {
		if info.GetName() == "uploads" && !info.GetShared() {
			t.Error("ListConnections doesn't say uploads is shared")
		}
	}

	if o := storageOption(t, cat); o.GetWhy() != "" || !o.GetConnectionShared() {
		t.Errorf("shared, uploads is offered as %v", o)
	}
	// It's the only connection that meets the need, so it's the default.
	blog := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "blog"})
	linked := false
	for _, r := range blog.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "link" && r.GetResource().GetName() == "storage" {
			linked = true
			if s := r.GetStorage(); s.GetConnection() != "uploads" || s.GetPrefix() != "blog/" {
				t.Errorf("blog's storage keeps its files on %v", s)
			}
		}
	}
	if !linked {
		t.Error("blog's install links no storage")
	}
	if done := apply(t, c, blog); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing blog ended %v: %s", done.GetState(), done.GetError())
	}

	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Unshare uploads", Edits: []*dockwellv1.Edit{connectionEdit("uploads", uploadsSpec(false))}}))
	if err == nil || !strings.Contains(err.Error(), "the connection uploads serves 2 apps, which keep their files side by side in its bucket, so it stays shared while more than one does. Disconnect all but one on its page first, or keep it shared") {
		t.Errorf("unsharing uploads while two apps use it: %v", err)
	}
}

// A need that can't keep to a prefix, as Ghost's own can't, never takes a shared
// connection another app uses: their files would mix (ADR 0099).
func TestASharedConnectionNeverMeetsANeedWithoutAPrefix(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	_, _, c, cat, _ := recheckSetup(t, raw)
	apply(t, c, propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true))))
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing site ended %v: %s", done.GetState(), done.GetError())
	}
	if o := storageOption(t, cat); !strings.HasPrefix(o.GetWhy(), "it serves another app already, and this app can't keep to a prefix of the bucket") {
		t.Errorf("shared, uploads is offered to an app that can't keep to a prefix as %v", o)
	}
	if _, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{
		Id: "ghost", Project: "blog", Needs: map[string]string{"storage": "connection:uploads"},
	})); err == nil || !strings.Contains(err.Error(), "can't keep to a prefix of the bucket") {
		t.Errorf("a second Ghost on shared uploads: %v", err)
	}
}

// Sharing a connection, or unsharing it, is an install admin's, in the engine, not
// only in the panel: below one it's refused in its own words. Unsharing one that
// serves one app, or none, plans, and says what it stops.
func TestOnlyAnInstallAdminSharesAConnection(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	f, _, c, _, _ := recheckSetup(t, raw)
	apply(t, c, propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true))))
	unshare := propose(t, c, "Unshare uploads", connectionEdit("uploads", uploadsSpec(false)))
	if notes := notesOf(unshare, revision.ConnectionUnsharedNote); len(notes) != 1 || notes[0] != "Only one app may keep its files on Uploads from now on, so no other app is offered it" {
		t.Errorf("unsharing uploads says %v", notes)
	}
	discard(t, c, unshare)

	hold := aMember(t, f)
	hold(store.Grant{ProjectID: store.Install, Role: store.RoleDeveloper})
	for _, shared := range []bool{false, true} {
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Share uploads", Edits: []*dockwellv1.Edit{connectionEdit("uploads", uploadsSpec(shared))}}))
		if shared {
			// Unchanged, it's still a connection, an admin's to change, in the usual words.
			if err == nil || strings.Contains(err.Error(), "sharing a connection") {
				t.Errorf("a developer's unchanged edit of uploads: %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "sharing a connection between apps is an admin's") {
			t.Errorf("a developer unsharing uploads: %v", err)
		}
	}
}

// A change to a connection reaches the apps that read it only where it changes what
// they read: sharing it, or renaming its label, with its keys left out as the panel
// sends it, plans the connection alone, restarts nothing and restates no key. An edit
// that restates the keys, the same or new, takes in every value that reads it, and
// each app restarts with it, so no plan says whether a guess at a key is right (ADR
// 0099).
func TestAConnectionChangeReachesOnlyWhatReadsIt(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	_, _, c, cat, _ := recheckSetup(t, raw)
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing site ended %v: %s", done.GetState(), done.GetError())
	}
	alone := func(what string, spec *resourcev1.ConnectionSpec) {
		t.Helper()
		ch := propose(t, c, what, connectionEdit("uploads", spec))
		// Nothing was restated, so it says nothing of the key it kept (the review's L3).
		if notes := notesOf(ch, "kept_key"); len(notes) != 0 {
			t.Errorf("%s says %v", what, notes)
		}
		for _, r := range ch.GetPlan().GetResources() {
			if r.GetResource().GetKind() != "connection" && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
				t.Errorf("%s plans %v for %s %s", what, r.GetAction(), r.GetResource().GetKind(), r.GetResource().GetName())
			}
			for _, f := range r.GetChanges() {
				if f.GetSensitive() {
					t.Errorf("%s restates %s", what, f.GetPath())
				}
			}
		}
		apply(t, c, ch)
	}
	// As the panel sends them: no keys, which the update keeps.
	shared := uploadsSpec(true)
	shared.S3.AccessKeyId, shared.S3.SecretKey = "", ""
	alone("Share uploads", shared)
	renamed := proto.CloneOf(shared)
	renamed.DisplayName = "Media library"
	alone("Rename uploads", renamed)

	restarts := func(what string, id, key string) {
		t.Helper()
		spec := proto.CloneOf(renamed)
		spec.S3.AccessKeyId, spec.S3.SecretKey = id, key
		ch := propose(t, c, what, connectionEdit("uploads", spec))
		for _, r := range ch.GetPlan().GetResources() {
			if r.GetResource().GetKind() == "service" && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
				discard(t, c, ch)
				return
			}
		}
		t.Errorf("%s restarts no app that reads it", what)
		discard(t, c, ch)
	}
	restarts("Restate the key", "AKIDEXAMPLE", connectionSecret)
	restarts("Replace the key", "AKIDROTATED", "rotated-"+connectionSecret)
}

// Undoing a change to a connection reaches its apps only where what they read
// changes back: undoing a new key restarts each app, as the new key did (the review's
// M1).
func TestUndoingANewKeyRestartsItsApps(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	_, _, c, cat, _ := recheckSetup(t, raw)
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing site ended %v: %s", done.GetState(), done.GetError())
	}
	rotated := uploadsSpec(false)
	rotated.S3.AccessKeyId, rotated.S3.SecretKey = "AKIDROTATED", "rotated-"+connectionSecret
	done := apply(t, c, propose(t, c, "Replace the key", connectionEdit("uploads", rotated)))
	res, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: done.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
			return
		}
	}
	t.Error("undoing a new key restarts no app that reads it")
}

// One app with two needs on a shared connection is one app: unsharing it plans, and
// a second app makes two (the review's L1, the re-check's N2).
func TestUnsharingCountsApps(t *testing.T) {
	t.Parallel()
	_, raw := ghostKeepingToAPrefixIn(t, false)
	f, b, c, _, _ := recheckSetup(t, raw)
	env := listedEnv(t, b)
	apply(t, c, propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true))))
	// The gallery reads two links to uploads, each under its own prefix: one app.
	files, media := appOn(orgID(t, f), env, "uploads", "files", "gallery/files/"), appOn(orgID(t, f), env, "uploads", "media", "gallery/media/")
	gallery := files[3]
	gallery.Resource.Name = "gallery"
	for name, from := range media[3].GetSpec().GetService().GetVariables() {
		gallery.GetSpec().GetService().Variables[name] = from
	}
	apply(t, c, propose(t, c, "Run the gallery", files[0], files[1], files[2], media[0], media[1], media[2], gallery))
	unshare := func() error {
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Unshare uploads", Edits: []*dockwellv1.Edit{connectionEdit("uploads", uploadsSpec(false))}}))
		return err
	}
	if err := unshare(); err != nil {
		t.Errorf("unsharing uploads, which one app uses for two needs: %v", err)
	}
	apply(t, c, propose(t, c, "Run notes", appOn(orgID(t, f), env, "uploads", "notes", "notes/")...))
	if err := unshare(); err == nil || !strings.Contains(err.Error(), "serves 2 apps") {
		t.Errorf("unsharing uploads, which the gallery and notes use: %v", err)
	}
}

// A plan that reads a connection, applied after the connection was renamed, is
// refused as stale in words that claim no wrong value, as a rename leaves what's read
// as it was (the review's L4).
func TestAPlanStaleAfterARenameSaysSoTruthfully(t *testing.T) {
	t.Parallel()
	raw := ghostKeepingToAPrefix(t, true)
	_, _, c, cat, _ := recheckSetup(t, raw)
	blog := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "blog"})
	renamed := uploadsSpec(false)
	renamed.DisplayName = "Media library"
	renamed.S3.AccessKeyId, renamed.S3.SecretKey = "", ""
	apply(t, c, propose(t, c, "Rename uploads", connectionEdit("uploads", renamed)))
	_, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: blog.GetId()}))
	if err == nil {
		w := watchWithin(t, c, blog.GetId(), 20*time.Second)
		done := waitWatchWithin(t, w, 20*time.Second).last()
		if done.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatal("blog's install, planned before uploads was renamed, applied")
		}
		err = errors.New(done.GetError())
	}
	if strings.Contains(err.Error(), "wrong value") || !strings.Contains(err.Error(), "connection uploads changed after") {
		t.Errorf("a plan stale after a rename: %v", err)
	}
}
