// SPDX-License-Identifier: AGPL-3.0-only

package onepanel

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	_ "modernc.org/sqlite" // SQLite writes the records a test reads, as 1Panel's GORM does
)

// The tables as GORM makes them for 1Panel, the columns this reads and the secret
// ones beside them, from 1Panel's models at v2.4.2. v1.10's have the same names.
var schema = []string{
	"CREATE TABLE `settings` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`key` text NOT NULL,`value` text,`about` text)",
	"CREATE TABLE `apps` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`name` text NOT NULL,`key` text NOT NULL,`type` text NOT NULL,`status` text NOT NULL,`resource` text NOT NULL DEFAULT \"remote\")",
	"CREATE TABLE `app_installs` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`name` text NOT NULL UNIQUE,`app_id` integer NOT NULL,`app_detail_id` integer NOT NULL,`version` text NOT NULL,`param` text,`env` text,`docker_compose` text,`status` text NOT NULL,`description` text,`message` text,`container_name` text NOT NULL,`service_name` text NOT NULL,`http_port` integer,`https_port` integer,`web_ui` text)",
	"CREATE TABLE `runtimes` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`name` text NOT NULL,`app_detail_id` integer,`image` text,`work_dir` text,`docker_compose` text,`env` text,`params` text,`version` text NOT NULL,`type` text NOT NULL,`status` text NOT NULL,`resource` text NOT NULL,`port` text,`message` text,`code_dir` text,`container_name` text)",
	"CREATE TABLE `websites` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`protocol` text NOT NULL,`primary_domain` text NOT NULL,`type` text NOT NULL,`alias` text NOT NULL,`remark` text,`status` text NOT NULL,`http_config` text NOT NULL,`expire_date` datetime,`proxy` text,`proxy_type` text,`site_dir` text,`error_log` numeric,`access_log` numeric,`default_server` numeric,`rewrite` text,`website_group_id` integer,`website_ssl_id` integer,`runtime_id` integer,`app_install_id` integer,`ftp_id` integer,`parent_website_id` integer,`user` text,`group` text,`db_type` text,`db_id` integer)",
	"CREATE TABLE `website_domains` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`website_id` integer NOT NULL,`domain` text NOT NULL,`ssl` numeric,`port` integer)",
	"CREATE TABLE `website_ssls` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`primary_domain` text,`private_key` text,`pem` text,`domains` text,`cert_url` text,`type` text,`provider` text,`organization` text,`auto_renew` numeric,`expire_date` datetime,`start_date` datetime,`status` text,`shell` text)",
	"CREATE TABLE `databases` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`app_install_id` integer,`name` text NOT NULL UNIQUE,`type` text NOT NULL,`version` text NOT NULL,`from` text NOT NULL,`address` text NOT NULL,`port` integer NOT NULL,`username` text,`password` text,`client_key` text)",
	"CREATE TABLE `database_mysqls` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`name` text NOT NULL,`from` text NOT NULL DEFAULT \"local\",`mysql_name` text NOT NULL,`format` text NOT NULL,`collation` text,`username` text NOT NULL,`password` text NOT NULL,`permission` text NOT NULL,`is_delete` numeric,`description` text)",
	"CREATE TABLE `database_postgresqls` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`name` text NOT NULL,`from` text NOT NULL DEFAULT \"local\",`postgresql_name` text NOT NULL,`format` text NOT NULL,`username` text NOT NULL,`password` text NOT NULL,`super_user` numeric,`is_delete` numeric,`description` text)",
	"CREATE TABLE `cronjobs` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`name` text NOT NULL,`type` text NOT NULL,`spec_custom` numeric,`spec` text NOT NULL,`executor` text,`command` text,`container_name` text,`script_mode` text,`script` text,`user` text,`website` text,`app_id` text,`db_type` text,`db_name` text,`url` text,`source_dir` text,`status` text,`secret` text)",
}

// secrets are planted in every column that holds one, and none may come back.
var secrets = []string{"s3cret-db", "s3cret-env", "s3cret-script", "s3cret-key", "s3cret-token", "s3cret-proxy", "s3cret-compose", "s3cret-cmd"}

var data = []string{
	`INSERT INTO settings (key, value) VALUES ('SystemVersion', 'v2.4.2'), ('EncryptKey', 's3cret-key'), ('WEBSITE_DIR', '/data/www')`,
	`INSERT INTO apps (id, name, key, type, status, resource) VALUES (1, 'MySQL', 'mysql', 'runtime', 'Ready', 'remote'), (2, 'OpenResty', 'openresty', 'website', 'Ready', 'remote'), (3, 'WordPress', 'wordpress', 'website', 'Ready', 'remote'), (4, 'Mine', 'localmine', 'tool', 'Ready', 'local')`,
	`INSERT INTO app_installs (id, name, app_id, app_detail_id, version, param, env, docker_compose, status, container_name, service_name, http_port, https_port) VALUES
		(1, 'mysql', 1, 1, '8.4.3', '{"PANEL_DB_ROOT_PASSWORD":"s3cret-env"}', '{"PANEL_DB_ROOT_PASSWORD":"s3cret-env"}', 'image: mysql # s3cret-compose', 'Running', '1Panel-mysql-AbCd', 'mysql', 3306, 0),
		(2, 'openresty', 2, 2, '1.27.1.2-5-1-focal', '{}', '{}', '', 'Running', '1Panel-openresty-EfGh', 'openresty', 80, 443),
		(3, 'wordpress', 3, 3, '6.7.1', '{}', '{"PANEL_DB_USER_PASSWORD":"s3cret-env"}', '', 'Stopped', '1Panel-wordpress-IjKl', 'wordpress', 40080, 0),
		(4, 'mine', 4, 4, '1.0', '{}', '{}', '', 'Running', 'mine', 'mine', 0, 0)`,
	`INSERT INTO runtimes (id, name, image, env, version, type, status, resource, code_dir, container_name) VALUES
		(1, 'php82', '1panel/php:8.2', 'PASS=s3cret-env', '8.2.27', 'php', 'running', 'appstore', '', 'php82'),
		(2, 'api', 'node:22', '', '22.11.0', 'node', 'running', 'appstore', '/srv/api', 'api')`,
	`INSERT INTO website_ssls (id, primary_domain, private_key, pem, domains, type, provider, auto_renew, expire_date) VALUES
		(1, 'blog.example.com', '-----BEGIN PRIVATE KEY----- s3cret-key', 'CERT', 'www.blog.example.com,static.example.com', 'autoSSL', 'letsencrypt', 1, '2027-01-02 03:04:05.123456789+00:00')`,
	`INSERT INTO websites (id, protocol, primary_domain, type, alias, status, http_config, proxy, site_dir, rewrite, website_ssl_id, runtime_id, app_install_id, parent_website_id) VALUES
		(1, 'HTTPS', 'blog.example.com', 'runtime', 'blog', 'Running', '', '127.0.0.1:9000', '/public', 'location / { try_files $uri /index.php; }', 1, 1, 0, 0),
		(2, 'HTTP', 'static.example.com', 'static', 'static', 'Running', '', '', '/', '', 0, 0, 0, 0),
		(3, 'HTTP', 'app.example.com', 'proxy', 'app', 'Stopped', '', 'http://admin:s3cret-proxy@10.0.0.7:8080/path?token=s3cret-token', '/', '', 0, 0, 0, 0),
		(4, 'HTTP', 'wp.example.com', 'deployment', 'wp', 'Running', '', '', '/', '', 0, 0, 3, 0),
		(5, 'HTTP', 'docs.blog.example.com', 'subsite', 'docs', 'Running', '', '', '/', '', 0, 0, 0, 1)`,
	`INSERT INTO website_domains (website_id, domain, ssl, port) VALUES (1, 'blog.example.com', 1, 443), (1, 'www.blog.example.com', 1, 443), (2, 'static.example.com', 0, 80), (3, 'app.example.com', 0, 80), (4, 'wp.example.com', 0, 80), (5, 'docs.blog.example.com', 0, 80)`,
	`INSERT INTO databases (id, app_install_id, name, type, version, "from", address, port, username, password, client_key) VALUES
		(1, 1, 'mysql', 'mysql', '8.4.3', 'local', '1Panel-mysql-AbCd', 3306, 'root', 's3cret-db', ''),
		(2, 0, 'reports', 'postgresql', '17', 'remote', '203.0.113.20', 5432, 'admin', 's3cret-db', 's3cret-key')`,
	`INSERT INTO database_mysqls (name, mysql_name, format, username, password, permission, is_delete) VALUES ('wp', 'mysql', 'utf8mb4', 'wp_user', 's3cret-db', '%', 0), ('gone', 'mysql', 'utf8mb4', 'old', 's3cret-db', '%', 1)`,
	`INSERT INTO database_postgresqls (name, postgresql_name, format, username, password, is_delete) VALUES ('stats', 'reports', 'UTF8', 'stats_user', 's3cret-db', 0)`,
	`INSERT INTO cronjobs (name, type, spec, executor, command, container_name, script, url, website, app_id, db_name, source_dir, status, secret) VALUES
		('nightly', 'shell', '0 3 * * *', 'bash', 'echo s3cret-cmd', 'mine', 'export PASS=s3cret-script
mysqldump --password=$PASS > /backup.sql
', '', '', '', '', '', 'Enable', 's3cret-key'),
		('ping', 'curl', '*/5 * * * *', '', '', '', '', 'https://hooks.example.com/ping?token=s3cret-token', '', '', '', '', 'Enable', ''),
		('site backup', 'website', '0 4 * * *', '', '', '', '', '', 'blog', '', '', '', 'Enable', ''),
		('app backup', 'app', '0 5 * * 0', '', '', '', '', '', '', '1,3', '', '', 'Disable', ''),
		('db backup', 'database', '0 6 * * *', '', '', '', '', '', '', '', 'wp', '', 'Enable', ''),
		('dir backup', 'directory', '0 7 * * *', '', '', '', '', '', '', '', '', '/srv/files', 'Enable', ''),
		('time', 'ntp', '0 0 * * *', '', '', '', '', '', '', '', '', '', 'Enable', '')`,
}

// a1Panel makes a server's root with 1Panel installed under base, its records
// written as 1Panel writes them, in WAL mode and still open, so some are in the
// WAL. v1 names its database 1Panel.db.
func a1Panel(t *testing.T, base string, v1 bool) string {
	t.Helper()
	root := t.TempDir()
	if base != "/opt" {
		writeFile(t, filepath.Join(root, ctl), "#!/bin/bash\nBASE_DIR="+base+"\nORIGINAL_PORT=12345\n")
	}
	dir := filepath.Join(root, base, "1panel", "db")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "agent.db"
	if v1 {
		name = "1Panel.db"
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, name)+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range append(append([]string(nil), schema...), data...) {
		if v1 {
			q = strings.ReplaceAll(q, "('SystemVersion', 'v2.4.2')", "('SystemVersion', 'v1.10.34-lts')")
		}
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		t.Fatal(err)
	}
	// Written after the checkpoint, so it's in the WAL alone.
	if _, err := db.Exec(`UPDATE app_installs SET status = 'Running' WHERE name = 'wordpress'`); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readOK(t *testing.T, root string) *dockwellv1.OnePanel {
	t.Helper()
	res, err := Read(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if res.GetFound() == nil {
		t.Fatalf("found nothing: %s", res.GetNotHere())
	}
	return res.GetFound()
}

// Everything a plan shows is read, from v2's records and the WAL beside them.
func TestReadsWhat1PanelKeeps(t *testing.T) {
	found := readOK(t, a1Panel(t, "/opt", false))
	if found.GetVersion() != "v2.4.2" || !found.GetKnownVersion() || found.GetBaseDir() != "/opt" || found.GetRecords() != "/opt/1panel/db/agent.db" {
		t.Fatalf("found %v", found)
	}
	apps := map[string]*dockwellv1.OnePanelApp{}
	for _, a := range found.GetApps() {
		apps[a.GetName()] = a
	}
	if wp := apps["wordpress"]; wp.GetStatus() != "Running" || wp.GetAppKey() != "wordpress" || wp.GetFolder() != "/opt/1panel/apps/wordpress/wordpress" || wp.GetHttpPort() != 40080 {
		t.Errorf("wordpress = %v", wp)
	}
	if mine := apps["mine"]; mine.GetFolder() != "/opt/1panel/apps/local/mine/mine" || mine.GetStore() != "local" {
		t.Errorf("a local app = %v", mine)
	}
	sites := map[string]*dockwellv1.OnePanelWebsite{}
	for _, w := range found.GetWebsites() {
		sites[w.GetAlias()] = w
	}
	blog := sites["blog"]
	if blog.GetFolder() != "/data/www/sites/blog/index/public" || blog.GetRuntime() != "php82" || !blog.GetRewrite() || !blog.GetHttps() ||
		blog.GetCertificate() != "blog.example.com" || len(blog.GetDomains()) != 2 {
		t.Errorf("blog = %v", blog)
	}
	if app := sites["app"]; app.GetProxy() != "http://10.0.0.7:8080" {
		t.Errorf("a proxy's target = %q", app.GetProxy())
	}
	if wp := sites["wp"]; wp.GetApp() != "wordpress" {
		t.Errorf("an app's site = %v", wp)
	}
	if docs := sites["docs"]; docs.GetParent() != "blog" {
		t.Errorf("a subsite = %v", docs)
	}
	if c := found.GetCertificates(); len(c) != 1 || c[0].GetKind() != "autoSSL" || len(c[0].GetDomains()) != 2 || c[0].GetExpires().AsTime().Year() != 2027 {
		t.Errorf("certificates = %v", c)
	}
	if s := found.GetDatabaseServers(); len(s) != 2 || !s[0].GetLocal() || s[0].GetApp() != "mysql" || s[1].GetLocal() {
		t.Errorf("database servers = %v", s)
	}
	if d := found.GetDatabases(); len(d) != 2 || d[0].GetName() != "wp" || d[0].GetUser() != "wp_user" || d[1].GetType() != "postgresql" {
		t.Errorf("databases = %v", d)
	}
	jobs := map[string]*dockwellv1.OnePanelCronJob{}
	for _, j := range found.GetCronJobs() {
		jobs[j.GetName()] = j
	}
	if j := jobs["nightly"]; j.GetScriptLines() != 2 || j.GetContainer() != "mine" || j.GetSchedule() != "0 3 * * *" {
		t.Errorf("a shell job = %v", j)
	}
	if j := jobs["ping"]; j.GetUrlHost() != "hooks.example.com" {
		t.Errorf("a URL job = %v", j)
	}
	if j := jobs["app backup"]; j.GetTarget() != "mysql, wordpress" || j.GetStatus() != "Disable" {
		t.Errorf("an app backup = %v", j)
	}
	if j := jobs["dir backup"]; j.GetTarget() != "/srv/files" {
		t.Errorf("a folder backup = %v", j)
	}
}

// Nothing secret comes back: no password, key, environment, script, command,
// Compose file, or a URL's user, path or query.
func TestNothingSecretComesBack(t *testing.T) {
	found := readOK(t, a1Panel(t, "/opt", false))
	text := prototext.Format(found)
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("%s came back:\n%s", s, text)
		}
	}
}

// 1pctl names where 1Panel is when it's not under /opt, and v1 keeps one database,
// with its sites under OpenResty's own folder.
func TestReadsV1UnderAnotherBase(t *testing.T) {
	found := readOK(t, a1Panel(t, "/data", true))
	if found.GetVersion() != "v1.10.34-lts" || !found.GetKnownVersion() || found.GetRecords() != "/data/1panel/db/1Panel.db" {
		t.Fatalf("found %v", found)
	}
	for _, w := range found.GetWebsites() {
		if w.GetAlias() == "static" && w.GetFolder() != "/data/1panel/apps/openresty/openresty/www/sites/static/index" {
			t.Errorf("v1's static site = %v", w)
		}
	}
}

// A version this wasn't written from is read the same way, and says so.
func TestAnotherVersionSaysSo(t *testing.T) {
	root := a1Panel(t, "/opt", false)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, "opt", "1panel", "db", "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE settings SET value = 'v2.5.0' WHERE key = 'SystemVersion'`); err != nil {
		t.Fatal(err)
	}
	if found := readOK(t, root); found.GetKnownVersion() || found.GetVersion() != "v2.5.0" || len(found.GetApps()) != 4 {
		t.Errorf("found %v", found)
	}
}

// Where 1Panel isn't installed, it says so, and where its records aren't a
// database, that's an error with where.
func TestNo1PanelSaysSo(t *testing.T) {
	res, err := Read(t.Context(), t.TempDir())
	if err != nil || res.GetFound() != nil || !strings.Contains(res.GetNotHere(), "/opt/1panel/db/agent.db") {
		t.Errorf("= %v, %v", res, err)
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "opt", "1panel", "db", "agent.db"), "not a database")
	if _, err := Read(t.Context(), root); err == nil || !strings.Contains(err.Error(), "/opt/1panel/db/agent.db") {
		t.Errorf("records that aren't a database: %v", err)
	}
}

func TestOnlyTheOriginOfAProxy(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:8080":                     "http://127.0.0.1:8080",
		"https://user:pass@example.com/a?token=x#y": "https://example.com",
		"127.0.0.1:9000":                            "",
		"":                                          "",
	} {
		if got := origin(in); got != want {
			t.Errorf("origin(%q) = %q, want %q", in, got, want)
		}
	}
}
