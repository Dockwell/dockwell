// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"slices"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

func TestMaySecret(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		secret      bool
	}{
		// What a catalog entry writes that is no secret.
		{"WORDPRESS_DB_HOST", "db", false},
		{"WORDPRESS_DB_USER", "wordpress", false},
		{"WORDPRESS_DB_NAME", "wordpress", false},
		{"MARIADB_AUTO_UPGRADE", "1", false},
		{"TZ", "Europe/Amsterdam", false},
		{"PORT", "8080", false},
		{"url", "http://localhost:2368", false},
		{"DATA_DIR", "/var/lib/app/data", false},
		{"DB_COLLATION", "utf8mb4_unicode_ci", false},
		{"KEYCLOAK_HOSTNAME", "auth.example.com", false},
		{"IMAGE_VERSION", "1.27.3-alpine", false},
		{"EMPTY", "", false},

		// By name, whatever the value.
		{"WORDPRESS_DB_PASSWORD", "x", true},
		{"MYSQL_ROOT_PASSWORD", "", true},
		{"database__connection__password", "x", true},
		{"dbPassword", "x", true},
		{"API_KEY", "abc", true},
		{"SECRET_KEY_BASE", "abc", true},
		{"JWT_SECRET", "abc", true},
		{"AUTH_TOKEN", "abc", true},
		{"ADMIN_PASS", "abc", true},
		{"DB_PWD", "abc", true},
		{"PRIVATE_KEY_FILE", "/run/key", true},
		{"PASSPHRASE", "abc", true},
		{"SALT", "abc", true},
		{"SENTRY_DSN", "abc", true},
		{"SMTP_CREDENTIALS", "abc", true},
		{"MYAPIKEY", "abc", true},
		{"POSTGRES_HOST_AUTH_METHOD", "trust", true},

		// By shape, whatever the name.
		{"DATABASE_URL", "postgres://app:hunter2@db:5432/app", true},
		{"ERRORS", "https://0123abcd@o1.ingest.example.com/1", true},
		{"CERTIFICATE", "-----BEGIN PRIVATE KEY-----\nMII", true},
		{"OPENAI", "sk-proj-abc", true},
		{"GH", "ghp_0123456789", true},
		{"MIXED", "aB3dE5fG7hJ9kL1mN2pQ", true},
		{"HEX", "0123456789abcdef0123456789abcdef", true},
		{"UUID", "123e4567-e89b-12d3-a456-426614174000", true},
		{"HOOK_URL", "https://hooks.example.com/services/T0A1B2C3D/B4E5F6G7H/aBcDeFgHiJkLmNoPqRsTuVwX", true},
		{"JWTISH", "eyJhbGciOiJIUzI1NiJ9.e30.abc", true},

		// Names written together, or in camelCase, as stores write them.
		{"dbPass", "x", true},
		{"smtpPass", "x", true},
		{"adminPwd", "x", true},
		{"appKey", "x", true},
		{"masterKey", "x", true},
		{"LDAP_BINDPW", "x", true},
		{"REDISPASS", "x", true},
		{"PGPASSWORD", "x", true},
		{"ACCESS_CODE", "x", true},
		{"RABBITMQ_ERLANG_COOKIE", "x", true},
		{"SESSION_SECRET", "x", true},
		{"SESSION_DRIVER", "file", true},
		{"CSRF_NONCE", "x", true},

		// Credentials inside a value.
		{"DB", "postgresql://db/app?user=app&password=hunter2", true},
		{"JDBC", "jdbc:mysql://db:3306/app?user=app&password=x", true},
		{"MSSQL", "Server=db;User Id=sa;Password=Hunter2!", true},
		{"BASIC", "admin:hunter2", true},
		{"HOSTPORT", "db:5432", false},
		{"IMAGE", "nginx:1.27", false},

		// Short runs that read as random.
		{"A", "aB3dE5fG7hJ9kL1m", true},
		{"B", "0123456789abcdef01234567", true},
		{"C", "JBSWY3DPEHPK3PXP", true},
		{"D", "production", false},
	} {
		if got := catalog.MaySecret(tc.name, tc.value); got != tc.secret {
			t.Errorf("MaySecret(%q, %q) = %t, want %t", tc.name, tc.value, got, tc.secret)
		}
	}
}

func TestShownEnv(t *testing.T) {
	app := &resourcev1.Container{Env: map[string]string{
		"WORDPRESS_DB_HOST":     "db",
		"WORDPRESS_DB_NAME":     "wordpress",
		"WORDPRESS_DB_PASSWORD": "hunter2",
		"WORDPRESS_CONFIG":      "postgres://u:p@db/x",
	}}
	got := catalog.ShownEnv(app)
	if want := []string{"WORDPRESS_DB_HOST", "WORDPRESS_DB_NAME"}; !slices.Equal(got, want) {
		t.Fatalf("ShownEnv = %v, want %v", got, want)
	}
	if got := catalog.ShownEnv(&resourcev1.Container{}); got != nil {
		t.Errorf("ShownEnv of no environment = %v, want nil", got)
	}
}
