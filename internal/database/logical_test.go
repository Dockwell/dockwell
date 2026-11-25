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
	e.AddLocalImage("postgres:17", dockertest.Image{
		Config: docker.Config{Cmd: []string{"postgres"}},
		Exec: func(x dockertest.Exec) int {
			raw, _ := io.ReadAll(x.Stdin)
			sql := string(raw)
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
	ref := resource.Ref{Kind: LogicalKind, Scope: "env_1", Name: "analytics"}

	// Refuses missing owner on Judge.
	if _, err := d.Judge(t.Context(), ref, nil, &resourcev1.LogicalDatabase{Service: "db", Owner: "ghost"}, nil); err == nil {
		t.Fatal("expected Judge to refuse non-existent owner")
	}
	// Allows existing owner.
	desired := &resourcev1.LogicalDatabase{Service: "db", Owner: "alice"}
	dec, err := d.Judge(t.Context(), ref, nil, desired, nil)
	if err != nil || dec.Action != resource.ActionCreate {
		t.Fatalf("Judge create = %v, %v", dec, err)
	}
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

	// Delete drops database and keeps role.
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
