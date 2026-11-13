// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
	"github.com/dockwell/dockwell/internal/store"
)

// A store's app shows the store's own logo and words: the logo is read only for the
// apps that convert, in a round of its own, kept as svgclean writes it, and served
// as an image alone; the catalog lists the app without its README, which its page
// has; and the stores Dockwell reads list what this build measured of each, with no
// source of one until a source reads the store's own repository (ADR 0083).
func TestAStoresAppShowsItsOwnLogoAndWords(t *testing.T) {
	repo := gitfetchtest.New(t)
	repo.Filter()
	board := strings.Replace(boardTemplate, "# port: 80\n", "# port: 80\n# logo: svgs/board.svg\n", 1)
	head := repo.Commit(map[string]string{
		"LICENSE":                       "                                 Apache License\n                           Version 2.0, January 2004\n",
		"templates/compose/board.yaml":  board,
		"templates/compose/moving.yaml": strings.Replace(board, "nginx:1.27", "nginx:latest", 1),
		"public/svgs/board.svg":         `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 2 2"><script>alert(1)</script><rect width="2" height="2" onclick="x()"/></svg>`,
		"public/svgs/unused.svg":        `<svg xmlns="http://www.w3.org/2000/svg"/>`,
	})
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.CatalogSources = t.TempDir() })
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	res, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "coolify", Repository: repo.URL, Commit: head}))
	if err != nil {
		t.Fatalf("SetCatalogSource: %v", err)
	}
	apply(t, c, res.Msg.GetChange())

	entries, err := cat.ListCatalogEntries(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogEntriesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(entries.Msg.GetEntries(), func(e *catalogv1.Entry) bool { return e.GetId() == "board" })
	if i < 0 {
		t.Fatal("the catalog has no board")
	}
	icon := entries.Msg.GetEntries()[i].GetIcon()
	if !strings.HasPrefix(icon, "/catalog-media/") || !strings.HasSuffix(icon, "/"+head+"/board.svg") {
		t.Fatalf("board's icon is %q", icon)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, b.origin+icon, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 2 2"><rect width="2" height="2"/></svg>` ||
		resp.Header.Get("Content-Type") != "image/svg+xml" || resp.Header.Get("Content-Disposition") != "attachment" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("board's logo: %d %q %v", resp.StatusCode, body, resp.Header)
	}

	stores, err := cat.ListCatalogStores(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogStoresRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range stores.Msg.GetStores() {
		ids = append(ids, s.GetId())
		if s.GetSource() != "" || s.GetLive() || s.GetApps() == 0 || s.GetInstalls() == 0 || len(s.GetCommit()) != 40 || s.GetBranch() == "" ||
			!strings.HasPrefix(s.GetRepository(), "https://github.com/") || s.GetLicense() == "" || s.GetFormat() == resourcev1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED {
			t.Errorf("a store no source reads at its own repository: %v", s)
		}
	}
	// Umbrel's reads from a link, but isn't one a person adds in one click (ADR 0083).
	if len(ids) != 5 || !slices.Contains(ids, "onepanel") || !slices.Contains(ids, "dokploy") || !slices.Contains(ids, "coolify") ||
		!slices.Contains(ids, "runtipi") || !slices.Contains(ids, "casaos") || slices.Contains(ids, "umbrel") {
		t.Errorf("the stores are %v", ids)
	}
	// A repository laid out as Coolify's is no store of Dockwell's list.
	if s := communitySourceNamed(t, cat, "coolify"); s.GetStore() != "" {
		t.Errorf("a copy of Coolify's layout reads as the store %q", s.GetStore())
	}
}

// communitySourceNamed is a source a test adds, by its name.
func communitySourceNamed(t *testing.T, cat dockwellv1connect.CatalogServiceClient, name string) *dockwellv1.CatalogSourceInfo {
	t.Helper()
	res, err := cat.ListCatalogSources(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogSourcesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Msg.GetSources() {
		if s.GetName() == name {
			return s
		}
	}
	t.Fatalf("no source %s", name)
	return nil
}

// A store's screenshots, where it keeps them in its own repository, come with its
// app: CasaOS links them through jsDelivr, and they're read from the commit, raster
// alone, with a caption and the address the store gave each, and served as any
// source's image (ADR 0083).
func TestAStoresScreenshotsComeWithItsApp(t *testing.T) {
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01"
	link := func(file string) string {
		return "https://cdn.jsdelivr.net/gh/IceWhaleTech/CasaOS-AppStore@main/Apps/Board/" + file
	}
	compose := `name: board
services:
  board:
    image: nginx:1.27
    ports:
      - target: 80
        published: "8124"
x-casaos:
  architectures: [amd64, arm64]
  main: board
  title:
    en_US: Board
  tagline:
    en_US: A board for notes
  icon: ` + link("icon.svg") + `
  screenshot_link:
    - ` + link("screenshot-1.png") + `
    - ` + link("screenshot-2.svg") + `
    - ` + link("screenshot-3.png") + `
  port_map: "8124"
`
	repo := gitfetchtest.New(t)
	repo.Filter()
	head := repo.Commit(map[string]string{
		"LICENSE":                        "                                 Apache License\n                           Version 2.0, January 2004\n",
		"Apps/Board/docker-compose.yml":  compose,
		"Apps/Board/icon.svg":            `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 2 2"><rect width="2" height="2"/></svg>`,
		"Apps/Board/screenshot-1.png":    png,
		"Apps/Board/screenshot-2.svg":    `<svg xmlns="http://www.w3.org/2000/svg"/>`,
		"Apps/Board/screenshot-3.png":    png + "3",
		"Apps/Board/not-linked-shot.png": png,
	})
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.CatalogSources = t.TempDir() })
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	res, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "casaos", Repository: repo.URL, Commit: head}))
	if err != nil {
		t.Fatalf("SetCatalogSource: %v", err)
	}
	apply(t, c, res.Msg.GetChange())
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "board"}))
	if err != nil {
		t.Fatal(err)
	}
	e := got.Msg.GetEntry()
	if !strings.HasSuffix(e.GetIcon(), "/"+head+"/board.svg") {
		t.Errorf("board's icon is %q", e.GetIcon())
	}
	shots := e.GetScreenshots()
	if len(shots) != 2 {
		t.Fatalf("board's screenshots = %v, want the two raster images it links", shots)
	}
	for i, n := range []string{"1", "3"} {
		s := shots[i]
		if !strings.HasSuffix(s.GetPath(), "/"+head+"/board-"+n+".png") || s.GetSource() != link("screenshot-"+n+".png") || s.GetCaption() == "" {
			t.Errorf("screenshot %d = %v", i, s)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, b.origin+shots[0].GetPath(), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || string(body) != png {
		t.Errorf("the first screenshot: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
