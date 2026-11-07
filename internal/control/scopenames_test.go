// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"log/slog"
	"testing"

	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// An environment goes by its project's name and its own, which its services'
// containers are named after; anything else by nothing (ADR 0035).
func TestScopeNamesSayWhatPeopleCallAnEnvironment(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		if _, err := revision.Setup(t.Context(), db, slog.New(slog.DiscardHandler)); err != nil {
			t.Fatal(err)
		}
		var tr scope.Tree
		if err := db.View(t.Context(), func(tx *store.Tx) (err error) {
			tr, err = scope.Load(t.Context(), tx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		project, env, err := tr.Resolve("", "")
		if err != nil {
			t.Fatal(err)
		}
		n := scopeNames{db: db}
		for id, want := range map[string]string{env.ID: "default.production", project.ID: "", "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M": "", "srv_1": ""} {
			if got := n.ScopeName(t.Context(), id); got != want {
				t.Errorf("ScopeName(%s) = %q, want %q", id, got, want)
			}
		}
	})
}
