// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // MariaDB's own password hash, which is what it compares
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/variable"
)

// UserKind is a database and the user who owns it, in a database server a service
// runs (ADR 0038).
const UserKind resource.Kind = "database.user"

// passwordUse is what a user's spec calls the secret it uses.
const passwordUse = "PASSWORD"

// UserDriver makes, changes and drops a database and its user, in the database
// server's own container, with its own tools. On the control plane, where it only
// reads and checks specs, it needs no client.
type UserDriver struct {
	Client *docker.Client
	// Dir keeps what each user was given, which nothing in a database says: its
	// service, database and secret, and that secret's revision.
	Dir string
	mu  sync.Mutex
}

var (
	_ resource.Driver   = (*UserDriver)(nil)
	_ resource.Binder   = (*UserDriver)(nil)
	_ resource.Referrer = (*UserDriver)(nil)
	_ resource.Judge    = (*UserDriver)(nil)
)

// Kind returns "database.user".
func (*UserDriver) Kind() resource.Kind { return UserKind }

// Level is the environment, as its service's.
func (*UserDriver) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "dbu".
func (*UserDriver) IDPrefix() string { return "dbu" }

// New returns an empty DatabaseUser.
func (*UserDriver) New() proto.Message { return &resourcev1.DatabaseUser{} }

// sqlName is a user's or a database's name: what every engine takes without quoting.
var sqlName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// CheckName takes a name PostgreSQL, MySQL and MariaDB all take as it is.
func (*UserDriver) CheckName(name string) error { return CheckSQLName("user", name) }

// CheckSQLName says whether name is one a user or a database can take.
func CheckSQLName(what, name string) error {
	if !sqlName.MatchString(name) {
		return fmt.Errorf("%q isn't a valid %s name: use up to 32 lowercase letters, digits and underscores, starting with a letter, such as shop", name, what)
	}
	return nil
}

// Validate requires a service and a secret, and a database name any engine takes.
func (*UserDriver) Validate(spec proto.Message) error {
	u := spec.(*resourcev1.DatabaseUser)
	if err := resource.CheckLabel("service", u.GetService(), "db"); err != nil {
		return fmt.Errorf("a database user needs the service that runs its database server: %w", err)
	}
	if u.GetDatabaseName() != "" {
		if err := CheckSQLName("database", u.GetDatabaseName()); err != nil {
			return err
		}
	}
	if err := variable.CheckName("secret", u.GetPassword()); err != nil {
		return fmt.Errorf("a database user needs the secret its password is in: %w", err)
	}
	return nil
}

// Uses is the secret its password is in.
func (*UserDriver) Uses(spec proto.Message) []resource.Use {
	u := spec.(*resourcev1.DatabaseUser)
	return []resource.Use{{As: passwordUse, Kind: variable.SecretKind, Name: u.GetPassword(), Revision: int(u.GetRevisions()[passwordUse])}}
}

// Pin sets the revision of the secret it uses.
func (*UserDriver) Pin(spec proto.Message, revisions map[string]int) {
	u := spec.(*resourcev1.DatabaseUser)
	u.Revisions = nil
	if r, ok := revisions[passwordUse]; ok {
		u.Revisions = map[string]int64{passwordUse: int64(r)}
	}
}

// References names its service, which has to run where it's made.
func (*UserDriver) References(spec proto.Message) []resource.Reference {
	return []resource.Reference{{Kind: "service", Name: spec.(*resourcev1.DatabaseUser).GetService(), Verb: "make it in", Apart: true}}
}

// ReferenceKinds is services.
func (*UserDriver) ReferenceKinds() []resource.Kind { return []resource.Kind{"service"} }

// Owned is the database a user owns: its spec's, or the user's own name.
func Owned(name string, u *resourcev1.DatabaseUser) string {
	if u.GetDatabaseName() != "" {
		return u.GetDatabaseName()
	}
	return name
}

// Plan judges a step without the server: a user is made, given a new password, or
// dropped with its database, which destroys what's in it.
func (*UserDriver) Plan(actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	switch {
	case actual == nil:
		return resource.Decision{Action: resource.ActionCreate}, nil
	case desired == nil:
		return resource.Decision{Action: resource.ActionDelete, Destroys: true}, nil
	}
	for _, c := range changes {
		if c.Path == "service" || c.Path == "database_name" {
			return resource.Decision{}, errors.New("a database user's service and database can't change. Make another user, then delete this one")
		}
	}
	return resource.Decision{Action: resource.ActionUpdate}, nil
}

// Judge plans, and for a new user, refuses one that's there already, which Dockwell
// didn't make.
func (d *UserDriver) Judge(ctx context.Context, ref resource.Ref, actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	dec, err := d.Plan(actual, desired, changes)
	if err != nil || dec.Action != resource.ActionCreate {
		return dec, err
	}
	u := desired.(*resourcev1.DatabaseUser)
	c, e, err := d.server(ctx, ref, u.GetService())
	if err != nil {
		return resource.Decision{}, err
	}
	there, err := d.there(ctx, c, e, ref.Name, Owned(ref.Name, u))
	if err != nil {
		return resource.Decision{}, err
	}
	switch {
	case there.user:
		return resource.Decision{}, fmt.Errorf("%s has a user %s already, which Dockwell didn't make. Choose another name", u.GetService(), ref.Name)
	case there.owner != "":
		return resource.Decision{}, fmt.Errorf("%s has a database %s already, which Dockwell didn't make. Choose another name", u.GetService(), Owned(ref.Name, u))
	}
	return dec, nil
}

// server finds the running container of a database server, and its engine.
func (d *UserDriver) server(ctx context.Context, ref resource.Ref, service string) (*docker.Container, *Engine, error) {
	if d.Client == nil {
		return nil, nil, errors.New("this agent can't reach Docker")
	}
	c, err := d.Client.ContainerOf(ctx, docker.Project(ref.Scope), service, 1)
	switch {
	case docker.IsNotFound(err):
		return nil, nil, fmt.Errorf("%s's container isn't on this server. Deploy it first", service)
	case err != nil:
		return nil, nil, err
	case !c.State.Running || c.State.Restarting:
		return nil, nil, fmt.Errorf("%s isn't running, and a database user is made in it. Start it, then try again", service)
	}
	e := Recognise(c.Config.Image)
	if e == nil || !e.SQL() {
		return nil, nil, fmt.Errorf("%s doesn't run PostgreSQL, MySQL or MariaDB from its official image, so Dockwell can't make users in it", service)
	}
	return c, e, nil
}

// found is what a database server has of a user and its database.
type found struct {
	user  bool
	owner string // the database's owner, or "" with no such database
}

// there reads whether a database server has a user and a database.
func (d *UserDriver) there(ctx context.Context, c *docker.Container, e *Engine, user, db string) (found, error) {
	var sql string
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		sql = fmt.Sprintf(`SELECT json_build_object('user', EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%s'), 'owner', coalesce((SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = '%s'), ''));`, user, db)
	} else {
		sql = fmt.Sprintf("SELECT 'u', COUNT(*) FROM mysql.user WHERE User = '%s';\nSELECT 'd', COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = '%s';\n", user, db)
	}
	cmd, _ := e.reading(PortOf(e, c.Config.Cmd))
	said, err := Reader{Client: d.Client}.ask(ctx, c.ID, e, cmd, sql)
	if err != nil {
		return found{}, err
	}
	var f found
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		var out struct {
			User  bool   `json:"user"`
			Owner string `json:"owner"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(said))), &out); err != nil {
			return found{}, fmt.Errorf("can't read what %s said: %w", e.Name, err)
		}
		return found{user: out.User, owner: out.Owner}, nil
	}
	for _, line := range strings.Split(strings.TrimSpace(string(said)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) == 2 && fields[1] != "0" {
			switch fields[0] {
			case "u":
				f.user = true
			case "d":
				f.owner = user
			}
		}
	}
	return f, nil
}

// given is what a user was given, which the agent keeps beside its other state.
type given struct {
	Service  string `json:"service"`
	Database string `json:"database"`
	// Uses names the secret the password is in, never the password.
	Uses     string `json:"uses"`
	Revision int64  `json:"revision"`
}

func (d *UserDriver) recordPath() string { return filepath.Join(d.Dir, "database-users.json") }

func (d *UserDriver) records() (map[string]given, error) {
	out := map[string]given{}
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

func (d *UserDriver) record(ref resource.Ref, g *given) error {
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

func (d *UserDriver) recorded(ref resource.Ref) (given, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	all, err := d.records()
	if err != nil {
		return given{}, false, err
	}
	g, ok := all[ref.String()]
	return g, ok, nil
}

// Observe reads a user as what it was given, while its database server still has
// it. One dropped by hand reads as gone.
func (d *UserDriver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	g, ok, err := d.recorded(ref)
	if err != nil || !ok {
		return nil, err
	}
	out := &resourcev1.DatabaseUser{Service: g.Service, DatabaseName: g.Database, Password: g.Uses}
	if g.Revision > 0 {
		out.Revisions = map[string]int64{passwordUse: g.Revision}
	}
	c, e, err := d.server(ctx, ref, g.Service)
	if err != nil {
		// What can't be read is as it was given: a server that's stopped hasn't lost it.
		return out, nil //nolint:nilerr // a stopped server reads as it was left
	}
	f, err := d.there(ctx, c, e, ref.Name, Owned(ref.Name, out))
	if err != nil {
		return nil, err
	}
	if !f.user {
		return nil, nil
	}
	return out, nil
}

// Apply makes a user and its database, gives it its password anew, or drops both.
func (d *UserDriver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	ref := step.Ref
	if desired == nil {
		g, ok, err := d.recorded(ref)
		if err != nil || !ok {
			return err
		}
		c, e, err := d.server(ctx, ref, g.Service)
		if err != nil {
			return err
		}
		if err := d.run(ctx, c, e, e.drop(ref.Name, Owned(ref.Name, &resourcev1.DatabaseUser{DatabaseName: g.Database}))); err != nil {
			return err
		}
		return d.record(ref, nil)
	}
	u := desired.(*resourcev1.DatabaseUser)
	password := resource.ValuesFrom(ctx)[passwordUse]
	if password == "" {
		return fmt.Errorf("no password came for %s from its secret %s. Update this server's agent to the control plane's version", ref.Name, u.GetPassword())
	}
	c, e, err := d.server(ctx, ref, u.GetService())
	if err != nil {
		return err
	}
	sql, err := e.make(ref.Name, Owned(ref.Name, u), password, step.Action == resource.ActionCreate)
	if err != nil {
		return err
	}
	if err := d.run(ctx, c, e, sql); err != nil {
		return err
	}
	return d.record(ref, &given{Service: u.GetService(), Database: u.GetDatabaseName(), Uses: u.GetPassword(), Revision: u.GetRevisions()[passwordUse]})
}

// run runs SQL as the database server's first user, as the image signs in.
func (d *UserDriver) run(ctx context.Context, c *docker.Container, e *Engine, sql string) error {
	cmd, _ := e.reading(PortOf(e, c.Config.Cmd))
	_, err := Reader{Client: d.Client}.ask(ctx, c.ID, e, cmd, sql)
	return err
}

// Verify checks the database server has the user, and that it owns its database.
func (d *UserDriver) Verify(ctx context.Context, ref resource.Ref, desired proto.Message) error {
	u := desired.(*resourcev1.DatabaseUser)
	c, e, err := d.server(ctx, ref, u.GetService())
	if err != nil {
		return err
	}
	db := Owned(ref.Name, u)
	f, err := d.there(ctx, c, e, ref.Name, db)
	switch {
	case err != nil:
		return err
	case !f.user:
		return fmt.Errorf("%s has no user %s after it was made", u.GetService(), ref.Name)
	case f.owner == "":
		return fmt.Errorf("%s has no database %s after it was made", u.GetService(), db)
	case e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL && f.owner != ref.Name:
		return fmt.Errorf("the database %s is %s's, not %s's", db, f.owner, ref.Name)
	}
	return nil
}

// make is the SQL that makes a user and its database, or gives a user its password
// anew. A password never goes in as it is where a server could log it: PostgreSQL
// gets a SCRAM verifier and MariaDB its own hash, and MySQL leaves a password out of
// every log it keeps.
func (e *Engine) make(user, db, password string, create bool) (string, error) {
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		verifier, err := scram(password)
		if err != nil {
			return "", err
		}
		role := "ROLE " + pgName(user) + " LOGIN PASSWORD '" + verifier + "';\n"
		if !create {
			return "ALTER " + role, nil
		}
		return "CREATE " + role + "CREATE DATABASE " + pgName(db) + " OWNER " + pgName(user) + ";\n", nil
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		identified := "IDENTIFIED BY PASSWORD '" + mysqlNative(password) + "'"
		if !create {
			return "ALTER USER " + myUser(user) + " " + identified + ";\n", nil
		}
		return myCreate(user, db, identified), nil
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL:
		// With no sql_mode, a backslash escapes, as quoted below.
		identified := "IDENTIFIED BY '" + mysqlQuoted(password) + "'"
		if !create {
			return "SET SESSION sql_mode = '';\nALTER USER " + myUser(user) + " " + identified + ";\n", nil
		}
		return "SET SESSION sql_mode = '';\n" + myCreate(user, db, identified), nil
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED, dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS,
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY:
	}
	return "", fmt.Errorf("%s has no users of its own", e.Name)
}

// myCreate makes a MySQL or MariaDB user who can do anything in a new database.
func myCreate(user, db, identified string) string {
	return "CREATE DATABASE " + myName(db) + ";\nCREATE USER " + myUser(user) + " " + identified + ";\nGRANT ALL PRIVILEGES ON " + myName(db) + ".* TO " + myUser(user) + ";\n"
}

// drop is the SQL that drops a user and its database, with everything in it.
func (e *Engine) drop(user, db string) string {
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		return "DROP DATABASE IF EXISTS " + pgName(db) + " WITH (FORCE);\nDROP ROLE IF EXISTS " + pgName(user) + ";\n"
	}
	return "DROP DATABASE IF EXISTS " + myName(db) + ";\nDROP USER IF EXISTS " + myUser(user) + ";\n"
}

// pgName, myName and myUser quote a name CheckSQLName took, which holds nothing a
// quote has to escape.
func pgName(name string) string { return `"` + name + `"` }
func myName(name string) string { return "`" + name + "`" }
func myUser(name string) string { return "'" + name + "'@'%'" }

// scram is PostgreSQL's SCRAM-SHA-256 verifier for a password, as its own clients
// make one (RFC 7677), so the server stores and logs no password.
func scram(password string) (string, error) {
	const iterations = 4096
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	client := mac(salted, "Client Key")
	stored := sha256.Sum256(client)
	server := mac(salted, "Server Key")
	enc := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, enc(salt), enc(stored[:]), enc(server)), nil
}

// mysqlNative is mysql_native_password's hash of a password, as MariaDB keeps it.
func mysqlNative(password string) string {
	first := sha1.Sum([]byte(password)) //nolint:gosec // MariaDB's own hash
	second := sha1.Sum(first[:])        //nolint:gosec // MariaDB's own hash
	return "*" + strings.ToUpper(hex.EncodeToString(second[:]))
}

// mysqlQuoted escapes a password for a single-quoted string, with no sql_mode.
func mysqlQuoted(password string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\x00", `\0`).Replace(password)
}
