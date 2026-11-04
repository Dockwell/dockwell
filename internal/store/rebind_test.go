// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Queries reach the database only through Tx's exec, query and queryRow,
// which rewrite placeholders for Postgres. A query sent on t.tx directly
// works on SQLite and fails on Postgres, as the API tokens' did.
func TestQueriesGoThroughTheDialect(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "store.go" {
			continue
		}
		src, err := os.ReadFile(f) //nolint:gosec // the package's own sources
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "t.tx.") {
				t.Errorf("%s:%d sends a query on t.tx; use t.exec, t.query or t.queryRow, which work on Postgres too", f, i+1)
			}
		}
	}
}
