// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	engines "github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/volumetool"
)

// databaseKind is a database Dockwell can dump and load with its own tools, in
// its own container, where those are the version that wrote the data. The
// commands read the environment the image itself was given, so no password
// passes through the agent.
type databaseKind struct {
	// name is what people call it.
	name string
	// image is its official image, and data where that keeps its data, unless the
	// variable in dataVar says otherwise.
	image, data, dataVar string
	// series is how many parts of an image's version have to match for its files to
	// open: the part that names a release of the database.
	series int
	// needs are the variables one of which it has to have set for the commands to
	// sign in.
	needs []string
	// dump writes the whole server to stdout; load reads that from stdin, and
	// ready exits 0 once the server takes connections over TCP, which the
	// temporary server its image runs while it sets up doesn't.
	dump, load, ready []string
	// complete is what a dump's last lines say if the tool finished it.
	complete string
	// loadFailed reads what load wrote to stderr, one line at a time, for
	// what it can only say there.
	loadFailed func(line string) bool
	// neverReady says what ready exited with and printed won't change however
	// long it's given.
	neverReady func(code int, said string) bool
	// portFlags says the server's command line can set the port the tools have to
	// ask for, as -p or -c port=, and portVar is the variable the tools take it in,
	// PGPORT unless it says.
	portFlags bool
	portVar   string
	// dataFlag is an option of the server's command that moves the data, as
	// MongoDB's --dbpath does.
	dataFlag string
	// format is what its dump is kept as, formatDump unless it says, and verify
	// reads one back to say it's whole, by its last words unless it says.
	format string
	verify verifier
	// replays says a dump may hold an oplog to replay, which its prelude says.
	replays bool
	// For a move to a newer release (ADR 0080). preflight prints a line for each
	// thing that stands in its way, with DW_NATIVE set where users who sign in with
	// mysql_native_password do. census prints what the server holds, sorted, the
	// same on either side of a move, for the new release to be held to. upgradeArgs
	// starts the new release so it brings what it loaded up to its own layout, and
	// upgrade does that in a running one; either fails if that does.
	preflight, census, upgradeArgs, upgrade []string
	// probe, where set, prints ok when a running server's dump keeps everything it
	// holds; one that doesn't is backed up as files, as before it was dumped.
	probe []string
	// dumpOne writes one database, its name the shell's first argument, with what
	// drops what it makes first, owned by no one, so what restores it as the
	// database's owner makes it theirs, for an app's database on a server it shares;
	// completeOne is what such a dump's last lines say when whole. PostgreSQL's runs
	// as the database's owner, the second argument, never the administrator, which a
	// user of a shared server could turn a dump against (CVE-2024-7348). Unset where
	// Dockwell doesn't dump one database alone (ADR 0084).
	dumpOne     []string
	completeOne string
}

// Each official image lets these run from inside with what its environment says.
var databaseKinds = []*databaseKind{
	{
		name: "PostgreSQL", image: "postgres", data: "/var/lib/postgresql/data", dataVar: "PGDATA", series: 1, portFlags: true,
		dump:        []string{"sh", "-c", `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec pg_dumpall -U "${POSTGRES_USER:-postgres}" --clean --if-exists`},
		load:        []string{"sh", "-c", `export PGPASSWORD="${POSTGRES_PASSWORD:-}" PGOPTIONS='-c lc_messages=C'; exec psql -X -q -U "${POSTGRES_USER:-postgres}" -d postgres -f -`},
		ready:       []string{"sh", "-c", `exec pg_isready -q -h 127.0.0.1 -p "${PGPORT:-5432}" -U "${POSTGRES_USER:-postgres}"`},
		complete:    "PostgreSQL database cluster dump complete",
		dumpOne:     []string{"sh", "-c", `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec pg_dump -U "$2" ${3:+"--role=$3"} --no-owner --no-acl --clean --if-exists -d "$1"`, "sh"},
		completeOne: "PostgreSQL database dump complete",
		loadFailed:  postgresLoadFailed,
		// pg_isready exits 3 when it never tried: its own command line is wrong.
		neverReady: func(code int, _ string) bool { return code == 3 },
		preflight:  []string{"sh", "-c", postgresPreflight},
		census:     []string{"sh", "-c", postgresCensus},
	},
	{
		name: "MySQL", image: "mysql", data: "/var/lib/mysql", needs: []string{"MYSQL_ROOT_PASSWORD"}, series: 2,
		dump:        []string{"sh", "-c", `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysqldump -uroot --all-databases --single-transaction --routines --events --triggers --hex-blob --flush-privileges --set-gtid-purged=OFF`},
		dumpOne:     []string{"sh", "-c", `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysqldump -uroot --single-transaction --routines --events --triggers --hex-blob --set-gtid-purged=OFF "$1"`, "sh"},
		completeOne: "Dump completed",
		load:        []string{"sh", "-c", `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysql -uroot --binary-mode=1`},
		ready:       []string{"sh", "-c", `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysql --protocol=TCP -h127.0.0.1 -uroot -e 'SELECT 1'`},
		complete:    "Dump completed",
		neverReady:  func(_ int, said string) bool { return strings.Contains(said, "Access denied") },
		preflight:   []string{"sh", "-c", `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; c=mysql; ` + sqlPreflight},
		census:      []string{"sh", "-c", `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysql -uroot -N -B -e "` + sqlCensus + `"`},
		// From 8.0.16 the server upgrades what it holds as it starts; FORCE makes it
		// go through tables a load wrote in the old layout.
		upgradeArgs: []string{"--upgrade=FORCE"},
	},
	{
		// Its tools are mariadb and mariadb-dump from 10.5, and mysql and mysqldump
		// before; its root password is in either variable, its own first.
		name: "MariaDB", image: "mariadb", data: "/var/lib/mysql", needs: []string{"MARIADB_ROOT_PASSWORD", "MYSQL_ROOT_PASSWORD"}, series: 2,
		// From 10.11 its dump keeps a system-versioned table's history, with
		// --dump-history; an older one's would lose it, so a server with such a table
		// is backed up as files (probe).
		dump: []string{"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}"; ` +
			`tool=mariadb-dump; command -v "$tool" >/dev/null 2>&1 || tool=mysqldump; ` +
			`history=""; "$tool" --help 2>/dev/null | grep -q -- '--dump-history' && history=--dump-history; ` +
			`exec "$tool" -uroot --all-databases --single-transaction --routines --events --triggers --hex-blob --flush-privileges $history`},
		dumpOne: []string{"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}"; ` +
			`tool=mariadb-dump; command -v "$tool" >/dev/null 2>&1 || tool=mysqldump; ` +
			`exec "$tool" -uroot --single-transaction --routines --events --triggers --hex-blob "$1"`, "sh"},
		completeOne: "Dump completed",
		probe: []string{"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}"; ` +
			`tool=mariadb-dump; command -v "$tool" >/dev/null 2>&1 || tool=mysqldump; ` +
			`if "$tool" --help 2>/dev/null | grep -q -- '--dump-history'; then echo ok; exit 0; fi; ` +
			`c=mariadb; command -v "$c" >/dev/null 2>&1 || c=mysql; ` +
			`n=$("$c" -uroot -N -B -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_type = 'SYSTEM VERSIONED'") || exit 1; ` +
			`[ "$n" = 0 ] && echo ok; exit 0`},
		preflight: []string{"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}"; ` +
			`c=mariadb; command -v "$c" >/dev/null 2>&1 || c=mysql; ` + sqlPreflight},
		census: []string{"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}"; ` +
			`c=mariadb; command -v "$c" >/dev/null 2>&1 || c=mysql; exec "$c" -uroot -N -B -e "` + sqlCensus + `"`},
		upgrade: []string{"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}"; ` +
			`tool=mariadb-upgrade; command -v "$tool" >/dev/null 2>&1 || tool=mysql_upgrade; exec "$tool" -uroot --force`},
		load: []string{"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}"; ` +
			`tool=mariadb; command -v "$tool" >/dev/null 2>&1 || tool=mysql; exec "$tool" -uroot --binary-mode=1`},
		ready: []string{"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}"; ` +
			`tool=mariadb; command -v "$tool" >/dev/null 2>&1 || tool=mysql; exec "$tool" --protocol=TCP -h127.0.0.1 -uroot -e 'SELECT 1'`},
		complete:   "Dump completed",
		neverReady: func(_ int, said string) bool { return strings.Contains(said, "Access denied") },
	},
	{
		name: "MongoDB", image: "mongo", data: "/data/db", dataFlag: "--dbpath", series: 2, portFlags: true, portVar: "DW_PORT",
		dump: mongoDump, load: mongoLoad, ready: mongoReady,
		format: formatMongoDump, verify: verifyMongoArchive, replays: true,
		// mongoReady exits 2 for a sign-in its server refuses for good, and 3 for a
		// member of a set with no configuration.
		neverReady: func(code int, _ string) bool { return code == 2 || code == 3 },
	},
}

// What a move asks PostgreSQL before it stops anything, a line for each thing in the
// way: a database that takes no connections, which pg_dumpall leaves out, and
// replication either way. pg_subscription is from 10.
const postgresPreflight = `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; u="${POSTGRES_USER:-postgres}"; ` +
	`psql -X -q -At -U "$u" -d postgres -c "SELECT 'noconnect ' || datname FROM pg_database WHERE NOT datallowconn AND datname <> 'template0' ` +
	`UNION ALL SELECT 'replica' WHERE pg_is_in_recovery() ` +
	`UNION ALL SELECT 'replicas ' || count(*) FROM pg_stat_replication HAVING count(*) > 0 ` +
	`UNION ALL SELECT 'slot ' || slot_name FROM pg_replication_slots" || exit 1; ` +
	`psql -X -q -At -U "$u" -d postgres -c "SELECT 'subscription ' || subname FROM pg_subscription" 2>/dev/null; exit 0`

// What PostgreSQL holds, as a move compares it: its roles but those it makes itself,
// and each database that takes connections, with how many tables it has.
const postgresCensus = `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; u="${POSTGRES_USER:-postgres}"; ` +
	`q() { psql -X -q -At -U "$u" -d "$1" -c "$2"; }; ` +
	`q postgres "SELECT 'role ' || rolname FROM pg_roles WHERE rolname !~ '^pg_' ORDER BY 1" || exit 1; ` +
	`dbs=$(q postgres "SELECT datname FROM pg_database WHERE datallowconn ORDER BY 1") || exit 1; ` +
	`printf '%s\n' "$dbs" | while IFS= read -r d; do [ -n "$d" ] || continue; ` +
	`n=$(q "$d" "SELECT count(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema')") || exit 1; ` +
	`echo "database $d $n"; done`

// What a move asks MySQL or MariaDB, with $c its client: replication either way, and
// with DW_NATIVE, its users who sign in with mysql_native_password, which MySQL 8.4
// doesn't load unless told to.
const sqlPreflight = `r=$("$c" -uroot -N -B -e 'SHOW REPLICA STATUS' 2>/dev/null || "$c" -uroot -N -B -e 'SHOW SLAVE STATUS') || exit 1; ` +
	`[ -n "$r" ] && echo replica; ` +
	`h=$("$c" -uroot -N -B -e 'SHOW REPLICAS' 2>/dev/null || "$c" -uroot -N -B -e 'SHOW REPLICA HOSTS' 2>/dev/null || "$c" -uroot -N -B -e 'SHOW SLAVE HOSTS') || exit 1; ` +
	`[ -n "$h" ] && echo replicas; ` +
	`if [ -n "$DW_NATIVE" ]; then "$c" -uroot -N -B -e "SELECT CONCAT('native ', user, '@', host) FROM mysql.user WHERE plugin = 'mysql_native_password' AND user NOT LIKE 'mysql.%'" || exit 1; fi; exit 0`

// What MySQL or MariaDB holds, as a move compares it: each database but its own,
// with how many tables it has, and its users but those it makes itself.
const sqlCensus = `SELECT CONCAT('database ', s.schema_name, ' ', COUNT(t.table_name)) FROM information_schema.schemata s ` +
	`LEFT JOIN information_schema.tables t ON t.table_schema = s.schema_name ` +
	`WHERE s.schema_name NOT IN ('mysql', 'sys', 'information_schema', 'performance_schema') GROUP BY s.schema_name ORDER BY 1; ` +
	`SELECT CONCAT('user ', user, '@', host) FROM mysql.user WHERE user NOT LIKE 'mysql.%' AND user <> 'mariadb.sys' ORDER BY 1`

// dumpFormat is what a kind's dump is kept as.
func (k *databaseKind) dumpFormat() string {
	if k.format != "" {
		return k.format
	}
	return formatDump
}

// postgresLoadFailed reads psql's errors. A dump of a whole server drops and makes
// each role, and the role it signs in as can't go, nor can what a new server has be
// made again. Those two are all it may say.
func postgresLoadFailed(line string) bool {
	if !strings.Contains(line, "ERROR:") && !strings.Contains(line, "FATAL:") {
		return false
	}
	if strings.Contains(line, "current user cannot be dropped") {
		return false
	}
	return !strings.Contains(line, "role \"") || !strings.Contains(line, "already exists")
}

// kindOf finds a known database by what a container runs and where it has the
// volume. Only the official images: others keep things elsewhere, or need a
// test that restores them for real first (ADR 0033).
func kindOf(c *docker.Container, volumeName string) *databaseKind {
	repo, _, _ := strings.Cut(c.Config.Image, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	repo = strings.TrimPrefix(strings.TrimPrefix(repo, "docker.io/"), "library/")
	for _, k := range databaseKinds {
		if repo != k.image {
			continue
		}
		if len(k.needs) > 0 && !slices.ContainsFunc(c.Config.Env, func(e string) bool {
			name, value, _ := strings.Cut(e, "=")
			return slices.Contains(k.needs, name) && value != ""
		}) {
			continue
		}
		if m := dataMount(c, k.dataDir(c)); m != nil && m.Name == volumeName {
			return k
		}
	}
	return nil
}

// dataMount is the mount a data directory is on: the deepest that holds it. One above
// that has the directory's name in it, and none of the data.
func dataMount(c *docker.Container, dir string) *docker.MountPoint {
	var deepest *docker.MountPoint
	for i := range c.Mounts {
		m := &c.Mounts[i]
		if holds(m.Destination, dir) && (deepest == nil || len(path.Clean(m.Destination)) > len(path.Clean(deepest.Destination))) {
			deepest = m
		}
	}
	return deepest
}

// dataDir is where a container of this database keeps its data.
func (k *databaseKind) dataDir(c *docker.Container) string {
	if v := envOf(c, k.dataVar); k.dataVar != "" && v != "" {
		return path.Clean(v)
	}
	if v := engines.FlagValue(c.Config.Cmd, k.dataFlag); v != "" {
		return path.Clean(v)
	}
	return k.data
}

// envOf reads a variable a container has, which holds what its image sets too.
func envOf(c *docker.Container, variable string) string {
	for _, e := range c.Config.Env {
		if name, value, _ := strings.Cut(e, "="); name == variable {
			return value
		}
	}
	return ""
}

// holds says a mount has dir on it: it's dir, or dir is under it. A mount beside dir
// or inside it doesn't, however alike their paths look, and its files are its own.
func holds(mount, dir string) bool {
	mount = strings.TrimSuffix(path.Clean(mount), "/")
	return mount == dir || strings.HasPrefix(dir, mount+"/")
}

// portOf reads the port a server's command line sets: -p 5433, -p5433, --port=5433
// or -c port=5433.
func portOf(cmd []string) string {
	for i, arg := range cmd {
		next := ""
		if i+1 < len(cmd) {
			next = cmd[i+1]
		}
		switch {
		case arg == "-p" || arg == "--port":
			return next
		case arg == "-c" && strings.HasPrefix(next, "port="):
			return strings.TrimPrefix(next, "port=")
		case strings.HasPrefix(arg, "-cport="):
			return strings.TrimPrefix(arg, "-cport=")
		case strings.HasPrefix(arg, "--port="):
			return strings.TrimPrefix(arg, "--port=")
		case strings.HasPrefix(arg, "-p") && len(arg) > 2 && !strings.HasPrefix(arg, "--"):
			return arg[2:]
		}
	}
	return ""
}

// seriesOf is the release of a database an image's tag names, or empty if it doesn't.
func (k *databaseKind) seriesOf(image string) string {
	image, _, _ = strings.Cut(image, "@")
	i := strings.LastIndex(image, ":")
	if i <= strings.LastIndex(image, "/") {
		return ""
	}
	tag, _, _ := strings.Cut(image[i+1:], "-")
	parts := strings.Split(tag, ".")
	if len(parts) > k.series {
		parts = parts[:k.series]
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			return ""
		}
	}
	return strings.Join(parts, ".")
}

// sameSeries says a database's files from one image should open under another. Two
// images whose tags name no release can't be told apart, so only the same image is.
func (k *databaseKind) sameSeries(a, b string) bool {
	sa, sb := k.seriesOf(a), k.seriesOf(b)
	if sa == "" || sb == "" {
		return a == b
	}
	return sa == sb
}

// user is a container with a volume mounted.
type user struct {
	id, name  string
	container string // what Docker calls it, which docker start takes
	running   bool
	stopWait  time.Duration
}

// dockerName is what to pass to docker start.
func (u *user) dockerName() string {
	if u.container != "" {
		return u.container
	}
	return u.id
}

// database is a known database that has a volume as its data.
type database struct {
	*user
	kind  *databaseKind
	image string
	port  string // what its command line set, for its tools to ask for
	// set is the MongoDB replica set its command line makes it a member of.
	set string
}

// tool is one of the database's commands, with the port it listens on.
func (d *database) tool(cmd []string) []string {
	if d.port == "" {
		return cmd
	}
	variable := d.kind.portVar
	if variable == "" {
		variable = "PGPORT"
	}
	return append([]string{"env", variable + "=" + d.port}, cmd...)
}

// users is what has a volume mounted.
type users struct {
	list []*user
	db   *database
}

func (u *users) database() *database { return u.db }

// running is what runs on the volume now.
func (u *users) running() []*user {
	var out []*user
	for _, c := range u.list {
		if c.running {
			out = append(out, c)
		}
	}
	return out
}

// users reads what has a volume mounted.
func (r *Runner) users(ctx context.Context, t volume.Target) (*users, error) {
	found, err := r.Client.ListContainers(ctx, map[string][]string{"volume": {t.Name}})
	if err != nil {
		return nil, err
	}
	out := &users{}
	for i := range found {
		s := &found[i]
		if _, helper := s.Labels[labelHelper]; helper {
			continue
		}
		c, err := r.Client.InspectContainer(ctx, s.ID)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		u := userOf(c)
		out.list = append(out.list, u)
		if k := kindOf(c, t.Name); k != nil && out.db == nil && r.dumpable(ctx, k, c) {
			out.db = &database{user: u, kind: k, image: c.Config.Image}
			if k.portFlags {
				out.db.port = portOf(c.Config.Cmd)
			}
			if k.replays {
				out.db.set = replicaSetOf(c.Config.Cmd)
			}
		}
	}
	slices.SortFunc(out.list, func(a, b *user) int { return strings.Compare(a.name, b.name) })
	return out, nil
}

// dumpable says a running database's dump keeps what it holds, as its probe says;
// one with no probe, or not running, does as far as a dump can tell.
func (r *Runner) dumpable(ctx context.Context, k *databaseKind, c *docker.Container) bool {
	if k.probe == nil || !c.State.Running {
		return true
	}
	out := &tail{}
	code, err := r.Client.Exec(ctx, c.ID, k.probe, docker.Streams{Stdout: out})
	return err == nil && code == 0 && strings.TrimSpace(out.String()) == "ok"
}

// userOf is a container as a plan names it: by its service, or its own name.
func userOf(c *docker.Container) *user {
	name := c.Config.Labels[docker.LabelService]
	if name == "" {
		name = strings.TrimPrefix(c.Name, "/")
	}
	wait := stopWait
	if t := c.Config.StopTimeout; t != nil {
		wait = max(wait, time.Duration(*t)*time.Second)
	}
	return &user{id: c.ID, name: name, container: strings.TrimPrefix(c.Name, "/"), running: c.State.Running, stopWait: wait}
}

// namesOf lists names, sorted, each once.
func namesOf(list []*user) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.name)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// tail keeps the last of what's written, for what a command said when it failed.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

const tailSize = 4 << 10

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - tailSize; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// dump writes a database's own dump to w, gzipped, and says how large it is unzipped:
// the whole server's, or where one names a database, that database's alone.
func (r *Runner) dump(ctx context.Context, ref resource.Ref, db *database, one string, w io.Writer) (int64, error) {
	return r.dumpAs(ctx, ref, db, one, nil, w)
}

// dumpAs is dump, of one database as a user and role a restore has it dump as, where
// as names them: its owner can't sign in while a restore keeps it out.
func (r *Runner) dumpAs(ctx context.Context, ref resource.Ref, db *database, one string, as []string, w io.Writer) (int64, error) {
	cmd := db.kind.dump
	if one != "" {
		resource.Progress(ctx, "Dumping "+one+" on "+ref.Name+" with "+db.kind.name+"'s own tools")
		cmd = append(slices.Clone(db.kind.dumpOne), one)
		if db.kind.name == "PostgreSQL" && len(as) > 0 {
			cmd = append(cmd, as...)
		} else if db.kind.name == "PostgreSQL" {
			c, err := r.Client.InspectContainer(ctx, db.id)
			if err != nil {
				return 0, err
			}
			owner, err := engines.OwnerOf(ctx, r.Client, c, engines.Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL), one)
			if err != nil {
				return 0, fmt.Errorf("can't read who owns %s: %w", one, err)
			}
			cmd = append(cmd, owner)
		}
	} else {
		resource.Progress(ctx, "Dumping "+ref.Name+" with "+db.kind.name+"'s own tools")
	}
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return 0, err
	}
	errs := &tail{}
	plain := &counter{w: gz}
	code, err := r.Client.Exec(ctx, db.id, db.tool(cmd), docker.Streams{Stdout: plain, Stderr: errs})
	switch {
	case err != nil:
		return 0, fmt.Errorf("can't dump %s: %w", db.kind.name, err)
	case code != 0:
		return 0, fmt.Errorf("%s's dump exited with code %d: %s. Check the database, then try again", db.kind.name, code, scrubbed(errs.String()))
	}
	return plain.n, gz.Close()
}

// quoted finds what a database quotes in what it says.
var quoted = regexp.MustCompile(`'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"`)

// scrubbed keeps the first line of what a database said, without what it quotes. What
// it says of a row can quote the row, and a change's error is there for anyone who
// can read the change, however much the backup was kept from them.
func scrubbed(said string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(said), "\n")
	return withoutValues(first)
}

// withoutValues takes out what a line quotes, and the values PostgreSQL names a key
// by, unquoted: Key (email)=(someone@example.com) reads Key (email)=(...).
func withoutValues(line string) string {
	line = quoted.ReplaceAllString(line, "'...'")
	var b strings.Builder
	for {
		i := strings.Index(line, ")=(")
		if i < 0 {
			break
		}
		b.WriteString(line[:i+3])
		line = line[i+3:]
		depth, end := 1, len(line)
		for j := range len(line) {
			switch line[j] {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 {
				end = j
				break
			}
		}
		b.WriteString("...")
		line = line[end:]
	}
	b.WriteString(line)
	return b.String()
}

// verifyDump reads a dump back, to the words the tool ends a whole one with, or as
// the kind reads its own.
func verifyDump(k *databaseKind) verifier {
	if k.verify != nil {
		return k.verify
	}
	return verifyEnd(k.complete)
}

// verifyOne reads back a dump of one database.
func verifyOne(k *databaseKind) verifier { return verifyEnd(k.completeOne) }

// verifyEnd reads back a gzipped dump to its end, which says complete when whole.
func verifyEnd(complete string) verifier {
	return func(r io.Reader) error {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("the dump doesn't open: %w", err)
		}
		defer gz.Close()
		end := &tail{}
		if _, err := io.Copy(end, gz); err != nil { //nolint:gosec // a backup this agent wrote, read once and kept to its last 4 KiB
			return fmt.Errorf("the dump stops short or is damaged: %w", err)
		}
		if !strings.Contains(end.String(), complete) {
			return fmt.Errorf("the dump doesn't end as a whole one does, with %q", complete)
		}
		return nil
	}
}

func verifyArchive(r io.Reader) error {
	_, err := volumetool.Verify(r)
	return err
}

// readyWait is how long a database gets to set itself up and take connections.
const readyWait = 10 * time.Minute

// readyEvery is how often that is asked.
const readyEvery = time.Second

// awaitReady waits until a database takes connections. Its image sets up a new
// one first, behind a temporary server that takes none over TCP. A database that
// has stopped, or whose tools say more time won't help, fails at once.
func (r *Runner) awaitReady(ctx context.Context, db *database) error {
	ctx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	var last error
	for {
		said := &tail{}
		code, err := r.Client.Exec(ctx, db.id, db.tool(db.kind.ready), docker.Streams{Stderr: said})
		switch {
		case err == nil && code == 0:
			return nil
		case err == nil && db.kind.neverReady != nil && db.kind.neverReady(code, said.String()):
			return fmt.Errorf("%s won't take connections: %s. See what it printed in its logs", db.kind.name, scrubbed(said.String()))
		case err != nil:
			last = err
		default:
			last = errors.New("it isn't taking connections yet")
		}
		if c, err := r.Client.InspectContainer(ctx, db.id); err == nil && (!c.State.Running || c.State.Restarting) {
			return fmt.Errorf("%s stopped while it set itself up, with code %d. See what it printed in its logs", db.kind.name, c.State.ExitCode)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s didn't start within %s: %w. See what it printed in its logs", db.kind.name, readyWait, last)
		case <-time.After(readyEvery):
		}
	}
}

// lines hands a writer's text to fn a line at a time, and keeps the last of it.
type lines struct {
	fn  func(string)
	buf bytes.Buffer
}

func (l *lines) Write(b []byte) (int, error) {
	l.buf.Write(b)
	for {
		i := bytes.IndexByte(l.buf.Bytes(), '\n')
		if i < 0 {
			return len(b), nil
		}
		l.fn(strings.TrimRight(string(l.buf.Next(i+1)), "\r\n"))
	}
}

// loadDump feeds a dump to a database that's taking connections.
func (r *Runner) loadDump(ctx context.Context, db *database, dump io.Reader) error {
	gz, err := gzip.NewReader(dump)
	if err != nil {
		return fmt.Errorf("the dump doesn't open: %w", err)
	}
	defer gz.Close()
	return r.loadPlain(ctx, db, gz)
}

// loadPlain feeds a dump that's out of its gzip to a database.
func (r *Runner) loadPlain(ctx context.Context, db *database, plain io.Reader) error {
	var failed []string
	errs := &tail{}
	sink := io.Writer(errs)
	if db.kind.loadFailed != nil {
		sink = io.MultiWriter(errs, &lines{fn: func(line string) {
			if db.kind.loadFailed(line) && len(failed) < 5 {
				failed = append(failed, scrubbed(line))
			}
		}})
	}
	load, in := db.tool(db.kind.load), plain
	if db.kind.replays {
		ns, whole, err := mongoPrelude(plain)
		if err != nil {
			return err
		}
		in = whole
		if slices.Contains(ns, oplog) {
			load = append([]string{"env", "DW_OPLOG=1"}, load...)
		}
	}
	code, err := r.Client.Exec(ctx, db.id, load, docker.Streams{Stdin: in, Stderr: sink})
	switch {
	case err != nil:
		return fmt.Errorf("can't load the dump: %w", err)
	case code != 0:
		return fmt.Errorf("%s stopped loading the dump with code %d: %s", db.kind.name, code, scrubbed(errs.String()))
	case len(failed) > 0:
		return errors.New(db.kind.name + " couldn't load all of the dump: " + strings.Join(failed, "; "))
	}
	return nil
}
