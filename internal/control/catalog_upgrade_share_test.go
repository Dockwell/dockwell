// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// A server another app's install keeps in its own file, at a release from before its
// database was a need, isn't shared as it is, and says why with the way out: an
// install of an app that takes it reads "Ghost's release 4 keeps it in its own
// install", with that install and the release to upgrade to. Once Ghost upgrades, the
// same server is offered (ADR 0084).
func TestAServerAnOlderReleaseKeepsSaysWhichUpgradeSharesIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	useCatalogDir(t, dir)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	// Ghost's MySQL, with the user its image made, whose client finds another once
	// it's told to make it.
	raw, err := os.ReadFile(filepath.Join("testdata", "ghost-needs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	made := map[string]bool{"ghost": true}
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}, Exec: func(x dockertest.Exec) int {
		sql, _ := io.ReadAll(x.Stdin)
		mu.Lock()
		defer mu.Unlock()
		for _, m := range regexp.MustCompile("CREATE USER IF NOT EXISTS '([a-z0-9_]+)'").FindAllSubmatch(sql, -1) {
			made[string(m[1])] = true
		}
		if m := regexp.MustCompile("User = '([a-z0-9_]+)'").FindSubmatch(sql); len(m) == 2 && made[string(m[1])] {
			_, _ = io.WriteString(x.Stdout, "u\t1\nd\t1\n")
		}
		return 0
	}}
	c := changeClient(b, b.origin)
	cat := catalogClient(b)

	// Ghost at release 4, from before its database was a need, as the fixture holds
	// it: the catalog this build holds has moved on to the need.
	bundled, err := os.ReadFile(filepath.Join("testdata", "ghost-bundled.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), bundled, 0o600); err != nil {
		t.Fatal(err)
	}
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site"})
	if got := apply(t, c, installed); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Ghost ended %v: %s", got.GetState(), got.GetError())
	}
	// The catalog moves on: Ghost's and WordPress's releases with needs.
	for _, id := range []string{"ghost", "wordpress"} {
		raw, err := os.ReadFile(filepath.Join("testdata", id+"-needs.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".yaml"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ghostsServer := func() *dockwellv1.CatalogNeedOption {
		t.Helper()
		got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "wordpress"}))
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range got.Msg.GetNeeds() {
			if o.GetService() == "db" && o.GetProject() == "site" {
				return o
			}
		}
		t.Fatalf("WordPress's sheet doesn't list Ghost's server: %v", got.Msg.GetNeeds())
		return nil
	}

	before := ghostsServer()
	if before.GetWhy() != "Ghost's release 4 keeps it in its own install" || before.GetUpgradeApp() != "Ghost" || before.GetUpgradeRelease() != 5 ||
		!strings.HasPrefix(before.GetUpgradeInstall(), "prj_") {
		t.Fatalf("before Ghost upgrades, its server reads %v", before)
	}

	// The upgrade it names, as the sheet's button plans it.
	up, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: before.GetUpgradeInstall(), SkipBackup: true}))
	if err != nil {
		t.Fatal(err)
	}
	if got := apply(t, c, up.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("upgrading Ghost ended %v: %s", got.GetState(), got.GetError())
	}

	after := ghostsServer()
	if after.GetWhy() != "" || after.GetUpgradeInstall() != "" || after.GetUpgradeRelease() != 0 {
		t.Errorf("once Ghost upgrades, its server reads %v", after)
	}
}
