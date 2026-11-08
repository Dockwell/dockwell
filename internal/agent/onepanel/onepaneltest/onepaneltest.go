// SPDX-License-Identifier: AGPL-3.0-only

// Package onepaneltest makes a server's root with 1Panel installed on it, its
// records written by SQLite as 1Panel's GORM writes them, for tests of the importer
// (ADR 0056). Nothing in it is a real server's: its domains are example.com's and
// its addresses documentation ranges.
package onepaneltest

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // SQLite writes the records, as 1Panel's GORM does
)

// Ctl is 1Panel's own command, which names the folder it's installed under.
const Ctl = "/usr/local/bin/1pctl"

// The tables as GORM makes them for 1Panel, the columns this reads and the secret
// ones beside them, from 1Panel's models at v2.4.2. v1.10's have the same names.
var schema = []string{
	"CREATE TABLE `settings` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`key` text NOT NULL,`value` text,`about` text)",
	"CREATE TABLE `apps` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`name` text NOT NULL,`key` text NOT NULL,`type` text NOT NULL,`status` text NOT NULL,`resource` text NOT NULL DEFAULT \"remote\")",
	"CREATE TABLE `app_installs` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`name` text NOT NULL UNIQUE,`app_id` integer NOT NULL,`app_detail_id` integer NOT NULL,`version` text NOT NULL,`param` text,`env` text,`docker_compose` text,`status` text NOT NULL,`description` text,`message` text,`container_name` text NOT NULL,`service_name` text NOT NULL,`http_port` integer,`https_port` integer,`web_ui` text)",
	"CREATE TABLE `runtimes` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`name` text NOT NULL,`app_detail_id` integer,`image` text,`work_dir` text,`docker_compose` text,`env` text,`params` text,`version` text NOT NULL,`type` text NOT NULL,`status` text NOT NULL,`resource` text NOT NULL,`port` text,`message` text,`code_dir` text,`container_name` text)",
	"CREATE TABLE `websites` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`protocol` text NOT NULL,`primary_domain` text NOT NULL,`type` text NOT NULL,`alias` text NOT NULL,`remark` text,`status` text NOT NULL,`http_config` text NOT NULL,`expire_date` datetime,`proxy` text,`proxy_type` text,`site_dir` text,`error_log` numeric,`access_log` numeric,`default_server` numeric,`rewrite` text,`website_group_id` integer,`website_ssl_id` integer,`runtime_id` integer,`app_install_id` integer,`ftp_id` integer,`parent_website_id` integer,`user` text,`group` text,`db_type` text,`db_id` integer)",
	"CREATE TABLE `website_domains` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`website_id` integer NOT NULL,`domain` text NOT NULL,`ssl` numeric,`port` integer)",
	"CREATE TABLE `website_acme_accounts` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`email` text NOT NULL,`url` text NOT NULL,`private_key` text NOT NULL,`type` text NOT NULL DEFAULT 'letsencrypt',`eab_kid` text,`eab_hmac_key` text,`key_type` text NOT NULL DEFAULT 'RSA2048')",
	"CREATE TABLE `website_ssls` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`primary_domain` text,`private_key` text,`pem` text,`domains` text,`cert_url` text,`type` text,`provider` text,`organization` text,`acme_account_id` integer,`auto_renew` numeric,`expire_date` datetime,`start_date` datetime,`status` text,`shell` text)",
	"CREATE TABLE `databases` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`app_install_id` integer,`name` text NOT NULL UNIQUE,`type` text NOT NULL,`version` text NOT NULL,`from` text NOT NULL,`address` text NOT NULL,`port` integer NOT NULL,`username` text,`password` text,`client_key` text)",
	"CREATE TABLE `database_mysqls` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`name` text NOT NULL,`from` text NOT NULL DEFAULT \"local\",`mysql_name` text NOT NULL,`format` text NOT NULL,`collation` text,`username` text NOT NULL,`password` text NOT NULL,`permission` text NOT NULL,`is_delete` numeric,`description` text)",
	"CREATE TABLE `database_postgresqls` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`name` text NOT NULL,`from` text NOT NULL DEFAULT \"local\",`postgresql_name` text NOT NULL,`format` text NOT NULL,`username` text NOT NULL,`password` text NOT NULL,`super_user` numeric,`is_delete` numeric,`description` text)",
	"CREATE TABLE `cronjobs` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`name` text NOT NULL,`type` text NOT NULL,`spec_custom` numeric,`spec` text NOT NULL,`executor` text,`command` text,`container_name` text,`script_mode` text,`script` text,`user` text,`website` text,`app_id` text,`db_type` text,`db_name` text,`url` text,`source_dir` text,`status` text,`secret` text)",
}

// Secrets are planted in every column that holds one, and none may come back.
var Secrets = []string{"s3cret-db", "s3cret-env", "s3cret-script", "s3cret-key", "s3cret-token", "s3cret-proxy", "s3cret-compose", "s3cret-cmd"}

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
	`INSERT INTO website_acme_accounts (id, email, url, private_key, type) VALUES (1, 'admin@example.com', 'https://acme.example.com/acct/1', 's3cret-key', 'letsencrypt')`,
	`INSERT INTO website_ssls (id, primary_domain, private_key, pem, domains, type, organization, provider, acme_account_id, auto_renew, expire_date) VALUES
		(1, 'blog.example.com', '-----BEGIN PRIVATE KEY----- s3cret-key', 'CERT', 'www.blog.example.com,static.example.com', 'R11', 'Let''s Encrypt', 'http', 1, 1, '2027-01-02 03:04:05.123456789+08:00'),
		(2, 'internal.example.com', '-----BEGIN PRIVATE KEY----- s3cret-key', 'CERT', '', '1Panel CA', '', 'selfSigned', 0, 0, '2036-01-01 00:00:00+00:00')`,
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

// Make makes a server's root with 1Panel installed under base, its records
// written as 1Panel writes them, in WAL mode and still open, so some are in the
// WAL. v1 names its database 1Panel.db.
func Make(t testing.TB, base string, v1 bool) string {
	t.Helper()
	root := t.TempDir()
	if base != "/opt" {
		WriteFile(t, filepath.Join(root, Ctl), "#!/bin/bash\nBASE_DIR="+base+"\nORIGINAL_PORT=12345\n")
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
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		t.Fatal(err)
	}
	// Written after the checkpoint, so it's in the WAL alone.
	if _, err := db.ExecContext(t.Context(), `UPDATE app_installs SET status = 'Running' WHERE name = 'wordpress'`); err != nil {
		t.Fatal(err)
	}
	return root
}

// WriteFile writes text at path, making its folder.
func WriteFile(t testing.TB, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
