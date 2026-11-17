// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// insideBlueprints are Dokploy templates shaped as calcom's, plane's and snipe-it's
// are, each with a value made up inside a longer one in its environment: a URL from a
// variable that holds the password (calcom), URLs written in the environment beside
// the password whole, and one whose password nothing else names (plane), and a key
// after a prefix, base64:${app_key} (snipe-it). And one that writes a value it makes
// up into a file it mounts, which stays left out.
var insideBlueprints = map[string]string{
	"calcom/docker-compose.yml": `services:
  calcom:
    image: calcom/cal.com:v5.0.0
    environment:
      - DATABASE_URL=${DATABASE_URL}
      - NEXTAUTH_SECRET=${NEXTAUTH_SECRET}
      - NEXT_PUBLIC_WEBAPP_URL=${NEXT_PUBLIC_WEBAPP_URL}
  postgres:
    image: postgres:16
    environment:
      - POSTGRES_PASSWORD=${POSTGRES_PASSWORD}
`,
	"calcom/template.toml": `[variables]
main_domain = "${domain}"
postgres_password = "${password:32}"
database_url = "postgresql://calcom:${postgres_password}@postgres:5432/calendso"
nextauth_secret = "${base64:32}"

[config]
env = [
  "POSTGRES_PASSWORD=${postgres_password}",
  "DATABASE_URL=${database_url}",
  "NEXTAUTH_SECRET=${nextauth_secret}",
  "NEXT_PUBLIC_WEBAPP_URL=http://${main_domain}",
]

[[config.domains]]
serviceName = "calcom"
port = 3000
host = "${main_domain}"
`,
	"plane/docker-compose.yml": `services:
  api:
    image: makeplane/plane-backend:v1.3.1
    environment:
      - DATABASE_URL=${DATABASE_URL}
      - AMQP_URL=${AMQP_URL}
  plane-db:
    image: postgres:16
    environment:
      - POSTGRES_PASSWORD=${POSTGRES_PASSWORD}
`,
	"plane/template.toml": `[variables]
main_domain = "${domain}"
postgres_password = "${password:32}"
rabbitmq_password = "${password:24}"

[config]
env = [
  "POSTGRES_PASSWORD=${postgres_password}",
  "DATABASE_URL=postgresql://plane:${postgres_password}@plane-db:5432/plane",
  "AMQP_URL=amqp://plane:${rabbitmq_password}@plane-mq:5672/plane",
]

[[config.domains]]
serviceName = "api"
port = 8000
host = "${main_domain}"
`,
	"snipe-it/docker-compose.yml": `services:
  snipe-it:
    image: snipe/snipe-it:v8.0.0
    environment:
      APP_KEY: ${APP_KEY}
      DB_PASSWORD: ${DB_PASSWORD}
`,
	"snipe-it/template.toml": `[variables]
main_domain = "${domain}"
app_key = "${base64:32}"
db_password = "${password:24}"

[config]
[[config.domains]]
serviceName = "snipe-it"
port = 80
host = "${main_domain}"

[config.env]
APP_KEY = "base64:${app_key}"
DB_PASSWORD = "${db_password}"
`,
	"filed/docker-compose.yml": `services:
  app:
    image: example/app:1.0.0
    volumes:
      - ../files/app.conf:/etc/app.conf
`,
	"filed/template.toml": `[variables]
db_password = "${password:32}"

[config]
env = []

[[config.mounts]]
filePath = "app.conf"
content = "url = postgres://app:${db_password}@db/app"

[[config.domains]]
serviceName = "app"
port = 80
host = "${domain}"
`,
}

func insideApps(t *testing.T) map[string]App {
	t.Helper()
	root := t.TempDir()
	for rel, text := range insideBlueprints {
		file := filepath.Join(root, "blueprints", rel)
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return appsOf(t, Dokploy, root)
}

// A value Dokploy makes up inside a longer one in an app's environment is an input
// of its own, generated as the template asks, which the longer value names; so the
// catalog writes the longer value from a template, and an upgrade and a rotation
// reach it (#570). Before, each of these apps was left out.
func TestDokployHandsOnAValueMadeUpInsideALongerOne(t *testing.T) {
	apps := insideApps(t)
	conv := templateConverter(DokploySource("abc"))
	conv.Overlays = map[string]Overlay{
		"calcom": written("Schedules meetings", "productivity"), "plane": written("Tracks work", "productivity"),
		"snipe-it": written("Tracks assets", "productivity"),
	}
	for id, want := range map[string]struct {
		lines  []string
		inputs map[string]struct {
			n  uint32
			as catalogv1.GenerateAs
		}
	}{
		// The variable that holds the URL comes to it, with the password named by the
		// input its whole use makes.
		"calcom": {
			lines: []string{"DATABASE_URL=postgresql://calcom:${POSTGRES_PASSWORD}@postgres:5432/calendso", "POSTGRES_PASSWORD=${POSTGRES_PASSWORD}"},
			inputs: map[string]struct {
				n  uint32
				as catalogv1.GenerateAs
			}{"POSTGRES_PASSWORD": {32, 0}, "NEXTAUTH_SECRET": {32, catalogv1.GenerateAs_GENERATE_AS_BASE64}},
		},
		// A password nothing names whole is an input under its variable's name.
		"plane": {
			lines: []string{"DATABASE_URL=postgresql://plane:${POSTGRES_PASSWORD}@plane-db:5432/plane", "AMQP_URL=amqp://plane:${RABBITMQ_PASSWORD}@plane-mq:5672/plane"},
			inputs: map[string]struct {
				n  uint32
				as catalogv1.GenerateAs
			}{"POSTGRES_PASSWORD": {32, 0}, "RABBITMQ_PASSWORD": {24, 0}},
		},
		// APP_KEY is the value's own name, so the key inside it is APP_KEY_SECRET: 32
		// bytes in base64, as ${base64:32} makes, after a prefix that stays as it is.
		"snipe-it": {
			lines: []string{"APP_KEY: base64:${APP_KEY_SECRET}", "DB_PASSWORD: ${DB_PASSWORD}"},
			inputs: map[string]struct {
				n  uint32
				as catalogv1.GenerateAs
			}{"APP_KEY_SECRET": {32, catalogv1.GenerateAs_GENERATE_AS_BASE64}, "DB_PASSWORD": {24, 0}},
		},
	} {
		t.Run(id, func(t *testing.T) {
			r := conv.Convert(t.Context(), apps[id])
			if r.Entry == nil {
				t.Fatalf("left out: %s", r.LeftOut)
			}
			file := r.Entry.GetCompose()
			for _, line := range want.lines {
				if !strings.Contains(file, line) {
					t.Errorf("its file doesn't say %s:\n%s", line, file)
				}
			}
			if strings.Contains(file, "\x00") {
				t.Errorf("its file holds a mark:\n%q", file)
			}
			in := inputsByName(r.Entry)
			for name, w := range want.inputs {
				got := in[name]
				if got.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || got.GetGenerate() != w.n || got.GetGenerateAs() != w.as || got.GetDefaultValue() != "" {
					t.Errorf("%s is %v, want a secret Dockwell generates, %d %v", name, got, w.n, w.as)
				}
			}
		})
	}
	// A value made up inside a file it mounts stays left out: a file isn't written
	// from a template.
	if r := conv.Convert(t.Context(), apps["filed"]); r.Entry != nil || r.Class != ClassInside ||
		!strings.Contains(r.LeftOut, "its template writes a value it makes up into its file app.conf") {
		t.Errorf("filed: %v, %q", r.Class, r.LeftOut)
	}
}
