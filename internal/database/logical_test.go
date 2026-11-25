// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

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

	// H4: if pre-delete backup fails, Delete is refused and database stays untouched.
	dumpFail = true
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Action: resource.ActionDelete}, nil); err == nil {
		t.Fatal("H4: Delete succeeded even though backup failed")
	}
	if _, exists := dbs["analytics"]; !exists {
		t.Fatal("H4: database was dropped despite failed backup")
	}
	dumpFail = false

	// H4: Delete backs up first, drops database, keeps role, and Undo restores that backup.
	delDec, err := d.Judge(t.Context(), ref, desired, nil, nil)
	if err != nil || delDec.Action != resource.ActionDelete || !delDec.Destroys {
		t.Fatalf("Judge delete = %v, %v", delDec, err)
	}
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Action: resource.ActionDelete}, nil); err != nil {
		t.Fatalf("Apply delete: %v", err)
	}
	if _, exists := dbs["analytics"]; exists {
		t.Fatal("analytics database still exists after delete")
	}
	if !roles["alice"] {
		t.Fatal("owner role alice should remain after logical database delete")
	}
	undoDec, err := d.Judge(t.Context(), ref, nil, desired, nil)
	if err != nil || undoDec.Action != resource.ActionCreate || !undoDec.Restores {
		t.Fatalf("H4: Judge after delete = %+v, %v, want ActionCreate with Restores=true", undoDec, err)
	}
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: undoDec}, desired); err != nil {
		t.Fatalf("H4: Apply undo create+restore: %v", err)
	}
	if !strings.Contains(restoredSQL, "CREATE TABLE reports_tbl (id int);") {
		t.Errorf("H4: Undo did not restore deleted backup SQL; got %q", restoredSQL)
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
