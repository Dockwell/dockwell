// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// Restoring one database alone, an app's on a server it shares (ADR 0084), keeps
// the app out of it while it loads, and lets it back in after, whatever came of the
// load. PostgreSQL's owner can't sign in while it's out, and a role made for the
// restore, a member of the owner, loads, as the owner once it has set its role, so
// what it makes is the owner's, and has no rights of its own. MySQL and MariaDB keep a
// view's or routine's definer as whoever loads it, so the owner loads, with a password
// of the restore's own that the app doesn't have, and gets its own back, exactly as it
// was, after. Nothing loads as the server's administrator.

// Lockout is who a restore of one database keeps out, and how to let them back in,
// which the agent keeps with the restore, as a crash mustn't leave an app out.
type Lockout struct {
	// Owner is the database's owner, the app's user.
	Owner string `json:"owner"`
	// Login says PostgreSQL's owner could sign in before.
	Login bool `json:"login,omitempty"`
	// Loader is the PostgreSQL role made to load, a member of the owner.
	Loader string `json:"loader,omitempty"`
	// Host is a MySQL or MariaDB owner's host, and Restore the statement that gives
	// it back how it signed in, as the server says it: what the server keeps of its
	// password, never the password.
	Host    string `json:"host,omitempty"`
	Restore string `json:"restore,omitempty"`
}

// Signer is who loads, with their password, and what they run first.
func (l *Lockout) Signer(password string) (user, prelude string) {
	if l.Loader != "" {
		return l.Loader, "SET ROLE " + pgName(l.Owner) + ";\n"
	}
	return l.Owner, ""
}

// LockOut keeps an app out of its database db, on the server c runs, and says who
// loads it, with a password made for this, which it gives each time it's asked, as a
// restore that goes on after a crash doesn't have the last. A lockout from before is
// taken up again, never made anew, so what lets the app back in stays what it was.
func LockOut(ctx context.Context, client *docker.Client, c *docker.Container, e *Engine, db string, was *Lockout) (*Lockout, string, error) {
	if err := CheckSQLName("database", db); err != nil {
		return nil, "", err
	}
	password := randomHex(24)
	r := Reader{Client: client}
	cmd, _ := e.reading(PortOf(e, c.Config.Cmd))
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		l := was
		if l == nil {
			said, err := r.ask(ctx, c.ID, e, cmd, "SELECT json_build_object('owner', r.rolname, 'login', r.rolcanlogin, 'super', r.rolsuper) FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE d.datname = '"+db+"';\n")
			if err != nil {
				return nil, "", err
			}
			var got struct {
				Owner string `json:"owner"`
				Login bool   `json:"login"`
				Super bool   `json:"super"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(said))), &got); err != nil || got.Owner == "" {
				return nil, "", fmt.Errorf("it has no database %s", db)
			}
			if got.Super {
				return nil, "", fmt.Errorf("%s is %s's, who runs the whole server, so it can't be kept out to restore %s alone. Restore the whole server instead", db, got.Owner, db)
			}
			if err := CheckSQLName("user", got.Owner); err != nil {
				return nil, "", err
			}
			l = &Lockout{Owner: got.Owner, Login: got.Login, Loader: "dw_restore_" + randomHex(6)}
		}
		verifier, err := scram(password)
		if err != nil {
			return nil, "", err
		}
		sql := "SELECT 'CREATE ROLE " + pgName(l.Loader) + " NOINHERIT IN ROLE " + pgName(l.Owner) + "' WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '" + l.Loader + "')\n\\gexec\n" +
			"ALTER ROLE " + pgName(l.Loader) + " LOGIN PASSWORD '" + verifier + "';\n" +
			"GRANT CONNECT ON DATABASE " + pgName(db) + " TO " + pgName(l.Loader) + ";\n" +
			"ALTER ROLE " + pgName(l.Owner) + " NOLOGIN;\n" +
			"SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE usename = '" + l.Owner + "';\n"
		if _, err := r.ask(ctx, c.ID, e, cmd, sql); err != nil {
			return nil, "", err
		}
		return l, password, nil
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		l := was
		if l == nil {
			said, err := r.ask(ctx, c.ID, e, cmd, "SELECT User, Host FROM mysql.db WHERE Db IN ('"+db+"', '"+myGranted(db)+"');\n")
			if err != nil {
				return nil, "", err
			}
			lines := strings.Split(strings.TrimSpace(string(said)), "\n")
			if len(lines) != 1 || lines[0] == "" {
				return nil, "", fmt.Errorf("%s has %d users of its own where one app's database has one, so it can't be kept to one to restore it alone. Restore the whole server instead", db, len(lines))
			}
			user, host, _ := strings.Cut(lines[0], "\t")
			if CheckSQLName("user", user) != nil || host != "%" {
				return nil, "", fmt.Errorf("%s's user isn't one Dockwell made, so it can't be kept out to restore %s alone. Restore the whole server instead", db, db)
			}
			show := ""
			if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL {
				// What it keeps of a password can hold any byte, which only hex keeps whole.
				show = "SET SESSION print_identified_with_as_hex = ON;\n"
			}
			said, err = r.ask(ctx, c.ID, e, cmd, show+"SHOW CREATE USER "+myUser(user)+";\n")
			if err != nil {
				return nil, "", err
			}
			create := strings.TrimSpace(string(said))
			if !strings.HasPrefix(create, "CREATE USER ") || strings.Contains(create, "\n") {
				return nil, "", errors.New("can't read how its user signs in, so it isn't kept out. Restore the whole server instead")
			}
			l = &Lockout{Owner: user, Host: host, Restore: "ALTER USER " + strings.TrimPrefix(create, "CREATE USER ") + ";\n"}
		}
		identified := "IDENTIFIED BY '" + mysqlQuoted(password) + "'"
		mode := "SET SESSION sql_mode = '';\n"
		if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB {
			identified, mode = "IDENTIFIED BY PASSWORD '"+mysqlNative(password)+"'", ""
		}
		said, err := r.ask(ctx, c.ID, e, cmd, mode+"ALTER USER "+myUser(l.Owner)+" "+identified+";\n"+
			"SELECT ID FROM information_schema.PROCESSLIST WHERE USER = '"+l.Owner+"';\n")
		if err != nil {
			return nil, "", err
		}
		var kills strings.Builder
		for _, id := range strings.Fields(string(said)) {
			if strings.Trim(id, "0123456789") == "" {
				kills.WriteString("KILL " + id + ";\n")
			}
		}
		if kills.Len() > 0 {
			// One may have ended by itself in between, which isn't a failure.
			_, _ = r.ask(ctx, c.ID, e, cmd, kills.String())
		}
		return l, password, nil
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB,
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS, dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY:
	}
	return nil, "", fmt.Errorf("restoring one %s database alone isn't something Dockwell does yet", e.Name)
}

// LetIn lets the app back into its database: PostgreSQL's owner signs in again, if
// it could before, and the role that loaded goes, with anything it made of its own;
// MySQL's and MariaDB's owner signs in as it did.
func LetIn(ctx context.Context, client *docker.Client, c *docker.Container, e *Engine, db string, l *Lockout) error {
	if l == nil {
		return nil
	}
	if err := checkNames(l.Owner, db); err != nil {
		return err
	}
	r := Reader{Client: client}
	cmd, _ := e.reading(PortOf(e, c.Config.Cmd))
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		if err := CheckSQLName("user", l.Loader); err != nil {
			return err
		}
		sql := "\\connect " + pgName(db) + "\n" +
			"SELECT 'DROP OWNED BY " + pgName(l.Loader) + "' WHERE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '" + l.Loader + "')\n\\gexec\n" +
			"\\connect postgres\n" +
			"DROP ROLE IF EXISTS " + pgName(l.Loader) + ";\n"
		if l.Login {
			sql += "ALTER ROLE " + pgName(l.Owner) + " LOGIN;\n"
		}
		_, err := r.ask(ctx, c.ID, e, cmd, sql)
		return err
	}
	if !strings.HasPrefix(l.Restore, "ALTER USER ") {
		return errors.New("what lets the app back in isn't one statement of its user's, so it's left out. Set its password anew from its page")
	}
	mode := ""
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL {
		mode = "SET SESSION sql_mode = '';\n"
	}
	_, err := r.ask(ctx, c.ID, e, cmd, mode+l.Restore)
	return err
}
