// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// realPostgres is a database of its own on the PostgreSQL that DOCKWELL_TEST_POSTGRES
// names, owned by a role of its own, both gone when the test ends: a connection to it
// as the server's administrator, and the owner's name. The test skips without one.
func realPostgres(t *testing.T) (*pgx.Conn, string) {
	t.Helper()
	base := os.Getenv("DOCKWELL_TEST_POSTGRES")
	if base == "" {
		t.Skip("DOCKWELL_TEST_POSTGRES isn't set, so the SQL a restore runs isn't tried on a real PostgreSQL. Set it to a server's URL, such as postgres://postgres@127.0.0.1:5432/postgres")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var b [6]byte
	_, _ = rand.Read(b[:])
	owner, db := "dwt_o_"+hex.EncodeToString(b[:]), "dwt_d_"+hex.EncodeToString(b[:])
	run(t, admin, "CREATE ROLE "+pgName(owner)+" LOGIN")
	run(t, admin, "CREATE DATABASE "+pgName(db)+" OWNER "+pgName(owner))
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close(ctx)
		run(t, c, "DROP DATABASE IF EXISTS "+pgName(db)+" WITH (FORCE)")
		run(t, c, "DROP ROLE IF EXISTS "+pgName(owner))
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	c, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(ctx) })
	return c, owner
}

// run runs sql, statements and all, as the simple protocol does, as psql would.
func run(t *testing.T, c *pgx.Conn, sql string) {
	t.Helper()
	if _, err := c.PgConn().Exec(context.Background(), sql).ReadAll(); err != nil {
		t.Fatalf("%v, running %s", err, sql)
	}
}

func one(t *testing.T, c *pgx.Conn, sql string) string {
	t.Helper()
	var out string
	if err := c.QueryRow(context.Background(), sql).Scan(&out); err != nil {
		t.Fatalf("%v, asking %s", err, sql)
	}
	return out
}

// olderDump is a dump of the database before an update added a table that refers to
// orders, as pg_dump --clean --if-exists --no-owner writes one.
const olderDump = `SELECT pg_catalog.set_config('search_path', '', false);
DROP TABLE IF EXISTS public.orders;
CREATE TABLE public.orders (id integer NOT NULL, note text);
INSERT INTO public.orders VALUES (1, 'as backed up');
ALTER TABLE ONLY public.orders ADD CONSTRAINT orders_pkey PRIMARY KEY (id);
`

// On a real PostgreSQL, a dump from before an update loads into its database only
// once what the owner has there goes, in the same transaction: a newer table that
// refers to an older one stops the dump's own drops, and the transaction leaves the
// database as it was. Emptied first, the database holds the dump alone: the newer
// table, a schema of the owner's own, a type, a function and a view go (ADR 0084).
func TestADumpFromBeforeAnUpdateLoadsOnARealPostgres(t *testing.T) {
	c, owner := realPostgres(t)
	run(t, c, "SET ROLE "+pgName(owner)+";\n"+
		"CREATE TABLE public.orders (id integer PRIMARY KEY, note text); INSERT INTO public.orders VALUES (1, 'written since');\n"+
		"CREATE TABLE public.items (id integer, order_id integer REFERENCES public.orders);\n"+
		"CREATE SCHEMA app; CREATE TABLE app.jobs (id integer);\n"+
		"CREATE TYPE public.mood AS ENUM ('ok'); CREATE DOMAIN public.positive AS integer CHECK (VALUE > 0);\n"+
		"CREATE FUNCTION public.answer() RETURNS integer LANGUAGE sql AS 'SELECT 42';\n"+
		"CREATE VIEW public.notes AS SELECT note FROM public.orders; CREATE SEQUENCE public.tickets;\n"+
		"SELECT lo_create(0);\n"+
		"RESET ROLE")

	// Without emptying it first, the dump's own drop of orders stops it.
	_, err := c.PgConn().Exec(context.Background(), "BEGIN; SET ROLE "+pgName(owner)+";\n"+olderDump+"COMMIT").ReadAll()
	if err == nil || !strings.Contains(err.Error(), "other objects depend on it") {
		t.Errorf("loading the older dump as it is = %v", err)
	}
	run(t, c, "ROLLBACK")
	if got := one(t, c, "SELECT note FROM public.orders"); got != "written since" {
		t.Errorf("a load that stopped left orders holding %q", got)
	}

	run(t, c, "BEGIN; SET ROLE "+pgName(owner)+";\n"+PgEmptying+olderDump+"COMMIT")
	if got := one(t, c, "SELECT note FROM public.orders"); got != "as backed up" {
		t.Errorf("after the load orders holds %q", got)
	}
	left := one(t, c, "SELECT concat_ws(', ', "+
		"(SELECT string_agg(relname, ' ') FROM pg_class WHERE relname IN ('items', 'jobs', 'notes', 'tickets')), "+
		"(SELECT string_agg(nspname, ' ') FROM pg_namespace WHERE nspname = 'app'), "+
		"(SELECT string_agg(typname, ' ') FROM pg_type WHERE typname IN ('mood', 'positive')), "+
		"(SELECT string_agg(proname, ' ') FROM pg_proc WHERE proname = 'answer'), "+
		"(SELECT count(*)::text FROM pg_largeobject_metadata WHERE lomowner = '"+owner+"'::regrole))")
	if left != "0" {
		t.Errorf("after the load the database still has %q", left)
	}
}

// On a real PostgreSQL, what a database holds that its owner doesn't own is listed,
// and nothing else: not PostgreSQL's own, nor what the owner made (ADR 0084).
func TestWhatAnOwnerDoesntOwnIsListedOnARealPostgres(t *testing.T) {
	c, owner := realPostgres(t)
	run(t, c, "SET ROLE "+pgName(owner)+";\n"+
		"CREATE TABLE public.orders (id serial PRIMARY KEY); CREATE SCHEMA app; CREATE TYPE app.mood AS (a integer);\n"+
		"CREATE FUNCTION app.answer() RETURNS integer LANGUAGE sql AS 'SELECT 42';\n"+
		"RESET ROLE")
	all := "SELECT coalesce(string_agg(what, '; '), '') FROM (" + strings.TrimSuffix(PgNotOwned(owner), " LIMIT 3;\n") + ") w"
	if got := one(t, c, all); got != "" {
		t.Errorf("a database its owner made all of lists %q", got)
	}
	run(t, c, "CREATE TABLE public.admins (id integer); CREATE SCHEMA ops; CREATE FUNCTION public.audit() RETURNS integer LANGUAGE sql AS 'SELECT 1'")
	got := one(t, c, all)
	for _, want := range []string{"the table admins", "the schema ops", "the function audit()"} {
		if !strings.Contains(got, want) {
			t.Errorf("an administrator's things in it list as %q, want %s", got, want)
		}
	}
}
