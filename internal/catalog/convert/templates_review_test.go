// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// writeTree writes a checkout of a source, each file by its path in it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// convertTree converts each app of a checkout, with an overlay that writes its words.
func convertTree(t *testing.T, read func(string) ([]App, error), src Source, files map[string]string) map[string]Result {
	t.Helper()
	files["LICENSE"] = "the licence"
	c := templateConverter(src)
	out := map[string]Result{}
	apps := appsOf(t, read, writeTree(t, files))
	for id := range apps {
		if _, ok := c.Overlays[id]; !ok {
			c.Overlays[id] = written("Stands in", "ai")
		}
		out[id] = c.Convert(t.Context(), apps[id])
	}
	return out
}

const (
	coolifyHead  = "# documentation: https://app.example.com\n# slogan: A slogan the overlay keeps out.\n# tags: ai\n# logo: svgs/app.svg\n# port: 3000\n\n"
	dokployRoute = "[[config.domains]]\nserviceName = \"web\"\nport = 3000\nhost = \"${main_domain}\"\n"
	dokployVars  = "[variables]\nmain_domain = \"${domain}\"\n"
)

func entryOf(t *testing.T, results map[string]Result, id string) *catalogv1.Entry {
	t.Helper()
	r, ok := results[id]
	if !ok {
		t.Fatalf("%s wasn't read", id)
	}
	if r.Entry == nil {
		t.Fatalf("%s is left out: %s", id, r.LeftOut)
	}
	return r.Entry
}

// A secret a template writes out, as a value or as a variable's default, is one every
// install would share and every plan would show. Each becomes a secret Dockwell
// generates, one for each value, so a database and its app still agree, in all
// three sources (the first review of #327, its high).
func TestASecretTheTemplateWritesIsGenerated(t *testing.T) {
	dokploy := convertTree(t, Dokploy, DokploySource("abc"), map[string]string{
		"blueprints/numeric/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n      ADMIN_PASSWORD: ${ADMIN_PASSWORD}\n",
		"blueprints/numeric/template.toml":      dokployVars + "[config]\nenv = [\"ADMIN_PASSWORD=123456\"]\n" + dokployRoute,
		"blueprints/composelit/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n" +
			"      ADMIN_PASSWORD: changeme\n      SECRET_KEY: ${SECRET_KEY:-insecure-default-key}\n      SIGNUPS: \"true\"\n" +
			"      CERT_KEY: /run/secrets/key.pem\n      DB_HOST: db\n" +
			"  db:\n    image: postgres:17.6\n    environment:\n      POSTGRES_USER: app\n      POSTGRES_PASSWORD: example\n      POSTGRES_DB: app\n",
		"blueprints/composelit/template.toml": dokployVars + dokployRoute,
		"blueprints/inside/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    command: serve --password example\n" +
			"    environment:\n      DB_PASSWORD: example\n",
		"blueprints/inside/template.toml": dokployVars + dokployRoute,
		"blueprints/inurl/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n" +
			"    environment:\n      DB_PASSWORD: example\n      DATABASE_URL: postgres://app:example@db/app\n",
		"blueprints/inurl/template.toml": dokployVars + dokployRoute,
	})
	coolify := convertTree(t, Coolify, CoolifySource("abc"), map[string]string{
		"templates/compose/listed.yaml": coolifyHead + "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n" +
			"      - SERVICE_URL_WEB_3000\n      - INIT_CLIENT_API_TOKENS=default:development.unleash-insecure-api-token\n" +
			"      - LT_API_KEYS=true\n",
		"templates/compose/defaults.yaml": coolifyHead + "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n" +
			"      - SERVICE_URL_WEB_3000\n      - DB_PASS=${DB_PASS:-abc}\n      - PGPASSWORD=${PGPASSWORD:-abc}\n" +
			"      - ROOT_PASSWORD=${ROOT_PASSWORD:-123456}\n      - MAX_TOKENS=${MAX_TOKENS}\n      - BYPASS_CACHE=${BYPASS_CACHE}\n" +
			"  db:\n    image: postgres:17.6\n    environment:\n      - POSTGRES_PASSWORD=${DB_PASS:-abc}\n",
	})

	numeric := entryOf(t, dokploy, "numeric")
	if in := inputsByName(numeric)["ADMIN_PASSWORD"]; in.GetGenerate() != 32 || strings.Contains(numeric.GetCompose(), "123456") {
		t.Errorf("Dokploy's ADMIN_PASSWORD=123456 is %v, and its file:\n%s", in, numeric.GetCompose())
	}

	lit := entryOf(t, dokploy, "composelit")
	file, in := lit.GetCompose(), inputsByName(lit)
	for name, want := range map[string]uint32{"ADMIN_PASSWORD": 32, "SECRET_KEY": 32, "POSTGRES_PASSWORD": 32} {
		if got := in[name].GetGenerate(); got != want {
			t.Errorf("%s generates %d, want %d", name, got, want)
		}
	}
	// One value, one secret: the database's password, and the app's URL that holds it.
	for _, want := range []string{
		"POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}", "POSTGRES_USER: app", "POSTGRES_DB: app",
		"SIGNUPS: \"true\"", "CERT_KEY: /run/secrets/key.pem", "SECRET_KEY: ${SECRET_KEY}",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("its file doesn't say %q:\n%s", want, file)
		}
	}
	for _, gone := range []string{"changeme", "insecure-default-key", "example"} {
		if strings.Contains(strings.ReplaceAll(file, "example/web", ""), gone) {
			t.Errorf("its file still says %q:\n%s", gone, file)
		}
	}
	if r := dokploy["inside"]; r.Entry != nil || r.Class != ClassInside || !strings.Contains(r.LeftOut, `"serve --password example"`) {
		t.Errorf("a secret written in a command too: %s, %q", r.Class, r.LeftOut)
	}
	if r := dokploy["inurl"]; r.Entry != nil || r.Class != ClassInside || !strings.Contains(r.LeftOut, "postgres://app:example@db/app") {
		t.Errorf("a secret written in a URL too: %s, %q", r.Class, r.LeftOut)
	}

	// Secrets in a list of the app's own form, as Unleash's tokens are, leave it out.
	if r := coolify["listed"]; r.Entry != nil || r.Class != ClassCantSay || !strings.Contains(r.LeftOut, "INIT_CLIENT_API_TOKENS writes out secrets") {
		t.Errorf("Unleash's tokens: %s, %q", r.Class, r.LeftOut)
	}

	def := entryOf(t, coolify, "defaults")
	file, in = def.GetCompose(), inputsByName(def)
	if in["DB_PASS"].GetGenerate() != 32 || in["ROOT_PASSWORD"].GetGenerate() != 32 || in["PGPASSWORD"] != nil {
		t.Errorf("Coolify's defaults: %v", def.GetInputs())
	}
	for _, want := range []string{"DB_PASS=${DB_PASS}", "PGPASSWORD=${DB_PASS}", "POSTGRES_PASSWORD=${DB_PASS}", "ROOT_PASSWORD=${ROOT_PASSWORD}"} {
		if !strings.Contains(file, want) {
			t.Errorf("its file doesn't say %q:\n%s", want, file)
		}
	}
	// A name with a secret's letters in it, and none of its words, isn't one.
	for _, name := range []string{"MAX_TOKENS", "BYPASS_CACHE"} {
		if in[name].GetType() != catalogv1.InputType_INPUT_TYPE_TEXT {
			t.Errorf("%s is %v, want text", name, in[name])
		}
	}
}

// What a converted template's entry says is the entry's own: a template's header and
// comments, its slogan, tags and logo, and its advice for its own platform, stay out.
func TestAConvertedTemplateCarriesNoComments(t *testing.T) {
	for id, r := range map[string]Result{
		"coolify": convertTree(t, Coolify, CoolifySource("abc"), map[string]string{
			"templates/compose/app.yaml": coolifyHead + "services:\n  web:\n    image: example/web:1.0.0 # the image\n" +
				"    environment:\n      # Use the server IP, not the domain.\n      - SERVICE_URL_WEB_3000\n      - MODE=prod\n",
		})["app"],
		"dokploy": convertTree(t, Dokploy, DokploySource("abc"), map[string]string{
			"blueprints/app/docker-compose.yml": "# A header.\nservices:\n  web:\n    image: example/web:1.0.0\n" +
				"    environment:\n      # Use the server IP, not the domain.\n      MODE: prod # inline\n",
			"blueprints/app/template.toml": dokployVars + dokployRoute,
		})["app"],
	} {
		if r.Entry == nil {
			t.Fatalf("%s: %s", id, r.LeftOut)
		}
		if file := r.Entry.GetCompose(); strings.Contains(file, "#") {
			t.Errorf("%s: its file has a comment:\n%s", id, file)
		}
	}
}

// A port the platform's proxy routes is published by an input of its own, whatever
// the template calls its own port, and none is published on the server's own network.
func TestARoutedPortTakesANameOfItsOwn(t *testing.T) {
	dokploy := convertTree(t, Dokploy, DokploySource("abc"), map[string]string{
		"blueprints/ownport/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    ports:\n      - ${PORT}:3000\n    environment:\n      PORT: ${PORT}\n",
		"blueprints/ownport/template.toml":      dokployVars + "[config]\nenv = [\"PORT=3000\"]\n" + dokployRoute,
		"blueprints/empty/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n" +
			"      DISCORD_TOKEN: ${DISCORD_TOKEN}\n      GREETING: ${GREETING}\n",
		"blueprints/empty/template.toml":        dokployVars + "[config]\nenv = [\"DISCORD_TOKEN=\", \"GREETING=\"]\n" + dokployRoute,
		"blueprints/intport/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    ports:\n      - 3000\n      - 9000:9000\n",
		"blueprints/intport/template.toml":      dokployVars + dokployRoute,
	})
	coolify := convertTree(t, Coolify, CoolifySource("abc"), map[string]string{
		"templates/compose/ownport.yaml": coolifyHead + "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n" +
			"      - SERVICE_URL_WEB_3000\n      - PORT=${PORT:-3000}\n",
		"templates/compose/dupport.yaml": coolifyHead + "services:\n  web:\n    image: example/web:1.0.0\n    ports:\n      - 3000:3000\n" +
			"    environment:\n      - SERVICE_URL_WEB_3000\n",
	})

	own := entryOf(t, dokploy, "ownport")
	if in := inputsByName(own)["HOST_PORT"]; in.GetType() != catalogv1.InputType_INPUT_TYPE_PORT || in.GetDefaultValue() != "3000" ||
		!strings.Contains(own.GetCompose(), `"127.0.0.1:${HOST_PORT}:3000"`) || own.GetWeb().GetService() != "web" {
		t.Errorf("Dokploy, a template with its own PORT: %v\n%s", own.GetInputs(), own.GetCompose())
	}
	own = entryOf(t, coolify, "ownport")
	if !strings.Contains(own.GetCompose(), `"127.0.0.1:${HOST_PORT}:3000"`) || !strings.Contains(own.GetCompose(), "PORT=${PORT:-3000}") {
		t.Errorf("Coolify, a template with its own PORT:\n%s", own.GetCompose())
	}

	// Nothing written is an input a person may give, a secret where it's named as one.
	empty := inputsByName(entryOf(t, dokploy, "empty"))
	if d := empty["DISCORD_TOKEN"]; d.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || !d.GetOptional() || d.GetGenerate() != 0 {
		t.Errorf("DISCORD_TOKEN= is %v, want a secret a person may give", d)
	}
	if g := empty["GREETING"]; g.GetType() != catalogv1.InputType_INPUT_TYPE_TEXT || !g.GetOptional() {
		t.Errorf("GREETING= is %v, want text a person may give", g)
	}

	// A port written as a number encodes, and one the file publishes already isn't
	// published twice.
	ports := entryOf(t, dokploy, "intport").GetCompose()
	if !strings.Contains(ports, `"127.0.0.1:${PORT}:3000"`) || !strings.Contains(ports, `"127.0.0.1:9000:9000"`) || strings.Count(ports, ":3000") != 1 {
		t.Errorf("its ports:\n%s", ports)
	}
	dup := entryOf(t, coolify, "dupport").GetCompose()
	if strings.Count(dup, ":3000") != 1 || strings.Contains(dup, "environment") {
		t.Errorf("a port Coolify routes that the file publishes:\n%s", dup)
	}
}

// A service on the server's own network answers on the server's own addresses: it
// publishes no port and asks for none, and its first steps say so.
func TestAHostNetworkServicePublishesNoPort(t *testing.T) {
	for _, tc := range []struct {
		read  func(string) ([]App, error)
		src   Source
		files map[string]string
	}{
		{Coolify, CoolifySource("abc"), map[string]string{
			"templates/compose/hostnet.yaml": coolifyHead + "services:\n  web:\n    image: example/web:1.0.0\n    network_mode: host\n" +
				"    environment:\n      - SERVICE_URL_WEB_3000\n      - BASE=${SERVICE_URL_WEB}\n",
		}},
		{Dokploy, DokploySource("abc"), map[string]string{
			"blueprints/hostnet/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    network_mode: host\n    environment:\n      BASE: http://${main_domain}\n",
			"blueprints/hostnet/template.toml":      dokployVars + dokployRoute,
		}},
	} {
		tc.files["LICENSE"] = "the licence"
		c := templateConverter(tc.src)
		c.Overlays["hostnet"] = Overlay{
			Entry:   &catalogv1.Entry{Summary: "Stands in", Category: "ai", HostAccess: "The server's own network, to find devices"},
			Reaches: []string{"web: the server's own network"},
		}
		app := appsOf(t, tc.read, writeTree(t, tc.files))["hostnet"]
		r := c.Convert(t.Context(), app)
		if r.Entry == nil {
			t.Fatalf("%s: %s", tc.src.Name, r.LeftOut)
		}
		file := r.Entry.GetCompose()
		if strings.Contains(file, "ports") || len(r.Entry.GetInputs()) != 0 || !strings.Contains(file, "BASE=http://localhost:3000") && !strings.Contains(file, "BASE: http://localhost:3000") {
			t.Errorf("%s: %v\n%s", tc.src.Name, r.Entry.GetInputs(), file)
		}
		if r.Entry.GetFirstSteps() != "" || len(r.Entry.GetNotes()) != 1 || !strings.Contains(r.Entry.GetNotes()[0], "answers on every port it listens on, on every address") {
			t.Errorf("%s: its first steps say %q, its notes %q", tc.src.Name, r.Entry.GetFirstSteps(), r.Entry.GetNotes())
		}
	}
}

// Coolify's routes with a path, the values it makes declared bare, its own platform
// values, and the names of what it makes (the first review of #327, its lows).
func TestCoolifyRoutesAndValues(t *testing.T) {
	coolify := convertTree(t, Coolify, CoolifySource("abc"), map[string]string{
		"templates/compose/routepath.yaml": coolifyHead + "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n" +
			"      - SERVICE_URL_WEB_3000\n      - SERVICE_URL_API_8080=/api\n      - API_URL=${SERVICE_URL_API}\n  api:\n    image: example/api:1.0.0\n",
		"templates/compose/bare.yaml": coolifyHead + "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n" +
			"      - SERVICE_URL_WEB_3000\n      - SERVICE_PASSWORD_ADMIN\n      - JWT=${SERVICE_HEX_32_SECRET}\n      - SIGNING=${SERVICE_BASE64_64_JWTSECRET}\n",
		"templates/compose/platformvol.yaml": coolifyHead + "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n" +
			"      - SERVICE_URL_WEB_3000\n    volumes:\n      - '${COOLIFY_VOLUME_APP}:/data'\n",
	})
	route := entryOf(t, coolify, "routepath")
	if in := inputsByName(route)["API_PORT"]; in.GetType() != catalogv1.InputType_INPUT_TYPE_PORT || in.GetDefaultValue() != "8080" {
		t.Errorf("API_PORT is %v", in)
	}
	for _, want := range []string{"API_URL=http://localhost:${API_PORT}/api", `"127.0.0.1:${API_PORT}:8080"`} {
		if !strings.Contains(route.GetCompose(), want) {
			t.Errorf("its file doesn't say %q:\n%s", want, route.GetCompose())
		}
	}
	bare := entryOf(t, coolify, "bare")
	in := inputsByName(bare)
	for name, label := range map[string]string{"ADMIN_PASSWORD": "Admin password", "SECRET": "Secret", "JWTSECRET": "Jwtsecret"} {
		if in[name].GetGenerate() == 0 || in[name].GetLabel() != label {
			t.Errorf("%s is %v, want a generated secret labelled %s", name, in[name], label)
		}
	}
	if !strings.Contains(bare.GetCompose(), "SERVICE_PASSWORD_ADMIN=${ADMIN_PASSWORD}") {
		t.Errorf("a value Coolify makes, declared bare:\n%s", bare.GetCompose())
	}
	if r := coolify["platformvol"]; r.Entry != nil || r.Class != ClassPlatform || !strings.Contains(r.LeftOut, "COOLIFY_VOLUME_APP") {
		t.Errorf("COOLIFY_VOLUME_APP: %s, %q", r.Class, r.LeftOut)
	}
}

// Generated inputs take their labels from what they're named, so two of one kind
// read apart.
func TestDokployLabelsWhatItMakes(t *testing.T) {
	in := inputsByName(entryOf(t, convertTree(t, Dokploy, DokploySource("abc"), map[string]string{
		"blueprints/two/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n      A: ${UI_PASSWORD}\n      B: ${SETTINGS_PASSWORD}\n",
		"blueprints/two/template.toml":      dokployVars + "[config]\nenv = [\"UI_PASSWORD=${password:16}\", \"SETTINGS_PASSWORD=${password:16}\"]\n" + dokployRoute,
	}), "two"))
	if in["UI_PASSWORD"].GetLabel() != "UI password" && in["UI_PASSWORD"].GetLabel() != "Ui password" || in["SETTINGS_PASSWORD"].GetLabel() != "Settings password" {
		t.Errorf("their labels: %q and %q", in["UI_PASSWORD"].GetLabel(), in["SETTINGS_PASSWORD"].GetLabel())
	}
}

// A template whose folder carries a licence, a notice or someone else's files under
// vendor/ is under terms of its own, which a person reads first.
func TestATemplateWithTermsOfItsOwnIsLeftOut(t *testing.T) {
	compose := "services:\n  web:\n    image: example/web:1.0.0\n"
	results := convertTree(t, Dokploy, DokploySource("abc"), map[string]string{
		"blueprints/vendored/docker-compose.yml":     compose,
		"blueprints/vendored/template.toml":          dokployVars + dokployRoute,
		"blueprints/vendored/vendor/harbor.yml.tmpl": "x",
		"blueprints/noticed/docker-compose.yml":      compose,
		"blueprints/noticed/template.toml":           dokployVars + dokployRoute,
		"blueprints/noticed/NOTICE.md":               "x",
		"blueprints/licensed/docker-compose.yml":     compose,
		"blueprints/licensed/template.toml":          dokployVars + dokployRoute,
		"blueprints/licensed/conf/HARBOR-LICENSE":    "x",
		"blueprints/plain/docker-compose.yml":        compose,
		"blueprints/plain/template.toml":             dokployVars + dokployRoute,
	})
	for id, want := range map[string]string{"vendored": "in vendor,", "noticed": "in NOTICE.md,", "licensed": "in conf/HARBOR-LICENSE,"} {
		if r := results[id]; r.Entry != nil || r.Class != ClassTerms || !strings.Contains(r.LeftOut, want) {
			t.Errorf("%s: %s, %q", id, r.Class, r.LeftOut)
		}
	}
	if results["plain"].Entry == nil {
		t.Errorf("plain: %s", results["plain"].LeftOut)
	}
}

// Each entry from Dokploy's templates says Dokploy's copyright, as MIT asks of a copy,
// so one copied out alone keeps it. Coolify's licence, Apache-2.0, has none to say.
func TestAnEntryKeepsItsSourcesCopyright(t *testing.T) {
	for _, tc := range []struct {
		src  Source
		read func(string) ([]App, error)
		id   string
		want string
	}{
		{DokploySource("abc"), Dokploy, "wiki", "# SPDX-License-Identifier: MIT\n# Copyright (c) 2024 Dokploy and Carlos Ortiz\n# Converted from Dokploy's templates"},
		{CoolifySource("abc"), Coolify, "board", "# SPDX-License-Identifier: Apache-2.0\n# Converted from Coolify's templates"},
	} {
		tc.src.LicenseText = []byte("MIT License\n\nCopyright (c) 2024 Dokploy and Carlos Ortiz\n\nPermission is hereby granted")
		if !tc.src.CopyrightInLicense {
			tc.src.LicenseText = []byte("Apache License\n...\n   Copyright [yyyy] [name of copyright owner]\n")
		}
		c := templateConverter(tc.src)
		root := "testdata/dokploy"
		if tc.id == "board" {
			root = "testdata/coolify"
		}
		r := c.Convert(t.Context(), appsOf(t, tc.read, root)[tc.id])
		dir := t.TempDir()
		if err := Write(dir, tc.src, []Result{r}, nil, c.Pins, true); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, tc.id+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(raw), tc.want) {
			t.Errorf("%s starts:\n%s", tc.id, strings.Join(strings.SplitN(string(raw), "\n", 4)[:3], "\n"))
		}
	}
}

// A registry that asks a client to wait says how long in seconds or until a time.
func TestRetryAfterReadsSecondsAndDates(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for header, want := range map[string]time.Duration{
		"7": 7 * time.Second,
		now.Add(20 * time.Second).Format(http.TimeFormat): 20 * time.Second,
		now.Add(-time.Minute).Format(http.TimeFormat):     2 * time.Second,
		"soon": 2 * time.Second,
		"":     2 * time.Second,
	} {
		if got := retryAfter(header, now); got != want {
			t.Errorf("Retry-After %q: %v, want %v", header, got, want)
		}
	}
}

// A secret's value inside a longer one leaves an app out only where it's the secret:
// a word of its own, and not where a name is written.
func TestASecretInsideALongerValue(t *testing.T) {
	for s, want := range map[string]bool{
		"postgres://app:postgres@db/app":                         true,
		"postgresql://postgres:postgres@docu-postgres:5432/x":    true,
		"Server=db;User Id=postgres;Password=postgres;":          true,
		"PGPASSWORD=postgres psql -h db":                         true,
		"redis-server --requirepass postgres":                    true,
		"postgresql://postgres:${POSTGRES_PASSWORD}@db/postgres": false,
		"pg_isready -U postgres":                                 false,
		"pg_isready -U app -d postgres":                          false,
		"postgres-data:/var/lib/postgresql/data":                 false,
		"http://postgres-backend:8080":                           false,
		"postgres_server":                                        false,
	} {
		if got := holdsSecret(s, "postgres"); got != want {
			t.Errorf("holdsSecret(%q) = %v, want %v", s, got, want)
		}
	}
	for name, want := range map[string]bool{
		"AUTHENTICATION_APIKEY_USERS": false, "GOTRUE_SECURITY_REFRESH_TOKEN_REUSE_INTERVAL": false,
		"OAUTHLIB_RELAX_TOKEN_SCOPE": false, "SMTP_PASSWORD_PROVIDER": false, "WA_BUSINESS_TOKEN_WEBHOOK": true,
	} {
		if got := secretNamed(name); got != want {
			t.Errorf("secretNamed(%s) = %v, want %v", name, got, want)
		}
	}
	for name, want := range map[string]bool{
		"INIT_CLIENT_API_TOKENS": true, "INIT_FRONTEND_API_TOKENS": true, "LT_API_KEYS": true, "APIKEYS": true,
		"LT_API_KEYS_DB_PATH": false, "KEYS_STORAGE": false, "NGINX_CACHE_KEYS_ZONE_SIZE": false, "MAX_TOKENS_PER_USER": false, "ADMIN_PASSWORD": false,
	} {
		if got := listsSecrets(name); got != want {
			t.Errorf("listsSecrets(%s) = %v, want %v", name, got, want)
		}
	}
	for value, want := range map[string]bool{"0": false, "1": false, "null": false, "123456": true, "abc": true, "/run/secrets/db": false} {
		if got := literalSecret(value); got != want {
			t.Errorf("literalSecret(%q) = %v, want %v", value, got, want)
		}
	}
}

// A secret a Dokploy template writes out keeps its name wherever the file uses it, so
// it's generated in a command as anywhere else, and the catalog refuses it inside a
// longer one. A code to get in with is a secret too (the narrow check of #327).
func TestADokploySecretKeepsItsName(t *testing.T) {
	results := convertTree(t, Dokploy, DokploySource("abc"), map[string]string{
		"blueprints/redis/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n  cache:\n    image: example/cache:1.0.0\n" +
			"    command: redis-server --requirepass ${REDIS_PASSWORD}\n",
		"blueprints/redis/template.toml":     dokployVars + "[config]\nenv = [\"REDIS_PASSWORD=hunter22\"]\n" + dokployRoute,
		"blueprints/auth/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n      APP_DB_AUTH: ${DB_PASSWORD}\n",
		"blueprints/auth/template.toml":      dokployVars + "[config]\nenv = [\"DB_PASSWORD=hunter22\"]\n" + dokployRoute,
		"blueprints/lobe/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n      ACCESS_CODE: lobe66\n",
		"blueprints/lobe/template.toml":      dokployVars + dokployRoute,
	})
	if r := results["redis"]; r.Entry != nil || !strings.Contains(r.LeftOut, "REDIS_PASSWORD") || strings.Contains(r.LeftOut, "hunter22") {
		t.Errorf("a secret in a command: %s, %q", r.Class, r.LeftOut)
	}
	auth := entryOf(t, results, "auth")
	if in := inputsByName(auth)["DB_PASSWORD"]; in.GetGenerate() != 32 || strings.Contains(auth.GetCompose(), "hunter22") ||
		!strings.Contains(auth.GetCompose(), "APP_DB_AUTH: ${DB_PASSWORD}") {
		t.Errorf("DB_PASSWORD is %v, and its file:\n%s", in, auth.GetCompose())
	}
	lobe := entryOf(t, results, "lobe")
	if in := inputsByName(lobe)["ACCESS_CODE"]; in.GetGenerate() != 32 || strings.Contains(lobe.GetCompose(), "lobe66") {
		t.Errorf("ACCESS_CODE is %v, and its file:\n%s", in, lobe.GetCompose())
	}
	for name, want := range map[string]bool{"ACCESS_CODE": true, "PASS_CODE": true, "PASSCODE": true, "ACCESSCODE": true, "COUNTRY_CODE": false, "CODE": false, "EXIT_CODE": false, "STORAGE_ENCRYPTION_KEY_VERSION": false} {
		if got := secretNamed(name); got != want {
			t.Errorf("secretNamed(%s) = %v, want %v", name, got, want)
		}
	}
}
