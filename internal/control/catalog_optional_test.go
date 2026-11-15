// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// An entry with an optional bucket installs without one: the sheet offers it unset
// alone, which is its default, and the app runs on its own disk, its storage names
// out of its environment and the one that says so at the app's word for unmet
// (ADR 0099).
func TestAnOptionalNeedInstallsWithoutIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	useCatalogDir(t, dir)
	raw, err := os.ReadFile(filepath.Join("testdata", "ghost-storage.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	cat := catalogClient(b)

	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "ghost"}))
	if err != nil {
		t.Fatal(err)
	}
	var storage []string
	for _, o := range got.Msg.GetNeeds() {
		if o.GetNeed() == "storage" {
			storage = append(storage, o.GetChoice())
		}
	}
	if !slices.Equal(storage, []string{catalog.ChoiceUnset}) {
		t.Fatalf("the sheet offers storage as %v, want unset alone", storage)
	}

	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site"})
	if done := apply(t, changeClient(b, b.origin), installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Ghost ended %v: %s", done.GetState(), done.GetError())
	}
	var env []string
	for _, name := range e.Containers() {
		if strings.Contains(name, "ghost") {
			c, err := e.Client().InspectContainer(t.Context(), name)
			if err != nil {
				t.Fatal(err)
			}
			env = c.Config.Env
		}
	}
	if !slices.Contains(env, "storage__active=local") {
		t.Errorf("Ghost runs without its switch at local: %v", env)
	}
	for _, v := range env {
		if strings.HasPrefix(v, "storage__s3__") {
			t.Errorf("Ghost runs with %s, which its unset storage leaves out", v)
		}
	}
	// A choice to leave it unset is taken too, and a choice it doesn't have isn't.
	if _, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{
		Id: "ghost", Project: "blog", Needs: map[string]string{"storage": "new"},
	})); err == nil || !strings.Contains(err.Error(), "takes unset") {
		t.Errorf("asking for a new bucket: %v", err)
	}
}
