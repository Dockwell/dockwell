// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

func TestLogicalDriverPostgresLifecycleKeepsOwner(t *testing.T) {
	dbs := map[string]string{"postgres": "postgres"}
	roles := map[string]bool{"postgres": true, "alice": true}
	reState := regexp.MustCompile(`datname = '([a-z0-9_]+)'\), 'owner_exists', (true|EXISTS \(SELECT 1 FROM pg_roles WHERE rolname = '([a-z0-9_]+)'\))`)
	reCreate := regexp.MustCompile(`CREATE DATABASE "([a-z0-9_]+)"(?: OWNER "([a-z0-9_]+)")?`)
	reDrop := regexp.MustCompile(`DROP DATABASE IF EXISTS "([a-z0-9_]+)"`)

	e := dockertest.New(t)
	dumpFail := false
	dumpData := "CREATE TABLE reports_tbl (id int);\n"
	restoredSQL := ""
	activeUsers := ""
	e.AddLocalImage("postgres:17", dockertest.Image{
		Config: docker.Config{Cmd: []string{"postgres"}},
		Exec: func(x dockertest.Exec) int {
			script := strings.Join(x.Cmd, " ")
			if strings.Contains(script, "pg_dump ") {
				if dumpFail {
					_, _ = io.WriteString(x.Stderr, "pg_dump: error: connection failed\n")
					return 1
				}
				_, _ = io.WriteString(x.Stdout, dumpData+"-- PostgreSQL database dump complete\n")
				return 0
			}
			if len(x.Cmd) >= 2 && x.Cmd[0] == "psql" && x.Cmd[1] == "-V" {
				_, _ = io.WriteString(x.Stdout, "psql (PostgreSQL) 17.6\n")
				return 0
			}
			if strings.Contains(script, " -w ") && strings.Contains(script, "SELECT 1") {
				_, _ = io.WriteString(x.Stderr, "psql: error: fe_sendauth: no password supplied\n")
				return 2
			}
			raw, _ := io.ReadAll(x.Stdin)
			sql := string(raw)
			if strings.Contains(script, `-d "$d"`) {
				restoredSQL = sql
				return 0
			}
			if strings.Contains(sql, "SELECT DISTINCT usename FROM pg_stat_activity") {
				_, _ = io.WriteString(x.Stdout, activeUsers)
				return 0
			}
			if strings.Contains(sql, "'login', r.rolcanlogin") {
				_, _ = io.WriteString(x.Stdout, `{"owner":"alice","login":true,"super":false,"loader":""}`+"\n")
				return 0
			}
			if strings.Contains(sql, "json_build_object('owner', r.rolname)") {
				if owner := dbs["analytics"]; owner != "" {
					_, _ = fmt.Fprintf(x.Stdout, `{"owner":%q}`+"\n", owner)
				}
				return 0
			}
			if strings.Contains(sql, "FROM pg_extension x WHERE x.oid >= 16384") {
				return 0
			}
			if m := reState.FindStringSubmatch(sql); m != nil {
				owner := dbs[m[1]]
				ownerOK := true
				if m[3] != "" {
					ownerOK = roles[m[3]]
				}
				_, _ = fmt.Fprintf(x.Stdout, `{"exists":%t,"owner_exists":%t,"owner":%q}`+"\n", owner != "", ownerOK, owner)
				return 0
			}
			if m := reCreate.FindStringSubmatch(sql); m != nil {
				if _, exists := dbs[m[1]]; exists && !strings.Contains(sql, "WHERE NOT EXISTS") {
					_, _ = io.WriteString(x.Stderr, "ERROR:  database \""+m[1]+"\" already exists\n")
					return 3
				}
				owner := m[2]
				if owner == "" {
					owner = "postgres"
				}
				dbs[m[1]] = owner
				return 0
			}
			if m := reDrop.FindStringSubmatch(sql); m != nil {
				if strings.Contains(sql, "DROP ROLE") {
					t.Errorf("LogicalDriver delete must not drop the owner role: %s", sql)
				}
				delete(dbs, m[1])
				return 0
			}
			return 0
		},
	})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_1-db-1", &docker.CreateRequest{
		Config: docker.Config{
			Image:  "postgres:17",
			Labels: map[string]string{docker.LabelProject: "env_1", docker.LabelService: "db", docker.LabelNumber: "1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	d := &LogicalDriver{Client: client, Dir: t.TempDir()}
	// L5: resource name <service>-<db> with explicit database_name.
	ref := resource.Ref{Kind: LogicalKind, Scope: "env_1", Name: LogicalResourceName("db", "analytics")}
	if ref.Name != "db-analytics" {
		t.Fatalf("LogicalResourceName = %q, want db-analytics", ref.Name)
	}

	// Refuses missing owner on Judge.
	if _, err := d.Judge(t.Context(), ref, nil, &resourcev1.LogicalDatabase{Service: "db", DatabaseName: "analytics", Owner: "ghost"}, nil); err == nil {
		t.Fatal("expected Judge to refuse non-existent owner")
	}
	// Allows existing owner.
	desired := &resourcev1.LogicalDatabase{Service: "db", DatabaseName: "analytics", Owner: "alice"}
	dec, err := d.Judge(t.Context(), ref, nil, desired, nil)
	if err != nil || dec.Action != resource.ActionCreate {
		t.Fatalf("Judge create = %v, %v", dec, err)
	}
	// L6: if a database with the same name appears between Judge and Apply,
	// first Apply fails instead of adopting it silently.
	dbs["analytics"] = "postgres"
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Action: resource.ActionCreate}, desired); err == nil {
		t.Fatal("L6: Apply adopted concurrently created database instead of failing")
	}
	if _, ok, err := d.recorded(ref); err != nil || ok {
		t.Fatalf("L6: failed Apply must not record the database (ok=%t, err=%v)", ok, err)
	}
	delete(dbs, "analytics")

	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Action: resource.ActionCreate}, desired); err != nil {
		t.Fatalf("Apply create: %v", err)
	}
	if err := d.Verify(t.Context(), ref, desired); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	got, err := d.Observe(t.Context(), ref)
	if err != nil || got == nil || got.(*resourcev1.LogicalDatabase).GetOwner() != "alice" {
		t.Fatalf("Observe = %v, %v", got, err)
	}

	// H3: Delete is refused at Judge while active sessions are connected.
	activeUsers = "alice\n"
	if _, err := d.Judge(t.Context(), ref, desired, nil, nil); err == nil || !strings.Contains(err.Error(), "active connections from alice") {
		t.Fatalf("H3: Judge delete with active session = %v, want refusal naming alice", err)
	}
	activeUsers = ""

	// H4: if pre-delete backup fails, Delete is refused and database stays untouched.
	delCtx1 := resource.WithDeploy(t.Context(), resource.Deploy{Change: "chg_del_1"})
	dumpFail = true
	if err := d.Apply(delCtx1, resource.Step{Ref: ref, Action: resource.ActionDelete}, nil); err == nil {
		t.Fatal("H4: Delete succeeded even though backup failed")
	}
	if _, exists := dbs["analytics"]; !exists {
		t.Fatal("H4: database was dropped despite failed backup")
	}
	dumpFail = false

	// H4/N3/N4: Delete backs up keyed to chg_del_1, drops database, keeps role.
	delDec, err := d.Judge(t.Context(), ref, desired, nil, nil)
	if err != nil || delDec.Action != resource.ActionDelete || !delDec.Destroys {
		t.Fatalf("Judge delete = %v, %v", delDec, err)
	}
	if err := d.Apply(delCtx1, resource.Step{Ref: ref, Action: resource.ActionDelete}, nil); err != nil {
		t.Fatalf("Apply delete 1: %v", err)
	}
	if _, exists := dbs["analytics"]; exists {
		t.Fatal("analytics database still exists after delete")
	}
	if !roles["alice"] {
		t.Fatal("owner role alice should remain after logical database delete")
	}

	// N3: A plain New database / reinstall with the same name (no Undoes) must NOT restore backup A.
	plainDec, err := d.Judge(t.Context(), ref, nil, desired, nil)
	if err != nil || plainDec.Action != resource.ActionCreate || plainDec.Restores {
		t.Fatalf("N3: plain Judge after delete = %+v, %v, want ActionCreate with Restores=false", plainDec, err)
	}
	restoredSQL = ""
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: plainDec}, desired); err != nil {
		t.Fatalf("N3: plain Apply create after delete: %v", err)
	}
	if restoredSQL != "" {
		t.Fatalf("N3: plain create unexpectedly restored deleted backup: %q", restoredSQL)
	}

	// N4: A second delete (chg_del_2) keeps its own backup B without overwriting chg_del_1's backup A.
	dumpData = "CREATE TABLE second_version (id int);\n"
	delCtx2 := resource.WithDeploy(t.Context(), resource.Deploy{Change: "chg_del_2"})
	if err := d.Apply(delCtx2, resource.Step{Ref: ref, Action: resource.ActionDelete}, nil); err != nil {
		t.Fatalf("Apply delete 2: %v", err)
	}

	// Undo of chg_del_1 specifically restores backup A (reports_tbl), not backup B!
	undoCtx1 := resource.WithDeploy(t.Context(), resource.Deploy{Undoes: "chg_del_1"})
	undoDec1, err := d.Judge(undoCtx1, ref, nil, desired, nil)
	if err != nil || undoDec1.Action != resource.ActionCreate || !undoDec1.Restores {
		t.Fatalf("H4/N3: Judge undo chg_del_1 = %+v, %v, want ActionCreate with Restores=true", undoDec1, err)
	}
	if err := d.Apply(undoCtx1, resource.Step{Ref: ref, Decision: undoDec1}, desired); err != nil {
		t.Fatalf("H4/N3: Apply undo chg_del_1: %v", err)
	}
	if !strings.Contains(restoredSQL, "CREATE TABLE reports_tbl (id int);") {
		t.Errorf("N4: Undo of chg_del_1 restored wrong backup; got %q", restoredSQL)
	}
	// Backup B for chg_del_2 is still intact.
	if !hasDeletedBackup(d.Dir, time.Now(), "chg_del_2", ref.Scope, "db", "analytics") {
		t.Error("N4: chg_del_2 backup disappeared after undoing chg_del_1")
	}
}

func TestPruneDeletedBackupsRemovesCrashLeftovers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	deletedDir := deletedBackupsDir(dir)
	if err := os.MkdirAll(deletedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-2 * time.Hour)
	freshTime := now.Add(-10 * time.Minute)

	// Crash leftover .tmp and orphan .sql.gz older than 1 hour -> pruned.
	oldTmp := filepath.Join(deletedDir, "deadbeef.sql.gz.tmp")
	oldOrphan := filepath.Join(deletedDir, "orphan123456.sql.gz")
	// Fresh .tmp and orphan .sql.gz younger than 1 hour -> kept (might be in flight).
	freshTmp := filepath.Join(deletedDir, "inflight.sql.gz.tmp")
	freshOrphan := filepath.Join(deletedDir, "fresh123456.sql.gz")
	for _, p := range []string{oldTmp, oldOrphan, freshTmp, freshOrphan} {
		if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{oldTmp, oldOrphan} {
		if err := os.Chtimes(p, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{freshTmp, freshOrphan} {
		if err := os.Chtimes(p, freshTime, freshTime); err != nil {
			t.Fatal(err)
		}
	}
	if err := PruneDeletedBackups(dir, now); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{oldTmp, oldOrphan} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("N4: old leftover %s was not pruned", filepath.Base(gone))
		}
	}
	for _, kept := range []string{freshTmp, freshOrphan} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("N4: recent file %s was pruned too early: %v", filepath.Base(kept), err)
		}
	}
}

func TestValueNamesDatabase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		val, svc, db string
		want         bool
	}{
		{"postgres://app:pw@pg:5432/shop?sslmode=disable", "pg", "shop", true},
		{"postgresql://app:${SHOP_PW}@pg/shop", "pg", "shop", true},
		{"mysql://u:p@my:3306/shop", "my", "shop", true},
		{"mariadb://u:p@my:3306/shop?charset=utf8mb4", "my", "shop", true},
		{"jdbc:postgresql://pg:5432/shop", "pg", "shop", true},
		{"host=pg port=5432 dbname=shop user=app", "pg", "shop", true},
		{"postgres://app:pw@other:5432/shop", "pg", "shop", false},
		{"postgres://app:pw@pg:5432/other", "pg", "shop", false},
		{"postgres://app:pw@pg:5432/shop_2", "pg", "shop", false},
		{"https://pg/shop", "pg", "shop", false},
		{"plain-password", "pg", "shop", false},
	}
	for _, tc := range cases {
		if got := ValueNamesDatabase(tc.val, tc.svc, tc.db); got != tc.want {
			t.Errorf("ValueNamesDatabase(%q, %q, %q) = %t, want %t", tc.val, tc.svc, tc.db, got, tc.want)
		}
	}
}

func TestParseInnerDatabaseConnectionsAndOwner(t *testing.T) {
	pgJSON := `{"version":"17.2","user":"postgres","connections":5,"max_connections":100,"uptime":60,"hit":10,"read":2,"size":1000,"database_count":2,"user_count":2,` +
		`"databases":[{"name":"postgres","owner":"postgres","size":500,"connections":1},{"name":"shop","owner":"shop","size":500,"connections":4}],` +
		`"users":[{"name":"postgres","super":true,"login":true},{"name":"shop","super":false,"login":true}]}`
	pg, err := parsePostgres([]byte(pgJSON))
	if err != nil {
		t.Fatal(err)
	}
	if pg.GetDatabases()[1].GetConnections() != 4 || pg.GetDatabases()[1].GetOwner() != "shop" {
		t.Fatalf("pg shop = %+v", pg.GetDatabases()[1])
	}

	myText := strings.Join([]string{
		"v\t8.4.3\troot@localhost\t151",
		"Threads_connected\t4",
		"Uptime\t100",
		"d\tmysql\t100",
		"d\tshop\t2048",
		"u\troot\tlocalhost\tY",
		"u\tshop\t%\tN",
		"g\tshop\t%\tshop",
		"c\tshop\t3",
	}, "\n")
	my, err := parseMySQL(Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL), []byte(myText))
	if err != nil {
		t.Fatal(err)
	}
	if my.GetDatabases()[1].GetConnections() != 3 || my.GetDatabases()[1].GetOwner() != "shop" {
		t.Fatalf("mysql shop = %+v", my.GetDatabases()[1])
	}
}
