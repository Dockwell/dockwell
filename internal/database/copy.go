// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// Copying one database from a server into another's, as Use another database does
// (ADR 0084): its own dump tool in the old server's container, its own client in the
// new one's, the dump streamed from one to the other through the agent, each signing
// in as its image does, so no password passes through the agent. The database names
// go as arguments of the shell, never inside its script.

// dumpOne writes one database's schema and data to stdout, owned by no one, so what
// loads it makes them its own.
var dumpOne = map[dockwellv1.DatabaseEngine][]string{
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL: {"sh", "-c", `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec pg_dump -U "${POSTGRES_USER:-postgres}" --no-owner --no-acl -d "$1"`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:      {"sh", "-c", `export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-}"; exec mysqldump -uroot --single-transaction --routines --triggers --events --no-create-db "$1"`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:    {"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; c=mysqldump; command -v mariadb-dump >/dev/null 2>&1 && c=mariadb-dump; exec "$c" -uroot --single-transaction --routines --triggers --events --no-create-db "$1"`, "sh"},
}

// loadOne reads such a dump into one database, stopping at the first error.
var loadOne = map[dockwellv1.DatabaseEngine][]string{
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL: {"sh", "-c", `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec psql -X -q -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-postgres}" -d "$1" -f -`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:      {"sh", "-c", `export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-}"; exec mysql -uroot "$1"`, "sh"},
	dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:    {"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; c=mysql; command -v mariadb >/dev/null 2>&1 && c=mariadb; exec "$c" -uroot "$1"`, "sh"},
}

// Copies reports an engine Dockwell copies one database of across.
func Copies(e dockwellv1.DatabaseEngine) bool { _, ok := dumpOne[e]; return ok }

// copyInto copies the database from names into db, which the user owns, in the
// container to: the database a step made empty a moment before. PostgreSQL loads it
// as the user, so what it makes is theirs.
func (d *UserDriver) copyInto(ctx context.Context, to *docker.Container, e *Engine, user, db string, from *resourcev1.DatabaseCopy) error {
	if err := checkNames(user, from.GetDatabase()); err != nil {
		return err
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
	dump = append(withPort("PGPORT", PortOf(e, src.Config.Cmd), dump), from.GetDatabase())
	load = append(withPort("PGPORT", PortOf(e, to.Config.Cmd), load), db)
	var first io.Reader = strings.NewReader("")
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		first = strings.NewReader(`SET ROLE "` + user + `";` + "\n")
	}
	pr, pw := io.Pipe()
	dumpSaid := &capped{max: 4 << 10}
	dumped := make(chan error, 1)
	go func() {
		// The dump reads nothing; an input that ends at once lets no one wait on it.
		code, err := d.Client.Exec(ctx, src.ID, dump, docker.Streams{Stdin: strings.NewReader(""), Stdout: pw, Stderr: dumpSaid})
		if err == nil && code != 0 {
			err = fmt.Errorf("%s's dump of %s stopped with code %d: %s", from.GetService(), from.GetDatabase(), code, scrubbed(strings.TrimSpace(dumpSaid.String())))
		}
		pw.CloseWithError(err)
		dumped <- err
	}()
	loadSaid := &capped{max: 4 << 10}
	code, err := d.Client.Exec(ctx, to.ID, load, docker.Streams{Stdin: io.MultiReader(first, pr), Stderr: loadSaid})
	_ = pr.CloseWithError(errors.New("the load ended"))
	dumpErr := <-dumped
	switch {
	case dumpErr != nil:
		return fmt.Errorf("can't copy %s across: %w", from.GetDatabase(), dumpErr)
	case err != nil:
		return fmt.Errorf("can't copy %s across: %w", from.GetDatabase(), err)
	case code != 0:
		return fmt.Errorf("%s couldn't load the copy of %s, code %d: %s", e.Name, from.GetDatabase(), code, scrubbed(strings.TrimSpace(loadSaid.String())))
	}
	return nil
}
