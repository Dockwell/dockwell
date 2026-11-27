// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"path"
	"slices"
	"strconv"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// ClickHouseHTTPPort is the image's default HTTP endpoint.
const ClickHouseHTTPPort = 8123

// ClickHouseAuthSetup hashes inside the container before running its entrypoint.
// The read-only users config refers to the resulting hash.
const ClickHouseAuthSetup = `test -n "$CLICKHOUSE_PASSWORD" || { echo "` + EmptyClickHouseCredential + `" >&2; exit 1; }; ` + clickHouseHashEnv

const clickHouseHashEnv = `hash=$(printf %s "$CLICKHOUSE_PASSWORD" | sha256sum); export DOCKWELL_CLICKHOUSE_PASSWORD_SHA256="$(printf %s "$hash" | cut -d " " -f 1)"; exec "$@"`

// ClickHouseAuthTarget is the users config kept outside the data volume.
const ClickHouseAuthTarget = "/etc/clickhouse-server/users.d/zz-dockwell-password.xml"

// ClickHouseLogsTarget is the bounded logging config for ClickHouse.
const ClickHouseLogsTarget = "/etc/clickhouse-server/config.d/zz-dockwell-logs.xml"

// ClickHouseAuthWrapped recognizes the managed startup wrapper, including the
// original wrapper already used by installs.
func ClickHouseAuthWrapped(cmd []string) bool {
	return len(cmd) > 4 && cmd[0] == "sh" && cmd[1] == "-ec" && cmd[3] == "sh" &&
		(cmd[2] == ClickHouseAuthSetup || cmd[2] == `test -n "$CLICKHOUSE_PASSWORD"; `+clickHouseHashEnv)
}

// WrapClickHouseAuth runs the ClickHouse server command with the managed password hash.
func WrapClickHouseAuth(cmd []string) []string {
	if ClickHouseAuthWrapped(cmd) {
		cmd = cmd[4:]
	}
	if len(cmd) == 0 || strings.HasPrefix(cmd[0], "-") {
		cmd = append([]string{"/entrypoint.sh"}, cmd...)
	}
	return append([]string{"sh", "-ec", ClickHouseAuthSetup, "sh"}, cmd...)
}

// EmptyClickHouseCredential is safe to show in a plan and in startup logs.
const EmptyClickHouseCredential = "CLICKHOUSE_PASSWORD is empty. Set it in the service's secret"

// ClickHouse reports whether this engine needs the ClickHouse client and endpoints.
func (e *Engine) ClickHouse() bool {
	return e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_CLICKHOUSE
}

func (e *Engine) clickHouseServer(cmd []string) bool {
	return e.ClickHouse() && (path.Base(cmd[0]) == "entrypoint.sh" ||
		path.Base(cmd[0]) == "clickhouse" && len(cmd) > 1 && cmd[1] == "server")
}

func clickHousePort(cmd []string, flag string, fallback uint32) uint32 {
	if p, ok := port(FlagValue(cmd, flag)); ok {
		return p
	}
	return fallback
}

// The native client reads CLICKHOUSE_USER and CLICKHOUSE_PASSWORD itself.
func clickHouseClient(port uint32, database string, args ...string) []string {
	cmd := []string{"clickhouse-client", "--host=127.0.0.1"}
	if port != 0 {
		cmd = append(cmd, "--port="+strconv.FormatUint(uint64(port), 10))
	}
	if database != "" {
		cmd = append(cmd, "--database="+database)
	}
	return append(cmd, args...)
}

// WithPasswordFile has the ClickHouse client read the image's password file when set.
func (e *Engine) WithPasswordFile(cmd, env []string) []string {
	if !e.ClickHouse() || envMap(env)["CLICKHOUSE_PASSWORD_FILE"] == "" {
		return cmd
	}
	return append([]string{
		"sh", "-c",
		`CLICKHOUSE_PASSWORD="$(cat "$CLICKHOUSE_PASSWORD_FILE")" || exit 1; export CLICKHOUSE_PASSWORD; exec "$@"`, "sh",
	}, cmd...)
}

func (r Reader) checkClickHouse(ctx context.Context, c *docker.Container, port uint32, reading *dockwellv1.DatabaseReading) {
	// An explicit empty config prevents client-side defaults from supplying
	// credentials. The native client rejects an empty --password argument.
	cmd := clickHouseClient(port, "", "--user="+reading.GetUser(), "--config-file=/dev/stdin", "--query=SELECT 1")
	var out, stderr capped
	out.max, stderr.max = 128, 4<<10
	code, err := r.Client.ExecWith(ctx, c.ID, cmd, docker.ExecOptions{Env: []string{"CLICKHOUSE_PASSWORD="}},
		docker.Streams{Stdin: strings.NewReader("<clickhouse/>"), Stdout: &out, Stderr: &stderr})
	if err == nil {
		switch {
		case code == 0 && strings.TrimSpace(out.String()) == "1":
			reading.ChecksNoOne = true
			reading.ClickhousePlaintextPassword = false
		case code != 0 && strings.Contains(stderr.String(), "AUTHENTICATION_FAILED"):
			reading.ClickhousePasswordVerified = true
		}
	}
	if slices.ContainsFunc(c.Mounts, func(m docker.MountPoint) bool { return m.Destination == ClickHouseLogsTarget && !m.RW }) {
		// Turning the writers off leaves historical system tables on disk.
		reading.ClickhouseUnboundedLogs = false
	} else if !reading.GetClickhouseUnboundedLogs() {
		reading.ClickhouseUnboundedLogs = r.clickHouseVerboseLog(ctx, c.ID)
	}
}

func (r Reader) clickHouseVerboseLog(ctx context.Context, id string) bool {
	out := &capped{max: maxSaid}
	code, err := r.Client.Exec(ctx, id, []string{"cat", "/var/lib/clickhouse/preprocessed_configs/config.xml"}, docker.Streams{Stdout: out})
	if err != nil || code != 0 || out.over {
		return false
	}
	var config struct {
		Logger struct {
			Level    string `xml:"level"`
			Log      string `xml:"log"`
			ErrorLog string `xml:"errorlog"`
			Size     string `xml:"size"`
			Count    string `xml:"count"`
		} `xml:"logger"`
	}
	if xml.Unmarshal(out.Bytes(), &config) != nil || config.Logger.Log == "" && config.Logger.ErrorLog == "" {
		return false
	}
	log := config.Logger
	return log.Level == "trace" || log.Level == "debug" || log.Level == "test" || log.Size == "" || log.Count == ""
}

// Each SELECT returns metadata only. The size covers active MergeTree parts, and
// the counters are for ClickHouse's mark cache and TCP and HTTP listeners.
const clickHouseRead = `
SELECT version() AS version, currentUser() AS user, uptime() AS uptime,
  (SELECT sum(bytes_on_disk) FROM system.parts WHERE active) AS size_bytes,
  (SELECT sum(value) FROM system.metrics WHERE metric IN ('TCPConnection', 'HTTPConnection')) AS connections,
  (SELECT toUInt64OrZero(any(value)) FROM system.server_settings WHERE name = 'max_connections') AS max_connections,
  (SELECT sum(value) FROM system.events WHERE event = 'MarkCacheHits') AS hits,
  (SELECT sum(value) FROM system.events WHERE event = 'MarkCacheMisses') AS misses,
  (SELECT toBool(countIf(toString(auth_type) LIKE '%plaintext_password%') > 0) FROM system.users WHERE name = currentUser()) AS plaintext_password,
  (SELECT toBool(count() > 0) FROM system.tables WHERE database = 'system' AND endsWith(name, '_log')
    AND engine LIKE '%MergeTree' AND NOT match(engine_full, '(?i)[[:space:]]TTL[[:space:]]')) AS unbounded_logs,
  greatest((SELECT count() FROM system.databases), 200) - 200 AS unlisted
FORMAT JSONEachRow SETTINGS output_format_json_quote_64bit_integers = 0;

SELECT d.name AS name, d.engine AS engine, coalesce(p.size_bytes, 0) AS size_bytes
FROM system.databases AS d LEFT JOIN
  (SELECT database, sum(bytes_on_disk) AS size_bytes FROM system.parts WHERE active GROUP BY database) AS p
ON d.name = p.database ORDER BY d.name LIMIT 200
FORMAT JSONEachRow SETTINGS output_format_json_quote_64bit_integers = 0;
`

func parseClickHouse(said []byte) (*dockwellv1.DatabaseReading, error) {
	rows := bufio.NewScanner(bytes.NewReader(said))
	rows.Buffer(make([]byte, 4096), maxSaid)
	if !rows.Scan() {
		return nil, errors.New("no server facts")
	}
	var server struct {
		Version           string `json:"version"`
		User              string `json:"user"`
		Uptime            int64  `json:"uptime"`
		SizeBytes         int64  `json:"size_bytes"`
		Connections       int64  `json:"connections"`
		MaxConnections    int64  `json:"max_connections"`
		Hits              int64  `json:"hits"`
		Misses            int64  `json:"misses"`
		Unlisted          int32  `json:"unlisted"`
		PlaintextPassword bool   `json:"plaintext_password"`
		UnboundedLogs     bool   `json:"unbounded_logs"`
	}
	if json.Unmarshal(rows.Bytes(), &server) != nil || server.Version == "" {
		return nil, errors.New("invalid server facts")
	}
	read := &dockwellv1.DatabaseReading{
		Version: server.Version, User: server.User, UptimeSeconds: server.Uptime,
		SizeBytes: server.SizeBytes, Connections: server.Connections, MaxConnections: server.MaxConnections,
		CacheHit: share(server.Hits, server.Misses), Unlisted: server.Unlisted,
		ClickhousePlaintextPassword: server.PlaintextPassword, ClickhouseUnboundedLogs: server.UnboundedLogs,
	}
	for rows.Scan() {
		var db struct {
			Name      string `json:"name"`
			Engine    string `json:"engine"`
			SizeBytes int64  `json:"size_bytes"`
		}
		if json.Unmarshal(rows.Bytes(), &db) != nil || db.Name == "" {
			return nil, errors.New("invalid database facts")
		}
		name := strings.ToLower(db.Name)
		read.Databases = append(read.Databases, &dockwellv1.InnerDatabase{
			Name: db.Name, Engine: db.Engine, SizeBytes: db.SizeBytes,
			System: name == "system" || name == "information_schema",
		})
	}
	return read, rows.Err()
}
