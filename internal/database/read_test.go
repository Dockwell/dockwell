// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// A database that doesn't answer costs its own row, within readWait, and not the
// others'.
func TestADatabaseThatHangsCostsItsOwnRow(t *testing.T) {
	was := readWait
	readWait = 200 * time.Millisecond
	t.Cleanup(func() { readWait = was })

	e := dockertest.New(t)
	e.AddLocalImage("postgres:17", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: func(dockertest.Exec) int {
		time.Sleep(2 * time.Second)
		return 0
	}})
	e.AddLocalImage("redis:7", dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}, Exec: func(x dockertest.Exec) int {
		_, _ = x.Stdout.Write([]byte("redis_version:7.4.1\r\nconnected_clients:3\r\n"))
		return 0
	}})
	client := e.Client()
	for _, s := range []struct{ name, image string }{{"db", "postgres:17"}, {"cache", "redis:7"}} {
		id, err := client.CreateContainer(t.Context(), "env_1-"+s.name+"-1", &docker.CreateRequest{Config: docker.Config{
			Image: s.image, Labels: map[string]string{docker.LabelProject: "env_1", docker.LabelService: s.name, docker.LabelNumber: "1"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.StartContainer(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}

	started := time.Now()
	got, err := Reader{Client: client}.Read(t.Context(), &agentv1.DatabaseCall{Databases: []*agentv1.DatabaseAsked{
		{Service: &dockwellv1.ResourceRef{Kind: "service", Scope: "env_1", Name: "db"}, Engine: dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL},
		{Service: &dockwellv1.ResourceRef{Kind: "service", Scope: "env_1", Name: "cache"}, Engine: dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > time.Second {
		t.Errorf("reading took %s, past what one database gets", took)
	}
	if why := got.GetReadings()[0].GetUnread(); !strings.Contains(why, "PostgreSQL didn't answer within") {
		t.Errorf("the database that hangs reads as %q", why)
	}
	if r := got.GetReadings()[1]; r.GetUnread() != "" || r.GetVersion() != "7.4.1" || r.GetConnections() != 2 {
		t.Errorf("the one beside it reads %v", r)
	}
}

// Each tool runs under timeout, which ends it within what the reading waits however
// its connection and statements go: their limits alone add up past it, as a slow
// connect and a slow statement come one after the other. Each engine's own limits
// are shorter still, so its own words come first.
func TestEachEngineGivesUpBeforeTheReading(t *testing.T) {
	outer := regexp.MustCompile(`exec timeout (\d+) `)
	for _, c := range []struct {
		engine dockwellv1.DatabaseEngine
		limits map[string]time.Duration
	}{
		{dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL, map[string]time.Duration{`statement_timeout=(\d+)`: time.Millisecond, `PGCONNECT_TIMEOUT=(\d+)`: time.Second}},
		{dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, map[string]time.Duration{`max_execution_time = (\d+)`: time.Millisecond, `--connect-timeout=(\d+)`: time.Second}},
		{dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB, map[string]time.Duration{`max_statement_time = (\d+)`: time.Second, `--connect-timeout=(\d+)`: time.Second}},
		{dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS, nil},
		{dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY, nil},
	} {
		cmd, sql := Of(c.engine).reading(0)
		said := strings.Join(cmd, " ") + "\n" + sql
		m := outer.FindStringSubmatch(said)
		if m == nil {
			t.Errorf("%s reads with no timeout around its tool: %q", c.engine, cmd)
			continue
		}
		n, _ := strconv.Atoi(m[1])
		total := time.Duration(n) * time.Second
		if total != clientWait || total >= readWait {
			t.Errorf("%s's tool runs for %s at most, which isn't within the reading's %s", c.engine, total, readWait)
		}
		for limit, unit := range c.limits {
			m := regexp.MustCompile(limit).FindStringSubmatch(said)
			if m == nil {
				t.Errorf("%s reads with no %s", c.engine, limit)
				continue
			}
			n, _ := strconv.Atoi(m[1])
			if got := time.Duration(n) * unit; got <= 0 || got >= total {
				t.Errorf("%s gives up after %s by %s, which isn't before its tool's %s", c.engine, got, limit, total)
			}
		}
	}
}

// timeout ending a tool reads as a database that didn't answer, as coreutils and
// busybox each say it.
func TestAToolTimeoutEndsSaysItDidntAnswer(t *testing.T) {
	for _, code := range []int{124, 143} {
		e := dockertest.New(t)
		e.AddLocalImage("redis:7", dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}, Exec: func(dockertest.Exec) int { return code }})
		client := e.Client()
		id, err := client.CreateContainer(t.Context(), "c-redis", &docker.CreateRequest{Config: docker.Config{Image: "redis:7"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.StartContainer(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		eng := Recognise("redis:7")
		cmd, _ := eng.reading(0)
		_, err = Reader{Client: client}.ask(t.Context(), id, eng, cmd, "")
		if want := "Redis didn't answer within 7s"; err == nil || err.Error() != want {
			t.Errorf("a tool timeout ended with %d says %v, want %q", code, err, want)
		}
	}
}

func TestARefusedStatementSaysWhyInItsEnginesWords(t *testing.T) {
	e := dockertest.New(t)
	said := map[string]string{
		"postgres:17": "psql:<stdin>:1: NOTICE:  role \"x\" does not exist, skipping\npsql:<stdin>:2: ERROR:  shop has something outside its database shop, so nothing was dropped\nCONTEXT:  PL/pgSQL function inline_code_block line 1 at RAISE\n",
		"mysql:8.4":   "ERROR 1049 (42000) at line 3: Unknown database 'shopx'\n",
	}
	for image, stderr := range said {
		e.AddLocalImage(image, dockertest.Image{Config: docker.Config{Cmd: []string{"x"}}, Exec: func(x dockertest.Exec) int {
			_, _ = io.WriteString(x.Stderr, stderr)
			return 3
		}})
	}
	client := e.Client()
	for image, want := range map[string]string{
		"postgres:17": "PostgreSQL refused it: shop has something outside its database shop, so nothing was dropped",
		"mysql:8.4":   "MySQL refused it: Unknown database '...'",
	} {
		id, err := client.CreateContainer(t.Context(), "c-"+strings.Split(image, ":")[0], &docker.CreateRequest{Config: docker.Config{Image: image}})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.StartContainer(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		eng := Recognise(image)
		cmd, _ := eng.reading(0)
		_, err = Reader{Client: client}.ask(t.Context(), id, eng, cmd, "SELECT 1;")
		if err == nil || err.Error() != want {
			t.Errorf("%s says %v, want %q", image, err, want)
		}
	}
}

// A database that doesn't take connections yet, as it starts, says so in a way a
// caller can wait on, rather than as a statement it refused.
func TestADatabaseThatsStartingSaysSo(t *testing.T) {
	e := dockertest.New(t)
	said := map[string]string{
		"postgres:17": "psql: error: connection to server on socket \"/var/run/postgresql/.s.PGSQL.5432\" failed: FATAL:  the database system is starting up\n",
		"postgres:16": "psql: error: connection to server on socket \"/var/run/postgresql/.s.PGSQL.5432\" failed: No such file or directory\n",
		"mysql:8.4":   "ERROR 2002 (HY000): Can't connect to local MySQL server through socket '/var/run/mysqld/mysqld.sock' (2)\n",
		"mariadb:12":  "ERROR 2002 (HY000): Can't connect to local server through socket '/run/mysqld/mysqld.sock' (2)\n",
	}
	for image, stderr := range said {
		e.AddLocalImage(image, dockertest.Image{Config: docker.Config{Cmd: []string{"x"}}, Exec: func(x dockertest.Exec) int {
			_, _ = io.WriteString(x.Stderr, stderr)
			return 2
		}})
	}
	client := e.Client()
	for image := range said {
		id, err := client.CreateContainer(t.Context(), "c-"+strings.ReplaceAll(image, ":", "-"), &docker.CreateRequest{Config: docker.Config{Image: image}})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.StartContainer(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		eng := Recognise(image)
		cmd, _ := eng.reading(0)
		_, err = Reader{Client: client}.ask(t.Context(), id, eng, cmd, "SELECT 1;")
		if !errors.Is(err, ErrStarting) || err.Error() != eng.Name+" isn't taking connections yet" {
			t.Errorf("%s says %v, want that it isn't taking connections yet", image, err)
		}
	}
}
