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
	// needs is a variable it has to have set for the commands to sign in.
	needs string
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
	// ask for, as -p or -c port=.
	portFlags bool
}

// Each official image lets these run from inside with what its environment says.
var databaseKinds = []*databaseKind{
	{
		name: "PostgreSQL", image: "postgres", data: "/var/lib/postgresql/data", dataVar: "PGDATA", series: 1, portFlags: true,
		dump:       []string{"sh", "-c", `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec pg_dumpall -U "${POSTGRES_USER:-postgres}" --clean --if-exists`},
		load:       []string{"sh", "-c", `export PGPASSWORD="${POSTGRES_PASSWORD:-}" PGOPTIONS='-c lc_messages=C'; exec psql -X -q -U "${POSTGRES_USER:-postgres}" -d postgres -f -`},
		ready:      []string{"sh", "-c", `exec pg_isready -q -h 127.0.0.1 -p "${PGPORT:-5432}" -U "${POSTGRES_USER:-postgres}"`},
		complete:   "PostgreSQL database cluster dump complete",
		loadFailed: postgresLoadFailed,
		// pg_isready exits 3 when it never tried: its own command line is wrong.
		neverReady: func(code int, _ string) bool { return code == 3 },
	},
	{
		name: "MySQL", image: "mysql", data: "/var/lib/mysql", needs: "MYSQL_ROOT_PASSWORD", series: 2,
		dump:       []string{"sh", "-c", `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysqldump -uroot --all-databases --single-transaction --routines --events --triggers --hex-blob --flush-privileges --set-gtid-purged=OFF`},
		load:       []string{"sh", "-c", `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysql -uroot --binary-mode=1`},
		ready:      []string{"sh", "-c", `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysql --protocol=TCP -h127.0.0.1 -uroot -e 'SELECT 1'`},
		complete:   "Dump completed",
		neverReady: func(_ int, said string) bool { return strings.Contains(said, "Access denied") },
	},
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
		if k.needs != "" && !slices.ContainsFunc(c.Config.Env, func(e string) bool {
			name, value, _ := strings.Cut(e, "=")
			return name == k.needs && value != ""
		}) {
			continue
		}
		dir := k.dataDir(c)
		for i := range c.Mounts {
			if m := &c.Mounts[i]; m.Name == volumeName && holds(m.Destination, dir) {
				return k
			}
		}
	}
	return nil
}

// dataDir is where a container of this database keeps its data.
func (k *databaseKind) dataDir(c *docker.Container) string {
	if v := envOf(c, k.dataVar); v != "" {
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
}

// tool is one of the database's commands, with the port it listens on.
func (d *database) tool(cmd []string) []string {
	if d.port == "" {
		return cmd
	}
	return append([]string{"env", "PGPORT=" + d.port}, cmd...)
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
		if k := kindOf(c, t.Name); k != nil && out.db == nil {
			out.db = &database{user: u, kind: k, image: c.Config.Image}
			if k.portFlags {
				out.db.port = portOf(c.Config.Cmd)
			}
		}
	}
	slices.SortFunc(out.list, func(a, b *user) int { return strings.Compare(a.name, b.name) })
	return out, nil
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

// dump writes a database's own dump to w, gzipped, and says how large it is unzipped.
func (r *Runner) dump(ctx context.Context, ref resource.Ref, db *database, w io.Writer) (int64, error) {
	resource.Progress(ctx, "Dumping "+ref.Name+" with "+db.kind.name+"'s own tools")
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return 0, err
	}
	errs := &tail{}
	plain := &counter{w: gz}
	code, err := r.Client.Exec(ctx, db.id, db.tool(db.kind.dump), docker.Streams{Stdout: plain, Stderr: errs})
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
	return quoted.ReplaceAllString(first, "'...'")
}

// verifyDump reads a dump back, to the words the tool ends a whole one with.
func verifyDump(k *databaseKind) verifier {
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
		if !strings.Contains(end.String(), k.complete) {
			return fmt.Errorf("the dump doesn't end as a whole one does, with %q", k.complete)
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
	code, err := r.Client.Exec(ctx, db.id, db.tool(db.kind.load), docker.Streams{Stdin: gz, Stderr: sink})
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
