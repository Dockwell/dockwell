// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// On a real PostgreSQL, through psql as the user driver runs it: a database whose
// public schema its owner can't make things in, as one made on a server before 15
// after template1 was closed, or on one upgraded from before 15, reads as such, and
// is put right when its user is made again or given its password anew (ADR 0084).
func TestAPublicSchemaTheServerOwnsIsGivenToItsUserOnARealPostgres(t *testing.T) {
	base := os.Getenv("DOCKWELL_TEST_POSTGRES")
	psql, err := exec.LookPath("psql")
	if base == "" || err != nil {
		t.Skip("DOCKWELL_TEST_POSTGRES isn't set or psql isn't installed, so the SQL isn't tried on a real PostgreSQL; the test on real Docker tries it")
	}
	run := func(as, sql string) (string, error) {
		t.Helper()
		var out, errs bytes.Buffer
		cmd := exec.CommandContext(t.Context(), psql, base, "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-f", "-")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(as+sql), &out, &errs
		err := cmd.Run()
		return strings.TrimSpace(out.String()), err
	}
	must := func(sql string) string {
		t.Helper()
		out, err := run("", sql)
		if err != nil {
			t.Fatalf("%v, running %s", err, sql)
		}
		return out
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	user := "dwt_p_" + hex.EncodeToString(b[:])
	t.Cleanup(func() {
		_, _ = run("", "DROP DATABASE IF EXISTS "+pgName(user)+" WITH (FORCE);\n")
		_, _ = run("", "DROP ROLE IF EXISTS "+pgName(user)+";\n")
	})
	pg := Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL)
	create, err := pg.make(user, user, "a-password-no-one-typed", true)
	if err != nil {
		t.Fatal(err)
	}
	must(create)
	cant := "\\connect " + pgName(user) + "\nSELECT " + pgCantMake(user) + ";\n"
	makes := "\\connect " + pgName(user) + "\nSET ROLE " + pgName(user) + ";\nCREATE TABLE IF NOT EXISTS posts (id integer);\n"
	if got := must(cant); got != "f" {
		t.Fatalf("a new user can't make things in its public schema: %q", got)
	}
	// Its public schema as a server before 15, or upgraded from one, leaves it.
	broken := "\\connect " + pgName(user) + "\nALTER SCHEMA public OWNER TO CURRENT_USER;\nREVOKE CREATE ON SCHEMA public FROM PUBLIC;\n"
	for name, again := range map[string]bool{"given its password anew": false, "made again": true} {
		must(broken)
		if got := must(cant); got != "t" {
			t.Fatalf("%s: a public schema the server owns reads as %q", name, got)
		}
		if _, err := run("", makes); err == nil || !strings.Contains(err.Error(), "exit status") {
			t.Fatalf("%s: its user makes a table in a public schema the server owns", name)
		}
		sql, err := pg.make(user, user, "a-password-no-one-typed", again)
		if err != nil {
			t.Fatal(err)
		}
		must(sql)
		if got := must(cant); got != "f" {
			t.Errorf("%s: its public schema still reads as closed to it: %q", name, got)
		}
		must(makes)
	}
}
