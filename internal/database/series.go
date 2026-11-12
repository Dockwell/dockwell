// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// MovesBySeries says an engine's data moves to another release series only by a
// dump and a restore, or only one way, so changing its image across one is never
// a plain swap (ADR 0080). PostgreSQL can't start on another major's data at all.
// MySQL and MariaDB move their data on as a newer series starts, and an older one
// can't read it after, so undo and the automatic rollback, which put the old image
// back, would leave the database down.
func (e *Engine) MovesBySeries() bool {
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL,
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		return true
	default:
		return false
	}
}

// Series is the release series an image's tag names, of an engine whose data
// moves by series: PostgreSQL's major, such as "16", or "9.6" before 10, and MySQL's
// or MariaDB's, such as "8.0" or "11.4". Empty where its tag names less, such as
// latest, or mysql:8, which runs whatever 8.x is newest.
func Series(e *Engine, image string) string {
	return seriesOf(e, Version(image))
}

func seriesOf(e *Engine, version string) string {
	parts := strings.Split(version, ".")
	for _, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return ""
		}
	}
	major, _ := strconv.Atoi(parts[0])
	switch {
	case e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL && major >= 10:
		return parts[0]
	case len(parts) >= 2:
		return parts[0] + "." + parts[1]
	}
	return ""
}

// seriesRefused says why a change of a database's image from one series to
// another is refused, in a line: the fact, what it would mean, and what to pick.
// It starts with the engine, never the service's name, which a surface would
// give a capital.
func seriesRefused(name string, e *Engine, from, to string) string {
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		return fmt.Sprintf("PostgreSQL %s can't start on the data of %s, which runs PostgreSQL %s, so its image stays as it is. "+
			"Dockwell can't move a database across major releases yet: keep it on %s", to, name, from, from)
	}
	if newer(to, from) {
		return fmt.Sprintf("%s %s would move the data of %s, which runs %s, on for good, and %s can't read it again, so neither undo nor a rollback could put it back. "+
			"Dockwell can't move a database across release series yet: keep it on %s", e.Name, to, name, from, from, from)
	}
	return fmt.Sprintf("%s %s can't read the data of %s, which runs %s %s, so its image stays as it is. Keep it on %s", e.Name, to, name, e.Name, from, from)
}

// newer says series a is after series b, as "8.4" is after "8.0" and "17" after
// "9.6".
func newer(a, b string) bool {
	an, bn := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(an), len(bn)) {
		var x, y int
		if i < len(an) {
			x, _ = strconv.Atoi(an[i])
		}
		if i < len(bn) {
			y, _ = strconv.Atoi(bn[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// seriesMove refuses a change of a database's image across release series, as its
// tags say, before anything is planned (ADR 0080). A tag that names less, such as
// latest, is asked of the image itself as the change applies (sameSeries).
func seriesMove(name string, e *Engine, current, desired *resourcev1.Service) error {
	from, to := Series(e, current.GetApp().GetImage()), Series(e, desired.GetApp().GetImage())
	if from == "" || to == "" || from == to {
		return nil
	}
	return fmt.Errorf("%s", seriesRefused(name, e, from, to))
}

// seriesFromEnv is the series an official image says it runs, in its environment:
// PG_MAJOR for PostgreSQL, MYSQL_VERSION for MySQL, whose MYSQL_MAJOR reads
// "innovation" on 9.x, and MARIADB_VERSION for MariaDB, after its epoch.
func seriesFromEnv(e *Engine, env []string) string {
	vars := map[dockwellv1.DatabaseEngine]string{
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL: "PG_MAJOR",
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:      "MYSQL_VERSION",
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:    "MARIADB_VERSION",
	}
	want := vars[e.Kind] + "="
	for _, kv := range env {
		v, ok := strings.CutPrefix(kv, want)
		if !ok || v == "" {
			continue
		}
		if _, after, epoch := strings.Cut(v, ":"); epoch {
			v = after
		}
		// 8.4.3-1.el9, 11.4.4+maria~ubu2404, 17.2-1.pgdg120+1: the numbers up front.
		end := strings.IndexFunc(v, func(r rune) bool { return r != '.' && (r < '0' || r > '9') })
		if end >= 0 {
			v = v[:end]
		}
		return seriesOf(e, strings.Trim(v, "."))
	}
	return ""
}

// sameSeries refuses replacing a database with an image of another release series,
// as the images themselves say, after the pull and before anything stops (ADR 0080):
// a moving tag such as postgres:latest can cross a major release with nothing in its
// name to say so. Either image saying nothing leaves it to the database.
func (r Reader) sameSeries(ctx context.Context, e *Engine, old *docker.Container, spec *resourcev1.Container) error {
	was, err := r.Client.InspectImage(ctx, old.Image)
	if err != nil || was.Config == nil {
		return nil //nolint:nilerr // nothing to compare with: the database says it as it starts
	}
	now, err := r.Client.InspectImage(ctx, docker.WithTag(spec.GetImage()))
	if err != nil || now.Config == nil {
		return nil //nolint:nilerr // nothing to compare with: the database says it as it starts
	}
	from, to := seriesFromEnv(e, was.Config.Env), seriesFromEnv(e, now.Config.Env)
	if from == "" || to == "" || from == to {
		return nil
	}
	name := old.Config.Labels[docker.LabelService]
	if name == "" {
		name = strings.TrimPrefix(old.Name, "/")
	}
	plain := fmt.Sprintf("%s names %s %s now. %s, such as with %s:%s. Nothing was stopped.", spec.GetImage(), e.Name, to, seriesRefused(name, e, from, to), Repository(spec.GetImage()), from)
	return resource.PlainlyBecause(resource.ReasonDatabaseRelease, plain,
		fmt.Errorf("%s runs %s %s, and %s names %s", name, e.Name, from, spec.GetImage(), to))
}
