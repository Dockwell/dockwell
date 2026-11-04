// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

const secret = "s3cret-pw"

func TestTheDatabaseURLStaysOutOfHelpAndErrors(t *testing.T) {
	env := map[string]string{envDatabase: "postgres://dockwell:" + secret + "@db.internal/dockwell"}
	r := run(t, env, "run", "--help")
	if r.code != exitOK || !strings.Contains(r.out, "--database") || strings.Contains(r.out, secret) {
		t.Errorf("dockwell run --help = %d, shows the password: %t\n%s", r.code, strings.Contains(r.out, secret), r.out)
	}
	for _, args := range [][]string{
		{"token", "create", "--data-dir", t.TempDir(), "--database", "mysql://dockwell:" + secret + "@db/dockwell"},
		{"claim", "--data-dir", t.TempDir()},
	} {
		env := map[string]string{envDatabase: "mysql://dockwell:" + secret + "@db/dockwell"}
		if args[0] == "token" {
			env = nil
		}
		r := run(t, env, args...)
		if r.code != exitUsage || !strings.Contains(r.err, "must be a postgres:// URL") || strings.Contains(r.err, secret) {
			t.Errorf("dockwell %s with a MySQL URL = %d %q", args[0], r.code, r.err)
		}
	}
}

// A data directory whose key belongs to an install never starts a second
// install beside it, which its agents wouldn't recognize.
func TestRunNeverStartsASecondInstall(t *testing.T) {
	dir := t.TempDir()
	_, stop := startRunWithOutput(t, dir, "/claim#")
	stop()
	listen := []string{"run", "--listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0", "--data-dir", dir}

	r := run(t, nil, append(listen, "--database", "postgres://dockwell@127.0.0.1:1/dockwell")...)
	if r.code != exitError || !strings.Contains(r.err, "moving it to Postgres isn't supported yet") {
		t.Errorf("run on Postgres beside SQLite state = %d %q", r.code, r.err)
	}

	d := datadir.Dir{Root: dir}
	for _, f := range []string{d.StateDB(), d.StateDB() + "-wal", d.StateDB() + "-shm"} {
		_ = os.Remove(f)
	}
	r = run(t, nil, listen...)
	if r.code != exitError || !strings.Contains(r.err, "its state isn't in "+d.StateDB()) || !strings.Contains(r.err, envDatabase) {
		t.Errorf("run with the key but no state = %d %q", r.code, r.err)
	}
	if _, err := os.Stat(d.StateDB()); err == nil {
		t.Error("the refused start made an empty state.db")
	}
}

func TestRunOnPostgres(t *testing.T) {
	location := storetest.New(t, store.Postgres)
	dir := t.TempDir()
	out, logs, stop := startRunWithLogs(t, dir, "/claim#", "--database", location)
	stop()
	if !strings.Contains(logs.String(), `"backend":"postgres"`) {
		t.Errorf("dockwell run should log that it runs on Postgres:\n%s", logs.String())
	}
	if u, err := url.Parse(location); err == nil {
		if pw, ok := u.User.Password(); ok && strings.Contains(out.String()+logs.String(), pw) {
			t.Error("dockwell run printed the database's password")
		}
	}
	if r := run(t, nil, "role", "--data-dir", dir); r.out != "control\n" {
		t.Errorf("role of a control plane on Postgres = %q, want control", r.out)
	}

	// Commands beside it need the same database, and say so without it.
	none := map[string]string{envLogFormat: "json"}
	r := run(t, none, "claim", "--data-dir", dir)
	if r.code != exitError || !strings.Contains(r.err, "If it keeps its state in Postgres, pass --database or set "+envDatabase) {
		t.Errorf("dockwell claim without the database = %d %q", r.code, r.err)
	}
	with := map[string]string{envLogFormat: "json", envDatabase: location}
	if r := run(t, with, "claim", "--data-dir", dir); r.code != exitOK || !strings.Contains(r.out, "/claim#") {
		t.Errorf("dockwell claim with %s = %d %q", envDatabase, r.code, r.err)
	}

	// It starts again on the same database, and refuses an empty one.
	_, stop = startRunWithOutput(t, dir, "/claim#", "--database", location)
	stop()
	r = run(t, nil, "run", "--listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0", "--data-dir", dir, "--database", storetest.New(t, store.Postgres))
	if r.code != exitError || !strings.Contains(r.err, "holds no Dockwell install, but this data directory's key belongs to one") {
		t.Errorf("run on an empty database with an existing key = %d %q", r.code, r.err)
	}
}

var installRE = regexp.MustCompile(`"install":"([^"]+)"`)

// installOf is the install a dockwell run logged.
func installOf(t *testing.T, logs string) string {
	t.Helper()
	m := installRE.FindStringSubmatch(logs)
	if m == nil {
		t.Fatalf("dockwell run didn't log its install:\n%s", logs)
	}
	return m[1]
}

// A first start that fails partway, such as with the database out of
// reach, leaves no key that claims an install, so the next start makes the
// install. One that stopped once the install was on record, before its key
// took its place, carries on with that install.
func TestAFailedFirstStartCanStartAgain(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		dir := t.TempDir()
		d := datadir.Dir{Root: dir}
		r := run(t, nil, "run", "--listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0", "--data-dir", dir,
			"--database", "postgres://dockwell@127.0.0.1:1/dockwell")
		if r.code != exitError {
			t.Fatalf("a first start with the database out of reach = %d %q, want a failure", r.code, r.err)
		}
		if exists(d.SealKey()) {
			t.Error("the failed first start left a key that claims an install")
		}

		var database []string
		if b == store.Postgres {
			database = []string{"--database", storetest.New(t, b)}
		}
		_, logs, stop := startRunWithLogs(t, dir, "/claim#", database...)
		stop()
		install := installOf(t, logs.String())
		if !exists(d.SealKey()) || exists(d.NewSealKey()) {
			t.Fatal("once the install is on record, its key should be in place")
		}

		// As a first start that stopped right after its install was on
		// record: its key not yet in place, and its own agent not yet
		// enrolled.
		if err := os.Rename(d.SealKey(), d.NewSealKey()); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(d.Agent()); err != nil {
			t.Fatal(err)
		}
		_, logs, stop = startRunWithLogs(t, dir, "/claim#", database...)
		stop()
		if got := installOf(t, logs.String()); got != install {
			t.Errorf("the start after an unfinished one made install %s, want %s carried on", got, install)
		}
		if !exists(d.SealKey()) || exists(d.NewSealKey()) {
			t.Error("the start that finished the install should have put its key in place")
		}
	})
}
