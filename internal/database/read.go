// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// readWait is how long one database gets to answer, so one that hangs costs its own
// row and not the others'. It's a variable for a test.
var readWait = 8 * time.Second

// ReadWait is how long one database gets to answer. A server's answer for all of its
// databases waits for the slowest, so whatever asks a server waits longer than this.
func ReadWait() time.Duration { return readWait }

// MaxListed is how many databases, and how many users, a reading lists. The rest
// are counted.
const MaxListed = 200

// maxSaid is the most of what a database's tool says that's read. A reading of
// MaxListed databases and users comes to well under this.
const maxSaid = 1 << 20

// Reader reads databases on this server, each in its own container.
type Reader struct {
	Client *docker.Client
}

// Read answers a database call: one reading for each database asked about, in
// order. One that can't be read says why, and costs no other.
func (r Reader) Read(ctx context.Context, call *agentv1.DatabaseCall) (*agentv1.DatabaseResult, error) {
	if r.Client == nil {
		return nil, errors.New("this agent can't read databases, as it has no Docker driver")
	}
	out := &agentv1.DatabaseResult{Readings: make([]*dockwellv1.DatabaseReading, len(call.GetDatabases()))}
	var wg sync.WaitGroup
	for i, asked := range call.GetDatabases() {
		wg.Go(func() {
			out.Readings[i] = r.one(ctx, asked)
		})
	}
	wg.Wait()
	return out, ctx.Err()
}

// one reads a database, or says why it can't.
func (r Reader) one(ctx context.Context, asked *agentv1.DatabaseAsked) *dockwellv1.DatabaseReading {
	ctx, cancel := context.WithTimeout(ctx, readWait)
	defer cancel()
	e := Of(asked.GetEngine())
	if e == nil {
		return unread("This agent doesn't know that database. Update it to the control plane's version")
	}
	ref := resourcepb.FromRef(asked.GetService())
	c, err := r.Client.ContainerOf(ctx, docker.Project(ref.Scope), ref.Name, 1)
	switch {
	case docker.IsNotFound(err):
		return unread("Its container isn't on this server")
	case err != nil:
		return unread("Dockwell can't read its container: " + clause(err))
	case !c.State.Running || c.State.Restarting:
		return unread(e.Name + " isn't running, so it can't say how it is")
	}
	if why := e.TooOld(c.Config.Image); why != "" {
		return unread(why)
	}
	cmd, input := e.reading(PortOf(e, c.Config.Cmd))
	said, err := r.ask(ctx, c.ID, e, cmd, input)
	if err != nil {
		return unread(err.Error())
	}
	reading, err := e.parse(said)
	if err != nil {
		return unread(fmt.Sprintf("Dockwell can't read what %s said: %s", e.Name, clause(err)))
	}
	return reading
}

func unread(why string) *dockwellv1.DatabaseReading {
	return &dockwellv1.DatabaseReading{Unread: strings.TrimSuffix(why, ".")}
}

// ask runs one of the database's tools in its container, with input on its stdin,
// and gives what it printed. What fails says why in plain words.
func (r Reader) ask(ctx context.Context, container string, e *Engine, cmd []string, input string) ([]byte, error) {
	out := &capped{max: maxSaid}
	errs := &capped{max: 4 << 10}
	code, err := r.Client.Exec(ctx, container, cmd, docker.Streams{Stdin: strings.NewReader(input), Stdout: out, Stderr: errs})
	said := strings.TrimSpace(errs.String())
	if said == "" && code != 0 {
		said = strings.TrimSpace(out.String())
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		return nil, fmt.Errorf("%s didn't answer within %s", e.Name, readWait)
	case err != nil:
		return nil, fmt.Errorf("can't run %s's own tools in its container: %s", e.Name, clause(err))
	case code == 124 || code == 143:
		// timeout ended it: 124 from coreutils, 143 from busybox's SIGTERM.
		return nil, fmt.Errorf("%s didn't answer within %s", e.Name, clientWait)
	case code == 126 || code == 127 || notFound(said):
		return nil, fmt.Errorf("its image has no %s, so Dockwell can't ask it", e.tool())
	case refusedSignIn(said) || e.KeyValue() && redisRefused(out.String()):
		return nil, fmt.Errorf("%s refused Dockwell's sign-in with what its environment holds. Dockwell signs in as its image does, with %s", e.Name, e.SignsInWith())
	case code != 0:
		return nil, fmt.Errorf("%s didn't answer, with code %d: %s", e.Name, code, scrubbed(said))
	case out.over:
		return nil, fmt.Errorf("%s said more than Dockwell reads, %d bytes", e.Name, maxSaid)
	}
	return out.Bytes(), nil
}

// tool is the command a reading runs, for people.
func (e *Engine) tool() string {
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		return "psql"
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:
		return "mysql"
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		return "mariadb"
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY:
		return "valkey-cli"
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB:
		return "mongosh"
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED, dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS:
	}
	return "redis-cli"
}

// notFound says a shell or Docker couldn't find the tool.
func notFound(said string) bool {
	return strings.Contains(said, ": not found") || strings.Contains(said, "command not found") || strings.Contains(said, "executable file not found")
}

// redisRefused says what redis-cli printed is Redis refusing who signed in: a reply
// that is an error, as INFO's own lines can name past refusals, such as
// errorstat_WRONGPASS, which are anyone's.
func redisRefused(out string) bool {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	first = strings.TrimPrefix(first, "(error) ")
	return strings.HasPrefix(first, "NOAUTH") || strings.HasPrefix(first, "WRONGPASS") || strings.HasPrefix(first, "AUTH failed")
}

// refusedSignIn says a tool's words are a database refusing who signed in.
func refusedSignIn(said string) bool {
	for _, words := range []string{
		"password authentication failed", "Access denied", "NOAUTH", "WRONGPASS", "no password supplied", "invalid password",
		// MongoDB's, for a wrong password, and for none where it checks.
		"Authentication failed", "requires authentication",
	} {
		if strings.Contains(said, words) {
			return true
		}
	}
	return false
}

// reading is the command that reads the engine, and what it's given on stdin. It
// reads the password from the container's own environment, as the image does, so it
// never passes through the agent.
func (e *Engine) reading(port uint32) ([]string, string) {
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		return withPort("PGPORT", port, postgresClient), postgresRead
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:
		// MySQL and MariaDB answer on their socket, wherever their port is.
		return mysqlClient, mysqlSession + mysqlRead
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		return mysqlClient, mariadbSession + mysqlRead
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB:
		return withPort("DW_PORT", port, mongoClient), mongoRead
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED, dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS,
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY:
	}
	if port == 0 {
		port = e.Port
	}
	return withPort("DW_PORT", port, redisClient), ""
}

// withPort runs cmd with the port in variable, where the command line set one.
func withPort(variable string, port uint32, cmd []string) []string {
	if port == 0 {
		return cmd
	}
	return append([]string{"env", variable + "=" + strconv.FormatUint(uint64(port), 10)}, cmd...)
}

// The tools, as each official image has them, signing in as the image does. Each
// runs under timeout, from coreutils or busybox, which every official image has, so
// it ends within what Dockwell waits however its connection and statements go, and
// a database that's slow to answer isn't left with readings piling up on its
// connections: Docker's stream closing doesn't stop what runs in the container. The
// engines' own limits are shorter, so their own words come first.
var (
	postgresClient = []string{"sh", "-c", `export PGPASSWORD="${POSTGRES_PASSWORD:-}" PGCONNECT_TIMEOUT=5 PGOPTIONS='-c lc_messages=C -c statement_timeout=6000'; exec timeout 7 psql -X -q -A -t -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-postgres}" -d postgres -f -`}
	mysqlClient    = []string{"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; c=mysql; command -v mariadb >/dev/null 2>&1 && c=mariadb; exec timeout 7 "$c" -uroot -N -B --connect-timeout=5`}
	redisClient    = []string{"sh", "-c", `p="${VALKEY_PASSWORD:-${REDIS_PASSWORD:-}}"; [ -n "$p" ] && export REDISCLI_AUTH="$p"; c=redis-cli; command -v valkey-cli >/dev/null 2>&1 && c=valkey-cli; exec timeout 7 "$c" -p "${DW_PORT:-6379}" INFO`}
)

// mongoSignIn reads the root user the image made, from MONGO_INITDB_ROOT_USERNAME
// and MONGO_INITDB_ROOT_PASSWORD or their _FILE forms, into those variables, where
// mongosh's script reads them: never an argument, which ps shows anyone on the
// server.
const mongoSignIn = `[ -n "${MONGO_INITDB_ROOT_USERNAME_FILE:-}" ] && MONGO_INITDB_ROOT_USERNAME="$(cat "$MONGO_INITDB_ROOT_USERNAME_FILE")"; ` +
	`[ -n "${MONGO_INITDB_ROOT_PASSWORD_FILE:-}" ] && MONGO_INITDB_ROOT_PASSWORD="$(cat "$MONGO_INITDB_ROOT_PASSWORD_FILE")"; ` +
	`export MONGO_INITDB_ROOT_USERNAME MONGO_INITDB_ROOT_PASSWORD; `

// mongoClient runs mongosh on the script it's given on stdin, as the image's own
// entrypoint does. The image's HOME is the data directory, where mongosh would
// keep a log of each run, so it gets one of its own for the run, which goes after.
var mongoClient = []string{"sh", "-c", mongoSignIn +
	`d="$(mktemp -d)" || exit 1; HOME="$d" timeout 7 mongosh --quiet --norc --nodb --file /dev/stdin; c=$?; rm -rf "$d"; exit "$c"`}

// mongoReader is the name a reading's connections carry, so it counts only
// others'.
const mongoReader = "dockwell-reading"

// mongoRead asks MongoDB everything at once, as one line of JSON. It signs in as
// the image's root user where there is one, and asks nothing that holds a
// credential: usersInfo leaves them out unless asked for them.
var mongoRead = `const env = process.env;
const n = (v) => (v == null ? 0 : typeof v === "object" && typeof v.toNumber === "function" ? v.toNumber() : Number(v));
const ok = (r) => { if (!r.ok) throw new Error(r.errmsg || "it refused"); return r; };
const m = new Mongo("mongodb://127.0.0.1:" + (env.DW_PORT || "27017") + "/?directConnection=true&serverSelectionTimeoutMS=5000&connectTimeoutMS=5000&appName=` + mongoReader + `");
const admin = m.getDB("admin");
const user = env.MONGO_INITDB_ROOT_USERNAME || "";
if (user) admin.auth(user, env.MONGO_INITDB_ROOT_PASSWORD || "");
const st = ok(admin.runCommand({ serverStatus: 1 }));
const dbs = ok(admin.runCommand({ listDatabases: 1 }));
const users = ok(admin.runCommand({ usersInfo: { forAllDBs: true } })).users || [];
const fcv = admin.runCommand({ getParameter: 1, featureCompatibilityVersion: 1 });
const sec = (ok(admin.runCommand({ getCmdLineOpts: 1 })).parsed || {}).security || {};
const hello = admin.runCommand({ hello: 1 });
const cache = (st.wiredTiger || {}).cache || {};
let others = Math.max(0, n(st.connections.current) - 2);
try {
  others = admin.aggregate([{ $currentOp: { allUsers: true, idleConnections: true } },
    { $match: { client: { $exists: true }, appName: { $ne: "` + mongoReader + `" } } }, { $count: "n" }]).toArray().map((r) => r.n)[0] || 0;
} catch (e) {}
const byName = (a, b) => (a < b ? -1 : a > b ? 1 : 0);
dbs.databases.sort((a, b) => byName(a.name, b.name));
users.sort((a, b) => byName(a.db + "." + a.user, b.db + "." + b.user));
print(JSON.stringify({
  version: st.version, user: user, uptime: Math.floor(n(st.uptime)),
  connections: others, max_connections: n(st.connections.current) + n(st.connections.available),
  requested: n(cache["pages requested from the cache"]), read: n(cache["pages read into cache"]),
  size: n(dbs.totalSize), fcv: fcv.ok ? fcv.featureCompatibilityVersion.version : "",
  replica_set: hello.setName || "", auth: sec.authorization === "enabled" || !!sec.keyFile,
  database_count: dbs.databases.length, user_count: users.length,
  databases: dbs.databases.slice(0, ` + strconv.Itoa(MaxListed) + `).map((d) => ({ name: d.name, size: n(d.sizeOnDisk) })),
  users: users.slice(0, ` + strconv.Itoa(MaxListed) + `).map((u) => ({ name: u.user, db: u.db, roles: (u.roles || []).map((r) => ({ role: r.role, db: r.db })) })),
}));
`

// clientWait is the timeout each tool runs under, within readWait.
const clientWait = 7 * time.Second

// postgresRead asks PostgreSQL everything at once, as one line of JSON, which holds
// any name as it is.
var postgresRead = `SELECT json_build_object(
  'version', current_setting('server_version'),
  'user', current_user,
  'connections', (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()),
  'max_connections', current_setting('max_connections')::bigint,
  'uptime', extract(epoch FROM now() - pg_postmaster_start_time())::bigint,
  'hit', (SELECT coalesce(sum(blks_hit), 0) FROM pg_stat_database),
  'read', (SELECT coalesce(sum(blks_read), 0) FROM pg_stat_database),
  'size', (SELECT coalesce(sum(pg_database_size(oid)), 0) FROM pg_database WHERE NOT datistemplate),
  'database_count', (SELECT count(*) FROM pg_database WHERE NOT datistemplate),
  'user_count', (SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_'),
  'databases', (SELECT coalesce(json_agg(d ORDER BY d.name), '[]'::json) FROM (
    SELECT datname AS name, pg_get_userbyid(datdba) AS owner, pg_database_size(oid) AS size
    FROM pg_database WHERE NOT datistemplate ORDER BY datname LIMIT ` + strconv.Itoa(MaxListed) + `) d),
  'users', (SELECT coalesce(json_agg(u ORDER BY u.name), '[]'::json) FROM (
    SELECT rolname AS name, rolsuper AS super, rolcanlogin AS login
    FROM pg_roles WHERE rolname !~ '^pg_' ORDER BY rolname LIMIT ` + strconv.Itoa(MaxListed) + `) u)
);
`

// mysqlSession has MySQL read sizes as they are now, not as it cached them up to a
// day ago, and give up on a statement after 6 seconds. Each is in a comment that only
// a server new enough runs, as 5.7 has no stats cache and 5.7.8 brought the limit.
// It starts with a variable of its own, as a statement that's only such a comment
// fails on an older server.
const mysqlSession = "SET @dw = 0 /*!50708 , SESSION max_execution_time = 6000 */ /*!80003 , SESSION information_schema_stats_expiry = 0 */;\n"

// mariadbSession has MariaDB give up on a statement after 6 seconds. It keeps no
// cache of sizes.
const mariadbSession = "SET SESSION max_statement_time = 6;\n"

// mysqlRead asks MySQL or MariaDB in rows, each led by what it is, which batch mode
// prints with tabs between columns and any tab, newline or backslash in a name escaped.
var mysqlRead = `SELECT 'v', VERSION(), CURRENT_USER(), @@max_connections;
SHOW GLOBAL STATUS WHERE Variable_name IN ('Threads_connected', 'Uptime', 'Innodb_buffer_pool_read_requests', 'Innodb_buffer_pool_reads');
SELECT 'd', s.SCHEMA_NAME, COALESCE(SUM(t.DATA_LENGTH + t.INDEX_LENGTH), 0) FROM information_schema.SCHEMATA s LEFT JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = s.SCHEMA_NAME GROUP BY s.SCHEMA_NAME ORDER BY s.SCHEMA_NAME;
SELECT 'u', User, Host, Super_priv FROM mysql.user ORDER BY User, Host;
SELECT 'g', User, Host, Db FROM mysql.db ORDER BY User, Host, Db;
`

// capped keeps what's written up to max bytes, and says if there was more.
type capped struct {
	bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(b []byte) (int, error) {
	if room := c.max - c.Len(); len(b) > room {
		c.over = true
		if room > 0 {
			c.Buffer.Write(b[:room])
		}
		return len(b), nil
	}
	return c.Buffer.Write(b)
}

// quoted finds what a database quotes in what it says.
var quoted = regexp.MustCompile(`'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"`)

// scrubbed keeps the first line of what a database said, without what it quotes,
// which can be a value: a reading is there for anyone who can read the database.
func scrubbed(said string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(said), "\n")
	return quoted.ReplaceAllString(first, "'...'")
}

// clause is an error as the end of a sentence.
func clause(err error) string {
	return strings.TrimSuffix(err.Error(), ".")
}
