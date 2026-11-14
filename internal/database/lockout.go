// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
//
// Who is kept out, and how to let them back in, is read first, changing nothing
// (ReadLockout), and kept by the agent before anything changes (LockOut), as a
// crash or a failure after that mustn't leave an app out with no way back in.
// A lockout an earlier restore left is taken up, never read anew, which would
// take the restore's own way in for the app's.

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

// loaderPrefix starts the name of each role a restore makes to load.
const loaderPrefix = "dw_restore_"

// restoringPrefix starts the name of the database a MySQL or MariaDB owner is given a
// grant on while a restore keeps it out, which no one makes: it marks the restore's
// password, by what the server keeps of it, so a restore that finds it while its
// record is gone knows it would take that password for the app's. It has no
// underscore, which a grant reads as any character.
const restoringPrefix = "dockwellrestoring"

// ReadLockout reads who a restore of db alone keeps out, and how to let them back in,
// changing nothing. On PostgreSQL it refuses a database holding anything its owner
// doesn't own, which a load as the owner can't replace, and takes up a lockout an
// earlier restore left; on MySQL and MariaDB it refuses where an earlier restore's
// password is still the owner's, as how it signed in before is gone.
func ReadLockout(ctx context.Context, client *docker.Client, c *docker.Container, e *Engine, db string) (*Lockout, error) {
	if err := CheckSQLName("database", db); err != nil {
		return nil, err
	}
	r := Reader{Client: client}
	cmd, _ := e.reading(PortOf(e, c.Config.Cmd))
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		said, err := r.ask(ctx, c.ID, e, cmd, "SELECT json_build_object('owner', r.rolname, 'login', r.rolcanlogin, 'super', r.rolsuper, "+
			"'loader', (SELECT m.rolname FROM pg_auth_members a JOIN pg_roles m ON m.oid = a.member WHERE a.roleid = r.oid AND m.rolname LIKE 'dw\\_restore\\_%' ORDER BY m.rolname LIMIT 1)) "+
			"FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE d.datname = '"+db+"';\n")
		if err != nil {
			return nil, err
		}
		var got struct {
			Owner  string `json:"owner"`
			Login  bool   `json:"login"`
			Super  bool   `json:"super"`
			Loader string `json:"loader"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(said))), &got); err != nil || got.Owner == "" {
			return nil, fmt.Errorf("it has no database %s", db)
		}
		if got.Super {
			return nil, fmt.Errorf("%s is %s's, who runs the whole server, so it can't be kept out to restore %s alone. Restore the whole server instead", db, got.Owner, db)
		}
		if err := CheckSQLName("user", got.Owner); err != nil {
			return nil, err
		}
		if err := pgOwnsAll(ctx, r, c, e, cmd, db, got.Owner); err != nil {
			return nil, err
		}
		l := &Lockout{Owner: got.Owner, Login: got.Login, Loader: loaderPrefix + randomHex(6)}
		if got.Loader != "" {
			// An earlier restore made it and its record is gone: the owner signed in
			// before that restore, as an app's user does, and goes on with this one.
			if err := CheckSQLName("user", got.Loader); err != nil {
				return nil, err
			}
			l.Loader, l.Login = got.Loader, true
		}
		return l, nil
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		said, err := r.ask(ctx, c.ID, e, cmd, "SELECT User, Host FROM mysql.db WHERE Db IN ('"+db+"', '"+myGranted(db)+"');\n")
		if err != nil {
			return nil, err
		}
		lines := strings.Split(strings.TrimSpace(string(said)), "\n")
		if len(lines) != 1 || lines[0] == "" {
			return nil, fmt.Errorf("%s has %d users of its own where one app's database has one, so it can't be kept to one to restore it alone. Restore the whole server instead", db, len(lines))
		}
		user, host, _ := strings.Cut(lines[0], "\t")
		if CheckSQLName("user", user) != nil || host != "%" {
			return nil, fmt.Errorf("%s's user isn't one Dockwell made, so it can't be kept out to restore %s alone. Restore the whole server instead", db, db)
		}
		create, err := myCreateUser(ctx, r, c, e, cmd, user)
		if err != nil {
			return nil, err
		}
		marks, err := myMarks(ctx, r, c, e, cmd, user)
		if err != nil {
			return nil, err
		}
		for _, m := range marks {
			if m == restoringPrefix+fingerprint(create) {
				return nil, fmt.Errorf("%s still signs in to %s with the password an earlier restore gave it, whose record is gone, so how it signed in before can't be put back, and nothing was changed. "+
					"Set its password anew from its app's page, then try again", user, db)
			}
		}
		return &Lockout{Owner: user, Host: host, Restore: "ALTER USER " + strings.TrimPrefix(create, "CREATE USER ") + ";\n"}, nil
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB,
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS, dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY:
	}
	return nil, fmt.Errorf("restoring one %s database alone isn't something Dockwell does yet", e.Name)
}

// pgOwnsAll refuses a database holding anything its owner doesn't own, outside what
// PostgreSQL itself made and what an extension the owner made brings: an extension an
// administrator made, such as dblink, or an administrator's table. A load as the owner
// can't drop or make that again, so it would stop partway, and so would putting back
// what was there.
func pgOwnsAll(ctx context.Context, r Reader, c *docker.Container, e *Engine, cmd []string, db, owner string) error {
	said, err := r.ask(ctx, c.ID, e, cmd, "\\connect "+pgName(db)+"\n"+PgNotOwned(owner))
	if err != nil {
		return err
	}
	if what := strings.TrimSpace(string(said)); what != "" {
		first, _, _ := strings.Cut(what, "\n")
		return fmt.Errorf("%s holds %s, which isn't %s's, so a load as %s would stop partway, and nothing was changed. "+
			"Restore the whole server instead", db, first, owner, owner)
	}
	return nil
}

// PgNotOwned lists, a line each, what in the database it runs in owner doesn't own:
// past what initdb made (oids from 16384), and not an extension's own part, as an
// extension the owner made brings parts the bootstrap superuser owns. A temporary
// schema is a session's, not the database's.
func PgNotOwned(owner string) string {
	o := "(SELECT oid FROM pg_roles WHERE rolname = '" + owner + "')"
	part := func(class, alias string) string {
		return "NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = '" + class + "'::regclass AND d.objid = " + alias + ".oid AND d.deptype IN ('e', 'i'))"
	}
	return "SELECT what FROM (" +
		"SELECT 'the extension ' || quote_ident(x.extname) AS what FROM pg_extension x WHERE x.oid >= 16384 AND x.extowner <> " + o +
		" UNION ALL SELECT 'the event trigger ' || quote_ident(t.evtname) FROM pg_event_trigger t WHERE t.oid >= 16384 AND t.evtowner <> " + o +
		" UNION ALL SELECT 'the schema ' || quote_ident(n.nspname) FROM pg_namespace n WHERE n.oid >= 16384 AND n.nspowner <> " + o +
		" AND n.nspname !~ '^pg_(toast_)?temp_' AND " + part("pg_namespace", "n") +
		" UNION ALL SELECT 'the table ' || c.oid::regclass::text FROM pg_class c WHERE c.oid >= 16384 AND c.relowner <> " + o +
		" AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S', 'c') AND c.relpersistence <> 't' AND " + part("pg_class", "c") +
		" UNION ALL SELECT 'the type ' || t.oid::regtype::text FROM pg_type t WHERE t.oid >= 16384 AND t.typowner <> " + o +
		" AND t.typrelid = 0 AND " + part("pg_type", "t") +
		" UNION ALL SELECT 'the function ' || p.oid::regprocedure::text FROM pg_proc p WHERE p.oid >= 16384 AND p.proowner <> " + o +
		" AND " + part("pg_proc", "p") +
		") x LIMIT 3;\n"
}

// PgEmptying drops, as the owner, in the load's transaction, what the owner owns in
// the database: each schema of its own with what's in it, and, in a schema it doesn't
// own such as public, each relation, type and routine, and each large object. A
// newer table then can't hold an older one in place, and a backup from before an
// update brings back that database alone, not it with the update's tables beside.
// Nothing is revoked, so what the owner was granted, as on public, stays.
const PgEmptying = `DO $dw_empty$
DECLARE
  me oid := (SELECT oid FROM pg_roles WHERE rolname = current_user);
  r record;
BEGIN
  PERFORM set_config('search_path', '', true);
  FOR r IN SELECT quote_ident(x.extname) AS name FROM pg_extension x WHERE x.extowner = me AND x.oid >= 16384 LOOP
    EXECUTE 'DROP EXTENSION IF EXISTS ' || r.name || ' CASCADE';
  END LOOP;
  FOR r IN SELECT quote_ident(n.nspname) AS name FROM pg_namespace n WHERE n.nspowner = me AND n.oid >= 16384 AND n.nspname !~ '^pg_(toast_)?temp_' LOOP
    EXECUTE 'DROP SCHEMA IF EXISTS ' || r.name || ' CASCADE';
  END LOOP;
  FOR r IN SELECT format('%I.%I', n.nspname, c.relname) AS name, c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
      WHERE c.relowner = me AND c.oid >= 16384 AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S') AND c.relpersistence <> 't'
      AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype IN ('e', 'i', 'a')) LOOP
    EXECUTE format('DROP %s IF EXISTS %s CASCADE', CASE r.relkind WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' WHEN 'f' THEN 'FOREIGN TABLE' WHEN 'S' THEN 'SEQUENCE' ELSE 'TABLE' END, r.name);
  END LOOP;
  FOR r IN SELECT format('%I.%I', n.nspname, t.typname) AS name, t.typtype FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
      WHERE t.typowner = me AND t.oid >= 16384 AND (t.typrelid = 0 OR (SELECT c.relkind FROM pg_class c WHERE c.oid = t.typrelid) = 'c')
      AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype IN ('e', 'i')) LOOP
    EXECUTE format('DROP %s IF EXISTS %s CASCADE', CASE r.typtype WHEN 'd' THEN 'DOMAIN' ELSE 'TYPE' END, r.name);
  END LOOP;
  FOR r IN SELECT p.oid::regprocedure::text AS name FROM pg_proc p
      WHERE p.proowner = me AND p.oid >= 16384
      AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype IN ('e', 'i')) LOOP
    EXECUTE 'DROP ROUTINE IF EXISTS ' || r.name || ' CASCADE';
  END LOOP;
  PERFORM lo_unlink(l.oid) FROM pg_largeobject_metadata l WHERE l.lomowner = me;
END
$dw_empty$;
`

// Emptying is what a load of a dump of db alone runs first, as the owner: on
// PostgreSQL, PgEmptying, in the load's one transaction; on MySQL and MariaDB, which
// can't undo what they drop, a drop of each table, view, routine and event the
// database has now, as a dump of it drops only what it holds.
func Emptying(ctx context.Context, client *docker.Client, c *docker.Container, e *Engine, db string) (string, error) {
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		return PgEmptying, nil
	}
	if err := CheckSQLName("database", db); err != nil {
		return "", err
	}
	cmd, _ := e.reading(PortOf(e, c.Config.Cmd))
	said, err := Reader{Client: client}.ask(ctx, c.ID, e, cmd,
		"SELECT IF(TABLE_TYPE = 'VIEW', 'VIEW', 'TABLE'), HEX(TABLE_NAME) FROM information_schema.TABLES WHERE TABLE_SCHEMA = '"+db+"';\n"+
			"SELECT ROUTINE_TYPE, HEX(ROUTINE_NAME) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = '"+db+"';\n"+
			"SELECT 'EVENT', HEX(EVENT_NAME) FROM information_schema.EVENTS WHERE EVENT_SCHEMA = '"+db+"';\n")
	if err != nil {
		return "", err
	}
	return MyEmptying(string(said))
}

// MyEmptying is the SQL that drops what a MySQL or MariaDB database holds, as listed a
// line each, a kind and a name in hex: views first, then tables with no foreign key
// in the way, then routines and events.
func MyEmptying(listed string) (string, error) {
	byKind := map[string][]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(listed), "\n") {
		if line == "" {
			continue
		}
		kind, hexName, ok := strings.Cut(line, "\t")
		name, err := hex.DecodeString(hexName)
		if !ok || err != nil {
			return "", fmt.Errorf("can't read what the database holds from %q", line)
		}
		byKind[kind] = append(byKind[kind], "`"+strings.ReplaceAll(string(name), "`", "``")+"`")
	}
	var b strings.Builder
	b.WriteString("SET FOREIGN_KEY_CHECKS = 0;\n")
	for _, kind := range []string{"VIEW", "TABLE"} {
		if names := byKind[kind]; len(names) > 0 {
			b.WriteString("DROP " + kind + " IF EXISTS " + strings.Join(names, ", ") + ";\n")
		}
	}
	for _, kind := range []string{"PROCEDURE", "FUNCTION", "EVENT"} {
		for _, name := range byKind[kind] {
			b.WriteString("DROP " + kind + " IF EXISTS " + name + ";\n")
		}
	}
	return b.String(), nil
}

// LockOut keeps an app out of its database db, as l, which ReadLockout read and the
// caller kept, and says the password made for who loads, which it makes anew each
// time, as a restore that goes on after a crash doesn't have the last. A caller that
// gets an error lets the app back in with l, as part of this may have happened.
func LockOut(ctx context.Context, client *docker.Client, c *docker.Container, e *Engine, db string, l *Lockout) (string, error) {
	if l == nil {
		return "", errors.New("there's no record of who to keep out")
	}
	if err := checkNames(l.Owner, db); err != nil {
		return "", err
	}
	password := randomHex(24)
	r := Reader{Client: client}
	cmd, _ := e.reading(PortOf(e, c.Config.Cmd))
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		if err := CheckSQLName("user", l.Loader); err != nil || !strings.HasPrefix(l.Loader, loaderPrefix) {
			return "", fmt.Errorf("%q isn't a role Dockwell makes to load", l.Loader)
		}
		verifier, err := scram(password)
		if err != nil {
			return "", err
		}
		sql := "SELECT 'CREATE ROLE " + pgName(l.Loader) + " NOINHERIT IN ROLE " + pgName(l.Owner) + "' WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '" + l.Loader + "')\n\\gexec\n" +
			"ALTER ROLE " + pgName(l.Loader) + " LOGIN PASSWORD '" + verifier + "';\n" +
			"GRANT CONNECT ON DATABASE " + pgName(db) + " TO " + pgName(l.Loader) + ";\n" +
			"ALTER ROLE " + pgName(l.Owner) + " NOLOGIN;\n" +
			"SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE usename = '" + l.Owner + "';\n"
		if _, err := r.ask(ctx, c.ID, e, cmd, sql); err != nil {
			return "", err
		}
		return password, nil
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		if l.Host != "%" || !strings.HasPrefix(l.Restore, "ALTER USER ") {
			return "", errors.New("the record of how its user signs in isn't one Dockwell kept")
		}
		identified := "IDENTIFIED BY '" + mysqlQuoted(password) + "'"
		mode := "SET SESSION sql_mode = '';\n"
		if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB {
			identified, mode = "IDENTIFIED BY PASSWORD '"+mysqlNative(password)+"'", ""
		}
		said, err := r.ask(ctx, c.ID, e, cmd, mode+"ALTER USER "+myUser(l.Owner)+" "+identified+";\n"+
			"SELECT ID FROM information_schema.PROCESSLIST WHERE USER = '"+l.Owner+"';\n")
		if err != nil {
			return "", err
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
		// Marked by what the server keeps of the restore's password, which a
		// restore with no record of this one reads as not the app's.
		create, err := myCreateUser(ctx, r, c, e, cmd, l.Owner)
		if err != nil {
			return "", err
		}
		if _, err := r.ask(ctx, c.ID, e, cmd, "GRANT SELECT ON "+myName(restoringPrefix+fingerprint(create))+".* TO "+myUser(l.Owner)+";\n"); err != nil {
			return "", err
		}
		return password, nil
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB,
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS, dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY:
	}
	return "", fmt.Errorf("restoring one %s database alone isn't something Dockwell does yet", e.Name)
}

// ErrLoaderLeft is a LetIn that let the app back in, and left what loaded: a role
// that can't sign in, which the next restore of the database takes up and removes,
// or a MySQL or MariaDB grant on a database no one makes.
var ErrLoaderLeft = errors.New("the app's user signs in again, but what loaded is still there")

// LetIn lets the app back into its database: PostgreSQL's owner signs in again, if
// it could before, and only then the role that loaded goes, with anything it made of
// its own; MySQL's and MariaDB's owner signs in as it did, and then loses the mark
// of the restore's password. What fails after the app is back in is ErrLoaderLeft.
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
		in := "SELECT 'ALTER ROLE " + pgName(l.Loader) + " NOLOGIN' WHERE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '" + l.Loader + "')\n\\gexec\n"
		if l.Login {
			in = "ALTER ROLE " + pgName(l.Owner) + " LOGIN;\n" + in
		}
		if _, err := r.ask(ctx, c.ID, e, cmd, in); err != nil {
			return err
		}
		gone := "\\connect " + pgName(db) + "\n" +
			"SELECT 'DROP OWNED BY " + pgName(l.Loader) + "' WHERE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '" + l.Loader + "')\n\\gexec\n" +
			"\\connect postgres\n" +
			"DROP ROLE IF EXISTS " + pgName(l.Loader) + ";\n"
		if _, err := r.ask(ctx, c.ID, e, cmd, gone); err != nil {
			return fmt.Errorf("%w: %w", ErrLoaderLeft, err)
		}
		return nil
	}
	if !strings.HasPrefix(l.Restore, "ALTER USER ") {
		return errors.New("what lets the app back in isn't one statement of its user's, so it's left out. Set its password anew from its app's page")
	}
	mode := ""
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL {
		mode = "SET SESSION sql_mode = '';\n"
	}
	if _, err := r.ask(ctx, c.ID, e, cmd, mode+l.Restore); err != nil {
		return err
	}
	marks, err := myMarks(ctx, r, c, e, cmd, l.Owner)
	if err == nil && len(marks) > 0 {
		var revoke strings.Builder
		for _, m := range marks {
			revoke.WriteString("REVOKE SELECT ON " + myName(m) + ".* FROM " + myUser(l.Owner) + ";\n")
		}
		_, err = r.ask(ctx, c.ID, e, cmd, revoke.String())
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrLoaderLeft, err)
	}
	return nil
}

// myCreateUser is how a MySQL or MariaDB user signs in, as SHOW CREATE USER says it:
// what the server keeps of its password, never the password.
func myCreateUser(ctx context.Context, r Reader, c *docker.Container, e *Engine, cmd []string, user string) (string, error) {
	show := ""
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL {
		// What it keeps of a password can hold any byte, which only hex keeps whole.
		show = "SET SESSION print_identified_with_as_hex = ON;\n"
	}
	said, err := r.ask(ctx, c.ID, e, cmd, show+"SHOW CREATE USER "+myUser(user)+";\n")
	if err != nil {
		return "", err
	}
	create := strings.TrimSpace(string(said))
	if !strings.HasPrefix(create, "CREATE USER ") || strings.Contains(create, "\n") {
		return "", errors.New("can't read how its user signs in, so it isn't kept out. Restore the whole server instead")
	}
	return create, nil
}

// myMarks are the marks of restores' passwords a MySQL or MariaDB user has.
func myMarks(ctx context.Context, r Reader, c *docker.Container, e *Engine, cmd []string, user string) ([]string, error) {
	said, err := r.ask(ctx, c.ID, e, cmd, "SELECT Db FROM mysql.db WHERE User = '"+user+"' AND Host = '%' AND Db LIKE '"+restoringPrefix+"%';\n")
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(said)), "\n") {
		if strings.HasPrefix(line, restoringPrefix) && CheckSQLName("database", line) == nil {
			out = append(out, line)
		}
	}
	return out, nil
}

// fingerprint is a short mark of how a user signs in.
func fingerprint(create string) string {
	sum := sha256.Sum256([]byte(create))
	return hex.EncodeToString(sum[:6])
}
