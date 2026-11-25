// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

const maxConsoleDBName = 128

// CheckConsoleDatabaseName checks a database name passed to OpenTerminal for a
// database console. Empty selects the engine's default. The name is handed to
// the client through an environment variable, never interpolated into shell
// syntax, and still rejects control characters, whitespace and URI delimiters
// so neither flags nor MongoDB connection URIs can be altered.
func CheckConsoleDatabaseName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > maxConsoleDBName {
		return fmt.Errorf("a database name is at most %d characters", maxConsoleDBName)
	}
	if strings.HasPrefix(name, "-") {
		return errors.New("a database name can't start with -")
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`/?#@%\'"`, r) {
			return errors.New("that database name has a character a database console won't take")
		}
	}
	return nil
}

// ConsoleCommand returns the interactive command, extra environment variables,
// and client tool name to open an admin console inside a container running e.
// Credentials are read inside the container from its own environment or _FILE
// variables and passed only via client environment variables, never on argv.
func ConsoleCommand(e *Engine, port uint32, dbName string) (cmd, env []string, tool string, err error) {
	if e == nil {
		return nil, nil, "", errors.New("that container doesn't run a database Dockwell knows")
	}
	if err := CheckConsoleDatabaseName(dbName); err != nil {
		return nil, nil, "", err
	}
	if e.KeyValue() && dbName != "" {
		return nil, nil, "", fmt.Errorf("%s has no named databases to connect to", e.Name)
	}
	if dbName != "" {
		env = append(env, "DW_DB="+dbName)
	}
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		if port != 0 {
			env = append(env, "PGPORT="+strconv.FormatUint(uint64(port), 10))
		}
		script := `p="${POSTGRES_PASSWORD:-}"; ` +
			`[ -z "$p" ] && [ -n "${POSTGRES_PASSWORD_FILE:-}" ] && p="$(cat "$POSTGRES_PASSWORD_FILE")"; ` +
			`[ -n "$p" ] && export PGPASSWORD="$p"; ` +
			`db="${DW_DB:-${POSTGRES_DB:-${POSTGRES_USER:-postgres}}}"; ` +
			`exec psql -U "${POSTGRES_USER:-postgres}" -d "$db"`
		return []string{"/bin/sh", "-c", script}, env, "psql", nil

	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		// MySQL and MariaDB connect on their local Unix socket regardless of TCP port.
		script := `p="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; ` +
			`[ -z "$p" ] && [ -n "${MARIADB_ROOT_PASSWORD_FILE:-}" ] && p="$(cat "$MARIADB_ROOT_PASSWORD_FILE")"; ` +
			`[ -z "$p" ] && [ -n "${MYSQL_ROOT_PASSWORD_FILE:-}" ] && p="$(cat "$MYSQL_ROOT_PASSWORD_FILE")"; ` +
			`[ -n "$p" ] && export MYSQL_PWD="$p"; ` +
			`c=mysql; command -v mariadb >/dev/null 2>&1 && c=mariadb; ` +
			`db="${DW_DB:-${MARIADB_DATABASE:-${MYSQL_DATABASE:-}}}"; ` +
			`if [ -n "$db" ]; then exec "$c" -uroot -- "$db"; else exec "$c" -uroot; fi`
		return []string{"/bin/sh", "-c", script}, env, e.tool(), nil

	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS, dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY:
		if port == 0 {
			port = e.Port
		}
		env = append(env, "DW_PORT="+strconv.FormatUint(uint64(port), 10))
		script := `p="${VALKEY_PASSWORD:-${REDIS_PASSWORD:-${REDISCLI_AUTH:-}}}"; ` +
			`[ -n "$p" ] && export REDISCLI_AUTH="$p"; ` +
			`c=redis-cli; command -v valkey-cli >/dev/null 2>&1 && c=valkey-cli; ` +
			`exec "$c" -p "${DW_PORT:-6379}"`
		return []string{"/bin/sh", "-c", script}, env, e.tool(), nil

	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB:
		if port == 0 {
			port = e.Port
		}
		env = append(env, "DW_PORT="+strconv.FormatUint(uint64(port), 10))
		// Authenticate via .mongoshrc.js reading process.env inside a private
		// temp HOME so neither argv nor any file on disk contains the password.
		script := mongoSignIn +
			`d="$(mktemp -d)" || exit 1; ` +
			`chmod 700 "$d"; ` +
			`trap 'rm -rf "$d"' EXIT HUP INT TERM; ` +
			`cat <<'EOF' >"$d/.mongoshrc.js"` + "\n" +
			`if (process.env.MONGO_INITDB_ROOT_USERNAME) { db.getSiblingDB("admin").auth(process.env.MONGO_INITDB_ROOT_USERNAME, process.env.MONGO_INITDB_ROOT_PASSWORD || ""); }` + "\n" +
			`EOF` + "\n" +
			`HOME="$d" mongosh "mongodb://127.0.0.1:${DW_PORT:-27017}/${DW_DB:-}"; ` +
			`c=$?; rm -rf "$d"; exit "$c"`
		return []string{"/bin/sh", "-c", script}, env, "mongosh", nil

	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED:
	}
	return nil, nil, "", fmt.Errorf("no interactive console is available for %s", e.Name)
}
