// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func databaseClient(c connect.HTTPClient, origin string) dockwellv1connect.DatabaseServiceClient {
	return dockwellv1connect.NewDatabaseServiceClient(c, origin, connect.WithProtoJSON())
}

// readablePostgres answers a reading as the official image's psql would, and counts
// how often it's asked. What it's asked with is kept, to see no password is in it.
func readablePostgres(asked *atomic.Int32, cmds *[]string) dockertest.Image {
	return dockertest.Image{
		Volumes: []string{"/var/lib/postgresql/data"}, Config: docker.Config{Cmd: []string{"postgres"}},
		Exec: func(x dockertest.Exec) int {
			sql, _ := io.ReadAll(x.Stdin)
			if !strings.Contains(string(sql), "json_build_object") {
				return 1
			}
			asked.Add(1)
			*cmds = append(*cmds, strings.Join(x.Cmd, " "))
			_, _ = io.WriteString(x.Stdout, `{"version":"17.2 (Debian 17.2-1.pgdg120+1)","user":"postgres","connections":4,"max_connections":100,"uptime":7200,"hit":99,"read":1,"size":9000000,"database_count":2,"user_count":2,`+
				`"databases":[{"name":"postgres","owner":"postgres","size":8000000},{"name":"shop","owner":"shop","size":1000000}],`+
				`"users":[{"name":"postgres","super":true,"login":true},{"name":"shop","super":false,"login":true}]}`+"\n")
			return 0
		},
	}
}

// readableRedis answers INFO as redis-cli would, and keeps the command it ran.
func readableRedis(cmds *[]string) dockertest.Image {
	return dockertest.Image{
		Volumes: []string{"/data"}, Config: docker.Config{Cmd: []string{"redis-server"}},
		Exec: func(x dockertest.Exec) int {
			*cmds = append(*cmds, strings.Join(x.Cmd, " "))
			_, _ = io.WriteString(x.Stdout, "# Server\r\nredis_version:7.4.1\r\nuptime_in_seconds:60\r\n# Clients\r\nconnected_clients:1\r\nmaxclients:10000\r\n# Memory\r\nused_memory:2048\r\n# Keyspace\r\ndb0:keys=5,expires=0,avg_ttl=0\r\n")
			return 0
		},
	}
}

const databaseStack = `
services:
  web:
    image: ghcr.io/acme/shop:1.4
  db:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
    volumes: [db-data:/var/lib/postgresql/data]
  cache:
    image: redis:7
    command: ["redis-server", "--port", "6380"]
volumes:
  db-data: {}
`

// The services that run a database are listed, each with what it says of itself,
// read in its own container with what its environment holds: no password goes
// anywhere, a reading is asked for once however many read it, and a server that's
// away says so.
func TestDatabasesAreTheServicesThatRunOne(t *testing.T) {
	storetest.Each(t, testDatabasesAreTheServicesThatRunOne)
}

func testDatabasesAreTheServicesThatRunOne(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	var asked atomic.Int32
	var pgCmds, redisCmds []string
	e.Registry["postgres:17"] = readablePostgres(&asked, &pgCmds)
	e.Registry["redis:7"] = readableRedis(&redisCmds)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	ch := changeClient(b, b.origin)
	const password = "a-password-nobody-reads"
	apply(t, ch, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": password}, false))
	dc := databaseClient(b, b.origin)

	server := f.lastDocker
	server.stop()
	waitConnected(t, f, server.id.ServerID, false)
	away, err := dc.ListDatabases(t.Context(), connect.NewRequest(&dockwellv1.ListDatabasesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range away.Msg.GetDatabases() {
		if r := d.GetReadings(); len(r) != 1 || r[0].GetUnread() != "hz-1 isn't connected, so it can't say how its databases are" || r[0].GetServerName() != "hz-1" {
			t.Errorf("with hz-1 away, %s reads %v", d.GetResource().GetName(), r)
		}
	}
	server.connect(t, f)

	listed, err := dc.ListDatabases(t.Context(), connect.NewRequest(&dockwellv1.ListDatabasesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	dbs := listed.Msg.GetDatabases()
	if len(dbs) != 2 || dbs[0].GetResource().GetName() != "cache" || dbs[1].GetResource().GetName() != "db" {
		t.Fatalf("the databases are %v, want cache and db, and not web", dbs)
	}
	cache, db := dbs[0], dbs[1]
	if db.GetEngine() != dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL || db.GetVersion() != "17" || db.GetVolume() != "db-data" || db.GetVolumeId() == "" ||
		db.GetPort() != 5432 || db.GetHost() != "db" || !slices.Equal(db.GetBeside(), []string{"cache", "web"}) || db.GetPasswordSecret() != "" {
		t.Errorf("db is %v", db)
	}
	r := db.GetReadings()
	if len(r) != 1 || r[0].GetUnread() != "" || r[0].GetVersion() != "17.2" || r[0].GetUser() != "postgres" || r[0].GetConnections() != 4 ||
		r[0].GetSizeBytes() != 9000000 || len(r[0].GetDatabases()) != 2 || r[0].GetDatabases()[1].GetName() != "shop" || r[0].GetReadTime() == nil {
		t.Errorf("db reads %v", r)
	}
	if cache.GetEngine() != dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS || cache.GetPort() != 6380 || cache.GetVolume() != "" {
		t.Errorf("cache is %v", cache)
	}
	if r := cache.GetReadings(); len(r) != 1 || r[0].GetVersion() != "7.4.1" || r[0].GetKeys() != 5 {
		t.Errorf("cache reads %v", r)
	}
	if len(redisCmds) == 0 || !strings.HasPrefix(redisCmds[0], "env DW_PORT=6380 sh -c") {
		t.Errorf("redis-cli ran as %q, without the port its command sets", redisCmds)
	}

	// Its tools read the password where the image does, so it's in no command.
	for _, cmd := range append(pgCmds, redisCmds...) {
		if strings.Contains(cmd, password) {
			t.Errorf("a reading ran with the password in its command: %q", cmd)
		}
	}
	text, err := protojson.Marshal(listed.Msg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), password) {
		t.Error("the list holds the password")
	}

	// A reading stands for a moment, so several readers ask once.
	before := asked.Load()
	read := databaseClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY), rt: b.client.Transport}, b.origin)
	byName, err := read.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: "db"}))
	if err != nil {
		t.Fatalf("a read-only token can't read a database: %v", err)
	}
	if byName.Msg.GetDatabase().GetId() != db.GetId() || len(byName.Msg.GetDatabase().GetReadings()[0].GetUsers()) != 2 {
		t.Errorf("by name, db is %v", byName.Msg.GetDatabase())
	}
	byID, err := dc.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: db.GetId()}))
	if err != nil || byID.Msg.GetDatabase().GetResource().GetName() != "db" {
		t.Errorf("by ID, db is %v, %v", byID, err)
	}
	if got := asked.Load(); got != before {
		t.Errorf("PostgreSQL was asked %d more times within a moment", got-before)
	}

	_, err = dc.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: "web"}))
	if connect.CodeOf(err) != connect.CodeNotFound || !strings.Contains(err.Error(), "there's no database web") {
		t.Errorf("web as a database: %v", err)
	}
}
