// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// asBeforeTheBranch rewrites the latest revision of each link to uploads without
// its KEY_PREFIX set, and each value of its prefix as a value of its own, as an install
// made before links recorded the prefix left them.
func asBeforeTheBranch(t *testing.T, f *fullServer) {
	t.Helper()
	now := time.Now()
	change := id.New(id.Change)
	err := f.store.Revise(t.Context(), func(tx *store.RevisionTx) error {
		if err := tx.CreateChange(t.Context(), store.Change{ID: change, Summary: "as before", Actor: revision.SystemActor, State: store.ChangeApplied, Fingerprint: "seeded", Plan: []byte("{}"), CreatedAt: now}); err != nil {
			return err
		}
		res, err := tx.ListKind(t.Context(), "link")
		if err != nil {
			return err
		}
		pos := 0
		prefixes := map[string]string{}
		for _, r := range res {
			msg, err := f.kinds.Decode(resource.Kind(r.Kind), r.Spec)
			if err != nil {
				return err
			}
			l := proto.Clone(msg).(*resourcev1.Link)
			if l.GetConnection() != "uploads" {
				continue
			}
			var sets []*resourcev1.LinkSet
			for _, s := range l.GetSets() {
				if s.GetPart() != resourcev1.LinkPart_LINK_PART_KEY_PREFIX {
					sets = append(sets, s)
				}
			}
			l.Sets = sets
			spec, err := resource.Encode(l)
			if err != nil {
				return err
			}
			if _, err := tx.AddRevision(t.Context(), store.NewRevision{Kind: r.Kind, Scope: r.Scope, Name: r.Name, Spec: spec, Change: change, Position: pos, At: now}); err != nil {
				return err
			}
			pos++
			prefixes[r.Scope+"/"+r.Name] = l.GetPrefix()
		}
		// Each value of a prefix was a value of its own then, not read through the link.
		vars, err := tx.ListKind(t.Context(), "variable")
		if err != nil {
			return err
		}
		for _, r := range vars {
			msg, err := f.kinds.Decode(resource.Kind(r.Kind), r.Spec)
			if err != nil {
				return err
			}
			from := msg.(*resourcev1.Variable).GetFrom().GetConnection()
			prefix, ok := prefixes[r.Scope+"/"+from.GetLink()]
			if from.GetPart() != resourcev1.LinkPart_LINK_PART_KEY_PREFIX || !ok {
				continue
			}
			spec, err := resource.Encode(&resourcev1.Variable{Value: prefix})
			if err != nil {
				return err
			}
			if _, err := tx.AddRevision(t.Context(), store.NewRevision{Kind: r.Kind, Scope: r.Scope, Name: r.Name, Spec: spec, Change: change, Position: pos, At: now}); err != nil {
				return err
			}
			pos++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Two catalog apps that keep to their prefixes, on one connection, from before the
// branch, which their links don't record: an upgrade of either records it and plans
// (the re-check's N1).
func TestAnUpgradeRecordsThePrefixOfAnInstallFromBefore(t *testing.T) {
	t.Parallel()
	dir, raw := ghostKeepingToAPrefixIn(t, false)
	f, _, c, cat, _ := recheckSetup(t, raw)
	apply(t, c, propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true))))
	for _, p := range []string{"site", "blog"} {
		if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: p, Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("installing %s ended %v: %s", p, done.GetState(), done.GetError())
		}
	}
	asBeforeTheBranch(t, f)
	recheckBump(t, dir, string(raw))
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "blog", SkipBackup: true}))
	if err != nil {
		t.Errorf("upgrade of blog, installed before the branch beside site: %v", err)
		return
	}
	if done := apply(t, c, res.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Errorf("upgrade ended %v: %s", done.GetState(), done.GetError())
	}
}

// The link owns the prefix's value: the value reads it through the link, so an edit
// of the value apart from the link is refused, and a change to the link's prefix
// reaches the app in the same plan (the re-check's N3).
func TestAPrefixsValueMovesWithItsLinkAlone(t *testing.T) {
	t.Parallel()
	_, raw := ghostKeepingToAPrefixIn(t, false)
	f, _, c, cat, e := recheckSetup(t, raw)
	apply(t, c, propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true))))
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing site ended %v: %s", done.GetState(), done.GetError())
	}
	env := installEnv(t, cat, "site")
	ghostEnv := func() []string {
		t.Helper()
		for _, name := range slices.Backward(e.Containers()) {
			if strings.Contains(name, "ghost") {
				got, err := e.Client().InspectContainer(t.Context(), name)
				if err != nil {
					t.Fatal(err)
				}
				return got.Config.Env
			}
		}
		t.Fatal("Ghost doesn't run")
		return nil
	}
	if got := ghostEnv(); !slices.Contains(got, "storage__s3__prefix=site/") {
		t.Fatalf("Ghost runs without its prefix: %v", got)
	}

	// An ordinary edit of the value, to the root.
	edit := variableSpec("S3_PREFIX", "")
	edit.Resource.Scope, edit.Resource.Kind = env, "variable"
	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Prefix to root", Edits: []*dockwellv1.Edit{edit}}))
	if err == nil || !strings.Contains(err.Error(), "S3_PREFIX is the setting that tells the app of link storage its prefix, which the link writes, so it changes with the link. Change the prefix of link storage instead") {
		t.Errorf("an edit of site's S3_PREFIX apart from its link: %v", err)
	}

	// The link's prefix moves, and the app with it.
	var l resourcev1.Link
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), "link", env, "storage")
		if err != nil {
			return err
		}
		return proto.Unmarshal(r.Spec, &l)
	}); err != nil {
		t.Fatal(err)
	}
	l.Prefix = "site-2/"
	moved := propose(t, c, "Move site's prefix", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: env, Name: "storage"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &l}},
	})
	follows := false
	for _, r := range moved.GetPlan().GetResources() {
		follows = follows || r.GetResource().GetName() == "S3_PREFIX" && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE
	}
	if !follows {
		t.Errorf("moving the link's prefix takes no S3_PREFIX in: %v", moved.GetPlan().GetResources())
	}
	if done := apply(t, c, moved); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("moving site's prefix ended %v: %s", done.GetState(), done.GetError())
	}
	if got := ghostEnv(); !slices.Contains(got, "storage__s3__prefix=site-2/") {
		t.Errorf("after the move, Ghost runs with %v", got)
	}
}

// A link that records the name of its prefix must have an app that reads it, and that
// name's value must read the link's prefix (the re-check's N3).
func TestALinksPrefixNameIsReadAndReadsTheLink(t *testing.T) {
	t.Parallel()
	_, raw := ghostKeepingToAPrefixIn(t, false)
	f, b, c, _, _ := recheckSetup(t, raw)
	env := listedEnv(t, b)
	apply(t, c, propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true))))
	app := appOn(orgID(t, f), env, "uploads", "notes", "notes/")
	refused := func(what string, edits []*dockwellv1.Edit, want string) {
		t.Helper()
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: what, Edits: edits}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", what, err, want)
		}
	}
	// No service reads S3_PREFIX_NOTES.
	refused("A prefix nobody reads", []*dockwellv1.Edit{app[0], app[1], app[2]},
		"link notes names S3_PREFIX_NOTES as the setting that tells its app the prefix, and no app in this environment reads S3_PREFIX_NOTES")
	// Its value is a value of its own.
	plain := variableSpec("S3_PREFIX_NOTES", "notes/")
	plain.Resource.Scope, plain.Resource.Kind = env, "variable"
	refused("A prefix of its own", []*dockwellv1.Edit{app[0], app[1], plain, app[3]},
		"link notes names S3_PREFIX_NOTES as the setting that tells its app the prefix, which the link writes. Set S3_PREFIX_NOTES to read the link's prefix")
	apply(t, c, propose(t, c, "Run notes", app...))
}

// prefixRun is the storage prefix project's Ghost runs with, or "".
func prefixRun(t *testing.T, e *dockertest.Engine, project string) string {
	t.Helper()
	out := ""
	for _, name := range e.Containers() {
		if !strings.Contains(name, "ghost") || !strings.Contains(name, project) {
			continue
		}
		got, err := e.Client().InspectContainer(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		for _, kv := range got.Config.Env {
			if v, ok := strings.CutPrefix(kv, "storage__s3__prefix="); ok {
				out = v
			}
		}
	}
	return out
}

// An install from before whose prefix a person changed by hand keeps it through the
// upgrade that makes its link name the setting, by hand or as an automatic update
// takes it: the link takes the setting's prefix, says so, and is checked against the
// other apps as any new prefix (#772's last check, C1).
func TestAnUpgradeKeepsAPrefixSetByHand(t *testing.T) {
	t.Parallel()
	dir, raw := ghostKeepingToAPrefixIn(t, false)
	f, b, c, cat, e := recheckSetup(t, raw)
	apply(t, c, propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true))))
	for _, p := range []string{"site", "blog"} {
		if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: p, Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("installing %s ended %v: %s", p, done.GetState(), done.GetError())
		}
	}
	asBeforeTheBranch(t, f)
	byHand := func(project, prefix string) {
		t.Helper()
		edit := variableSpec("S3_PREFIX", prefix)
		edit.Resource.Scope, edit.Resource.Kind = installEnv(t, cat, project), "variable"
		apply(t, c, propose(t, c, "Prefix by hand", edit))
	}
	byHand("site", "site-old/")
	recheckBump(t, dir, string(raw))

	// By hand: the upgrade keeps site-old/, and says so.
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "site", SkipBackup: true}))
	if err != nil {
		t.Fatalf("site's upgrade: %v", err)
	}
	if notes := notesOf(res.Msg.GetChange(), catalogapi.PrefixKeptNote); len(notes) != 1 ||
		notes[0] != "site keeps its files under site-old/, as its S3_PREFIX says, and its link storage keeps to that prefix from now on, in place of site/" {
		t.Errorf("site's upgrade says %v", notes)
	}
	if done := apply(t, c, res.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("site's upgrade ended %v: %s", done.GetState(), done.GetError())
	}
	if got := prefixRun(t, e, "site"); got != "site-old/" {
		t.Errorf("after its upgrade, site's Ghost keeps its files under %q, want site-old/", got)
	}

	// Through the service, as an automatic update upgrades it: a prefix another app
	// keeps to is refused, and one of its own is kept.
	s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	blogGhost := func() string {
		t.Helper()
		list, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, svc := range list.Msg.GetServices() {
			if svc.GetResource().GetName() == "ghost" && svc.GetResource().GetScope() == installEnv(t, cat, "blog") {
				return svc.GetId()
			}
		}
		t.Fatal("blog runs no Ghost")
		return ""
	}
	byHand("blog", "site-old/")
	if _, err := s.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: blogGhost(), SkipBackup: true})); err == nil ||
		!strings.Contains(err.Error(), "keeps its files under site-old/ on the connection uploads, where another app keeps its files already") {
		t.Errorf("blog's upgrade onto site's prefix: %v", err)
	}
	byHand("blog", "blog-old/")
	up, err := s.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: blogGhost(), SkipBackup: true}))
	if err != nil {
		t.Fatalf("blog's upgrade: %v", err)
	}
	if done := apply(t, c, up.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("blog's upgrade ended %v: %s", done.GetState(), done.GetError())
	}
	if got := prefixRun(t, e, "blog"); got != "blog-old/" {
		t.Errorf("after its upgrade, blog's Ghost keeps its files under %q, want blog-old/", got)
	}
}

// Undoing the upgrade that recorded the prefix of an install from before, beside
// another app from before, puts the link back as it was accepted, and plans (#772's
// last check, C2).
func TestUndoingTheUpgradeThatRecordedThePrefix(t *testing.T) {
	t.Parallel()
	dir, raw := ghostKeepingToAPrefixIn(t, false)
	f, _, c, cat, e := recheckSetup(t, raw)
	apply(t, c, propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true))))
	for _, p := range []string{"site", "blog"} {
		if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: p, Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("installing %s ended %v: %s", p, done.GetState(), done.GetError())
		}
	}
	asBeforeTheBranch(t, f)
	recheckBump(t, dir, string(raw))
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "blog", SkipBackup: true}))
	if err != nil {
		t.Fatalf("blog's upgrade: %v", err)
	}
	up := apply(t, c, res.Msg.GetChange())
	if up.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("blog's upgrade ended %v: %s", up.GetState(), up.GetError())
	}
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: up.GetId()}))
	if err != nil {
		t.Fatalf("undoing blog's upgrade: %v", err)
	}
	if done := apply(t, c, undo.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("undoing blog's upgrade ended %v: %s", done.GetState(), done.GetError())
	}
	if got := prefixRun(t, e, "blog"); got != "blog/" {
		t.Errorf("after the undo, blog's Ghost keeps its files under %q, want blog/", got)
	}
}

// A service edit is held to what a link change is: one that stops reading its
// link's prefix setting is refused, so it can't step off its link and back to let a
// second app onto an unshared connection; a second service that starts reading a
// link is a second app; and one reading a link's names reads its prefix setting
// (#772's last check, C3).
func TestAServiceEditIsHeldAsALinkIs(t *testing.T) {
	t.Parallel()
	_, raw := ghostKeepingToAPrefixIn(t, false)
	f, b, c, _, _ := recheckSetup(t, raw)
	env := listedEnv(t, b)
	notes := recheckApp(t, f, env, "notes", "notes/")
	apply(t, c, propose(t, c, "Run notes", notes...))
	refused := func(what, want string, edits ...*dockwellv1.Edit) {
		t.Helper()
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: what, Edits: edits}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", what, err, want)
		}
	}
	// P4: notes steps off its link, which would let gallery in.
	quiet := proto.CloneOf(notes[3])
	quiet.Spec.GetService().Variables = map[string]string{"OTHER": "OTHER"}
	other := variableSpec("OTHER", "x")
	other.Resource.Scope, other.Resource.Kind = env, "variable"
	refused("Stop reading", "notes stops reading S3_PREFIX_NOTES, the setting that tells it the prefix of link notes, so it would keep its files at the bucket's root. Remove the link first, or keep reading it", other, quiet)
	// Reading the bucket but not the prefix setting.
	half := proto.CloneOf(notes[3])
	half.Spec.GetService().Variables = map[string]string{"S3_BUCKET_NOTES": "S3_BUCKET_NOTES"}
	refused("Read half", "notes stops reading S3_PREFIX_NOTES", half)
	// P5: a second service reading notes' own link is a second app.
	worker := proto.CloneOf(notes[3])
	worker.Resource.Name = "notes-worker"
	refused("A worker", "the connection uploads serves another app already, and isn't shared. Give notes-worker a connection of its own, or have an admin share uploads", worker)
	// A service that reads the link's names and not its prefix setting, joining.
	partial := proto.CloneOf(worker)
	partial.Spec.GetService().Variables = map[string]string{"S3_BUCKET_NOTES": "S3_BUCKET_NOTES"}
	refused("A worker that ignores the prefix", "notes-worker reads the names of link notes but not S3_PREFIX_NOTES, the setting that tells it the prefix", partial)
	// Shared, the worker plans.
	apply(t, c, propose(t, c, "Share uploads", connectionEdit("uploads", uploadsSpec(true))))
	apply(t, c, propose(t, c, "A worker", worker))
}
