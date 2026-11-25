// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// LogicalKind is a logical database inside a PostgreSQL, MySQL or MariaDB
// server a service runs, owned by an existing user there.
const LogicalKind resource.Kind = "database.logical"

// LogicalDriver makes, reassigns and drops logical databases inside a database
// server's own container with its own tools. Unlike UserDriver, deleting a
// logical database drops only the database and keeps its owner user intact.
type LogicalDriver struct {
	Client *docker.Client
	// Dir keeps what each logical database was given, alongside database-users.json.
	Dir string
	// Wait and Every mirror UserDriver's startup wait for a newly started server.
	Wait, Every time.Duration
	mu          sync.Mutex
}

var (
	_ resource.Driver   = (*LogicalDriver)(nil)
	_ resource.Referrer = (*LogicalDriver)(nil)
	_ resource.Judge    = (*LogicalDriver)(nil)
)

// Kind returns "database.logical".
func (*LogicalDriver) Kind() resource.Kind { return LogicalKind }

// Level is the environment, as its service's.
func (*LogicalDriver) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "dbl".
func (*LogicalDriver) IDPrefix() string { return "dbl" }

// New returns an empty LogicalDatabase.
func (*LogicalDriver) New() proto.Message { return &resourcev1.LogicalDatabase{} }

// LogicalName is the SQL database name a LogicalDatabase resource manages.
func LogicalName(name string, spec *resourcev1.LogicalDatabase) string {
	return cmp.Or(spec.GetDatabaseName(), name)
}

// CheckName accepts a logical database resource name: either a plain SQL
// database name or a service-scoped resource name like "<service>-<db>" (up to
// 63 lowercase letters, digits, underscores and hyphens, starting with a
// letter). The actual SQL database name is checked by Validate and Judge.
func (*LogicalDriver) CheckName(name string) error {
	if name == "" || len(name) > 63 {
		return fmt.Errorf("%q isn't a valid database resource name: use 1 to 63 lowercase letters, digits, underscores and hyphens, starting with a letter", name)
	}
	for i := range len(name) {
		c := name[i]
		letter := c >= 'a' && c <= 'z'
		digit := c >= '0' && c <= '9'
		if i == 0 && !letter {
			return fmt.Errorf("%q isn't a valid database resource name: start with a lowercase letter", name)
		}
		if !letter && !digit && c != '_' && c != '-' {
			return fmt.Errorf("%q isn't a valid database resource name: use lowercase letters, digits, underscores and hyphens", name)
		}
	}
	if SystemDatabase(name) {
		return fmt.Errorf("%s is a database its server keeps for itself. Choose another name", name)
	}
	return nil
}

// Validate checks service, database name, and optional owner name.
func (*LogicalDriver) Validate(spec proto.Message) error {
	s := spec.(*resourcev1.LogicalDatabase)
	if err := resource.CheckLabel("service", s.GetService(), "db"); err != nil {
		return fmt.Errorf("a logical database needs the service that runs its database server: %w", err)
	}
	if db := s.GetDatabaseName(); db != "" {
		if err := CheckSQLName("database", db); err != nil {
			return err
		}
		if SystemDatabase(db) {
			return fmt.Errorf("%s is a database its server keeps for itself. Choose another name", db)
		}
	}
	if owner := s.GetOwner(); owner != "" {
		if err := CheckSQLName("owner", owner); err != nil {
			return err
		}
	}
	return nil
}

// References names its database service, which must run where it is made and
// goes before it if both are deleted together so its volume stays intact.
func (*LogicalDriver) References(spec proto.Message) []resource.Reference {
	return []resource.Reference{{Kind: "service", Name: spec.(*resourcev1.LogicalDatabase).GetService(), Verb: "make it in", Apart: true, Inside: true}}
}

// ReferenceKinds is services.
func (*LogicalDriver) ReferenceKinds() []resource.Kind { return []resource.Kind{"service"} }

// Plan judges a step without the server.
func (*LogicalDriver) Plan(actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	switch {
	case actual == nil:
		return resource.Decision{Action: resource.ActionCreate}, nil
	case desired == nil:
		return resource.Decision{Action: resource.ActionDelete, Destroys: true}, nil
	}
	for _, c := range changes {
		if c.Path == "service" || c.Path == "database_name" {
			return resource.Decision{}, errors.New("a database's server and name can't change. Make another database, then delete this one")
		}
	}
	return resource.Decision{Action: resource.ActionUpdate}, nil
}

// Judge checks the server on create, refusing a database that already exists
// or an owner user that doesn't exist on the server.
func (d *LogicalDriver) Judge(ctx context.Context, ref resource.Ref, actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	dec, err := d.Plan(actual, desired, changes)
	if err != nil || dec.Action != resource.ActionCreate {
		return dec, err
	}
	s := desired.(*resourcev1.LogicalDatabase)
	db := LogicalName(ref.Name, s)
	if err := CheckSQLName("database", db); err != nil {
		return resource.Decision{}, err
	}
	if SystemDatabase(db) {
		return resource.Decision{}, fmt.Errorf("%s is a database its server keeps for itself. Choose another name", db)
	}
	if owner := s.GetOwner(); owner != "" {
		if err := CheckSQLName("owner", owner); err != nil {
			return resource.Decision{}, err
		}
	}
	if _, ok, err := d.recorded(ref); err != nil || ok {
		return dec, err
	}
	c, e, err := d.server(ctx, ref, s.GetService())
	var gone noContainer
	switch {
	case errors.As(err, &gone) && madeHere(ctx, ref.Scope, s.GetService()):
		return dec, nil
	case err != nil:
		return resource.Decision{}, err
	}
	exists, ownerExists, _, err := d.state(ctx, c, e, db, s.GetOwner())
	if err != nil {
		return resource.Decision{}, err
	}
	if exists {
		return resource.Decision{}, fmt.Errorf("%s already has a database called %s. Choose another name", s.GetService(), db)
	}
	if s.GetOwner() != "" && !ownerExists {
		return resource.Decision{}, fmt.Errorf("%s has no user %s to own %s. Pick an existing user, or make a new one", s.GetService(), s.GetOwner(), db)
	}
	if hasDeletedBackup(d.Dir, time.Now(), ref.Scope, s.GetService(), db) {
		dec.Restores = true
	}
	return dec, nil
}

type logicalGiven struct {
	Service  string `json:"service"`
	Database string `json:"database,omitempty"`
	Owner    string `json:"owner,omitempty"`
}

func (d *LogicalDriver) recordPath() string { return filepath.Join(d.Dir, "database-logical.json") }

func (d *LogicalDriver) records() (map[string]logicalGiven, error) {
	out := map[string]logicalGiven{}
	b, err := os.ReadFile(d.recordPath())
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("can't read %s: %w", d.recordPath(), err)
	}
	return out, nil
}

func (d *LogicalDriver) recorded(ref resource.Ref) (logicalGiven, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	all, err := d.records()
	if err != nil {
		return logicalGiven{}, false, err
	}
	g, ok := all[ref.String()]
	return g, ok, nil
}

func (d *LogicalDriver) record(ref resource.Ref, g *logicalGiven) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	all, err := d.records()
	if err != nil {
		return err
	}
	if g == nil {
		delete(all, ref.String())
	} else {
		all[ref.String()] = *g
	}
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return err
	}
	tmp := d.recordPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.recordPath())
}

func (d *LogicalDriver) server(ctx context.Context, ref resource.Ref, svc string) (*docker.Container, *Engine, error) {
	u := &UserDriver{Client: d.Client}
	c, e, err := u.server(ctx, ref, svc)
	if err != nil {
		return nil, nil, err
	}
	if !e.SQL() {
		return nil, nil, fmt.Errorf("%s runs %s, which doesn't support logical databases here", svc, e.Name)
	}
	return c, e, nil
}

func (d *LogicalDriver) whileStarting(ctx context.Context, f func() error) error {
	return (&UserDriver{Wait: d.Wait, Every: d.Every}).whileStarting(ctx, f)
}

func (d *LogicalDriver) run(ctx context.Context, c *docker.Container, e *Engine, sql string) error {
	cmd, _ := e.reading(PortOf(e, c.Config.Cmd))
	_, err := Reader{Client: d.Client}.ask(ctx, c.ID, e, cmd, sql)
	return err
}

// state inspects whether db exists, whether owner exists (when non-empty), and
// the current owner on PostgreSQL.
func (d *LogicalDriver) state(ctx context.Context, c *docker.Container, e *Engine, db, owner string) (exists, ownerExists bool, pgOwner string, err error) {
	if err := CheckSQLName("database", db); err != nil {
		return false, false, "", err
	}
	if owner != "" {
		if err := CheckSQLName("owner", owner); err != nil {
			return false, false, "", err
		}
	}
	cmd, _ := e.reading(PortOf(e, c.Config.Cmd))
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		ownerCheck := "true"
		if owner != "" {
			ownerCheck = fmt.Sprintf("EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%s')", owner)
		}
		sql := fmt.Sprintf(
			`SELECT json_build_object('exists', EXISTS (SELECT 1 FROM pg_database WHERE datname = '%s'), 'owner_exists', %s, 'owner', coalesce((SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = '%s'), ''));`,
			db, ownerCheck, db,
		)
		said, err := Reader{Client: d.Client}.ask(ctx, c.ID, e, cmd, sql)
		if err != nil {
			return false, false, "", err
		}
		var out struct {
			Exists      bool   `json:"exists"`
			OwnerExists bool   `json:"owner_exists"`
			Owner       string `json:"owner"`
		}
		if err := json.Unmarshal(said, &out); err != nil {
			return false, false, "", fmt.Errorf("can't read what %s said: %w", e.Name, err)
		}
		return out.Exists, out.OwnerExists, out.Owner, nil
	}
	var sql strings.Builder
	fmt.Fprintf(&sql, "SELECT 'd', COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = '%s';\n", db)
	if owner != "" {
		fmt.Fprintf(&sql, "SELECT 'u', COUNT(*) FROM mysql.user WHERE User = '%s';\n", owner)
	}
	said, err := Reader{Client: d.Client}.ask(ctx, c.ID, e, cmd, sql.String())
	if err != nil {
		return false, false, "", err
	}
	ownerExists = owner == ""
	for _, line := range strings.Split(strings.TrimSpace(string(said)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) == 2 && fields[1] != "0" {
			switch fields[0] {
			case "d":
				exists = true
			case "u":
				ownerExists = true
			}
		}
	}
	return exists, ownerExists, "", nil
}

// mysqlHosts lists the Host entries for an existing MySQL/MariaDB user.
func (d *LogicalDriver) mysqlHosts(ctx context.Context, c *docker.Container, e *Engine, user string) ([]string, error) {
	if err := CheckSQLName("owner", user); err != nil {
		return nil, err
	}
	cmd, _ := e.reading(PortOf(e, c.Config.Cmd))
	said, err := Reader{Client: d.Client}.ask(ctx, c.ID, e, cmd, fmt.Sprintf("SELECT Host FROM mysql.user WHERE User = '%s' ORDER BY Host;\n", user))
	if err != nil {
		return nil, err
	}
	var hosts []string
	for _, line := range strings.Split(strings.TrimSpace(string(said)), "\n") {
		h := strings.TrimSpace(line)
		if h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts, nil
}

// Observe returns the recorded spec while the logical database still exists.
func (d *LogicalDriver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	g, ok, err := d.recorded(ref)
	if err != nil || !ok {
		return nil, err
	}
	out := &resourcev1.LogicalDatabase{Service: g.Service, DatabaseName: g.Database, Owner: g.Owner}
	c, e, err := d.server(ctx, ref, g.Service)
	if err != nil {
		return out, nil //nolint:nilerr // a stopped server reads as it was left
	}
	db := LogicalName(ref.Name, out)
	exists, _, pgOwner, err := d.state(ctx, c, e, db, "")
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL && g.Owner != "" && pgOwner != "" && pgOwner != g.Owner {
		out.Owner = pgOwner
	}
	return out, nil
}

// Apply creates, updates the owner of, or drops a logical database.
func (d *LogicalDriver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	ref := step.Ref
	if desired == nil {
		return d.drop(ctx, ref)
	}
	s := desired.(*resourcev1.LogicalDatabase)
	db := LogicalName(ref.Name, s)
	if err := CheckSQLName("database", db); err != nil {
		return err
	}
	if SystemDatabase(db) {
		return fmt.Errorf("%s is a database its server keeps for itself", db)
	}
	owner := s.GetOwner()
	if owner != "" {
		if err := CheckSQLName("owner", owner); err != nil {
			return err
		}
	}
	var c *docker.Container
	var e *Engine
	if err := d.whileStarting(ctx, func() error {
		var err error
		c, e, err = d.server(ctx, ref, s.GetService())
		return err
	}); err != nil {
		return err
	}
	g := &logicalGiven{Service: s.GetService(), Database: s.GetDatabaseName(), Owner: owner}
	_, alreadyRecorded, err := d.recorded(ref)
	if err != nil {
		return err
	}
	var sql string
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		if step.Action == resource.ActionCreate {
			createStmt := "CREATE DATABASE " + pgName(db)
			if owner != "" {
				createStmt += " OWNER " + pgName(owner)
			}
			if alreadyRecorded {
				// Retry of an earlier create that already recorded ownership.
				sql = "SELECT '" + createStmt + "' WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = '" + db + "')\n\\gexec\n"
			} else {
				// Fail if another database with this name appeared between Judge and Apply
				// so Dockwell never silently takes over a database it didn't make.
				sql = createStmt + ";\n"
			}
			sql += "REVOKE ALL ON DATABASE " + pgName(db) + " FROM PUBLIC;\n"
			if owner != "" && owner != "postgres" {
				sql += pgOwnPublic(owner, db) + pgOldPublic
			}
		} else if owner != "" {
			sql = "ALTER DATABASE " + pgName(db) + " OWNER TO " + pgName(owner) + ";\n"
			if owner != "postgres" {
				sql += pgOwnPublic(owner, db)
			}
		}
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		var b strings.Builder
		if step.Action == resource.ActionCreate {
			if alreadyRecorded {
				b.WriteString("CREATE DATABASE IF NOT EXISTS " + myName(db) + ";\n")
			} else {
				b.WriteString("CREATE DATABASE " + myName(db) + ";\n")
			}
		}
		if owner != "" && owner != "root" {
			var hosts []string
			if err := d.whileStarting(ctx, func() error {
				var err error
				hosts, err = d.mysqlHosts(ctx, c, e, owner)
				return err
			}); err != nil {
				return err
			}
			if len(hosts) == 0 {
				return fmt.Errorf("%s has no user %s to grant on %s", s.GetService(), owner, db)
			}
			for _, host := range hosts {
				fmt.Fprintf(&b, "GRANT ALL PRIVILEGES ON %s.* TO '%s'@'%s';\n", myName(myGranted(db)), owner, mysqlQuoted(host))
			}
		}
		sql = b.String()
	default:
		return fmt.Errorf("%s doesn't support logical databases here", e.Name)
	}
	if sql != "" {
		if err := d.whileStarting(ctx, func() error { return d.run(ctx, c, e, sql) }); err != nil {
			if step.Action == resource.ActionCreate && !alreadyRecorded && isDBExistsError(err) {
				return fmt.Errorf("%s already has a database called %s, which Dockwell didn't make, so nothing was changed. Choose another name", s.GetService(), db)
			}
			return err
		}
	}
	if step.Action == resource.ActionCreate && (step.Restores || hasDeletedBackup(d.Dir, time.Now(), ref.Scope, s.GetService(), db)) {
		if err := restoreDeletedBackup(ctx, d.Client, d.Dir, time.Now(), c, e, ref.Scope, s.GetService(), db, owner, ""); err != nil {
			// Undo failed to restore the kept backup: drop the empty database we just made.
			if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
				_ = d.run(context.WithoutCancel(ctx), c, e, "DROP DATABASE IF EXISTS "+pgName(db)+";\n")
			} else {
				_ = d.run(context.WithoutCancel(ctx), c, e, "DROP DATABASE IF EXISTS "+myName(db)+";\n")
			}
			return err
		}
	}
	return d.record(ref, g)
}

func isDBExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "database exists")
}

func (d *LogicalDriver) drop(ctx context.Context, ref resource.Ref) error {
	g, ok, err := d.recorded(ref)
	if err != nil || !ok {
		return err
	}
	db := LogicalName(ref.Name, &resourcev1.LogicalDatabase{DatabaseName: g.Database})
	if err := CheckSQLName("database", db); err != nil {
		return err
	}
	if SystemDatabase(db) {
		return fmt.Errorf("%s is a database its server keeps for itself, so it won't be dropped", db)
	}
	c, e, err := d.server(ctx, ref, g.Service)
	var gone noContainer
	switch {
	case errors.As(err, &gone):
		return d.record(ref, nil)
	case err != nil:
		return err
	}
	// Back up the database before dropping so Undo restores its data; refuse if backup fails.
	if err := backupBeforeDrop(ctx, d.Client, d.Dir, time.Now(), c, e, ref.Scope, g.Service, db, g.Owner); err != nil {
		return err
	}
	var sql string
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		sql = "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '" + db + "' AND pid <> pg_backend_pid();\n" +
			"DROP DATABASE IF EXISTS " + pgName(db) + ";\n"
	} else {
		sql = "DROP DATABASE IF EXISTS " + myName(db) + ";\n"
	}
	if err := d.run(ctx, c, e, sql); err != nil {
		return err
	}
	return d.record(ref, nil)
}

// Verify checks the logical database exists on the server with its expected owner.
func (d *LogicalDriver) Verify(ctx context.Context, ref resource.Ref, desired proto.Message) error {
	s := desired.(*resourcev1.LogicalDatabase)
	c, e, err := d.server(ctx, ref, s.GetService())
	if err != nil {
		return err
	}
	db := LogicalName(ref.Name, s)
	exists, _, pgOwner, err := d.state(ctx, c, e, db, "")
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s has no database %s after it was made", s.GetService(), db)
	}
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL && s.GetOwner() != "" && pgOwner != s.GetOwner() {
		return fmt.Errorf("the database %s is owned by %s, not %s", db, pgOwner, s.GetOwner())
	}
	return nil
}
