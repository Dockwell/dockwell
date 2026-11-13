// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// sourceEntry is an entry a source holds, of an ID and a release.
func sourceEntry(id string, release int) string {
	return strings.Replace(testEntry(release, "1."+string(rune('0'+release)), "nginx:1.27@sha256:"+strings.Repeat("a", 64), ""), "id: thing", "id: "+id, 1)
}

func notesOf(ch *dockwellv1.Change, reason string) []string {
	var out []string
	for _, n := range ch.GetPlan().GetNotes() {
		if n.GetReason() == reason {
			out = append(out, n.GetMessage())
		}
	}
	return out
}

// A catalog from a Git repository is a change with a plan, pinned by commit: its plan
// says what the commit holds and what Dockwell leaves out of it, its entries join
// the catalog once it's applied, a move to another commit says what that changes,
// and removing it takes them away (ADR 0055).
func TestACatalogFromGit(t *testing.T) { storetest.Each(t, testACatalogFromGit) }

func testACatalogFromGit(t *testing.T, backend store.Backend) {
	repo := gitfetchtest.New(t)
	first := repo.Commit(map[string]string{
		"README.md":              "Entries for Dockwell.\n",
		"entries/notes-src.yaml": sourceEntry("notes-src", 1),
		"entries/broken.yaml":    "id: broken\nname: Broken\n",
		"entries/postgres.yaml":  strings.Replace(sourceEntry("notes-src", 1), "id: notes-src", "id: postgres", 1),
		"entries/misnamed.yaml":  sourceEntry("other-name", 1),
		"elsewhere/ignored.yaml": sourceEntry("ignored", 1),
	})
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	pullsAnything(f.addDockerServer(t))
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	ids := func() []string {
		t.Helper()
		res, err := cat.ListCatalogEntries(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogEntriesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range res.Msg.GetEntries() {
			out = append(out, e.GetId())
		}
		return out
	}
	set := func(commit string) *dockwellv1.Change {
		t.Helper()
		res, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "community", Repository: repo.URL, Path: "entries", Commit: commit}))
		if err != nil {
			t.Fatalf("SetCatalogSource: %v", err)
		}
		return res.Msg.GetChange()
	}

	planned := set(first)
	if planned.GetSummary() != "Add the catalog community from "+repo.URL {
		t.Errorf("summary %q", planned.GetSummary())
	}
	if reads := notesOf(planned, "catalog_source_reads"); len(reads) != 1 || !strings.Contains(reads[0], "It reads 1 entry from the folder entries in "+repo.URL+", at "+first[:12]+": notes-src.") {
		t.Errorf("what it reads: %q", reads)
	}
	if at := notesOf(planned, "catalog_source_commit"); len(at) != 1 || at[0] != "It's the commit the branch main is at." {
		t.Errorf("which commit it is: %q", at)
	}
	for _, n := range planned.GetPlan().GetNotes() {
		if n.GetReason() == "catalog_source_commit" && !slices.Equal(n.GetNames(), []string{"refs/heads/main"}) {
			t.Errorf("the refs at it: %v", n.GetNames())
		}
	}
	refuses := strings.Join(notesOf(planned, "catalog_source_refuses"), "\n")
	for _, want := range []string{
		"It leaves out broken.yaml: ",
		"It leaves out postgres.yaml: Dockwell's own catalog has an entry of the ID postgres, which comes first. Give it another ID",
		"It leaves out misnamed.yaml: it's the entry other-name, in a file named for misnamed. Name the file other-name.yaml",
	} {
		if !strings.Contains(refuses, want) {
			t.Errorf("what it leaves out doesn't say %q:\n%s", want, refuses)
		}
	}
	if slices.Contains(ids(), "notes-src") {
		t.Fatal("a source's entry shows before its change is applied")
	}
	apply(t, c, planned)
	got := ids()
	if !slices.Contains(got, "notes-src") || slices.Contains(got, "ignored") || slices.Contains(got, "broken") || !slices.Contains(got, "postgres") {
		t.Errorf("the catalog after adding it: %v", got)
	}
	installing, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "notes-src"}))
	if err != nil || installing.Msg.GetChange().GetPlan() == nil {
		t.Errorf("installing a source's entry: %v", err)
	}

	second := repo.Commit(map[string]string{"entries/notes-src.yaml": sourceEntry("notes-src", 2), "entries/wiki-src.yaml": sourceEntry("wiki-src", 1)})
	repo.Commit(map[string]string{"README.md": "Entries for Dockwell, and more.\n"})
	moving := set(second)
	if at := notesOf(moving, "catalog_source_unnamed"); len(at) != 1 || !strings.Contains(at[0], "No branch or tag of "+repo.URL+" is at this commit now") {
		t.Errorf("a commit no branch is at: %q", at)
	}
	if moving.GetSummary() != "Move the catalog community from "+first[:12]+" to "+second[:12] {
		t.Errorf("summary %q", moving.GetSummary())
	}
	if adds := notesOf(moving, "catalog_source_adds"); len(adds) != 1 || adds[0] != "It adds wiki-src." {
		t.Errorf("what it adds: %q", adds)
	}
	if changes := notesOf(moving, "catalog_source_changes"); len(changes) != 1 || !strings.Contains(changes[0], "notes-src to 1.2") {
		t.Errorf("what it moves: %q", changes)
	}
	apply(t, c, moving)
	if got := ids(); !slices.Contains(got, "wiki-src") {
		t.Errorf("the catalog after moving it: %v", got)
	}
	sources, err := cat.ListCatalogSources(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogSourcesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	// Dockwell's own comes first, then community.
	if s := sources.Msg.GetSources(); len(s) != 2 || !s[0].GetOwn() || s[1].GetOwn() {
		t.Errorf("ListCatalogSources = %v", s)
	} else if s := s[1:]; s[0].GetName() != "community" || s[0].GetSource().GetCommit() != second ||
		!slices.Equal(s[0].GetEntries(), []string{"notes-src", "wiki-src"}) || len(s[0].GetRefused()) != 3 || s[0].GetUnread() != "" {
		t.Errorf("ListCatalogSources = %v", s)
	}

	removing, err := cat.RemoveCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.RemoveCatalogSourceRequest{Name: "community"}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, removing.Msg.GetChange())
	if got := ids(); slices.Contains(got, "notes-src") || slices.Contains(got, "wiki-src") {
		t.Errorf("the catalog after removing it: %v", got)
	}
}

// A source is a public repository's HTTPS URL at a commit in full: anything else is
// refused before anything is read.
func TestACatalogSourceIsPinnedByCommit(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	cat := catalogClient(b)
	for name, tc := range map[string]struct {
		req  *dockwellv1.SetCatalogSourceRequest
		want string
	}{
		"a branch":         {&dockwellv1.SetCatalogSourceRequest{Name: "c", Repository: "https://example.com/catalog", Commit: "main"}, "A branch moves"},
		"plain HTTP":       {&dockwellv1.SetCatalogSourceRequest{Name: "c", Repository: "http://example.com/catalog", Commit: strings.Repeat("a", 40)}, "isn't over HTTPS"},
		"credentials":      {&dockwellv1.SetCatalogSourceRequest{Name: "c", Repository: "https://u:p@example.com/catalog", Commit: strings.Repeat("a", 40)}, "holds credentials"},
		"a name of no use": {&dockwellv1.SetCatalogSourceRequest{Name: "Not A Name", Repository: "https://example.com/catalog", Commit: strings.Repeat("a", 40)}, "catalog source"},
		"a folder outside": {&dockwellv1.SetCatalogSourceRequest{Name: "c", Repository: "https://example.com/catalog", Path: "../x", Commit: strings.Repeat("a", 40)}, "folder is in its repository"},
	} {
		_, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(tc.req))
		if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if _, err := cat.RemoveCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.RemoveCatalogSourceRequest{Name: "none"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("removing a source there isn't: %v", err)
	}
}

// An install upgrades only from the catalog it came from, as its pin records it, not
// from whichever catalog has its ID now, and a source's plan says what changes hands
// (the first review of #332, its H4).
func TestAnInstallUpgradesFromWhereItCameFrom(t *testing.T) {
	zeta, aaa := gitfetchtest.New(t), gitfetchtest.New(t)
	// What zeta's file says of its source, Dockwell's own repository, is its own,
	// and counts for nothing.
	claims := sourceEntry("foo", 1)
	z1 := zeta.Commit(map[string]string{"foo.yaml": claims})
	stranger := strings.Replace(sourceEntry("foo", 9), "nginx:1.27@sha256:", "ghcr.io/stranger/evil@sha256:", 1)
	a1 := aaa.Commit(map[string]string{"foo.yaml": stranger})
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	pullsAnything(f.addDockerServer(t))
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	set := func(name, url, commit string) *dockwellv1.Change {
		t.Helper()
		res, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: name, Repository: url, Commit: commit}))
		if err != nil {
			t.Fatalf("SetCatalogSource: %v", err)
		}
		return res.Msg.GetChange()
	}
	installs := func() []*dockwellv1.CatalogInstall {
		t.Helper()
		res, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetInstalls()
	}

	apply(t, c, set("zeta", zeta.URL, z1))
	apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "foo"}))
	got := installs()
	if len(got) != 1 {
		t.Fatalf("installs: %v", got)
	}
	if pin := got[0].GetPin(); pin.GetCatalog() != "zeta" || pin.GetRepository() != zeta.URL || pin.GetCommit() != z1 || pin.GetPath() != "" {
		t.Errorf("the pin says it came from %q, %q at %q, not zeta's own", pin.GetCatalog(), pin.GetRepository(), pin.GetCommit())
	}

	// aaa comes before zeta, so its foo is the catalog's now: its plan says so, and
	// the install, zeta's, has no upgrade from it.
	taking := set("aaa", aaa.URL, a1)
	if takes := notesOf(taking, "catalog_source_takes"); len(takes) != 1 || !strings.Contains(takes[0], "foo from the catalog zeta") {
		t.Errorf("what aaa takes: %q", takes)
	}
	if strands := notesOf(taking, "catalog_source_strands"); len(strands) != 1 || !strings.Contains(strands[0], "foo, from the catalog zeta") {
		t.Errorf("what aaa leaves with no upgrade: %q", strands)
	}
	apply(t, c, taking)
	if got := installs(); len(got) != 1 || got[0].GetUpgrade() != nil {
		t.Errorf("after aaa: %v", got)
	}
	_, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "foo"}))
	if err == nil || !strings.Contains(err.Error(), "the catalog aaa's, another app") {
		t.Errorf("an upgrade from another catalog's foo: %v", err)
	}

	// Removing aaa gives zeta's foo back, and the install its upgrades.
	removing, err := cat.RemoveCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.RemoveCatalogSourceRequest{Name: "aaa"}))
	if err != nil {
		t.Fatal(err)
	}
	if gives := notesOf(removing.Msg.GetChange(), "catalog_source_gives"); len(gives) != 1 || !strings.Contains(gives[0], "foo comes from the catalog zeta") {
		t.Errorf("what removing aaa gives back: %q", gives)
	}
	if restores := notesOf(removing.Msg.GetChange(), "catalog_source_restores"); len(restores) != 1 || !strings.Contains(restores[0], "foo") {
		t.Errorf("whose upgrades come back: %q", restores)
	}
	apply(t, c, removing.Msg.GetChange())

	// Removing zeta leaves the install with no upgrade, saying so, and no other
	// catalog's foo is one.
	next := strings.Replace(sourceEntry("foo", 2), "nginx:1.27@sha256:"+strings.Repeat("a", 64), "nginx:1.28@sha256:"+strings.Repeat("b", 64), 1)
	z2 := zeta.Commit(map[string]string{"foo.yaml": next})
	apply(t, c, set("zeta", zeta.URL, z2))
	if got := installs(); len(got) != 1 || got[0].GetUpgrade() == nil {
		t.Errorf("zeta's next release isn't an upgrade of zeta's install: %v", got)
	}
	removing, err = cat.RemoveCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.RemoveCatalogSourceRequest{Name: "zeta"}))
	if err != nil {
		t.Fatal(err)
	}
	if strands := notesOf(removing.Msg.GetChange(), "catalog_source_strands"); len(strands) != 1 {
		t.Errorf("removing zeta: %q", strands)
	}
}

// A catalog source changes only through its own methods, which read its commit and
// say what it holds, not as a plain edit (the first review of #332, its M2).
func TestACatalogSourceIsNoPlainEdit(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Add a source",
		Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "catalog.source", Name: "c"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_CatalogSource{CatalogSource: &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: strings.Repeat("a", 40)}}},
		}},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "dockwell catalog set-source") {
		t.Errorf("a plain edit of a source: %v", err)
	}
}

// A source decides what anyone can install, and setting one reads the network
// when its plan is made, so setting and removing one are an admin's: a developer is
// refused before anything is read. Anyone with a role may list them (ADR 0049).
func TestACatalogSourceIsAnAdmins(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		repo := gitfetchtest.New(t)
		commit := repo.Commit(map[string]string{"entries/notes-src.yaml": sourceEntry("notes-src", 1)})
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		hold := aMember(t, f)
		cat := catalogClient(b)
		set := func() error {
			_, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "community", Repository: repo.URL, Path: "entries", Commit: commit}))
			return err
		}
		hold(onInstall(store.RoleDeveloper))
		for what, err := range map[string]error{
			"setting a source": set(),
			"removing a source": func() error {
				_, err := cat.RemoveCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.RemoveCatalogSourceRequest{Name: "community"}))
				return err
			}(),
		} {
			if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "needs_role" {
				t.Errorf("a developer %s: %v, want needs_role", what, err)
			}
		}
		if _, err := cat.ListCatalogSourceRefs(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogSourceRefsRequest{Repository: repo.URL})); connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "needs_role" {
			t.Errorf("a developer listing a repository's branches: %v, want needs_role", err)
		}
		hold(onInstall(store.RoleViewer))
		if _, err := cat.ListCatalogSources(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogSourcesRequest{})); err != nil {
			t.Errorf("a viewer listing sources: %v", err)
		}
		hold(onInstall(store.RoleAdmin))
		if err := set(); err != nil {
			t.Errorf("an admin setting a source: %v", err)
		}
	})
}

// A form checks a repository as it's typed: its branches and tags come back with
// the commit each is at, a repository that isn't there says so, and an address of
// this server is refused before anything is asked of it.
func TestARepositorysBranchesAreListed(t *testing.T) {
	repo := gitfetchtest.New(t)
	commit := repo.Commit(map[string]string{"entries/notes-src.yaml": sourceEntry("notes-src", 1)})
	f := startFull(t, store.SQLite, nil)
	cat := catalogClient(signedIn(t, f))
	refs := func(url string) (*dockwellv1.ListCatalogSourceRefsResponse, error) {
		res, err := cat.ListCatalogSourceRefs(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogSourceRefsRequest{Repository: url}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	got, err := refs(repo.URL)
	if err != nil || len(got.GetRefs()) != 1 || got.GetRefs()[0].GetName() != "refs/heads/main" || got.GetRefs()[0].GetCommit() != commit || got.GetMore() || got.GetHead() != "refs/heads/main" {
		t.Errorf("a repository's branches: %v, %v", got, err)
	}
	if _, err := refs(repo.URL + "-missing"); connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "unreadable" {
		t.Errorf("a repository that isn't there: %v", err)
	}
	if _, err := refs("https://127.0.0.1/catalog"); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "this server's own address") {
		t.Errorf("this server's own address: %v", err)
	}
	if _, err := refs("http://example.com/catalog"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("plain HTTP: %v", err)
	}
}
