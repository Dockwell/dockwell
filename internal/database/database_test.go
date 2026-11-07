// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

func TestRecogniseOnlyOfficialImages(t *testing.T) {
	cases := map[string]dockwellv1.DatabaseEngine{
		"postgres":             dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL,
		"postgres:17.2-alpine": dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL,
		"docker.io/library/postgres:16@sha256:ab": dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL,
		"mysql:8.4":                          dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL,
		"mariadb:11.4":                       dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB,
		"redis:7-alpine":                     dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS,
		"valkey/valkey:8.1":                  dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY,
		"docker.io/valkey/valkey":            dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY,
		"bitnami/postgresql:17":              dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED,
		"ghcr.io/acme/postgres:17":           dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED,
		"registry.example.com:5000/postgres": dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED,
		"pgvector/pgvector:pg17":             dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED,
		"adminer:6":                          dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED,
	}
	for image, want := range cases {
		got := dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED
		if e := Recognise(image); e != nil {
			got = e.Kind
		}
		if got != want {
			t.Errorf("Recognise(%q) = %v, want %v", image, got, want)
		}
	}
}

func TestVersionIsWhatTheTagNames(t *testing.T) {
	for image, want := range map[string]string{
		"postgres:17.2-alpine":        "17.2",
		"postgres:17.11@sha256:748a":  "17.11",
		"postgres":                    "latest",
		"redis:alpine":                "alpine",
		"mariadb:lts":                 "lts",
		"registry.example.com:5000/x": "latest",
		"mysql:8.4.3-oraclelinux9":    "8.4.3",
	} {
		if got := Version(image); got != want {
			t.Errorf("Version(%q) = %q, want %q", image, got, want)
		}
	}
}

func volume(name, target string) *resourcev1.Mount {
	return &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: name}, Target: target}
}

func TestFromSpecFindsTheVolumeThatHoldsTheData(t *testing.T) {
	svc := &resourcev1.Service{
		Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image:  "postgres:17",
			Mounts: []*resourcev1.Mount{volume("archive", "/archive"), volume("data", "/var/lib/postgresql/data"), volume("up", "/var/lib")},
		}},
		Secrets: map[string]string{"POSTGRES_PASSWORD": ""},
	}
	f, ok := FromSpec(svc)
	if !ok || f.Engine.Kind != dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		t.Fatalf("FromSpec = %+v, %v", f, ok)
	}
	if f.Volume != "data" || f.Port != 5432 || f.PasswordSecret != "POSTGRES_PASSWORD" {
		t.Errorf("found %+v, want the volume data, port 5432 and the secret POSTGRES_PASSWORD", f)
	}

	// postgres:18 asks for its volume one directory up.
	svc.GetApp().Mounts = []*resourcev1.Mount{volume("pg", "/var/lib/postgresql")}
	svc.GetApp().Command = []string{"postgres", "-c", "port=5433"}
	svc.Secrets = map[string]string{"POSTGRES_PASSWORD": "DB_PASSWORD"}
	if f, _ := FromSpec(svc); f.Volume != "pg" || f.Port != 5433 || f.PasswordSecret != "DB_PASSWORD" {
		t.Errorf("found %+v, want the volume pg, port 5433 and the secret DB_PASSWORD", f)
	}

	// PGDATA moves it, so a volume at the default holds none of it.
	svc.GetApp().Env = map[string]string{"PGDATA": "/srv/pg"}
	svc.GetApp().Mounts = []*resourcev1.Mount{volume("default", "/var/lib/postgresql/data"), volume("moved", "/srv")}
	if f, _ := FromSpec(svc); f.Volume != "moved" {
		t.Errorf("found the volume %q, want moved, which holds PGDATA", f.Volume)
	}

	// A server path holds the data, but it's no volume to back up.
	svc.GetApp().Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/pg"}, Target: "/srv/pg"}}
	if f, _ := FromSpec(svc); f.Volume != "" {
		t.Errorf("found the volume %q in a server path", f.Volume)
	}

	if _, ok := FromSpec(&resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx"}}}); ok {
		t.Error("nginx reads as a database")
	}
}

func TestPortOfReadsEachWay(t *testing.T) {
	pg, redis, mysql := Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL), Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS), Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL)
	cases := []struct {
		e    *Engine
		cmd  []string
		want uint32
	}{
		{pg, []string{"postgres", "-p", "5433"}, 5433},
		{pg, []string{"postgres", "-p5434"}, 5434},
		{pg, []string{"postgres", "-cport=5435"}, 5435},
		{redis, []string{"redis-server", "--port", "6380"}, 6380},
		{redis, []string{"redis-server", "--requirepass", "x"}, 0},
		{mysql, []string{"mysqld", "--port=3307"}, 3307},
		{mysql, []string{"mysqld", "-p", "3308"}, 0},
		{pg, []string{"postgres", "-p", "99999"}, 0},
	}
	for _, c := range cases {
		if got := PortOf(c.e, c.cmd); got != c.want {
			t.Errorf("PortOf(%s, %q) = %d, want %d", c.e.Name, c.cmd, got, c.want)
		}
	}
}

func TestParsePostgres(t *testing.T) {
	said := `{"version" : "17.2 (Debian 17.2-1.pgdg120+1)", "user" : "postgres", "connections" : 3, "max_connections" : 100, "uptime" : 3600, "hit" : 990, "read" : 10, "size" : 7500000, "database_count" : 3, "user_count" : 2,
	"databases" : [{"name":"postgres","owner":"postgres","size":7000000},{"name":"shop\tfront","owner":"shop","size":500000}],
	"users" : [{"name":"postgres","super":true,"login":true},{"name":"shop","super":false,"login":true}]}`
	r, err := parsePostgres([]byte(said))
	if err != nil {
		t.Fatal(err)
	}
	if r.GetVersion() != "17.2" || r.GetUser() != "postgres" || r.GetConnections() != 3 || r.GetMaxConnections() != 100 || r.GetSizeBytes() != 7500000 || r.GetUptimeSeconds() != 3600 {
		t.Errorf("read %v", r)
	}
	if r.CacheHit == nil || r.GetCacheHit() != 0.99 {
		t.Errorf("cache hit %v, want 0.99", r.CacheHit)
	}
	if len(r.GetDatabases()) != 2 || r.GetDatabases()[1].GetName() != "shop\tfront" || !r.GetDatabases()[0].GetSystem() {
		t.Errorf("databases %v", r.GetDatabases())
	}
	if u := r.GetUsers()[1]; u.GetName() != "shop" || u.GetSuperuser() || len(u.GetDatabases()) != 1 || u.GetDatabases()[0] != "shop\tfront" {
		t.Errorf("shop is %v, want the owner of shop\\tfront", u)
	}
	if r.GetUnlisted() != 1 {
		t.Errorf("unlisted %d, want 1 of 3 databases", r.GetUnlisted())
	}
	if _, err := parsePostgres([]byte("ERROR: nope")); err == nil {
		t.Error("an answer that isn't JSON reads")
	}
}

func TestParseMySQL(t *testing.T) {
	said := "v\t11.4.4-MariaDB-ubu2404\troot@localhost\t151\n" +
		"Innodb_buffer_pool_read_requests\t1000\nInnodb_buffer_pool_reads\t50\nThreads_connected\t4\nUptime\t120\n" +
		"d\tinformation_schema\t0\nd\tmysql\t2000\nd\tperformance_schema\t0\nd\tshop\\tfront\t5000\nd\tsys\t0\n" +
		"u\thealthcheck\tlocalhost\tN\nu\tmariadb.sys\tlocalhost\tN\nu\troot\t%\tY\nu\tshop\t%\tN\n" +
		"g\tshop\t%\tshop\\tfront\n"
	e := Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB)
	r, err := parseMySQL(e, []byte(said))
	if err != nil {
		t.Fatal(err)
	}
	if r.GetVersion() != "11.4.4" || r.GetUser() != "root" || r.GetMaxConnections() != 151 || r.GetConnections() != 4 || r.GetUptimeSeconds() != 120 || r.GetSizeBytes() != 7000 {
		t.Errorf("read %v", r)
	}
	if r.GetCacheHit() != 0.95 {
		t.Errorf("cache hit %v, want 0.95", r.GetCacheHit())
	}
	var names []string
	for _, d := range r.GetDatabases() {
		names = append(names, d.GetName())
	}
	if len(names) != 3 || names[1] != "shop\tfront" {
		t.Errorf("databases %q, want mysql, shop\\tfront and sys", names)
	}
	users := r.GetUsers()
	if len(users) != 4 || !users[0].GetSystem() || !users[1].GetSystem() || !users[2].GetSuperuser() || users[3].GetDatabases()[0] != "shop\tfront" {
		t.Errorf("users %v", users)
	}
	if _, err := parseMySQL(e, []byte("Uptime\t3\n")); err == nil {
		t.Error("an answer with no version reads")
	}
}

func TestParseRedis(t *testing.T) {
	said := "# Server\r\nredis_version:7.2.4\r\nvalkey_version:8.1.1\r\nuptime_in_seconds:99\r\n# Clients\r\nconnected_clients:2\r\nmaxclients:10000\r\n# Memory\r\nused_memory:1048576\r\n# Stats\r\nkeyspace_hits:30\r\nkeyspace_misses:10\r\n# Keyspace\r\ndb0:keys=12,expires=0,avg_ttl=0\r\ndb3:keys=3,expires=1,avg_ttl=5\r\n"
	r, err := parseRedis(Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY), []byte(said))
	if err != nil {
		t.Fatal(err)
	}
	if r.GetVersion() != "8.1.1" || r.GetKeys() != 15 || r.GetSizeBytes() != 1048576 || r.GetConnections() != 2 || r.GetMaxConnections() != 10000 || r.GetCacheHit() != 0.75 || r.GetUptimeSeconds() != 99 {
		t.Errorf("read %v", r)
	}
	r, _ = parseRedis(Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS), []byte(said))
	if r.GetVersion() != "7.2.4" {
		t.Errorf("Redis reads as %q, want its own version", r.GetVersion())
	}
}

func TestScrubbedLeavesOutWhatADatabaseQuotes(t *testing.T) {
	got := scrubbed(`FATAL:  password authentication failed for user "postgres"` + "\nmore")
	if got != `FATAL:  password authentication failed for user '...'` {
		t.Errorf("scrubbed to %q", got)
	}
}

func TestRedisRefusesOnlyWithAnError(t *testing.T) {
	info := "# Server\r\nredis_version:7.4.1\r\n# Errorstats\r\nerrorstat_WRONGPASS:count=1\r\nerrorstat_NOAUTH:count=2\r\n"
	if redisRefused(info) {
		t.Error("INFO that counts others' refusals reads as Dockwell refused")
	}
	for _, said := range []string{"NOAUTH Authentication required.", "(error) WRONGPASS invalid username-password pair or user is disabled.", "AUTH failed: WRONGPASS invalid username-password pair"} {
		if !redisRefused(said) {
			t.Errorf("%q doesn't read as refused", said)
		}
	}
}
