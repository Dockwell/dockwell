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
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// ghostShares is a fleet with Ghost installed at release 4, from before its database
// was a need, and a catalog moved on to Ghost's release 5, as ghost makes it from the
// fixture, and WordPress's with needs: the changes and catalog clients, and Ghost's
// server as WordPress's sheet lists it.
func ghostShares(t *testing.T, ghost func([]byte) []byte) (dockwellv1connect.ChangeServiceClient, dockwellv1connect.CatalogServiceClient, func() *dockwellv1.CatalogNeedOption) {
	t.Helper()
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
	// The catalog moves on: Ghost's and WordPress's releases with needs, Ghost's as
	// given.
	for _, id := range []string{"ghost", "wordpress"} {
		raw, err := os.ReadFile(filepath.Join("testdata", id+"-needs.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if id == "ghost" {
			raw = ghost(raw)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".yaml"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server := func() *dockwellv1.CatalogNeedOption {
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

	return c, cat, server
}

// A server another app's install keeps in its own file, at a release from before its
// database was a need, isn't shared as it is, and says why with the way out: an
// install of an app that takes it reads "Ghost's release 4 keeps it in its own
// install", with that install and the release to upgrade to. Once Ghost upgrades, the
// same server is offered (ADR 0084).
func TestAServerAnOlderReleaseKeepsSaysWhichUpgradeSharesIt(t *testing.T) {
	t.Parallel()
	c, cat, ghostsServer := ghostShares(t, func(raw []byte) []byte { return raw })

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

// Where Ghost's newer release can't be planned where Ghost runs, the way to share its
// server has no upgrade to offer: its reason says so beside why it's kept apart, and
// it names no install to upgrade, so the sheet has no Upgrade that would be refused
// (#771's review, L6).
func TestAServerWhoseUpgradeCantRunWhereItIsOffersNoUpgrade(t *testing.T) {
	t.Parallel()
	other := otherArch()
	_, _, ghostsServer := ghostShares(t, func(raw []byte) []byte {
		return []byte(strings.Replace(string(raw), "\ncompose: |", "\narchitectures:\n  - "+other+"\ncompose: |", 1))
	})
	var got *dockwellv1.CatalogNeedOption
	deadline := time.Now().Add(30 * time.Second)
	for {
		// The reason waits on the server's report of its architecture.
		if got = ghostsServer(); got.GetUpgradeInstall() == "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.HasPrefix(got.GetWhy(), "Ghost's release 4 keeps it in its own install. Release 5 of 6.72.0 runs on "+other+" alone, and ") ||
		got.GetUpgradeInstall() != "" || got.GetUpgradeRelease() != 0 || got.GetUpgradeApp() != "" {
		t.Errorf("where Ghost's upgrade can't run where it is, its server reads %v", got)
	}
}
