// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// On a real PostgreSQL, MySQL and MariaDB, a user and database made by hand, as an
// imported app signs in as, are taken on only with the password they sign in with
// from another host, where the server asks for one, never with a wrong one, which
// the image's own container would let in; a user with every right is refused; and
// letting go of the one taken on leaves it signing in as before (ADR 0084, Connect
// a database).
func TestADatabaseUserIsTakenOnOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	for _, server := range []struct {
		image, env string
		// admin runs SQL as the server's administrator, in its container.
		admin []string
		// setup makes the user blog with its database, and boss with every right;
		// the users in past reach past blog another way, and helper signs in with no
		// rights in blog. PostgreSQL makes a database outside a transaction, so each
		// part runs on its own.
		setup []string
		// past are the users that reach past blog without being superusers.
		past []string
		// patterns says the server reads a schema grant's _ as any one character: under
		// has my_app written so, which matches myxapp too, and escaped has it written
		// my\_app, which is its own alone.
		patterns bool
		// public is a user with rights in blog and only what PUBLIC has elsewhere,
		// which is taken on.
		public string
	}{
		{
			image: "postgres:17-alpine", env: "POSTGRES_PASSWORD=the-administrators",
			// On its input, so a part may \connect to another database.
			admin: []string{"sh", "-c", `printf '%s\n' "$1" | exec psql -X -q -v ON_ERROR_STOP=1 -U postgres -d postgres`, "sh"},
			setup: []string{
				"CREATE ROLE blog LOGIN PASSWORD 'the-blog-password'; CREATE ROLE boss LOGIN SUPERUSER PASSWORD 'the-bosss-password';" +
					" CREATE ROLE member LOGIN PASSWORD 'the-other-password'; GRANT postgres TO member; CREATE ROLE reader LOGIN PASSWORD 'the-other-password'; GRANT pg_read_all_data TO reader;" +
					" CREATE ROLE helper LOGIN PASSWORD 'the-other-password'; CREATE ROLE shop NOLOGIN;" +
					" CREATE ROLE othermember LOGIN PASSWORD 'the-other-password'; GRANT shop TO othermember; CREATE ROLE twodb LOGIN PASSWORD 'the-other-password';" +
					" CREATE ROLE maint LOGIN PASSWORD 'the-other-password'; GRANT pg_maintain TO maint; CREATE ROLE signal LOGIN PASSWORD 'the-other-password'; GRANT pg_signal_backend TO signal;" +
					" CREATE ROLE tablegrant LOGIN PASSWORD 'the-other-password'; CREATE ROLE publiconly LOGIN PASSWORD 'the-other-password';",
				"CREATE DATABASE blog OWNER blog",
				"CREATE DATABASE shop OWNER shop",
				"CREATE DATABASE blog_b OWNER twodb",
				"GRANT CREATE ON DATABASE blog TO othermember, twodb, maint, signal, tablegrant, publiconly",
				// shop's own table and function: tablegrant may read the table, and
				// publiconly has only what PUBLIC has, the function's EXECUTE among it.
				"\\connect shop\nSET ROLE shop; CREATE TABLE customers (id int); CREATE FUNCTION count_them() RETURNS bigint LANGUAGE sql AS 'SELECT count(*) FROM customers'; GRANT SELECT ON customers TO tablegrant;",
			},
			// othermember may act as another database's owner, twodb owns another, and
			// tablegrant may read a table in another.
			past:   []string{"member", "reader", "othermember", "twodb", "maint", "signal", "tablegrant"},
			public: "publiconly",
		},
		{
			image: "mysql:8.4", env: "MYSQL_ROOT_PASSWORD=the-administrators",
			admin: []string{"sh", "-c", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot -e "$1"`, "sh"},
			setup: []string{"CREATE DATABASE blog; CREATE USER 'blog'@'%' IDENTIFIED BY 'the-blog-password'; GRANT ALL ON blog.* TO 'blog'@'%'; CREATE USER 'boss'@'%' IDENTIFIED BY 'the-bosss-password'; GRANT ALL PRIVILEGES ON *.* TO 'boss'@'%' WITH GRANT OPTION;" +
				" CREATE USER 'wide'@'%' IDENTIFIED BY 'the-other-password'; GRANT SELECT ON *.* TO 'wide'@'%'; CREATE ROLE 'everything'; GRANT ALL ON *.* TO 'everything';" +
				" CREATE USER 'roled'@'%' IDENTIFIED BY 'the-other-password'; GRANT 'everything' TO 'roled'@'%'; SET DEFAULT ROLE ALL TO 'roled'@'%'; CREATE USER 'helper'@'%' IDENTIFIED BY 'the-other-password';" +
				" CREATE USER 'dyn'@'%' IDENTIFIED BY 'the-other-password'; GRANT SYSTEM_VARIABLES_ADMIN ON *.* TO 'dyn'@'%';" +
				" CREATE USER 'granter'@'%' IDENTIFIED BY 'the-other-password'; GRANT USAGE ON *.* TO 'granter'@'%' WITH GRANT OPTION;" +
				" CREATE USER 'maker'@'%' IDENTIFIED BY 'the-other-password'; GRANT CREATE USER ON *.* TO 'maker'@'%';" +
				" CREATE USER 'sysdb'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON blog.* TO 'sysdb'@'%'; GRANT SELECT ON mysql.* TO 'sysdb'@'%';" +
				" CREATE DATABASE shop; CREATE TABLE shop.customers (id INT); CREATE PROCEDURE shop.count_them() SELECT COUNT(*) FROM shop.customers;" +
				" CREATE USER 'colgrant'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON blog.* TO 'colgrant'@'%'; GRANT SELECT (id) ON shop.customers TO 'colgrant'@'%';" +
				" CREATE USER 'procs'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON blog.* TO 'procs'@'%'; GRANT EXECUTE ON PROCEDURE shop.count_them TO 'procs'@'%';" +
				" CREATE USER 'proxy'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON blog.* TO 'proxy'@'%'; GRANT PROXY ON 'boss'@'%' TO 'proxy'@'%';" +
				" CREATE DATABASE my_app; CREATE DATABASE myxapp; CREATE USER 'under'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON my_app.* TO 'under'@'%';" +
				" CREATE USER 'escaped'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON `my\\_app`.* TO 'escaped'@'%';"},
			past:     []string{"wide", "roled", "dyn", "granter", "maker", "sysdb", "colgrant", "procs", "proxy"},
			patterns: true,
		},
		// MySQL 5.7, which has no roles: imported servers still run it.
		{
			image: "mysql:5.7", env: "MYSQL_ROOT_PASSWORD=the-administrators",
			admin: []string{"sh", "-c", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot -e "$1"`, "sh"},
			setup: []string{"CREATE DATABASE blog; CREATE USER 'blog'@'%' IDENTIFIED BY 'the-blog-password'; GRANT ALL ON blog.* TO 'blog'@'%'; CREATE USER 'boss'@'%' IDENTIFIED BY 'the-bosss-password'; GRANT ALL PRIVILEGES ON *.* TO 'boss'@'%' WITH GRANT OPTION;" +
				" CREATE USER 'wide'@'%' IDENTIFIED BY 'the-other-password'; GRANT SELECT ON *.* TO 'wide'@'%';" +
				" CREATE USER 'helper'@'%' IDENTIFIED BY 'the-other-password';" +
				" CREATE USER 'granter'@'%' IDENTIFIED BY 'the-other-password'; GRANT USAGE ON *.* TO 'granter'@'%' WITH GRANT OPTION;" +
				" CREATE USER 'maker'@'%' IDENTIFIED BY 'the-other-password'; GRANT CREATE USER ON *.* TO 'maker'@'%';" +
				" CREATE USER 'sysdb'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON blog.* TO 'sysdb'@'%'; GRANT SELECT ON mysql.* TO 'sysdb'@'%';" +
				" CREATE DATABASE shop; CREATE TABLE shop.customers (id INT); CREATE PROCEDURE shop.count_them() SELECT COUNT(*) FROM shop.customers;" +
				" CREATE USER 'colgrant'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON blog.* TO 'colgrant'@'%'; GRANT SELECT (id) ON shop.customers TO 'colgrant'@'%';" +
				" CREATE USER 'procs'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON blog.* TO 'procs'@'%'; GRANT EXECUTE ON PROCEDURE shop.count_them TO 'procs'@'%';" +
				" CREATE USER 'proxy'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON blog.* TO 'proxy'@'%'; GRANT PROXY ON 'boss'@'%' TO 'proxy'@'%';" +
				" CREATE DATABASE my_app; CREATE DATABASE myxapp; CREATE USER 'under'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON my_app.* TO 'under'@'%';" +
				" CREATE USER 'escaped'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON `my\\_app`.* TO 'escaped'@'%';"},
			past:     []string{"wide", "granter", "maker", "sysdb", "colgrant", "procs", "proxy"},
			patterns: true,
		},
		{
			image: "mariadb:11.4", env: "MARIADB_ROOT_PASSWORD=the-administrators",
			admin: []string{"sh", "-c", `MYSQL_PWD="$MARIADB_ROOT_PASSWORD" exec mariadb -uroot -e "$1"`, "sh"},
			setup: []string{"CREATE DATABASE blog; CREATE USER 'blog'@'%' IDENTIFIED BY 'the-blog-password'; GRANT ALL ON blog.* TO 'blog'@'%'; CREATE USER 'boss'@'%' IDENTIFIED BY 'the-bosss-password'; GRANT ALL PRIVILEGES ON *.* TO 'boss'@'%' WITH GRANT OPTION;" +
				" CREATE USER 'wide'@'%' IDENTIFIED BY 'the-other-password'; GRANT SELECT ON *.* TO 'wide'@'%'; CREATE ROLE everything; GRANT ALL ON *.* TO everything;" +
				" CREATE USER 'roled'@'%' IDENTIFIED BY 'the-other-password'; GRANT everything TO 'roled'@'%'; SET DEFAULT ROLE everything FOR 'roled'@'%'; CREATE USER 'helper'@'%' IDENTIFIED BY 'the-other-password';" +
				" CREATE USER 'granter'@'%' IDENTIFIED BY 'the-other-password'; GRANT USAGE ON *.* TO 'granter'@'%' WITH GRANT OPTION;" +
				" CREATE USER 'maker'@'%' IDENTIFIED BY 'the-other-password'; GRANT CREATE USER ON *.* TO 'maker'@'%';" +
				" CREATE USER 'sysdb'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON blog.* TO 'sysdb'@'%'; GRANT SELECT ON mysql.* TO 'sysdb'@'%';" +
				" CREATE DATABASE shop; CREATE TABLE shop.customers (id INT); CREATE PROCEDURE shop.count_them() SELECT COUNT(*) FROM shop.customers;" +
				" CREATE USER 'colgrant'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON blog.* TO 'colgrant'@'%'; GRANT SELECT (id) ON shop.customers TO 'colgrant'@'%';" +
				" CREATE USER 'procs'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON blog.* TO 'procs'@'%'; GRANT EXECUTE ON PROCEDURE shop.count_them TO 'procs'@'%';" +
				" CREATE USER 'proxy'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON blog.* TO 'proxy'@'%'; GRANT PROXY ON 'boss'@'%' TO 'proxy'@'%';" +
				" CREATE DATABASE my_app; CREATE DATABASE myxapp; CREATE USER 'under'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON my_app.* TO 'under'@'%';" +
				" CREATE USER 'escaped'@'%' IDENTIFIED BY 'the-other-password'; GRANT ALL ON `my\\_app`.* TO 'escaped'@'%';"},
			past:     []string{"wide", "roled", "granter", "maker", "sysdb", "colgrant", "procs", "proxy"},
			patterns: true,
		},
	} {
		t.Run(server.image, func(t *testing.T) {
			env := dockertest.Scope(t)
			project := docker.Project(env)
			dockertest.Cleanup(t, client, project)
			if err := client.PullImage(t.Context(), server.image, nil); err != nil {
				t.Fatal(err)
			}
			id, err := client.CreateContainer(t.Context(), docker.ContainerName(project, "db"), &docker.CreateRequest{Config: docker.Config{
				Image: server.image, Env: []string{server.env},
				Labels: map[string]string{docker.LabelProject: project, docker.LabelService: "db", docker.LabelNumber: "1"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := client.StartContainer(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			// The image's first start ends with its setup server stopping, so the
			// setup waits until the server takes it.
			run := func(sql string) (string, bool) {
				var out, errs bytes.Buffer
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				code, err := client.Exec(ctx, id, append(append([]string(nil), server.admin...), sql), docker.Streams{Stdout: &out, Stderr: &errs})
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(errs.String() + out.String()), code == 0
			}
			var said string
			made := false
			for range 120 {
				if said, made = run(server.setup[0]); made {
					break
				}
				time.Sleep(time.Second)
			}
			if !made {
				t.Fatalf("making the users by hand: %s", said)
			}
			for _, sql := range server.setup[1:] {
				if said, ok := run(sql); !ok {
					t.Fatalf("making the users by hand, %s: %s", sql, said)
				}
			}

			// The sheet's check, as each is chosen: signs in, or not, and whether it's the
			// server's administrator, with nothing of the password in what it says.
			checkIn := func(db, user, password string) *agentv1.CheckDatabaseSignInResult {
				t.Helper()
				got, err := Reader{Client: client}.CheckSignIn(t.Context(), &agentv1.CheckDatabaseSignInCall{Environment: env, Service: "db", User: user, Database: db}, password)
				if err != nil {
					t.Fatalf("checking %s: %v", user, err)
				}
				return got
			}
			check := func(user, password string) *agentv1.CheckDatabaseSignInResult {
				t.Helper()
				return checkIn("blog", user, password)
			}
			if got := check("blog", "the-blog-password"); !got.GetSignsIn() || got.GetAdministrator() {
				t.Errorf("blog with its password checks as %v", got)
			}
			if got := check("blog", "not-its-password"); got.GetSignsIn() || strings.Contains(got.GetSaid(), "not-its-password") {
				t.Errorf("blog with a wrong password checks as %v", got)
			}
			if got := check("boss", "the-bosss-password"); !got.GetAdministrator() {
				t.Errorf("a user with every right checks as %v", got)
			}
			// Past its database another way than SUPER or a superuser's flag, as the
			// review found: each reads as the administrator, and isn't taken on.
			for _, user := range server.past {
				if got := check(user, "the-other-password"); !got.GetAdministrator() {
					t.Errorf("%s, which reaches every database, checks as %v", user, got)
				}
			}
			if got := check("helper", "the-other-password"); got.GetAdministrator() || !got.GetNoRights() {
				t.Errorf("helper, with no rights in blog, checks as %v", got)
			}
			if server.public != "" {
				if got := check(server.public, "the-other-password"); !got.GetSignsIn() || got.GetAdministrator() || got.GetNoRights() {
					t.Errorf("%s, with only what PUBLIC has past blog, checks as %v", server.public, got)
				}
			}
			if server.patterns {
				if got := checkIn("my_app", "under", "the-other-password"); !got.GetAdministrator() {
					t.Errorf("under, whose my_app matches myxapp too, checks as %v", got)
				}
				if got := checkIn("my_app", "escaped", "the-other-password"); !got.GetSignsIn() || got.GetAdministrator() || got.GetNoRights() {
					t.Errorf("escaped, with my\\_app alone, checks as %v", got)
				}
			}

			d := &UserDriver{Client: client, Dir: t.TempDir(), Wait: 2 * time.Minute, Every: time.Second}
			takeOn := func(name, password string) error {
				step := resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: env, Name: name}, Action: resource.ActionCreate}
				ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: password})
				return d.Apply(ctx, step, &resourcev1.DatabaseUser{Service: "db", DatabaseName: "blog", Password: "BLOG_PASSWORD", TakenOn: true})
			}
			if err := takeOn("blog", "not-its-password"); err == nil || !strings.Contains(err.Error(), "doesn't sign in") {
				t.Errorf("taking on blog with a wrong password = %v", err)
			}
			if err := takeOn("boss", "the-bosss-password"); err == nil || !strings.Contains(err.Error(), "administrator") {
				t.Errorf("taking on a user with every right = %v", err)
			}
			for _, user := range server.past {
				if err := takeOn(user, "the-other-password"); err == nil || !strings.Contains(err.Error(), "reaches past its database blog") {
					t.Errorf("taking on %s, which reaches every database = %v", user, err)
				}
			}
			if err := takeOn("helper", "the-other-password"); err == nil || !strings.Contains(err.Error(), "has no rights in blog") {
				t.Errorf("taking on helper, with no rights in blog = %v", err)
			}
			if err := takeOn("blog", "the-blog-password"); err != nil {
				t.Fatalf("taking on blog with its password: %v", err)
			}
			ref := resource.Ref{Kind: UserKind, Scope: env, Name: "blog"}
			got, err := d.Observe(t.Context(), ref)
			if u, ok := got.(*resourcev1.DatabaseUser); err != nil || !ok || !u.GetTakenOn() {
				t.Fatalf("taken on, blog reads %v, %v", got, err)
			}
			if err := d.Verify(t.Context(), ref, got); err != nil {
				t.Errorf("taken on, blog checks as %v", err)
			}
			if err := d.Apply(t.Context(), resource.Step{Ref: ref, Action: resource.ActionDelete}, nil); err != nil {
				t.Fatal(err)
			}
			// Let go of, it still signs in with its password, from another host.
			c, err := client.InspectContainer(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			ok, said, err := Loader{Client: client}.SignsIn(t.Context(), Load{To: c, Engine: Recognise(server.image), User: "blog", Password: "the-blog-password", Database: "blog"})
			if err != nil || !ok {
				t.Errorf("let go of, blog no longer signs in: %v, %s", err, said)
			}
		})
	}
}
