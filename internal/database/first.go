// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// FirstPasswords keeps a database's first user's password the one its service starts
// with (ADR 0038). The official images set POSTGRES_PASSWORD, MYSQL_ROOT_PASSWORD and
// MARIADB_ROOT_PASSWORD on their first start alone, from an empty volume, so a
// container that starts with another one would run with a password its database
// doesn't take: its apps and Dockwell's readings couldn't sign in. So before a
// service's container is replaced with another value of it, the agent changes it in
// the database, in the container that runs, signing in as the image does.
type FirstPasswords struct {
	Client *docker.Client
	// Every is how often putting a password back asks again while the database
	// starts. Zero means a second.
	Every time.Duration
}

// Undo puts back a password Change gave the first user, where what runs after a
// rollout that failed is the container from before.
type Undo = func(ctx context.Context) error

// Change gives the users what project's service runs made from its environment the
// passwords app starts with, where app's differ from what runs, and says how to put
// them back. It does nothing, and returns a nil Undo, for an image it doesn't know as
// PostgreSQL, MySQL or MariaDB, where nothing of the service runs, as a new container
// on an empty volume sets the passwords itself, and where no password changes.
func (f *FirstPasswords) Change(ctx context.Context, project, service string, app *resourcev1.Container) (Undo, error) {
	e := Recognise(app.GetImage())
	// A container of a database's image that runs something else, such as a client
	// that dumps another server, has no database of its own to change.
	if e == nil || !e.SQL() || !startsServer(e, app) || f.Client == nil {
		return nil, nil
	}
	c, err := f.running(ctx, project, service)
	if err != nil || c == nil {
		return nil, err
	}
	was, now := envMap(c.Config.Env), app.GetEnv()
	moves := passwordMoves(e, was, now)
	if len(moves) == 0 {
		return nil, nil
	}
	rootWas, _ := firstPassword(e, was)
	rootNow, ok := firstPassword(e, now)
	if !ok {
		rootNow = rootWas
	}
	if err := f.set(ctx, c, e, moves, rootWas, rootNow); err != nil {
		return nil, fmt.Errorf("can't give %s's %s the new password in %s before it restarts, so nothing was restarted: %w", service, names(moves), e.Name, err)
	}
	// The old container comes back as a rollout fails, and its database takes a moment
	// to take connections: putting the password back asks again until it does, within
	// what ctx gives it.
	return func(ctx context.Context) error {
		var last error
		for {
			err := f.putBack(ctx, project, service, e, moves, rootNow, rootWas)
			if ctx.Err() != nil && last != nil {
				// Its time ran out: what it waited on says more than the call that cut short.
				return last
			}
			if err == nil || !stillStarting(err) {
				return err
			}
			last = err
			select {
			case <-ctx.Done():
				return last
			case <-time.After(cmp.Or(f.Every, time.Second)):
			}
		}
	}, nil
}

// errNotUpYet is a container that isn't running yet, as one that's starting again.
var errNotUpYet = errors.New("isn't running yet")

// putBack gives the users back the passwords the old container starts with, once
// it's the one that runs.
func (f *FirstPasswords) putBack(ctx context.Context, project, service string, e *Engine, moves []passwordMove, rootNow, rootWas string) error {
	c, err := f.running(ctx, project, service)
	switch {
	case err != nil:
		return err
	case c == nil:
		return fmt.Errorf("%s %w, so %s's password in %s stays the new one, which the old container doesn't start with", service, errNotUpYet, names(moves), e.Name)
	}
	// What runs is the new container, or something else: the new passwords are its.
	if env := envMap(c.Config.Env); env[moves[0].variable] != moves[0].from {
		return nil
	}
	back := make([]passwordMove, len(moves))
	for i, m := range moves {
		back[i] = passwordMove{user: m.user, variable: m.variable, from: m.to, to: m.from}
	}
	if err := f.set(ctx, c, e, back, rootNow, rootWas); err != nil {
		return fmt.Errorf("can't put %s's password in %s back to the one the old container has: %w", names(moves), e.Name, err)
	}
	return nil
}

// stillStarting says the old container isn't running yet, or its database isn't
// taking connections yet, as it starts.
func stillStarting(err error) bool {
	return errors.Is(err, errNotUpYet) || errors.Is(err, ErrStarting)
}

// passwordMove is a user an image makes from its environment, whose password a new
// container starts with another of.
type passwordMove struct {
	user, variable string
	from, to       string
}

// passwordMoves lists the users whose passwords differ between what runs and what
// replaces it: the first user, and MySQL's and MariaDB's second, which their images
// make from MYSQL_USER or MARIADB_USER. A user the new container names otherwise
// isn't one this changes: the image made the old one.
func passwordMoves(e *Engine, was, now map[string]string) []passwordMove {
	var out []passwordMove
	add := func(user string, variables ...string) {
		for _, v := range variables {
			if from, to := was[v], now[v]; from != "" && to != "" {
				if from != to {
					out = append(out, passwordMove{user: user, variable: v, from: from, to: to})
				}
				return
			}
		}
	}
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		user := cmp.Or(was["POSTGRES_USER"], "postgres")
		if cmp.Or(now["POSTGRES_USER"], "postgres") == user {
			add(user, "POSTGRES_PASSWORD")
		}
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		add("root", e.Password...)
		for _, prefix := range []string{"MARIADB_", "MYSQL_"} {
			if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL && prefix == "MARIADB_" {
				continue
			}
			if user := was[prefix+"USER"]; user != "" && user != "root" {
				if now[prefix+"USER"] == user {
					add(user, prefix+"PASSWORD")
				}
				break
			}
		}
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED, dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS,
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY:
	}
	return out
}

// names says whose passwords move, for people.
func names(moves []passwordMove) string {
	var out []string
	for _, m := range moves {
		out = append(out, m.user)
	}
	return strings.Join(out, " and ")
}

// set gives each user their new password, signing in as root with rootFrom, or, where
// that's refused, with rootTo, as when a step that stopped partway set it already.
func (f *FirstPasswords) set(ctx context.Context, c *docker.Container, e *Engine, moves []passwordMove, rootFrom, rootTo string) error {
	for _, m := range moves {
		if strings.ContainsAny(m.from+m.to, "\r\n\x00") {
			return errors.New("a password with a line break in it can't be passed to its tools. Set one without")
		}
	}
	r := Reader{Client: f.Client}
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		// Its image trusts every connection inside its container, whatever the password.
		var sql strings.Builder
		for _, m := range moves {
			verifier, err := scram(m.to)
			if err != nil {
				return err
			}
			sql.WriteString("ALTER ROLE " + pgQuote(m.user) + " PASSWORD '" + verifier + "';\n")
		}
		cmd, _ := e.reading(PortOf(e, c.Config.Cmd))
		_, err := r.ask(ctx, c.ID, e, cmd, sql.String())
		return err
	}
	if strings.ContainsAny(rootFrom+rootTo, "\r\n\x00") {
		return errors.New("root's password has a line break in it, which can't be passed to its tools. Set one without")
	}
	users := make([]string, 0, len(moves))
	for _, m := range moves {
		if !aUser(m.user) {
			return fmt.Errorf("%s has a user Dockwell can't name safely, %d characters long, so it changed no password. Change it in %s, then try again", e.Name, len(m.user), e.Name)
		}
		users = append(users, "'"+m.user+"'")
	}
	ask := "SELECT User, Host FROM mysql.user WHERE User IN (" + strings.Join(users, ", ") + ");\n"
	signIn := rootFrom
	said, err := r.ask(ctx, c.ID, e, withPassword(), rootFrom+"\n"+ask)
	if err != nil {
		var refused error
		if said, refused = r.ask(ctx, c.ID, e, withPassword(), rootTo+"\n"+ask); refused != nil {
			if strings.Contains(err.Error(), "refused Dockwell's sign-in") && strings.Contains(refused.Error(), "refused Dockwell's sign-in") {
				// Root's password is neither: changed by hand, or random from the start.
				return fmt.Errorf("%w. Set the secret to the password root has now, then change it here", err)
			}
			return err
		}
		signIn = rootTo
	}
	sql, err := e.alterUsers(accounts(said), moves)
	if err != nil {
		return err
	}
	_, err = r.ask(ctx, c.ID, e, withPassword(), signIn+"\n"+sql)
	return err
}

// aUser and aHost are a user's name and a host it signs in from, which need nothing
// escaped in a quoted string. Their lengths are checked apart from their form, as a
// counted repetition compiles to that many copies, which the agent would build as
// it starts.
var (
	userForm = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	hostForm = regexp.MustCompile(`^[A-Za-z0-9._%:-]+$`)
)

func aUser(s string) bool { return len(s) <= 80 && userForm.MatchString(s) }

func aHost(s string) bool { return len(s) <= 255 && hostForm.MatchString(s) }

// accounts reads what the users query lists: each user with the hosts it has, as
// MySQL and MariaDB keep each of those as a user of its own, such as 'root'@'%' and
// 'root'@'localhost' as their images make them.
func accounts(said []byte) map[string][]string {
	out := map[string][]string{}
	for line := range strings.Lines(string(said)) {
		user, host, ok := strings.Cut(strings.TrimRight(line, "\r\n"), "\t")
		if ok && host != "" && !slices.Contains(out[user], host) {
			out[user] = append(out[user], host)
		}
	}
	return out
}

// alterUsers is the SQL that gives each user, from every host it has, its new
// password: as MariaDB's own hash, or for MySQL over stdin, which it leaves out of its
// logs.
func (e *Engine) alterUsers(hosts map[string][]string, moves []passwordMove) (string, error) {
	var b strings.Builder
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL {
		// With no sql_mode, a backslash escapes, as quoted below.
		b.WriteString("SET SESSION sql_mode = '';\n")
	}
	for _, m := range moves {
		if len(hosts[m.user]) == 0 {
			return "", fmt.Errorf("%s has no user %s, which its image made from %s", e.Name, m.user, m.variable)
		}
		for _, h := range hosts[m.user] {
			if !aHost(h) {
				return "", fmt.Errorf("%s has the user %s from a host Dockwell can't name safely, so it changed no password. Change it in %s, then try again", e.Name, m.user, e.Name)
			}
			if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL {
				b.WriteString("ALTER USER '" + m.user + "'@'" + h + "' IDENTIFIED BY '" + mysqlQuoted(m.to) + "';\n")
				continue
			}
			// MariaDB's own hash, as SET PASSWORD takes it: that changes the plugins that
			// take a password, and keeps root@localhost's unix_socket, where ALTER USER
			// would replace the whole chain.
			b.WriteString("SET PASSWORD FOR '" + m.user + "'@'" + h + "' = '" + mysqlNative(m.to) + "';\n")
		}
	}
	return b.String(), nil
}

// withPassword is the MySQL or MariaDB client signing in as root with the password on
// the first line of its input, so it never reaches a command line Docker keeps.
func withPassword() []string {
	return []string{"sh", "-c", `IFS= read -r MYSQL_PWD; export MYSQL_PWD; c=mysql; command -v mariadb >/dev/null 2>&1 && c=mariadb; exec timeout 7 "$c" -uroot -N -B --connect-timeout=5`}
}

// running finds the container of project's service that runs, if one does.
func (f *FirstPasswords) running(ctx context.Context, project, service string) (*docker.Container, error) {
	list, err := f.Client.ListContainers(ctx, map[string][]string{"label": {
		docker.LabelProject + "=" + project, docker.LabelService + "=" + service,
	}})
	if err != nil {
		return nil, err
	}
	for i := range list {
		s := &list[i]
		if s.State != "running" || s.Labels[docker.LabelOneoff] == "True" {
			continue
		}
		c, err := f.Client.InspectContainer(ctx, s.ID)
		switch {
		case docker.IsNotFound(err):
			continue
		case err != nil:
			return nil, err
		case c.State.Running && !c.State.Restarting:
			return c, nil
		}
	}
	return nil, nil
}

// firstPassword is the first user's password in env, as the image reads it.
func firstPassword(e *Engine, env map[string]string) (string, bool) {
	for _, name := range e.Password {
		if v := env[name]; v != "" {
			return v, true
		}
	}
	return "", false
}

// envMap reads Docker's KEY=value list.
func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// pgQuote quotes any name for PostgreSQL, as POSTGRES_USER may hold what CheckSQLName
// wouldn't take.
func pgQuote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
