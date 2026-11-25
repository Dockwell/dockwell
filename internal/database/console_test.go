// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

func TestConsoleCommandUsesEnvNeverArgvPasswords(t *testing.T) {
	cases := []struct {
		kind     dockwellv1.DatabaseEngine
		port     uint32
		db       string
		wantTool string
		wantEnv  []string
		wantSub  []string
		forbid   []string
	}{
		{
			kind:     dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL,
			port:     5433,
			db:       "shop",
			wantTool: "psql",
			wantEnv:  []string{"DW_DB=shop", "PGPORT=5433"},
			wantSub:  []string{"PGPASSWORD", "exec psql -U", `-d "$db"`},
			forbid:   []string{"-W", "password="},
		},
		{
			kind:     dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL,
			db:       "billing",
			wantTool: "mysql",
			wantEnv:  []string{"DW_DB=billing"},
			wantSub:  []string{"MYSQL_PWD", "exec \"$c\" -uroot"},
			forbid:   []string{"-p\"", "-p$"},
		},
		{
			kind:     dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB,
			wantTool: "mariadb",
			wantSub:  []string{"MYSQL_PWD", "mariadb", "exec \"$c\" -uroot"},
			forbid:   []string{"-p\"", "-p$"},
		},
		{
			kind:     dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS,
			port:     6380,
			wantTool: "redis-cli",
			wantEnv:  []string{"DW_PORT=6380"},
			wantSub:  []string{"REDISCLI_AUTH", `exec "$c" -p "${DW_PORT:-6379}"`},
			forbid:   []string{"-a "},
		},
		{
			kind:     dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY,
			wantTool: "valkey-cli",
			wantEnv:  []string{"DW_PORT=6379"},
			wantSub:  []string{"REDISCLI_AUTH", "valkey-cli"},
			forbid:   []string{"-a "},
		},
		{
			kind:     dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB,
			port:     27018,
			db:       "app",
			wantTool: "mongosh",
			wantEnv:  []string{"DW_DB=app", "DW_PORT=27018"},
			wantSub:  []string{".mongoshrc.js", "process.env.MONGO_INITDB_ROOT_PASSWORD", "mongosh"},
			forbid:   []string{"--password", " -p "},
		},
	}
	for _, tc := range cases {
		e := Of(tc.kind)
		cmd, env, tool, err := ConsoleCommand(e, tc.port, tc.db)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", e.Name, err)
		}
		if tool != tc.wantTool {
			t.Errorf("%s: tool = %q, want %q", e.Name, tool, tc.wantTool)
		}
		if len(cmd) != 3 || cmd[0] != "/bin/sh" || cmd[1] != "-c" {
			t.Fatalf("%s: cmd = %v, want [/bin/sh -c <script>]", e.Name, cmd)
		}
		script := cmd[2]
		for _, want := range tc.wantSub {
			if !strings.Contains(script, want) {
				t.Errorf("%s: script %q missing %q", e.Name, script, want)
			}
		}
		for _, bad := range tc.forbid {
			if strings.Contains(script, bad) {
				t.Errorf("%s: script %q must not contain %q", e.Name, script, bad)
			}
		}
		joinedEnv := strings.Join(env, "\n")
		for _, want := range tc.wantEnv {
			if !strings.Contains(joinedEnv, want) {
				t.Errorf("%s: env %v missing %q", e.Name, env, want)
			}
		}
	}
}

func TestCheckConsoleDatabaseNameRejectsUnsafe(t *testing.T) {
	for _, ok := range []string{"", "shop", "my-db", "App_DB.1"} {
		if err := CheckConsoleDatabaseName(ok); err != nil {
			t.Errorf("CheckConsoleDatabaseName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"-flag", "a b", "db/x", "db?auth=1", "db#frag", "db'quote", "db\"quote", "a\x00b", strings.Repeat("a", 129)} {
		if err := CheckConsoleDatabaseName(bad); err == nil {
			t.Errorf("CheckConsoleDatabaseName(%q) = nil, want error", bad)
		}
	}
	if _, _, _, err := ConsoleCommand(Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS), 0, "shop"); err == nil {
		t.Error("expected error when naming a database on Redis")
	}
}
