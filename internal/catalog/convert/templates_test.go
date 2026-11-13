// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

func templateConverter(src Source) *Converter {
	c := converter()
	c.Source = src
	c.Overlays = map[string]Overlay{
		"wiki": written("Keeps a wiki", "notes"), "board": written("Keeps a board", "notes"),
		"files": written("Stands in", "ai"), "urlsecret": written("Stands in", "ai"), "supa": written("Stands in", "ai"),
	}
	return c
}

func appsOf(t *testing.T, read func(string) ([]App, error), root string) map[string]App {
	t.Helper()
	apps, err := read(root)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]App{}
	for i := range apps {
		out[apps[i].ID] = apps[i]
	}
	return out
}

func inputsByName(e *catalogv1.Entry) map[string]*catalogv1.Input {
	out := map[string]*catalogv1.Input{}
	for _, in := range e.GetInputs() {
		out[in.GetName()] = in
	}
	return out
}

// Dokploy writes its template's environment at an install, and routes a domain to a
// port it doesn't publish. An entry says both in its own file.
func TestDokployWritesWhatItsTemplateMakes(t *testing.T) {
	apps := appsOf(t, Dokploy, "testdata/dokploy")
	wiki := apps["wiki"]
	if wiki.Name != "Wiki" || wiki.Version != "2.1.0" || wiki.Homepage != "https://wiki.example.com" || wiki.Category != "notes" || wiki.Path != "blueprints/wiki" {
		t.Errorf("wiki reads as %+v", wiki)
	}
	r := templateConverter(DokploySource("abc")).Convert(t.Context(), wiki)
	if r.Entry == nil {
		t.Fatal(r.LeftOut)
	}
	file := r.Entry.GetCompose()
	for _, want := range []string{
		`"127.0.0.1:${PORT}:3000"`,
		"BASE_URL=http://localhost:${PORT}",
		"ADMIN_USER=${ADMIN_USER}",
		"ADMIN_PASSWORD=${ADMIN_PASSWORD}",
		"DB_PASSWORD=${ADMIN_PASSWORD}",
		"MODE=production",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("its file doesn't say %s:\n%s", want, file)
		}
	}
	if strings.Contains(file, "env_file") || !strings.Contains(file, "SESSION_KEY: ${SESSION_KEY}") {
		t.Errorf("db's .env isn't its own environment:\n%s", file)
	}
	in := inputsByName(r.Entry)
	for name, want := range map[string]struct {
		kind catalogv1.InputType
		n    uint32
		as   catalogv1.GenerateAs
		def  string
	}{
		"PORT":           {catalogv1.InputType_INPUT_TYPE_PORT, 0, 0, "3000"},
		"ADMIN_USER":     {catalogv1.InputType_INPUT_TYPE_TEXT, 0, 0, "admin"},
		"ADMIN_PASSWORD": {catalogv1.InputType_INPUT_TYPE_SECRET, 24, 0, ""},
		"SESSION_KEY":    {catalogv1.InputType_INPUT_TYPE_SECRET, 64, catalogv1.GenerateAs_GENERATE_AS_HEX, ""},
		"TOTP_KEY":       {catalogv1.InputType_INPUT_TYPE_SECRET, 32, catalogv1.GenerateAs_GENERATE_AS_BASE64, ""},
	} {
		got := in[name]
		if got.GetType() != want.kind || got.GetGenerate() != want.n || got.GetGenerateAs() != want.as || got.GetDefaultValue() != want.def {
			t.Errorf("%s is %v", name, got)
		}
	}
	// A secret the template writes out, which everyone would share, is one Dockwell
	// generates, and a number named as being about one stays as the template wrote it.
	if k := in["SECRET_KEY"]; k.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || k.GetDefaultValue() != "" || k.GetGenerate() != 32 {
		t.Errorf("SECRET_KEY is %v, want a secret Dockwell generates", k)
	}
	if !strings.Contains(file, "PASSWORD_MIN_LENGTH=12") || strings.Contains(file, "changeme") {
		t.Errorf("its file:\n%s", file)
	}
	// What the file uses of the template's own, a folder Dokploy keeps, and a
	// variable the template leaves to a person.
	for _, want := range []string{"SITE=http://localhost:${PORT}/wiki", "- uploads:/uploads", "uploads: {}"} {
		if !strings.Contains(file, want) {
			t.Errorf("its file doesn't say %q:\n%s", want, file)
		}
	}
	if h := in["SMTP_HOST"]; h.GetType() != catalogv1.InputType_INPUT_TYPE_TEXT || !h.GetOptional() {
		t.Errorf("SMTP_HOST is %v", h)
	}
	if len(in) != 7 {
		t.Errorf("its inputs are %v, want seven", in)
	}
	if r.Entry.GetLicense() != "MIT" || r.Entry.GetSource().GetRepository() != "https://github.com/Dokploy/templates" {
		t.Errorf("it says it's %s, from %s", r.Entry.GetLicense(), r.Entry.GetSource().GetRepository())
	}
}

func TestDokployLeavesOutWhatAnEntryCantSay(t *testing.T) {
	apps := appsOf(t, Dokploy, "testdata/dokploy")
	for id, want := range map[string]string{
		"urlsecret": "puts a value it makes up, db_password, inside a longer one",
	} {
		r := templateConverter(DokploySource("abc")).Convert(t.Context(), apps[id])
		if r.Entry != nil || !strings.Contains(r.LeftOut, want) {
			t.Errorf("%s: %q, want it left out as %q", id, r.LeftOut, want)
		}
	}
}

// Coolify makes its SERVICE_ variables at an install, and asks a person for the rest.
// An entry generates the first, asks for the second, and answers on loopback.
func TestCoolifyWritesWhatItsTemplateMakes(t *testing.T) {
	apps := appsOf(t, Coolify, "testdata/coolify")
	board := apps["board"]
	if board.Name != "Board" || board.Version != "3.2.1" || board.Homepage != "https://board.example.com" || board.Category != "notes" {
		t.Errorf("board reads as %+v", board)
	}
	r := templateConverter(CoolifySource("abc")).Convert(t.Context(), board)
	if r.Entry == nil {
		t.Fatal(r.LeftOut)
	}
	file := r.Entry.GetCompose()
	for _, want := range []string{
		`"127.0.0.1:${PORT}:8080"`,
		"PUBLIC_URL=http://localhost:${PORT}",
		"HOSTNAME=localhost",
		"DB_USER=${POSTGRES_USER}",
		"POSTGRES_PASSWORD=${POSTGRES_PASSWORD}",
		"SECRET=${BOARD_SECRET}",
		"SIGNING=${BOARD_SECRET_2}",
		"LOG_LEVEL=${LOG_LEVEL:-info}",
		"COST=$$5",
		"board-data: {}",
		"pg-data: {}",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("its file doesn't say %s:\n%s", want, file)
		}
	}
	for _, gone := range []string{"SERVICE_", "exclude_from_hc", "traefik"} {
		if strings.Contains(file, gone) {
			t.Errorf("its file still says %s:\n%s", gone, file)
		}
	}
	in := inputsByName(r.Entry)
	if u := in["POSTGRES_USER"]; u.GetType() != catalogv1.InputType_INPUT_TYPE_TEXT || u.GetDefaultValue() != "postgres" {
		t.Errorf("POSTGRES_USER is %v", u)
	}
	if p := in["POSTGRES_PASSWORD"]; p.GetGenerate() != 32 || p.GetGenerateAs() != 0 {
		t.Errorf("POSTGRES_PASSWORD is %v", p)
	}
	if h := in["BOARD_SECRET"]; h.GetGenerate() != 32 || h.GetGenerateAs() != catalogv1.GenerateAs_GENERATE_AS_HEX {
		t.Errorf("BOARD_SECRET is %v, want 32 hex digits, as SERVICE_HEX_32 makes", h)
	}
	if b := in["BOARD_SECRET_2"]; b.GetGenerate() != 64 || b.GetGenerateAs() != catalogv1.GenerateAs_GENERATE_AS_BASE64 {
		t.Errorf("BOARD_SECRET_2 is %v, want 64 bytes in base64, as SERVICE_REALBASE64_64 makes", b)
	}
	if h := in["SMTP_HOST"]; h.GetType() != catalogv1.InputType_INPUT_TYPE_TEXT || !h.GetOptional() {
		t.Errorf("SMTP_HOST is %v, want text a person may leave", h)
	}
	if p := in["SMTP_PASSWORD"]; p.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || !p.GetOptional() || p.GetGenerate() != 0 {
		t.Errorf("SMTP_PASSWORD is %v, want a secret a person may type", p)
	}
	if k := in["LICENSE_KEY"]; k.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || k.GetOptional() {
		t.Errorf("LICENSE_KEY is %v, want a secret a person must type, as :? asks", k)
	}
	if _, ok := in["LOG_LEVEL"]; ok {
		t.Error("LOG_LEVEL, which always has a default, became an input")
	}
	// A secret whose only default is in the file is one Dockwell generates.
	if s := in["APP_SECRET"]; s.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || s.GetOptional() || s.GetGenerate() != 32 || strings.Contains(file, "changeme") || !strings.Contains(file, "APP_SECRET=${APP_SECRET}") {
		t.Errorf("APP_SECRET is %v, and its file:\n%s", s, file)
	}
	if r.Entry.GetLicense() != "Apache-2.0" || r.Entry.GetSource().GetPath() != "templates/compose/board.yaml" {
		t.Errorf("it says it's %s, at %s", r.Entry.GetLicense(), r.Entry.GetSource().GetPath())
	}
}

// An app Coolify says runs on amd64 alone converts, offered on amd64 servers
// (ADR 0085).
func TestCoolifysAmd64AloneIsOfferedOnAmd64Servers(t *testing.T) {
	board, err := os.ReadFile("testdata/coolify/templates/compose/board.yaml")
	if err != nil {
		t.Fatal(err)
	}
	results := convertTree(t, Coolify, CoolifySource("abc"), map[string]string{
		"templates/compose/board.yaml": strings.Replace(string(board), "# port: 8080", "# port: 8080\n# amd_only: true", 1),
	})
	if e := entryOf(t, results, "board"); !slices.Equal(e.GetArchitectures(), []string{"amd64"}) {
		t.Errorf("architectures = %v", e.GetArchitectures())
	}
}

// Common cases that left apps out convert (ADR 0085): a value a service takes from
// Dokploy's env by its name alone, a made-up value under a name an input can't take,
// an image whose tag a variable gives with a default, and an ID that starts with a
// digit.
func TestCommonCasesConvert(t *testing.T) {
	compose := "services:\n  web:\n    image: example/web:${WEB_TAG:-2.1.0}\n    environment:\n      - POSTGRES_DB\n      - SMTP_HOST\n      - DFLY_requirepass=${DFLY_requirepass}\n    ports:\n      - 3000\n  cache:\n    image: example/cache:1.0.0\n    environment:\n      POSTGRES_DB:\n"
	template := dokployVars + "pass = \"${password:32}\"\n" + "[config]\n[config.env]\nPOSTGRES_DB = \"notes\"\nDFLY_requirepass = \"${pass}\"\n" + dokployRoute
	results := convertTree(t, Dokploy, DokploySource("abc"), map[string]string{
		"blueprints/notes/docker-compose.yml": compose,
		"blueprints/notes/template.toml":      template,
		"blueprints/2fa/docker-compose.yml":   "services:\n  web:\n    image: example/web:2.1.0\n    ports:\n      - 3000\n",
		"blueprints/2fa/template.toml":        dokployVars + dokployRoute,
	})
	e := entryOf(t, results, "notes")
	file := e.GetCompose()
	for _, want := range []string{"POSTGRES_DB=notes", "POSTGRES_DB: notes", "DFLY_requirepass=${DFLY_REQUIREPASS}", "image: example/web:2.1.0"} {
		if !strings.Contains(file, want) {
			t.Errorf("its file doesn't say %q:\n%s", want, file)
		}
	}
	in := map[string]*catalogv1.Input{}
	for _, i := range e.GetInputs() {
		in[i.GetName()] = i
	}
	if p := in["DFLY_REQUIREPASS"]; p.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || p.GetGenerate() != 32 {
		t.Errorf("DFLY_REQUIREPASS is %v", p)
	}
	if h := in["SMTP_HOST"]; !h.GetOptional() || !strings.Contains(file, "SMTP_HOST=${SMTP_HOST}") {
		t.Errorf("a name the template doesn't set is %v, in\n%s", h, file)
	}
	if _, ok := in["WEB_TAG"]; ok {
		t.Errorf("the image's tag is still an input")
	}
	if e.GetVersion() != "2.1.0" {
		t.Errorf("its version is %q, from the image's tag", e.GetVersion())
	}
	if r := results["2fa"]; r.ID != "app-2fa" || strings.Contains(r.LeftOut, "isn't a valid catalog entry name") {
		t.Errorf("2fa: %s, %q", r.ID, r.LeftOut)
	}
}

func TestCoolifyLeavesOutWhatAnEntryCantSay(t *testing.T) {
	apps := appsOf(t, Coolify, "testdata/coolify")
	for id, want := range map[string]string{
		"ignored": "Coolify marks it as one to leave out",
		"supa":    "it uses SERVICE_SUPABASEANON_KEY, which Coolify makes and Dockwell doesn't yet",
	} {
		r := templateConverter(CoolifySource("abc")).Convert(t.Context(), apps[id])
		if r.Entry != nil || !strings.Contains(r.LeftOut, want) {
			t.Errorf("%s: %q, want it left out as %q", id, r.LeftOut, want)
		}
	}
}

// What's converted from a source goes with its licence's text, and each file says
// its own licence.
func TestALicenceGoesWithWhatsConverted(t *testing.T) {
	src := CoolifySource("abc")
	src.LicenseText = []byte("the licence's text")
	c := templateConverter(src)
	r := c.Convert(t.Context(), appsOf(t, Coolify, "testdata/coolify")["board"])
	dir := t.TempDir()
	if err := Write(dir, src, []Result{r}, nil, c.Pins, true); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(filepath.Join(dir, "LICENSE"))
	if err != nil || string(text) != "the licence's text" {
		t.Errorf("LICENSE is %q, %v", text, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "board.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "# SPDX-License-Identifier: Apache-2.0\n# Converted from Coolify's templates") {
		t.Errorf("board.yaml starts %q", strings.SplitN(string(raw), "\n", 3)[:2])
	}
	if _, err := catalog.Parse(raw); err != nil {
		t.Errorf("the catalog refuses board.yaml: %v", err)
	}
}

// Neither template source is read past its checkout, and every reason has a class.
func TestTemplatesReadOnlyTheirCheckout(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "elsewhere.yml")
	if err := os.WriteFile(outside, []byte("services:\n  a:\n    image: example/a:1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dokploy := t.TempDir()
	blueprint := filepath.Join(dokploy, "blueprints", "linked")
	coolify := t.TempDir()
	templates := filepath.Join(coolify, "templates", "compose")
	for _, err := range []error{
		os.MkdirAll(blueprint, 0o755),
		os.Symlink(outside, filepath.Join(blueprint, "docker-compose.yml")),
		os.MkdirAll(templates, 0o755),
		os.Symlink(outside, filepath.Join(templates, "linked.yaml")),
		os.WriteFile(filepath.Join(templates, "plain.yaml"), []byte("# port: 80\nservices:\n  a:\n    image: example/a:1.0\n"), 0o600),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	apps := appsOf(t, Dokploy, dokploy)
	if a := apps["linked"]; a.Compose != nil || a.Class != ClassFile {
		t.Errorf("Dokploy: a link out of the checkout was read: %s, %q", a.Class, a.LeftOut)
	}
	apps = appsOf(t, Coolify, coolify)
	if _, ok := apps["linked"]; ok {
		t.Error("Coolify: a link out of the checkout was listed")
	}
	for _, read := range []struct {
		f    func(string) ([]App, error)
		root string
	}{{Dokploy, "testdata/dokploy"}, {Coolify, "testdata/coolify"}} {
		for _, a := range appsOf(t, read.f, read.root) {
			if a.LeftOut != "" && a.Class == "" {
				t.Errorf("%s is left out with no class: %q", a.ID, a.LeftOut)
			}
		}
	}
}

// M1: a store's app that locks memory with no limit is left out, as locked memory
// can't be reclaimed from the server; one with a limit converts (ADR 0085).
func TestAStoresAppLocksNoUnlimitedMemory(t *testing.T) {
	board, err := os.ReadFile("testdata/coolify/templates/compose/board.yaml")
	if err != nil {
		t.Fatal(err)
	}
	with := func(limits string) string {
		return strings.Replace(string(board), "    image: example/board:3.2.1\n", "    image: example/board:3.2.1\n    ulimits:\n"+limits, 1)
	}
	results := convertTree(t, Coolify, CoolifySource("abc"), map[string]string{
		"templates/compose/unlimited.yaml": with("      memlock: -1\n"),
		"templates/compose/softhard.yaml":  with("      memlock: {soft: -1, hard: -1}\n"),
		"templates/compose/limited.yaml":   with("      memlock: {soft: 65536, hard: 65536}\n      nofile: 65536\n"),
	})
	for _, id := range []string{"unlimited", "softhard"} {
		if r := results[id]; r.Entry != nil || r.Class != ClassCantRun || !strings.Contains(r.LeftOut, "locks memory with no limit") {
			t.Errorf("%s: %s, %q", id, r.Class, r.LeftOut)
		}
	}
	if r := results["limited"]; strings.Contains(r.LeftOut, "locks memory") {
		t.Errorf("a limited memlock: %q", r.LeftOut)
	}
}

// A file a Dokploy template writes into its files folder, which its Compose file
// mounts, is a file Dockwell writes, read-only, as the template wrote it: its $ are
// its own, and a value it makes up can't go in one (ADR 0085).
func TestADokployTemplatesFilesAreFilesDockwellWrites(t *testing.T) {
	compose := "services:\n  web:\n    image: example/broker:2.0.0\n    volumes:\n      - ../files/broker.conf:/etc/broker/broker.conf:ro\n      - data:/data\n"
	results := convertTree(t, Dokploy, DokploySource("abc"), map[string]string{
		"blueprints/broker/docker-compose.yml": compose,
		"blueprints/broker/template.toml": dokployVars + dokployRoute + "[[config.mounts]]\nfilePath = \"/broker.conf\"\n" +
			"content = \"\"\"\nlistener 1883\nlog_dest $stdout\n\"\"\"\n",
		"blueprints/secret/docker-compose.yml": compose,
		"blueprints/secret/template.toml": dokployVars + dokployRoute + "[[config.mounts]]\nfilePath = \"broker.conf\"\n" +
			"content = \"\"\"\npassword ${password:32}\n\"\"\"\n",
		// A config file is larger than any value a template sets: 20 KiB is a file's.
		"blueprints/large/docker-compose.yml": compose,
		"blueprints/large/template.toml": dokployVars + dokployRoute + "[[config.mounts]]\nfilePath = \"broker.conf\"\n" +
			"content = \"\"\"\n" + strings.Repeat("# a line of the broker's own settings\n", 512) + "\"\"\"\n",
		"blueprints/huge/docker-compose.yml": compose,
		"blueprints/huge/template.toml": dokployVars + dokployRoute + "[[config.mounts]]\nfilePath = \"broker.conf\"\n" +
			"content = \"\"\"\n" + strings.Repeat("# a line of the broker's own settings\n", 4000) + "\"\"\"\n",
	})
	e := entryOf(t, results, "broker")
	for _, want := range []string{"configs:\n", "source: web-etc-broker-broker.conf", "target: /etc/broker/broker.conf", "listener 1883\n", "log_dest $$stdout"} {
		if !strings.Contains(e.GetCompose(), want) {
			t.Errorf("its file doesn't say %q:\n%s", want, e.GetCompose())
		}
	}
	if strings.Contains(e.GetCompose(), "../files") || strings.Contains(e.GetCompose(), "./broker.conf") {
		t.Errorf("its file still mounts the template's file:\n%s", e.GetCompose())
	}
	if r := results["secret"]; r.Entry != nil || r.Class != ClassInside {
		t.Errorf("a made-up value in a file: %s, %q", r.Class, r.LeftOut)
	}
	if !strings.Contains(entryOf(t, results, "large").GetCompose(), "a line of the broker's own settings") {
		t.Errorf("a file of 20 KiB isn't written")
	}
	if r := results["huge"]; r.Entry != nil || r.Class != ClassShipped || !strings.Contains(r.LeftOut, "broker.conf: it holds over 128 KiB") {
		t.Errorf("a file of 150 KiB: %s, %q", r.Class, r.LeftOut)
	}
}
