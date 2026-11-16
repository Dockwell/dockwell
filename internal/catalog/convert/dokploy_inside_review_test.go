// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// reviewApps reads Dokploy templates written into a folder of the test's own.
func reviewApps(t *testing.T, files map[string]string) map[string]App {
	t.Helper()
	root := t.TempDir()
	for rel, text := range files {
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

// Dokploy works a variable out once, so a password written inside db_url is one value
// in both services that read db_url: one input, which both URLs name (#570's review,
// H2). Before, each use made an input of its own, so web and worker connected with
// two different passwords.
func TestOneVariableMadeUpInsideIsOneValue(t *testing.T) {
	apps := reviewApps(t, map[string]string{
		"twice/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n      - DATABASE_URL=${DATABASE_URL}\n  worker:\n    image: example/worker:1.0.0\n    environment:\n      - WORKER_DATABASE_URL=${WORKER_DATABASE_URL}\n",
		"twice/template.toml": `[variables]
main_domain = "${domain}"
db_url = "postgresql://app:${password:32}@db:5432/app"

[config]
env = [
  "DATABASE_URL=${db_url}",
  "WORKER_DATABASE_URL=${db_url}",
]

[[config.domains]]
serviceName = "web"
port = 3000
host = "${main_domain}"
`,
	})
	conv := templateConverter(DokploySource("abc"))
	conv.Overlays = map[string]Overlay{"twice": written("Stands in", "productivity")}
	r := conv.Convert(t.Context(), apps["twice"])
	if r.Entry == nil {
		t.Fatalf("left out: %s", r.LeftOut)
	}
	var generated []string
	for _, in := range r.Entry.GetInputs() {
		if in.GetGenerate() > 0 {
			generated = append(generated, in.GetName())
		}
	}
	if len(generated) != 1 {
		t.Fatalf("db_url is one value; the entry generates %v:\n%s", generated, r.Entry.GetCompose())
	}
	// Named for the variable, whichever use comes first.
	for _, line := range []string{"DATABASE_URL=postgresql://app:${" + generated[0] + "}@db:5432/app", "WORKER_DATABASE_URL=postgresql://app:${" + generated[0] + "}@db:5432/app"} {
		if !strings.Contains(r.Entry.GetCompose(), line) {
			t.Errorf("its file doesn't say %s:\n%s", line, r.Entry.GetCompose())
		}
	}
	if !strings.HasPrefix(generated[0], "DB_URL") {
		t.Errorf("the input is %s, want it named for db_url", generated[0])
	}
}

// akaunting's DB_PREFIX is ${hash:3}_, a table prefix: an identifier, not a secret a
// person may rotate. Dockwell would generate 16 hex digits for it, and Laravel's
// longest index name would then pass MySQL's 64 characters, so the first migration
// fails. It's left out, saying so; and so is a short value made up inside a longer
// one, which would come out longer than the app reads (#570's review, H3).
func TestAShortValueMadeUpInsideIsLeftOut(t *testing.T) {
	apps := reviewApps(t, map[string]string{
		"akaunting/docker-compose.yml": "services:\n  akaunting:\n    image: akaunting/akaunting:3.1.0\n    environment:\n      - DB_PREFIX=${DB_PREFIX}\n      - DB_PASSWORD=${DB_PASSWORD}\n",
		"akaunting/template.toml": `[variables]
main_domain = "${domain}"
mysql_password = "${password:24}"
db_prefix = "${hash:3}_"

[[config.domains]]
serviceName = "akaunting"
port = 80
host = "${main_domain}"

[config]
mounts = []
env = [
  "DB_PASSWORD=${mysql_password}",
  "DB_PREFIX=${db_prefix}",
]
`,
		"pin/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n      - LOGIN=${LOGIN}\n",
		"pin/template.toml": `[variables]
pin = "${password:6}"

[config]
env = ["LOGIN=admin:${pin}"]

[[config.domains]]
serviceName = "web"
port = 80
host = "${domain}"
`,
	})
	conv := templateConverter(DokploySource("abc"))
	conv.Overlays = map[string]Overlay{"akaunting": written("Keeps accounts", "productivity"), "pin": written("Stands in", "productivity")}
	for id, want := range map[string]string{
		"akaunting": "its template's DB_PREFIX makes up 3 hex digits, an identifier such as a table prefix or an instance's ID rather than a secret",
		"pin":       "its template's LOGIN makes up 6 characters inside a longer value, fewer than the 16 of a secret Dockwell generates",
	} {
		if r := conv.Convert(t.Context(), apps[id]); r.Entry != nil || r.Class != ClassMadeUp || !strings.Contains(r.LeftOut, want) {
			t.Errorf("%s: converts %v, %v %q, want it left out as %q", id, r.Entry != nil, r.Class, r.LeftOut, want)
		}
	}
}

// A value made up inside a longer one that lands in a YAML extension a service's
// environment merges in, as chatwoot's REDIS_URL in x-chatwoot-base does, is left
// out saying where it is and which of the template's values it is, rather than a
// name the converter made (#570's review, L1).
func TestAValueMadeUpInsideAnExtensionSaysWhereItIs(t *testing.T) {
	apps := reviewApps(t, map[string]string{
		"chat/docker-compose.yml": "x-chat-base: &chat-base\n  image: example/chat:1.0.0\n  environment:\n    - REDIS_URL=${REDIS_URL}\n    - REDIS_PASSWORD=${REDIS_PASSWORD}\n\nservices:\n  chat:\n    <<: *chat-base\n  worker:\n    <<: *chat-base\n",
		"chat/template.toml": `[variables]
redis_password = "${password:32}"

[config]
env = ["REDIS_PASSWORD=${redis_password}", "REDIS_URL=redis://:${redis_password}@redis:6379"]

[[config.domains]]
serviceName = "chat"
port = 3000
host = "${domain}"
`,
	})
	conv := templateConverter(DokploySource("abc"))
	conv.Overlays = map[string]Overlay{"chat": written("Stands in", "productivity")}
	want := "its template puts a value it makes up, redis_password, inside a longer one in x-chat-base, a YAML extension a service's environment merges in"
	if r := conv.Convert(t.Context(), apps["chat"]); r.Entry != nil || r.Class != ClassInside || !strings.Contains(r.LeftOut, want) {
		t.Errorf("chat: converts %v, %v %q, want %q", r.Entry != nil, r.Class, r.LeftOut, want)
	}
}

// A helper written inline is named for the value it's in and for what it makes, so
// ADMIN=${email}:${password:16} asks for ADMIN_EMAIL, an email address, and
// generates ADMIN_PASSWORD (#570's review, L2). Before, the email was ADMIN_SECRET,
// labelled as a secret, and the password ADMIN_2.
func TestAnInlineHelperIsNamedForWhatItMakes(t *testing.T) {
	apps := reviewApps(t, map[string]string{
		"admin/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n      - ADMIN=${ADMIN}\n",
		"admin/template.toml": `[config]
env = ["ADMIN=${email}:${password:16}"]

[[config.domains]]
serviceName = "web"
port = 80
host = "${domain}"
`,
	})
	conv := templateConverter(DokploySource("abc"))
	conv.Overlays = map[string]Overlay{"admin": written("Stands in", "productivity")}
	r := conv.Convert(t.Context(), apps["admin"])
	if r.Entry == nil {
		t.Fatalf("left out: %s", r.LeftOut)
	}
	if !strings.Contains(r.Entry.GetCompose(), "ADMIN=${ADMIN_EMAIL}:${ADMIN_PASSWORD}") {
		t.Errorf("its file:\n%s", r.Entry.GetCompose())
	}
	in := inputsByName(r.Entry)
	if e := in["ADMIN_EMAIL"]; e.GetType() != catalogv1.InputType_INPUT_TYPE_EMAIL || e.GetLabel() != "Admin email" {
		t.Errorf("ADMIN_EMAIL is %v", e)
	}
	if p := in["ADMIN_PASSWORD"]; p.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || p.GetGenerate() != 16 {
		t.Errorf("ADMIN_PASSWORD is %v", p)
	}
}

// A Compose file that names one of the template's variables itself, as
// DATABASE_URL=${db_url}, gets what the variable comes to, as Dokploy writes it: here
// a URL with the password made up inside it, which an input names. A plain value, as
// TZ=${timezone}, is a person's to set: an input under the name an install made before
// had it by, TIMEZONE, with the template's value as its default (#570's re-check,
// H-A). Before, the variable was never worked out, and became an empty input of its
// own lowercase name (#570's review, L3).
func TestAVariableTheFileNamesIsWorkedOut(t *testing.T) {
	apps := reviewApps(t, map[string]string{
		"direct/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n      - DATABASE_URL=${db_url}\n      - TZ=${timezone}\n",
		"direct/template.toml": `[variables]
db_password = "${password:32}"
db_url = "postgresql://app:${db_password}@db:5432/app"
timezone = "UTC"

[config]
env = []

[[config.domains]]
serviceName = "web"
port = 80
host = "${domain}"
`,
	})
	conv := templateConverter(DokploySource("abc"))
	conv.Overlays = map[string]Overlay{"direct": written("Stands in", "productivity")}
	r := conv.Convert(t.Context(), apps["direct"])
	if r.Entry == nil {
		t.Fatalf("left out: %s", r.LeftOut)
	}
	for _, line := range []string{"DATABASE_URL=postgresql://app:${DB_PASSWORD}@db:5432/app", "TZ=${TIMEZONE:-UTC}"} {
		if !strings.Contains(r.Entry.GetCompose(), line) {
			t.Errorf("its file doesn't say %s:\n%s", line, r.Entry.GetCompose())
		}
	}
	in := inputsByName(r.Entry)
	if p := in["DB_PASSWORD"]; p.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || p.GetGenerate() != 32 {
		t.Errorf("DB_PASSWORD is %v", p)
	}
	if tz := in["TIMEZONE"]; tz.GetType() != catalogv1.InputType_INPUT_TYPE_TEXT || tz.GetDefaultValue() != "UTC" || !tz.GetOptional() {
		t.Errorf("TIMEZONE is %v, want text a person may set, UTC unless they do", tz)
	}
	for _, gone := range []string{"db_url", "DB_URL", "timezone"} {
		if _, ok := in[gone]; ok {
			t.Errorf("%s is an input: %v", gone, in[gone])
		}
	}
}

// A variable a template names "inline 1" is its own value, apart from a helper written
// inline in another value, as Dokploy works each out on its own (#570's re-check,
// N3). Before, the two shared an id, so the inline one read the variable's input.
func TestAVariableNamedLikeAnInlineHelperIsItsOwnValue(t *testing.T) {
	apps := reviewApps(t, map[string]string{
		"named/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n      - A=${A}\n      - B=${B}\n",
		"named/template.toml": `[variables]
"inline 1" = "${password:32}"

[config]
env = ["A=${inline 1}", "B=x:${password:20}"]

[[config.domains]]
serviceName = "web"
port = 80
host = "${domain}"
`,
	})
	conv := templateConverter(DokploySource("abc"))
	conv.Overlays = map[string]Overlay{"named": written("Stands in", "productivity")}
	r := conv.Convert(t.Context(), apps["named"])
	if r.Entry == nil {
		t.Fatalf("left out: %s", r.LeftOut)
	}
	in := inputsByName(r.Entry)
	if a, b := in["A"], in["B_PASSWORD"]; a.GetGenerate() != 32 || b.GetGenerate() != 20 {
		t.Errorf("A is %v and B_PASSWORD %v, want two values of 32 and 20:\n%s", a, b, r.Entry.GetCompose())
	}
}

// A licence key only the app's vendor issues, as Cfx.re issues FiveM's, is no value
// Dockwell can make: fivem's template makes one up for Dokploy to fill in, which the
// server refuses, so the app is left out, saying so (#570's re-check, N1). Before, it
// installed with a random key, and read as done until the game server failed.
func TestAMadeUpLicenceKeyIsLeftOut(t *testing.T) {
	apps := reviewApps(t, map[string]string{
		"fivem/docker-compose.yml": "services:\n  fivem:\n    image: example/fivem:latest\n    environment:\n      - LICENSE_KEY=${license_key}\n      - RCON_PASSWORD=${rcon_password}\n",
		"fivem/template.toml": `[variables]
license_key = "${password:32}"
rcon_password = "${password:16}"

[config]
env = ["LICENSE_KEY=${license_key}", "RCON_PASSWORD=${rcon_password}"]

[[config.domains]]
serviceName = "fivem"
port = 40120
host = "${domain}"
`,
	})
	conv := templateConverter(DokploySource("abc"))
	conv.Overlays = map[string]Overlay{"fivem": written("Runs a game server", "media")}
	want := "its template's LICENSE_KEY makes up a licence key, which only the app's vendor issues"
	if r := conv.Convert(t.Context(), apps["fivem"]); r.Entry != nil || r.Class != ClassMadeUp || !strings.Contains(r.LeftOut, want) {
		t.Errorf("fivem: converts %v, %v %q, want %q", r.Entry != nil, r.Class, r.LeftOut, want)
	}
}
