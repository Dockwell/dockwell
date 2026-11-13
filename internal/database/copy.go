// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
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
// container of its image, which shares only that server's network, signs in over
// TCP with the user's password from its environment, and holds nothing else. A
// statement in the dump then has the user's rights and no more, and a client
// command it carries, such as \!, runs where there's nothing to reach. The database
// names go as arguments of the shell, never inside its script.

// dumpOne writes one database's schema and data to stdout, owned by no one, so what
// loads it makes them its own.
var dumpOne = map[dockwellv1.DatabaseEngine][]string{
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL: {"sh", "-c", `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec pg_dump -U "${POSTGRES_USER:-postgres}" --no-owner --no-acl -d "$1"`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:      {"sh", "-c", `export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-}"; exec mysqldump -uroot --single-transaction --routines --triggers --events --hex-blob --no-create-db --set-gtid-purged=OFF "$1"`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:    {"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; c=mysqldump; command -v mariadb-dump >/dev/null 2>&1 && c=mariadb-dump; exec "$c" -uroot --single-transaction --routines --triggers --events --hex-blob --no-create-db "$1"`, "sh"},
}

// loadOne reads such a dump into one database as its user, stopping at the first
// error: the user, the database and the port are the shell's arguments, and the
// password is in the environment, as copyPassword names it.
var loadOne = map[dockwellv1.DatabaseEngine][]string{
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL: {"sh", "-c", `exec psql -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -p "$3" -U "$1" -d "$2" -f -`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:      {"sh", "-c", `exec mysql --protocol=TCP -h 127.0.0.1 -P "$3" -u "$1" "$2"`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:    {"sh", "-c", `c=mysql; command -v mariadb >/dev/null 2>&1 && c=mariadb; exec "$c" --protocol=TCP -h 127.0.0.1 -P "$3" -u "$1" "$2"`, "sh"},
}

// copyPassword is the variable each client reads its password from.
var copyPassword = map[dockwellv1.DatabaseEngine]string{
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL: "PGPASSWORD",
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:      "MYSQL_PWD",
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:    "MYSQL_PWD",
}

// LabelCopy marks the throwaway container that loads a copy, with the user it loads
// for, so nothing lists it as an app's.
const LabelCopy = docker.OwnPrefix + "database-copy"

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
	dump, load := dumpOne[e.Kind], loadOne[e.Kind]
	if dump == nil {
		return fmt.Errorf("copying a %s database across isn't something Dockwell does yet", e.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(d.CopyWait, copyWait))
	defer cancel()
	dump = append(withPort("PGPORT", PortOf(e, src.Config.Cmd), dump), from.GetDatabase())
	port := cmp.Or(PortOf(e, to.Config.Cmd), e.Port)
	load = append(slices.Clone(load), user, db, strconv.FormatUint(uint64(port), 10))

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
	var in io.Reader = pr
	if e.Kind != dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		in = withoutDefiners(pr)
	}
	loadSaid := &capped{max: 4 << 10}
	code, err := d.loadIn(ctx, to, e, user, password, load, in, loadSaid)
	stopTelling()
	_ = pr.CloseWithError(errLoadEnded)
	dumpErr := <-dumped
	if errors.Is(dumpErr, errLoadEnded) {
		// The load ended first, and what it says is why.
		dumpErr = nil
		if err == nil && code == 0 {
			err = errors.New("the load ended before the dump did")
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("copying %s across took longer than %s, so it stopped and nothing moved. Copy it by hand, then use start empty", from.GetDatabase(), cmp.Or(d.CopyWait, copyWait))
	case dumpErr != nil:
		return fmt.Errorf("can't copy %s across: %w", from.GetDatabase(), dumpErr)
	case err != nil:
		return fmt.Errorf("can't copy %s across: %w", from.GetDatabase(), err)
	case code != 0:
		return fmt.Errorf("%s couldn't load the copy of %s as %s, code %d: %s", e.Name, from.GetDatabase(), user, code, scrubbed(strings.TrimSpace(loadSaid.String())))
	}
	return nil
}

// loadIn runs the load in a throwaway container of the new server's image, which
// shares that server's network and nothing else, and removes it after.
func (d *UserDriver) loadIn(ctx context.Context, to *docker.Container, e *Engine, user, password string, load []string, in io.Reader, said io.Writer) (int, error) {
	run := make([]byte, 6)
	_, _ = rand.Read(run)
	req := &docker.CreateRequest{
		Config: docker.Config{
			Image: to.Config.Image, Entrypoint: []string{}, Cmd: load,
			Env:    []string{copyPassword[e.Kind] + "=" + password},
			Labels: map[string]string{docker.LabelOneoff: "True", LabelCopy: user},
			// Docker closes its input when the dump ends, which ends the load.
			OpenStdin: true, StdinOnce: true,
		},
		HostConfig: docker.HostConfig{
			NetworkMode: "container:" + to.ID, ReadonlyRootfs: true,
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"},
			// What it prints is the load's, which says nothing worth keeping.
			LogConfig: &docker.LogConfig{Type: "none"},
		},
	}
	id, err := d.Client.CreateContainer(ctx, "dockwell-copy-"+user+"-"+hex.EncodeToString(run), req)
	if err != nil {
		return 0, fmt.Errorf("docker can't make the container that loads the copy: %w", err)
	}
	defer func() {
		gone, cancel := resource.Finishing(ctx, time.Minute)
		defer cancel()
		_ = d.Client.RemoveOneOff(gone, id)
	}()
	return d.Client.RunAttached(ctx, id, docker.Streams{Stdin: in, Stdout: io.Discard, Stderr: said})
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

// definer is a DEFINER clause of MySQL's and MariaDB's dumps, by any account.
var definer = regexp.MustCompile("DEFINER\\s*=\\s*(?:`[^`]*`|'[^']*'|[^\\s@*]+)\\s*@\\s*(?:`[^`]*`|'[^']*'|[^\\s*]+)\\s*")

// withoutDefiners takes the DEFINER clauses out of a MySQL or MariaDB dump, so each
// view, routine, trigger and event is the loading user's, not an account of the old
// server's, which on a shared server can be another app's. Only the first line of a
// statement that makes one carries a clause, and a line of data never starts so.
func withoutDefiners(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		br := bufio.NewReaderSize(r, 64<<10)
		start := true // at the start of a line
		for {
			line, err := br.ReadSlice('\n')
			if len(line) > 0 {
				out := line
				if start && (bytes.HasPrefix(line, []byte("CREATE")) || bytes.HasPrefix(line, []byte("/*!"))) && bytes.Contains(line, []byte("DEFINER")) {
					out = definer.ReplaceAll(line, nil)
				}
				if _, werr := pw.Write(out); werr != nil {
					_ = pw.CloseWithError(werr)
					return
				}
				start = line[len(line)-1] == '\n'
			}
			switch {
			case errors.Is(err, bufio.ErrBufferFull):
				// A long line, such as an INSERT of many rows: the rest goes as it is.
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
