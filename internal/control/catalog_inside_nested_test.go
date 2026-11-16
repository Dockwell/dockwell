// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

// An entry whose password is inside another value's default, as in
// ${GIVEN_URL:-postgres://u:${PASSWORD}@db:5432/x}, is left out of the catalog, as a
// template can't hold it, so nothing installs it with the password's stand-in in its
// URL (#570's review, H1).
func TestASecretInsideAnotherValuesDefaultDoesntInstall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	useCatalogDir(t, dir)
	entry := strings.Replace(testEntry(1, "1.0", firstRelease, ""), "PASSWORD: ${PASSWORD}",
		"PASSWORD: ${PASSWORD}\n        URL: ${GIVEN_URL:-postgres://u:${PASSWORD}@db:5432/x}", 1)
	if err := os.WriteFile(filepath.Join(dir, "thing.yaml"), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	pullsAnything(f.addDockerServer(t))
	_, err := catalogClient(b).InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "thing"}))
	if err == nil {
		t.Error("an entry with its password inside another value's default installs")
	}
}

// The same in its bare spelling, $PASSWORD, which Compose reads too (#570's recheck,
// N4).
func TestABareSecretInsideAnotherValuesDefaultDoesntInstall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	useCatalogDir(t, dir)
	entry := strings.Replace(testEntry(1, "1.0", firstRelease, ""), "PASSWORD: ${PASSWORD}",
		"PASSWORD: ${PASSWORD}\n        URL: ${GIVEN_URL:-postgres://u:$PASSWORD@db:5432/x}", 1)
	if err := os.WriteFile(filepath.Join(dir, "thing.yaml"), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	pullsAnything(f.addDockerServer(t))
	_, err := catalogClient(b).InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "thing"}))
	if err == nil {
		t.Error("an entry with its password bare inside another value's default installs")
	}
}
