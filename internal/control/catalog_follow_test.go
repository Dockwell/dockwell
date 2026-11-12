// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A source that follows a branch moves along it by Dockwell's own change, applied
// at once, with the notes a person's move has. Undoing the move puts the old commit
// back and keeps it there until the branch moves again; a commit whose entries all
// fail leaves the source where it is, saying why; and a pinned source has no branch
// to look at (ADR 0077).
func TestACatalogFollowsItsBranch(t *testing.T) { storetest.Each(t, testACatalogFollowsItsBranch) }

func testACatalogFollowsItsBranch(t *testing.T, backend store.Backend) {
	repo := gitfetchtest.New(t)
	first := repo.Commit(map[string]string{"entries/notes-src.yaml": sourceEntry("notes-src", 1)})
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	source := func() *dockwellv1.CatalogSourceInfo {
		t.Helper()
		res, err := cat.ListCatalogSources(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogSourcesRequest{}))
		if err != nil || len(res.Msg.GetSources()) != 1 {
			t.Fatalf("ListCatalogSources: %v, %v", res, err)
		}
		return res.Msg.GetSources()[0]
	}
	check := func() *dockwellv1.CheckCatalogSourceResponse {
		t.Helper()
		res, err := cat.CheckCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.CheckCatalogSourceRequest{Name: "community"}))
		if err != nil {
			t.Fatalf("CheckCatalogSource: %v", err)
		}
		return res.Msg
	}

	// Following a branch with no commit named is the commit it's at now.
	res, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "community", Repository: repo.URL, Path: "entries", Follow: "main"}))
	if err != nil {
		t.Fatal(err)
	}
	planned := res.Msg.GetChange()
	if follows := notesOf(planned, "catalog_source_follows"); len(follows) != 1 || !strings.Contains(follows[0], "It follows the branch main") {
		t.Errorf("what following says: %q", follows)
	}
	apply(t, c, planned)
	if s := source(); s.GetSource().GetCommit() != first || s.GetSource().GetFollow() != "main" {
		t.Fatalf("the source after adding it: %v", s.GetSource())
	}
	if got := check(); got.GetChange() != nil || got.GetSource().GetProblem() != "" || got.GetSource().GetBranchCommit() != first || got.GetSource().GetChecked() == nil {
		t.Errorf("a look with nothing new: %v", got)
	}

	// The branch moves: Dockwell moves the source, as its own change, applied.
	second := repo.Commit(map[string]string{"entries/notes-src.yaml": sourceEntry("notes-src", 2), "entries/wiki-src.yaml": sourceEntry("wiki-src", 1)})
	got := check()
	moved := got.GetChange()
	if moved == nil {
		t.Fatalf("the branch moved and the source didn't: %v", got.GetSource())
	}
	if moved.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED || moved.GetProposedBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL {
		t.Errorf("the move is %v, by %v", moved.GetState(), moved.GetProposedBy())
	}
	if want := "Move the catalog community from " + first[:12] + " to " + second[:12] + ", as main moved"; moved.GetSummary() != want {
		t.Errorf("summary %q, want %q", moved.GetSummary(), want)
	}
	for reason, want := range map[string]string{
		"catalog_source_adds":     "It adds wiki-src.",
		"catalog_source_changes":  "notes-src to 1.2",
		"catalog_source_commit":   "It's the commit the branch main is at.",
		"catalog_source_followed": "What's installed doesn't change",
	} {
		if notes := notesOf(moved, reason); len(notes) != 1 || !strings.Contains(notes[0], want) {
			t.Errorf("%s: %q, want %q", reason, notes, want)
		}
	}
	if s := source(); s.GetSource().GetCommit() != second || !slices.Equal(s.GetEntries(), []string{"notes-src", "wiki-src"}) {
		t.Errorf("after the move: %v", s)
	}
	// It's on Activity, though Dockwell made it, as what can be installed moved.
	activity := listEvents(t, dockwellv1connect.NewEventServiceClient(b, b.origin), &dockwellv1.ListEventsRequest{})
	if !slices.ContainsFunc(activity, func(e *dockwellv1.Event) bool { return e.GetId() == moved.GetId() }) {
		t.Errorf("Dockwell's move isn't on Activity: %v", activity)
	}

	// Undoing the move puts the old commit back, and a person's look moves it
	// again, as they ask for the branch's commit.
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: moved.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, undo.Msg.GetChange())
	if s := source(); s.GetSource().GetCommit() != first || s.GetSource().GetFollow() != "main" {
		t.Errorf("after the undo: %v", s.GetSource())
	}

	// A commit whose entries all fail leaves the source where it is, and says why.
	broken := repo.Commit(map[string]string{"entries/notes-src.yaml": "id: notes-src\nname: Broken\n", "entries/wiki-src.yaml": ""})
	got = check()
	if got.GetChange() != nil || got.GetSource().GetSource().GetCommit() != first || got.GetSource().GetBranchCommit() != broken ||
		!strings.Contains(got.GetSource().GetProblem(), "None of the entries at "+broken[:12]+", where main is now, pass Dockwell's checks") ||
		!strings.Contains(got.GetSource().GetProblem(), "It stays at "+first[:12]) {
		t.Errorf("a broken commit: %v", got)
	}
	if got.GetSource().GetNextCheck() == nil {
		t.Error("a failed look says no time for the next")
	}

	// Pinned, there's no branch to look at.
	res, err = cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "community", Repository: repo.URL, Path: "entries", Commit: first}))
	if err != nil {
		t.Fatal(err)
	}
	if pinned := notesOf(res.Msg.GetChange(), "catalog_source_pinned"); len(pinned) != 1 || res.Msg.GetChange().GetSummary() != "Pin the catalog community at "+first[:12] {
		t.Errorf("pinning: %q, %q", res.Msg.GetChange().GetSummary(), pinned)
	}
	apply(t, c, res.Msg.GetChange())
	if _, err := cat.CheckCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.CheckCatalogSourceRequest{Name: "community"})); connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "not_following" {
		t.Errorf("looking at a pinned source: %v", err)
	}
	if _, err := cat.CheckCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.CheckCatalogSourceRequest{Name: "none"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("looking at a source there isn't: %v", err)
	}
}

// The control plane looks at each following source on its own, and moves it; once a
// person undid a move, the look leaves the source where they put it until the branch
// moves on (ADR 0077).
func TestACatalogFollowsOnItsOwn(t *testing.T) {
	every, first, tick := catalogapi.FollowEvery, catalogapi.FollowFirst, catalogapi.FollowTick
	catalogapi.FollowEvery, catalogapi.FollowFirst, catalogapi.FollowTick = 50*time.Millisecond, 0, 20*time.Millisecond
	t.Cleanup(func() { catalogapi.FollowEvery, catalogapi.FollowFirst, catalogapi.FollowTick = every, first, tick })
	repo := gitfetchtest.New(t)
	one := repo.Commit(map[string]string{"notes-src.yaml": sourceEntry("notes-src", 1)})
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.CatalogFollow = true })
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	source := func() *dockwellv1.CatalogSourceInfo {
		t.Helper()
		res, err := cat.ListCatalogSources(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogSourcesRequest{}))
		if err != nil || len(res.Msg.GetSources()) != 1 {
			t.Fatalf("ListCatalogSources: %v, %v", res, err)
		}
		return res.Msg.GetSources()[0]
	}
	res, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "community", Repository: repo.URL, Follow: "main"}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, res.Msg.GetChange())
	two := repo.Commit(map[string]string{"notes-src.yaml": sourceEntry("notes-src", 2)})
	waitFor(t, "the source to follow main to its second commit", func() bool { return source().GetSource().GetCommit() == two })

	// Undo Dockwell's move, then wait for a look after it: the source stays.
	list, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var moved *dockwellv1.Change
	for _, ch := range list.Msg.GetChanges() {
		if moved == nil && strings.HasSuffix(ch.GetSummary(), ", as main moved") {
			moved = ch
		}
	}
	if moved == nil {
		t.Fatal("no change moved the source")
	}
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: moved.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, undo.Msg.GetChange())
	undone := time.Now()
	waitFor(t, "a look after the undo", func() bool {
		s := source()
		return s.GetChecked() != nil && s.GetChecked().AsTime().After(undone.Add(100*time.Millisecond))
	})
	if s := source(); s.GetSource().GetCommit() != one {
		t.Errorf("a look moved the source back to the commit a person undid: %v", s.GetSource())
	}
	three := repo.Commit(map[string]string{"notes-src.yaml": sourceEntry("notes-src", 3)})
	waitFor(t, "the source to follow main to its third commit", func() bool { return source().GetSource().GetCommit() == three })
}

// A source's images are served by the control plane from the commit it keeps, to a
// signed-in person, and the catalog names them so (ADR 0077).
func TestACatalogSourcesImagesAreServed(t *testing.T) {
	repo := gitfetchtest.New(t)
	entry := strings.Replace(sourceEntry("notes-src", 1), "name: Thing\n", "name: Thing\nicon: media/notes-icon.png\n", 1)
	icon := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	commit := repo.Commit(map[string]string{"entries/notes-src.yaml": entry, "entries/media/notes-icon.png": icon, "entries/media/notes.svg": "<svg/>"})
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.CatalogSources = t.TempDir() })
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	res, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "community", Repository: repo.URL, Path: "entries", Commit: commit}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, res.Msg.GetChange())
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "notes-src"}))
	if err != nil {
		t.Fatal(err)
	}
	path := got.Msg.GetEntry().GetIcon()
	if !strings.HasPrefix(path, "/catalog-media/") || !strings.HasSuffix(path, "/"+commit+"/notes-icon.png") {
		t.Fatalf("the entry's icon is %q", path)
	}
	// fetch says what an image's request answers: its status, type, nosniff and body.
	fetch := func(client *http.Client) (int, string, string, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, b.origin+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("X-Content-Type-Options"), string(body)
	}
	if status, kind, sniff, body := fetch(b.client); status != http.StatusOK || body != icon || kind != "image/png" || sniff != "nosniff" {
		t.Errorf("signed in: %d %q %q %q", status, kind, sniff, body)
	}
	signedOut := newBrowser(t, b.origin, f.api, nil)
	if status, _, _, _ := fetch(signedOut.client); status != http.StatusUnauthorized {
		t.Errorf("signed out: %d", status)
	}
}
