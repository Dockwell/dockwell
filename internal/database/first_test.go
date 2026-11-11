// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bufio"
	"context"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// fakeMariaDB keeps each user's password hash for each host it signs in from, and
// refuses root's sign-in as MariaDB does, as its tools run in its container: from the
// environment the image reads, or from the first line of what it's given.
type fakeMariaDB struct {
	mu    sync.Mutex
	users map[string]map[string]string // a user's password hash, by host
	// socket names the accounts, user@host, that sign in over the socket too, as
	// root@localhost may. SET PASSWORD takes that away, as a real MariaDB did in CI.
	socket map[string]bool
	said   []string
	// starting refuses this many connections more, as a server does before its socket
	// is there.
	starting int
}

var (
	fakeAlterUser = regexp.MustCompile(`SET PASSWORD FOR '([^']+)'@'([^']+)' = '(\*[0-9A-F]{40})'`)
	fakeViaSocket = regexp.MustCompile(`ALTER USER '([^']+)'@'([^']+)' IDENTIFIED VIA unix_socket OR mysql_native_password USING '(\*[0-9A-F]{40})'`)
	fakeShowUser  = regexp.MustCompile(`SHOW CREATE USER '([^']+)'@'([^']+)'`)
	fakeAskUsers  = regexp.MustCompile(`SELECT User, Host FROM mysql.user WHERE User IN \(([^)]*)\)`)
)

func (f *fakeMariaDB) exec(x dockertest.Exec) int {
	in := bufio.NewReader(x.Stdin)
	password := ""
	if strings.Contains(strings.Join(x.Cmd, " "), "read -r MYSQL_PWD") {
		line, _ := in.ReadString('\n')
		password = strings.TrimSuffix(line, "\n")
	} else {
		for _, kv := range x.Env {
			if v, ok := strings.CutPrefix(kv, "MARIADB_ROOT_PASSWORD="); ok {
				password = v
			}
		}
	}
	rest, _ := io.ReadAll(in)
	sql := string(rest)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starting > 0 {
		f.starting--
		_, _ = io.WriteString(x.Stderr, "ERROR 2002 (HY000): Can't connect to local server through socket '/run/mysqld/mysqld.sock' (2)\n")
		return 1
	}
	f.said = append(f.said, sql)
	if mysqlNative(password) != f.users["root"]["localhost"] {
		_, _ = io.WriteString(x.Stderr, "ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: YES)\n")
		return 1
	}
	if m := fakeAskUsers.FindStringSubmatch(sql); m != nil {
		for user, hosts := range f.users {
			if strings.Contains(m[1], "'"+user+"'") {
				for host := range hosts {
					_, _ = io.WriteString(x.Stdout, user+"\t"+host+"\n")
				}
			}
		}
	}
	for _, m := range fakeShowUser.FindAllStringSubmatch(sql, -1) {
		via := "IDENTIFIED BY PASSWORD '" + f.users[m[1]][m[2]] + "'"
		if f.socket[m[1]+"@"+m[2]] {
			via = "IDENTIFIED VIA mysql_native_password USING '" + f.users[m[1]][m[2]] + "' OR unix_socket"
		}
		_, _ = io.WriteString(x.Stdout, "CREATE USER `"+m[1]+"`@`"+m[2]+"` "+via+"\n")
	}
	for _, m := range fakeAlterUser.FindAllStringSubmatch(sql, -1) {
		if f.users[m[1]] == nil {
			_, _ = io.WriteString(x.Stderr, "ERROR 1133 (28000): Can't find any matching row in the user table\n")
			return 1
		}
		f.users[m[1]][m[2]] = m[3]
		delete(f.socket, m[1]+"@"+m[2])
	}
	for _, m := range fakeViaSocket.FindAllStringSubmatch(sql, -1) {
		f.users[m[1]][m[2]] = m[3]
		f.socket[m[1]+"@"+m[2]] = true
	}
	return 0
}

// signsIn reports whether user signs in from every host it has with password.
func (f *fakeMariaDB) signsIn(user, password string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, hash := range f.users[user] {
		if hash != mysqlNative(password) {
			return false
		}
	}
	return len(f.users[user]) > 0
}

// everywhere is a user's hash of password from the hosts the image makes it at.
func everywhere(password string, hosts ...string) map[string]string {
	out := map[string]string{}
	for _, h := range hosts {
		out[h] = mysqlNative(password)
	}
	return out
}

// mariadbStand runs a MariaDB service's container, started with a root password, on
// a fake Docker Engine.
func mariadbStand(t *testing.T, password string) (*FirstPasswords, *fakeMariaDB, *docker.Client, string) {
	t.Helper()
	db := &fakeMariaDB{users: map[string]map[string]string{
		"root": everywhere(password, "%", "localhost"),
		"wp":   everywhere("app-password", "%"),
	}, socket: map[string]bool{"root@localhost": true}}
	e := dockertest.New(t)
	e.AddLocalImage("mariadb:12", dockertest.Image{Config: docker.Config{Cmd: []string{"mariadbd"}}, Exec: db.exec})
	client := e.Client()
	id := runDB(t, client, "mariadb:12", "MARIADB_ROOT_PASSWORD="+password, "MARIADB_USER=wp", "MARIADB_PASSWORD=app-password")
	return &FirstPasswords{Client: client}, db, client, id
}

func runDB(t *testing.T, client *docker.Client, image string, env ...string) string {
	t.Helper()
	id, err := client.CreateContainer(t.Context(), "env_1-db-1", &docker.CreateRequest{Config: docker.Config{
		Image: image, Env: env, Labels: map[string]string{docker.LabelProject: "env_1", docker.LabelService: "db", docker.LabelNumber: "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func startsWith(image, password string) *resourcev1.Container {
	return &resourcev1.Container{Image: image, Env: map[string]string{
		"MARIADB_ROOT_PASSWORD": password, "POSTGRES_PASSWORD": password, "MARIADB_USER": "wp", "MARIADB_PASSWORD": "app-password",
	}}
}

// A new password reaches root from every host it has, signing in with the one what
// runs started with, and never on a command line.
func TestANewRootPasswordReachesMariaDBBeforeItRestarts(t *testing.T) {
	f, db, client, id := mariadbStand(t, "old-password")
	undo, err := f.Change(t.Context(), "env_1", "db", startsWith("mariadb:12", "new-password"))
	if err != nil {
		t.Fatal(err)
	}
	if undo == nil || !db.signsIn("root", "new-password") {
		t.Fatalf("root doesn't sign in with the new password: %v", db.users)
	}
	// root@localhost still signs in over the socket, and root@'%' never did.
	if !db.socket["root@localhost"] || db.socket["root@%"] {
		t.Fatalf("over the socket, after a new password: %v", db.socket)
	}
	for _, sql := range db.said {
		if strings.Contains(sql, "new-password") || strings.Contains(sql, "old-password") {
			t.Fatalf("SQL holds a password as it is: %q", sql)
		}
	}
	// What runs is still the old container, as when its rollout failed: it goes back,
	// once its database takes connections, as it starts again.
	f.Every = 5 * time.Millisecond
	db.mu.Lock()
	db.starting = 3
	db.mu.Unlock()
	if err := undo(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !db.signsIn("root", "old-password") || !db.socket["root@localhost"] {
		t.Fatalf("root's old password isn't back, or its socket sign-in went: %v %v", db.users, db.socket)
	}
	// What runs is the new container: the new password is its, and stays.
	if _, err := f.Change(t.Context(), "env_1", "db", startsWith("mariadb:12", "new-password")); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveContainer(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	runDB(t, client, "mariadb:12", "MARIADB_ROOT_PASSWORD=new-password", "MARIADB_USER=wp", "MARIADB_PASSWORD=app-password")
	undo, err = f.Change(t.Context(), "env_1", "db", startsWith("mariadb:12", "new-password"))
	if err != nil || undo != nil {
		t.Fatalf("Change for what runs already = %v, %v; want nothing to do", undo != nil, err)
	}
}

// A step that stopped once the password was changed signs in with the new one next
// time, and finishes.
func TestAChangeThatStoppedPartwayFinishes(t *testing.T) {
	f, db, _, _ := mariadbStand(t, "old-password")
	db.users["root"] = everywhere("new-password", "%", "localhost")
	undo, err := f.Change(t.Context(), "env_1", "db", startsWith("mariadb:12", "new-password"))
	if err != nil || undo == nil {
		t.Fatalf("Change = %v, %v", undo != nil, err)
	}
	if !db.signsIn("root", "new-password") {
		t.Fatalf("root's password moved: %v", db.users)
	}
}

// A password neither side holds stops the step before anything restarts, and says so.
func TestAPasswordNeitherSideHoldsStopsTheStep(t *testing.T) {
	f, db, _, _ := mariadbStand(t, "old-password")
	db.users["root"] = everywhere("someone-elses", "%", "localhost")
	_, err := f.Change(t.Context(), "env_1", "db", startsWith("mariadb:12", "new-password"))
	if err == nil || !strings.Contains(err.Error(), "so nothing was restarted") || !strings.Contains(err.Error(), "refused") ||
		!strings.HasSuffix(err.Error(), "Set the secret to the password root has now, then change it here") {
		t.Fatalf("Change = %v", err)
	}
}

// A database that never takes connections again leaves the password where it is once
// its time is up, and says so.
func TestPuttingAPasswordBackGivesUpWithItsTime(t *testing.T) {
	f, db, _, _ := mariadbStand(t, "old-password")
	undo, err := f.Change(t.Context(), "env_1", "db", startsWith("mariadb:12", "new-password"))
	if err != nil || undo == nil {
		t.Fatalf("Change = %v, %v", undo != nil, err)
	}
	f.Every = 5 * time.Millisecond
	db.mu.Lock()
	db.starting = 1 << 30
	db.mu.Unlock()
	// Its time is long enough for a first try to finish on a loaded machine, which
	// a deadline that cut it would leave saying only that a call ran long.
	const given = 2 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), given)
	defer cancel()
	began := time.Now()
	err = undo(ctx)
	// It says what it waited on, or, when a try was still out as its time ran out,
	// that its time did, and never a time that wasn't its limit.
	if err == nil || !strings.Contains(err.Error(), "MariaDB isn't taking connections yet") && !strings.Contains(err.Error(), "MariaDB didn't answer in the time it had") {
		t.Fatalf("undo of a database that never starts = %v", err)
	}
	if took := time.Since(began); took > given+time.Second {
		t.Fatalf("undo gave up after %s, with %s to put the password back", took, given)
	}
}

// Nothing to do: the same password, nothing running, an engine with no first user to
// change, or an image Dockwell doesn't know.
func TestFirstPasswordsLeavesAloneWhatItNeedNotChange(t *testing.T) {
	f, db, _, _ := mariadbStand(t, "same-password")
	for _, app := range []*resourcev1.Container{
		startsWith("mariadb:12", "same-password"),
		startsWith("redis:8", "new-password"),
		startsWith("example/mariadb-fork:1", "new-password"),
		// Its image, as a client of another server: there's no database here to change.
		{Image: "mariadb:12", Command: []string{"mariadb-dump", "-h", "other", "--all-databases"}, Env: map[string]string{"MARIADB_ROOT_PASSWORD": "new-password"}},
	} {
		undo, err := f.Change(t.Context(), "env_1", "db", app)
		if err != nil || undo != nil {
			t.Fatalf("Change(%s) = %v, %v", app.GetImage(), undo != nil, err)
		}
	}
	undo, err := f.Change(t.Context(), "env_1", "other", startsWith("mariadb:12", "new-password"))
	if err != nil || undo != nil {
		t.Fatalf("Change with nothing running = %v, %v", undo != nil, err)
	}
	if len(db.said) != 0 {
		t.Fatalf("it asked MariaDB %q", db.said)
	}
}

// PostgreSQL gets a SCRAM verifier for its first user, POSTGRES_USER where it's set.
func TestANewPasswordReachesPostgreSQLsFirstUser(t *testing.T) {
	pg := &fakePostgres{roles: map[string]bool{"app": true}, databases: map[string]string{}}
	e := dockertest.New(t)
	e.AddLocalImage("postgres:17", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: pg.exec})
	client := e.Client()
	runDB(t, client, "postgres:17", "POSTGRES_PASSWORD=old-password", "POSTGRES_USER=app")
	f := &FirstPasswords{Client: client}
	app := startsWith("postgres:17", "new-password")
	app.Env["POSTGRES_USER"] = "app"
	undo, err := f.Change(context.Background(), "env_1", "db", app)
	if err != nil || undo == nil {
		t.Fatalf("Change = %v, %v", undo != nil, err)
	}
	if len(pg.said) != 1 || !strings.HasPrefix(pg.said[0], `ALTER ROLE "app" PASSWORD 'SCRAM-SHA-256$4096:`) || strings.Contains(pg.said[0], "new-password") {
		t.Fatalf("PostgreSQL was asked %q", pg.said)
	}
}

// The image's second user's password follows its secret too, signing in as root, whose
// own stays as it is; a user the new container names otherwise isn't the image's.
func TestTheImagesSecondUserFollowsItsSecret(t *testing.T) {
	f, db, _, _ := mariadbStand(t, "root-password")
	app := startsWith("mariadb:12", "root-password")
	app.Env["MARIADB_PASSWORD"] = "new-app-password"
	undo, err := f.Change(t.Context(), "env_1", "db", app)
	if err != nil || undo == nil {
		t.Fatalf("Change = %v, %v", undo != nil, err)
	}
	if !db.signsIn("wp", "new-app-password") || !db.signsIn("root", "root-password") {
		t.Fatalf("after the change, MariaDB has %v", db.users)
	}
	renamed := startsWith("mariadb:12", "root-password")
	renamed.Env["MARIADB_USER"], renamed.Env["MARIADB_PASSWORD"] = "blog", "another"
	if undo, err := f.Change(t.Context(), "env_1", "db", renamed); err != nil || undo != nil {
		t.Fatalf("Change for a renamed user = %v, %v; want nothing to do", undo != nil, err)
	}
}

// The SQL names each host a user has, and refuses one it can't name safely.
func TestAlterUsersNamesEachHost(t *testing.T) {
	mysql := Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL)
	moves := []passwordMove{{user: "root", variable: "MYSQL_ROOT_PASSWORD", to: "p'w"}}
	sql, err := mysql.alterUsers(map[string][]string{"root": {"%", "localhost"}}, moves, nil)
	if err != nil || !strings.Contains(sql, "SET SESSION sql_mode = '';") ||
		!strings.Contains(sql, `ALTER USER 'root'@'%' IDENTIFIED BY 'p\'w';`) || !strings.Contains(sql, "'root'@'localhost'") {
		t.Fatalf("alterUsers = %q, %v", sql, err)
	}
	if _, err := mysql.alterUsers(map[string][]string{"root": {"evil' OR 1"}}, moves, nil); err == nil {
		t.Fatal("alterUsers took a host it can't quote")
	}
	if _, err := mysql.alterUsers(nil, moves, nil); err == nil {
		t.Fatal("alterUsers took a user with no hosts")
	}
	// MariaDB keeps an account's socket sign-in, and sets the rest by its hash.
	maria := Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB)
	sql, err = maria.alterUsers(map[string][]string{"root": {"%", "localhost"}}, moves, map[string]bool{"root@localhost": true})
	hash := mysqlNative("p'w")
	if err != nil || !strings.Contains(sql, "ALTER USER 'root'@'localhost' IDENTIFIED VIA unix_socket OR mysql_native_password USING '"+hash+"';") ||
		!strings.Contains(sql, "SET PASSWORD FOR 'root'@'%' = '"+hash+"';") || strings.Contains(sql, "p'w") {
		t.Fatalf("MariaDB's alterUsers = %q, %v", sql, err)
	}
}
