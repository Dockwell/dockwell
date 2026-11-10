// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// The fake PostgreSQL answers a restore drill's questions from what was loaded, and
// never waits on a stdin the question doesn't send.
func TestTheFakePostgresAnswersADrill(t *testing.T) {
	dir := t.TempDir()
	dump := "\\connect template1\nCREATE DATABASE shop WITH TEMPLATE = template0;\n\\connect -reuse-previous=on \"dbname='shop'\"\nCREATE TABLE public.orders (\n    id integer\n);\n"
	if err := os.WriteFile(filepath.Join(dir, "data.sql"), []byte(dump), 0o600); err != nil {
		t.Fatal(err)
	}
	ask := func(query string) string {
		var out strings.Builder
		code := postgres(dockertest.Exec{
			Cmd:     []string{"psql", "-X", "-q", "-A", "-t", "-U", "postgres", "-d", "shop", "-c", query},
			Volumes: map[string]string{"/var/lib/postgresql/data": dir},
			Stdout:  &out, Stderr: &out,
		})
		if code != 0 {
			t.Fatalf("%s exited %d: %s", query, code, out.String())
		}
		return out.String()
	}
	if got := ask("SELECT 1"); got != "1\n" {
		t.Errorf("SELECT 1 = %q", got)
	}
	if got := ask("SELECT datname FROM pg_database"); !strings.Contains(got, "\nshop\n") || !strings.Contains(got, "template1") {
		t.Errorf("its databases = %q", got)
	}
	if got := ask("SELECT schemaname, tablename FROM pg_tables"); got != "public\torders\n" {
		t.Errorf("its tables = %q", got)
	}
	if got := ask(`SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM "public"."orders" LIMIT 1)`); got != "" {
		t.Errorf("an empty table = %q", got)
	}
	// A reading runs psql under sh -c, with its SQL on stdin, and still gets its answer.
	var read strings.Builder
	code := postgres(dockertest.Exec{
		Cmd:     []string{"sh", "-c", "exec psql -X -q -A -t -U postgres -d postgres -f -"},
		Volumes: map[string]string{"/var/lib/postgresql/data": dir},
		Stdin:   strings.NewReader("SELECT json_build_object('connections', 3)"),
		Stdout:  &read, Stderr: &read,
	})
	if code != 0 || read.Len() == 0 {
		t.Errorf("a reading under sh -c answered %d: %q", code, read.String())
	}
}
