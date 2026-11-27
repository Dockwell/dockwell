// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"path"
	"strconv"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

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

func clickHousePasswordFile(cmd []string) []string {
	return append([]string{
		"sh", "-c",
		`CLICKHOUSE_PASSWORD="$(cat "$CLICKHOUSE_PASSWORD_FILE")" || exit 1; export CLICKHOUSE_PASSWORD; exec "$@"`, "sh",
	}, cmd...)
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
		Version        string `json:"version"`
		User           string `json:"user"`
		Uptime         int64  `json:"uptime"`
		SizeBytes      int64  `json:"size_bytes"`
		Connections    int64  `json:"connections"`
		MaxConnections int64  `json:"max_connections"`
		Hits           int64  `json:"hits"`
		Misses         int64  `json:"misses"`
		Unlisted       int32  `json:"unlisted"`
	}
	if json.Unmarshal(rows.Bytes(), &server) != nil || server.Version == "" {
		return nil, errors.New("invalid server facts")
	}
	read := &dockwellv1.DatabaseReading{
		Version: server.Version, User: server.User, UptimeSeconds: server.Uptime,
		SizeBytes: server.SizeBytes, Connections: server.Connections, MaxConnections: server.MaxConnections,
		CacheHit: share(server.Hits, server.Misses), Unlisted: server.Unlisted,
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
