// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dockwell/dockwell/internal/control/alerts"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/resource/catalogsource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// startedSQLite is a read-only image of the first start's managed defaults. Each
// test copies it before opening its own writable store, and still bootstraps its
// own identity. Tests of first start use the empty store instead.
var startedSQLite = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "dockwell-control-store-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "state.db")
	ctx := context.Background()
	db, err := store.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := seedStartedStore(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
})

func seedStartedStore(ctx context.Context, db *store.DB) error {
	logger := slog.New(slog.DiscardHandler)
	if _, err := revision.Setup(ctx, db, logger); err != nil {
		return err
	}
	kinds := resource.NewRegistry(alert.Rule{}, catalogsource.Type{})
	keeper := secretstest.Keeper(db, kinds)
	engine := revision.New(db, kinds, nil, keeper, logger)
	e := &alerts.Evaluator{DB: db, Engine: engine, Keeper: keeper, Logger: logger}
	if err := e.MakeDefaults(ctx); err != nil {
		return err
	}
	catalog := catalogapi.New(nil, nil, db, keeper, nil, testBuild.Commit, logger)
	return catalog.Follow(engine).MakeDefault(ctx)
}

func openStartedStore(t testing.TB, backend store.Backend) (*store.DB, string) {
	t.Helper()
	if backend != store.SQLite {
		return storetest.Open(t, backend)
	}
	image, err := startedSQLite()
	if err != nil {
		t.Fatalf("prepare the SQLite startup fixture: %v", err)
	}
	path := storetest.New(t, backend)
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}
