// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
)

// Copying one database from a server into another's, as Use another database does
// (ADR 0084). The old server's own dump tool runs in its container, reading as its
// image signs in, which only reads. What the dump says is the old app's to write,
// so it's loaded as the new user alone: by the new server's client in a throwaway
// container of its image, holding nothing, on an internal network of the copy's
// own whose only other member is the new server, which it dials at its address
// there, so the server's rule for other hosts applies, a password, never its trust
// of its own loopback. A server that lets anyone on its networks in without a
// password isn't copied into. A statement in the dump then has the user's rights
// and no more. A client command in it, such as \connect or \!, which a dump tool
// before its fix lets an object's name carry, is refused where the client can
// refuse them (psql's \restrict, MariaDB's sandbox), and elsewhere signs in as
// no one else without their password, and reaches only the new server. The
// database names go as arguments of the shell, never inside its script.

// dumpOne writes one database's schema and data to stdout, owned by no one, so what
// loads it makes them its own. PostgreSQL's runs as the database's owner, the shell's
// second argument, not the server's administrator: on a server other apps share, a
// user of it could otherwise turn a dump that runs as the administrator against it
// (CVE-2024-7348, fixed in 16.4, 15.8, 14.13, 13.16 and 12.20).
var dumpOne = map[dockwellv1.DatabaseEngine][]string{
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL: {"sh", "-c", `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec pg_dump -U "$2" --no-owner --no-acl -d "$1"`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:      {"sh", "-c", `export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-}"; exec mysqldump -uroot --single-transaction --routines --triggers --events --hex-blob --no-create-db --set-gtid-purged=OFF "$1"`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:    {"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; c=mysqldump; command -v mariadb-dump >/dev/null 2>&1 && c=mariadb-dump; exec "$c" -uroot --single-transaction --routines --triggers --events --hex-blob --no-create-db "$1"`, "sh"},
}

// loadOne reads such a dump into one database as its user, stopping at the first
// error: the user, the database, the server's port and its address are the
// shell's arguments, then any options the client takes beside, and the password
// is in the environment, as copyPassword names it. PostgreSQL's loads as one
// transaction, so one that stops leaves the database as it was: a dump made with
// --clean drops each table before it makes it, and one that stopped partway would
// leave it empty.
var loadOne = map[dockwellv1.DatabaseEngine][]string{
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL: {"sh", "-c", `u=$1 d=$2 p=$3 h=$4; shift 4; exec psql -X -q -1 -v ON_ERROR_STOP=1 -h "$h" -p "$p" -U "$u" -d "$d" "$@" -f -`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:      {"sh", "-c", `u=$1 d=$2 p=$3 h=$4; shift 4; exec mysql --protocol=TCP -h "$h" -P "$p" -u "$u" "$@" "$d"`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:    {"sh", "-c", `u=$1 d=$2 p=$3 h=$4; shift 4; c=mysql; command -v mariadb >/dev/null 2>&1 && c=mariadb; exec "$c" --protocol=TCP -h "$h" -P "$p" -u "$u" "$@" "$d"`, "sh"},
}

// trustsAnyone signs in to a server as its administrator with no password, from
// another host, to the database the copy goes into: the administrator, the port, the
// address and the database are its arguments.
// One that lets it in trusts its networks, and a copy loaded into it could take it.
var trustsAnyone = map[dockwellv1.DatabaseEngine][]string{
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL: {"sh", "-c", `unset PGPASSWORD; exec psql -X -w -h "$3" -p "$2" -U "$1" -d "$4" -c 'SELECT 1'`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:      {"sh", "-c", `unset MYSQL_PWD; exec mysql --protocol=TCP -h "$3" -P "$2" -u "$1" --password= -e 'SELECT 1' "$4"`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:    {"sh", "-c", `unset MYSQL_PWD; c=mysql; command -v mariadb >/dev/null 2>&1 && c=mariadb; exec "$c" --protocol=TCP -h "$3" -P "$2" -u "$1" --password= -e 'SELECT 1' "$4"`, "sh"},
}

// clientSays is what the new server's client says of itself, from its own
// container, to tell what it refuses: psql's version, or a MySQL or MariaDB
// client's options.
var clientSays = map[dockwellv1.DatabaseEngine][]string{
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL: {"psql", "-V"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:      {"mysql", "--help"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:    {"sh", "-c", `c=mysql; command -v mariadb >/dev/null 2>&1 && c=mariadb; exec "$c" --help`},
}

// copyPassword is the variable each client reads its password from.
var copyPassword = map[dockwellv1.DatabaseEngine]string{
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL: "PGPASSWORD",
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:      "MYSQL_PWD",
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:    "MYSQL_PWD",
}

// LabelCopy marks the copy's network and throwaway containers, so nothing lists them
// as an app's, and an agent that starts again removes what a crash left.
const LabelCopy = docker.LabelCopy

// Copies reports an engine Dockwell copies one database of across.
func Copies(e dockwellv1.DatabaseEngine) bool { _, ok := dumpOne[e]; return ok }

// SystemDatabase reports a database a server keeps for itself, which is never copied:
// it holds the server's users, or nothing of an app's.
func SystemDatabase(name string) bool {
	return name == "postgres" || name == "template0" || name == "template1" || slices.Contains(mysqlSystem, name)
}

// copyWait bounds a copy unless the driver says otherwise: a database larger than a
// small server copies in that time is better moved by hand.
const copyWait = 2 * time.Hour

// copyInto copies the database from names into db, which the user owns, on the
// server in to: the database a step made empty a moment before.
func (d *UserDriver) copyInto(ctx context.Context, to *docker.Container, e *Engine, user, db, password string, from *resourcev1.DatabaseCopy) error {
	if err := checkNames(user, from.GetDatabase()); err != nil {
		return err
	}
	if SystemDatabase(from.GetDatabase()) {
		return fmt.Errorf("%s is a database %s keeps for itself, which Dockwell never copies", from.GetDatabase(), from.GetService())
	}
	src, err := d.Client.ContainerOf(ctx, docker.Project(from.GetEnvironment()), from.GetService(), 1)
	switch {
	case docker.IsNotFound(err):
		return fmt.Errorf("%s, whose database %s is to be copied, has no container on this server. Start it, then try again", from.GetService(), from.GetDatabase())
	case err != nil:
		return err
	case !src.State.Running:
		return fmt.Errorf("%s, whose database %s is to be copied, isn't running. Start it, then try again", from.GetService(), from.GetDatabase())
	}
	if se := Recognise(src.Config.Image); se == nil || se.Kind != e.Kind {
		return fmt.Errorf("%s doesn't run %s, so its database %s can't be copied into %s's", from.GetService(), e.Name, from.GetDatabase(), e.Name)
	}
	dump := dumpOne[e.Kind]
	if dump == nil || loadOne[e.Kind] == nil {
		return fmt.Errorf("copying a %s database across isn't something Dockwell does yet", e.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(d.CopyWait, copyWait))
	defer cancel()
	dump = append(withPort("PGPORT", PortOf(e, src.Config.Cmd), dump), from.GetDatabase())
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		owner, err := OwnerOf(ctx, d.Client, src, e, from.GetDatabase())
		if err != nil {
			return fmt.Errorf("can't read who owns %s on %s, so nothing was copied: %w", from.GetDatabase(), from.GetService(), err)
		}
		dump = append(dump, owner)
	}
	pr, pw := io.Pipe()
	moved := &atomic.Int64{}
	dumpSaid := &capped{max: 4 << 10}
	dumped := make(chan error, 1)
	go func() {
		// The dump reads nothing; an input that ends at once lets no one wait on it.
		code, err := d.Client.Exec(ctx, src.ID, dump, docker.Streams{Stdin: strings.NewReader(""), Stdout: &counting{w: pw, n: moved}, Stderr: dumpSaid})
		if err == nil && code != 0 {
			err = fmt.Errorf("%s's dump of %s stopped with code %d: %s", from.GetService(), from.GetDatabase(), code, scrubbed(strings.TrimSpace(dumpSaid.String())))
		}
		pw.CloseWithError(err)
		dumped <- err
	}()
	stopTelling := tellCopied(ctx, from.GetDatabase(), moved)
	err = Loader{Client: d.Client}.Load(ctx, Load{To: to, Engine: e, User: user, Password: password, Database: db}, pr)
	stopTelling()
	_ = pr.CloseWithError(errLoadEnded)
	dumpErr := <-dumped
	if errors.Is(dumpErr, errLoadEnded) {
		// The load ended first, and what it says is why.
		dumpErr = nil
		if err == nil {
			err = errors.New("the load ended before the dump did")
		}
	}
	var refused *LoadError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("copying %s across took longer than %s, so it stopped and nothing moved. Copy it by hand, then use start empty", from.GetDatabase(), cmp.Or(d.CopyWait, copyWait))
	case dumpErr != nil:
		return fmt.Errorf("can't copy %s across: %w", from.GetDatabase(), dumpErr)
	case errors.As(err, &refused):
		return fmt.Errorf("%s couldn't load the copy of %s as %s, code %d: %s", e.Name, from.GetDatabase(), user, refused.Code, refused.Said)
	case err != nil:
		return fmt.Errorf("can't copy %s across: %w", from.GetDatabase(), err)
	}
	return nil
}

// Loader loads a dump into one database as a user of it, as a copy does: by the
// server's own client in a throwaway container of its image, holding nothing, on an
// internal network made for the load with the server alone on it, dialling the
// server's address there, where a password is asked for. A server that lets its
// administrator in there with no password isn't loaded into. Where the client can,
// it refuses every client command in the dump; a MySQL or MariaDB dump's DEFINER
// clauses go, so what it makes is the user's (ADR 0084).
type Loader struct {
	Client *docker.Client
}

// Load is one load: into Database on the server To runs, as User with Password,
// running Prelude first, such as PostgreSQL's SET ROLE.
type Load struct {
	To                       *docker.Container
	Engine                   *Engine
	User, Password, Database string
	Prelude                  string
}

// LoadError is a load the server's client stopped, with what it said.
type LoadError struct {
	Code int
	Said string
}

func (e *LoadError) Error() string {
	return fmt.Sprintf("the load stopped with code %d: %s", e.Code, e.Said)
}

// Load loads dump. A load the client stopped is a *LoadError.
func (ld Loader) Load(ctx context.Context, l Load, dump io.Reader) error {
	e, to := l.Engine, l.To
	load := loadOne[e.Kind]
	if load == nil {
		return fmt.Errorf("loading a %s database isn't something Dockwell does yet", e.Name)
	}
	port := strconv.FormatUint(uint64(cmp.Or(PortOf(e, to.Config.Cmd), e.Port)), 10)
	network, addr, leave, err := ld.copyNetwork(ctx, to, l.User)
	if err != nil {
		return err
	}
	defer leave()
	trusts, err := ld.trusted(ctx, to, e, network, port, addr, l.Database)
	if err != nil {
		return err
	}
	if trusts {
		return fmt.Errorf("%s lets anyone on its networks sign in as its administrator with no password, so a dump loaded into it could take the whole server, and nothing was loaded. "+
			"Have it ask for passwords, as POSTGRES_HOST_AUTH_METHOD=scram-sha-256 or a root password does, then try again", serviceOf(to))
	}
	first, options := ld.refusals(ctx, to, e)
	cmd := append(slices.Clone(load), l.User, l.Database, port, addr)
	cmd = append(cmd, options...)
	var in io.Reader
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		in = withoutRestrict(dump)
	} else {
		in = withoutDefiners(dump)
	}
	in = io.MultiReader(strings.NewReader(first+l.Prelude), in)
	said := &capped{max: 4 << 10}
	code, err := ld.throwaway(ctx, to.Config.Image, network, l.User, []string{copyPassword[e.Kind] + "=" + l.Password}, cmd, in, said)
	switch {
	case err != nil:
		return err
	case code != 0:
		return &LoadError{Code: code, Said: scrubbed(strings.TrimSpace(said.String()))}
	}
	return nil
}

// OwnerOf is who owns a PostgreSQL database, which a dump of it alone runs as: on a
// server other apps share, a dump as the administrator is one a user of it could turn
// against the server (CVE-2024-7348).
func OwnerOf(ctx context.Context, client *docker.Client, c *docker.Container, e *Engine, db string) (string, error) {
	if err := CheckSQLName("database", db); err != nil {
		return "", err
	}
	cmd, _ := e.reading(PortOf(e, c.Config.Cmd))
	said, err := Reader{Client: client}.ask(ctx, c.ID, e, cmd,
		"SELECT json_build_object('owner', r.rolname) FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE d.datname = '"+db+"';\n")
	if err != nil {
		return "", err
	}
	var out struct {
		Owner string `json:"owner"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(said))), &out); err != nil || out.Owner == "" {
		return "", fmt.Errorf("it has no database %s", db)
	}
	return out.Owner, nil
}

// serviceOf is the service a container runs, as Compose labels it.
func serviceOf(c *docker.Container) string {
	return cmp.Or(c.Config.Labels[docker.LabelService], strings.TrimPrefix(c.Name, "/"))
}

// copyNetwork makes an internal network of the copy's own, joins the new server to
// it, and says its name and the server's address there; leave takes the server
// off it and removes it, whatever became of the copy.
func (ld Loader) copyNetwork(ctx context.Context, to *docker.Container, user string) (network, addr string, leave func(), err error) {
	network = docker.CopyPrefix + user + "-" + randomHex(6)
	// Internal, and with no address of the server's own on its bridge, so what's on
	// it reaches neither the internet nor the server itself, only the new database.
	if _, err := ld.Client.CreateNetwork(ctx, &docker.NetworkCreate{
		Name: network, Internal: true, CheckDuplicate: true, Labels: map[string]string{LabelCopy: user},
		Options: map[string]string{inhibitIPv4: "true"},
	}); err != nil {
		return "", "", nil, fmt.Errorf("docker can't make the network the copy loads on: %w", err)
	}
	joined := false
	leave = func() {
		gone, cancel := resource.Finishing(ctx, time.Minute)
		defer cancel()
		if joined {
			_ = ld.Client.DisconnectNetwork(gone, network, to.ID)
		}
		_ = ld.Client.RemoveNetwork(gone, network)
	}
	if err := ld.Client.ConnectNetwork(ctx, network, to.ID, nil); err != nil {
		leave()
		return "", "", nil, fmt.Errorf("docker can't put %s on the network the copy loads on: %w", serviceOf(to), err)
	}
	joined = true
	c, err := ld.Client.InspectContainer(ctx, to.ID)
	if err == nil {
		if ep := c.NetworkSettings.Networks[network]; ep != nil {
			addr = ep.IPAddress
		}
	}
	if addr == "" {
		leave()
		return "", "", nil, fmt.Errorf("docker gave %s no address on the network the copy loads on, so nothing was copied. Try again", serviceOf(to))
	}
	return network, addr, leave, nil
}

// inhibitIPv4 has Docker give a bridge network's own device no address on the
// server, so a container on it has no gateway to the server's own ports.
const inhibitIPv4 = "com.docker.network.bridge.inhibit_ipv4"

// trusted says whether a server lets its administrator into the copy's database with
// no password from another host on the copy's network. Only the server's refusal to
// let it in says it doesn't: anything else, such as a connection that fails, is no
// answer, and the copy doesn't go on.
func (ld Loader) trusted(ctx context.Context, to *docker.Container, e *Engine, network, port, addr, db string) (bool, error) {
	admin := "root"
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		admin = cmp.Or(envOf(to.Config.Env, "POSTGRES_USER"), "postgres")
	}
	cmd := append(slices.Clone(trustsAnyone[e.Kind]), admin, port, addr, db)
	said := &capped{max: 4 << 10}
	code, err := ld.throwaway(ctx, to.Config.Image, network, "check", nil, cmd, nil, said)
	switch {
	case err != nil:
		return false, fmt.Errorf("can't tell whether %s asks for a password on the copy's network, so nothing was copied: %w", serviceOf(to), err)
	case code == 0:
		return true, nil
	case refusedSignIn(said.String()):
		return false, nil
	}
	return false, fmt.Errorf("can't tell whether %s asks for a password on the copy's network, so nothing was copied: %s. Try again", serviceOf(to), cmp.Or(scrubbed(said.String()), fmt.Sprintf("its check stopped with code %d", code)))
}

// Clean removes what a copy an agent's crash cut short left: the throwaway
// containers and the network, each server taken off it first. Nothing copies while
// the agent starts.
func (d *UserDriver) Clean(ctx context.Context) {
	if d.Client == nil {
		return
	}
	if _, err := d.Client.RemoveContainersLabelled(ctx, LabelCopy); err != nil {
		return
	}
	nets, err := d.Client.ListNetworks(ctx)
	if err != nil {
		return
	}
	for i := range nets {
		n := &nets[i]
		if _, ok := n.Labels[LabelCopy]; !ok {
			continue
		}
		for id := range n.Containers {
			_ = d.Client.DisconnectNetwork(ctx, n.Name, id)
		}
		_ = d.Client.RemoveNetwork(ctx, n.Name)
	}
}

// envOf is a variable's value in a container's environment.
func envOf(env []string, name string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// refusals is what the new server's client is told to refuse client commands
// with: a first line for psql, from 17.6, 16.10, 15.14, 14.19 and 13.22 on, which
// takes no backslash command after it but one with a key no dump can know, or
// options for a MySQL or MariaDB client that has them.
func (ld Loader) refusals(ctx context.Context, to *docker.Container, e *Engine) (first string, options []string) {
	said := &capped{max: 256 << 10}
	if code, err := ld.Client.Exec(ctx, to.ID, clientSays[e.Kind], docker.Streams{Stdin: strings.NewReader(""), Stdout: said}); err != nil || code != 0 {
		return "", nil
	}
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		if restricts(said.String()) {
			return `\restrict ` + randomHex(16) + "\n", nil
		}
		return "", nil
	}
	if strings.Contains(said.String(), "--sandbox") {
		options = append(options, "--sandbox")
	}
	if strings.Contains(said.String(), "--system-command") {
		options = append(options, "--skip-system-command")
	}
	return "", options
}

// restricts reports a psql that takes \restrict, as psql -V says its release.
func restricts(version string) bool {
	m := psqlRelease().FindStringSubmatch(version)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	fixed := map[int]int{17: 6, 16: 10, 15: 14, 14: 19, 13: 22}
	if major >= 18 {
		return true
	}
	at, ok := fixed[major]
	return ok && minor >= at
}

var psqlRelease = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\(PostgreSQL\) (\d+)\.(\d+)`) })

// throwaway runs cmd in a container of image made for it, on network alone, with
// nothing mounted and every capability dropped, and removes it after.
func (ld Loader) throwaway(ctx context.Context, image, network, user string, env, cmd []string, in io.Reader, said io.Writer) (int, error) {
	req := &docker.CreateRequest{
		Config: docker.Config{
			Image: image, Entrypoint: []string{}, Cmd: cmd, Env: env,
			Labels: map[string]string{docker.LabelOneoff: "True", LabelCopy: user},
			// Docker closes its input when the dump ends, which ends the load.
			OpenStdin: in != nil, StdinOnce: in != nil,
		},
		HostConfig: docker.HostConfig{
			NetworkMode: network, ReadonlyRootfs: true,
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"},
			// What it prints is the load's, which says nothing worth keeping.
			LogConfig: &docker.LogConfig{Type: "none"},
		},
	}
	id, err := ld.Client.CreateContainer(ctx, docker.CopyPrefix+user+"-"+randomHex(6), req)
	if err != nil {
		return 0, fmt.Errorf("docker can't make the container that loads the copy: %w", err)
	}
	defer func() {
		gone, cancel := resource.Finishing(ctx, time.Minute)
		defer cancel()
		_ = ld.Client.RemoveOneOff(gone, id)
	}()
	return ld.Client.RunAttached(ctx, id, docker.Streams{Stdin: in, Stdout: io.Discard, Stderr: said})
}

// randomHex is n random bytes, as hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// errLoadEnded is what the dump hears when the load ended before it.
var errLoadEnded = errors.New("the load ended")

// tellCopied says how much of the dump has gone across, every few seconds, until
// the returned function is called.
func tellCopied(ctx context.Context, db string, moved *atomic.Int64) func() {
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		resource.Progress(ctx, "Copying "+db+" across")
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				resource.Progress(ctx, "Copying "+db+" across: "+human.Size(moved.Load())+" so far")
			}
		}
	}()
	return func() { close(done) }
}

// counting counts what goes through it.
type counting struct {
	w io.Writer
	n *atomic.Int64
}

func (c *counting) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n.Add(int64(n))
	return n, err
}

// definer is a DEFINER clause of MySQL's and MariaDB's dumps, in any case and
// spacing: an account, its host where it has one (a MariaDB role has none), or
// CURRENT_USER or CURRENT_ROLE.
var definer = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile("(?i)\\bDEFINER\\s*=\\s*(?:`[^`]*`|'[^']*'|\"[^\"]*\"|[^\\s@*/;]+)(?:\\s*@\\s*(?:`[^`]*`|'[^']*'|\"[^\"]*\"|[^\\s*/;]+))?(?:\\s*\\(\\s*\\))?\\s*")
})

// makes reports a line that starts a statement that can carry a DEFINER: one that
// makes or changes a view, routine, trigger or event, as itself or in a versioned
// comment, in any case. A line of data never starts so.
var makes = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)^\s*(?:/\*!\d*\s*)?(?:CREATE|ALTER)\b|^\s*/\*!\d*\s*DEFINER\b`)
})

// withoutDefiners takes the DEFINER clauses out of a MySQL or MariaDB dump, so each
// view, routine, trigger and event is the loading user's, not an account of the old
// server's, which on a shared server can be another app's: on each line that starts
// such a statement, wherever on it the clause stands.
func withoutDefiners(r io.Reader) io.Reader {
	return byLine(r, func(line []byte) []byte {
		if makes().Match(line) && bytes.Contains(bytes.ToUpper(line), []byte("DEFINER")) {
			return definer().ReplaceAll(line, nil)
		}
		return line
	})
}

// restrictLine is the line pg_dump from 17.6, 16.10, 15.14, 14.19 and 13.22 on
// starts a dump with, and unrestrictLine the one it ends it with, each with a key
// of its own. Each pattern here is made the first time a copy needs it, as the
// agent starts with as little as it can (ADR 0060).
var (
	restrictLine   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\\restrict ([A-Za-z0-9]+)\r?\n?$`) })
	unrestrictLine = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\\unrestrict ([A-Za-z0-9]+)\r?\n?$`) })
)

// withoutRestrict takes out the \restrict a dump starts with and the \unrestrict
// with its key, so the load's own \restrict, with a key of the load's, holds for the
// whole of it, and a psql that has none reads nothing it doesn't know.
func withoutRestrict(r io.Reader) io.Reader {
	key, lines := "", 0
	return byLine(r, func(line []byte) []byte {
		lines++
		if m := restrictLine().FindSubmatch(line); m != nil && key == "" && lines <= 20 {
			key = string(m[1])
			return nil
		}
		if m := unrestrictLine().FindSubmatch(line); m != nil && key != "" && string(m[1]) == key {
			return nil
		}
		return line
	})
}

// byLine passes what r reads through f a line at a time. A line longer than what's
// read at once, such as an INSERT of many rows, goes as it is, past f.
func byLine(r io.Reader, f func(line []byte) []byte) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		br := bufio.NewReaderSize(r, 64<<10)
		start := true // at the start of a line
		for {
			line, err := br.ReadSlice('\n')
			if len(line) > 0 {
				out := line
				if start && !errors.Is(err, bufio.ErrBufferFull) {
					out = f(line)
				}
				if _, werr := pw.Write(out); werr != nil {
					_ = pw.CloseWithError(werr)
					return
				}
				start = line[len(line)-1] == '\n'
			}
			switch {
			case errors.Is(err, bufio.ErrBufferFull):
			case err != nil:
				if errors.Is(err, io.EOF) {
					err = nil
				}
				_ = pw.CloseWithError(err)
				return
			}
		}
	}()
	return pr
}
