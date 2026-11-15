// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"maps"
	"slices"
	"strings"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// A 1Panel app that uses another app's database server takes it as a need (ADR 0084):
// met by a new one beside it by default, 1Panel's default engine first, and by one a
// person runs, on which the app gets a user of its own. Its database's values aren't
// inputs, the file reads each as a whole value the need sets, and the engine's word is
// an engine part, in the app's own word where its init.sh turns 1Panel's into one.
func TestAOnePanelAppThatUsesADatabaseHasANeed(t *testing.T) {
	r := convertFixture(t, converter(), "withdb")
	if r.Entry == nil {
		t.Fatalf("withdb was left out: %s", r.LeftOut)
	}
	needs := r.Entry.GetNeeds()
	if len(needs) != 1 {
		t.Fatalf("needs = %v, want one", needs)
	}
	n := needs[0]
	// The form offers PostgreSQL and MySQL, and its URL is postgres://: PostgreSQL alone.
	if n.GetName() != "db" || len(n.GetAccepts()) != 1 || n.GetAccepts()[0].GetEngine() != catalogv1.Engine_ENGINE_POSTGRESQL {
		t.Errorf("need = %v, want db, PostgreSQL alone", n)
	}
	if n.GetDatabase() != "withdb" || n.GetUser() != "withdb" {
		t.Errorf("database %q, user %q, want withdb for both", n.GetDatabase(), n.GetUser())
	}
	got := map[string]string{}
	for _, s := range n.GetSets() {
		got[s.GetName()] = s.GetPart().String() + " " + s.GetTemplate()
	}
	want := map[string]string{
		"DB_HOST":      "PART_HOST ",
		"DB_TYPE":      "PART_ENGINE ",
		"DATABASE_URL": "PART_URL postgres://{user}:{password}@{host}:{port}/{database}",
	}
	if !maps.Equal(got, want) {
		t.Errorf("sets = %v, want %v", got, want)
	}
	for _, s := range n.GetSets() {
		if s.GetName() == "DB_TYPE" && (len(s.GetWords()) != 1 || s.GetWords()[0].GetWord() != "pgsql") {
			t.Errorf("DB_TYPE's words = %v, want PostgreSQL's alone, as the app's own pgsql", s.GetWords())
		}
	}
	file := r.Entry.GetCompose()
	for _, gone := range []string{"PANEL_DB", "${DB_NAME}", "${DB_USER}"} {
		if strings.Contains(file, gone) {
			t.Errorf("the file still names %s\n%s", gone, file)
		}
	}
	if !strings.Contains(file, "DB_TYPE=${DB_TYPE}") {
		t.Errorf("the file doesn't read the engine's word as the need sets it\n%s", file)
	}
	for _, in := range r.Entry.GetInputs() {
		if strings.HasPrefix(in.GetName(), "DB_") {
			t.Errorf("%s is an input, which the need sets", in.GetName())
		}
	}
	if err := catalog.Check(r.Entry); err != nil {
		t.Errorf("the entry fails the catalog's checks: %v", err)
	}
}

// needApp is an app of one service whose environment is env, with drafts of what it
// takes from another app's servers.
func needApp(env string, drafts ...NeedDraft) App {
	return App{
		ID:      "shop",
		Compose: []byte("services:\n  shop:\n    image: example/shop:1.0\n" + env),
		Needs:   drafts,
	}
}

// postgres is a database need's draft, by 1Panel's names for its parts.
func postgres() NeedDraft {
	return NeedDraft{
		Name: "db", Label: "Database", Engines: []catalogv1.Engine{catalogv1.Engine_ENGINE_POSTGRESQL},
		Parts: map[string]catalogv1.Part{
			"PANEL_DB_HOST": catalogv1.Part_PART_HOST, "PANEL_DB_PORT": catalogv1.Part_PART_PORT,
			"PANEL_DB_NAME": catalogv1.Part_PART_DATABASE, "PANEL_DB_USER": catalogv1.Part_PART_USER,
			"PANEL_DB_USER_PASSWORD": catalogv1.Part_PART_PASSWORD,
		},
	}
}

// typedDraft is a database need's draft whose app reads which engine it's on, on
// PostgreSQL or MySQL.
func typedDraft() NeedDraft {
	d := postgres()
	d.Engines = append(d.Engines, catalogv1.Engine_ENGINE_MYSQL)
	d.Parts["PANEL_DB_TYPE"] = catalogv1.Part_PART_ENGINE
	d.Words = map[catalogv1.Engine]string{catalogv1.Engine_ENGINE_POSTGRESQL: "pgsql", catalogv1.Engine_ENGINE_MYSQL: "mysql"}
	return d
}

// Each value a need sets is a whole value of a service's environment, as an entry's
// need sets it: a part used whole under its plain name, a host and port as an address,
// and a value made of the need's parts as a URL from a template.
func TestANeedSetsWholeValues(t *testing.T) {
	for name, tc := range map[string]struct {
		env  string
		sets map[string]string
		file []string
	}{
		"used whole": {
			env:  "    environment:\n      DB_PASSWORD: ${PANEL_DB_USER_PASSWORD}\n      DB_HOST: ${PANEL_DB_HOST}\n",
			sets: map[string]string{"DB_USER_PASSWORD": "PART_PASSWORD ", "DB_HOST": "PART_HOST "},
			file: []string{"DB_PASSWORD: ${DB_USER_PASSWORD}"},
		},
		"a host and its port": {
			env:  "    environment:\n      - WORDPRESS_DB_HOST=${PANEL_DB_HOST}:${PANEL_DB_PORT}\n      - WORDPRESS_DB_PASSWORD=${PANEL_DB_USER_PASSWORD}\n",
			sets: map[string]string{"WORDPRESS_DB_HOST": "PART_ADDRESS ", "DB_USER_PASSWORD": "PART_PASSWORD "},
			file: []string{"WORDPRESS_DB_HOST=${WORDPRESS_DB_HOST}"},
		},
		"a URL": {
			env:  "    environment:\n      - DATABASE_URL=postgresql://${PANEL_DB_USER}:${PANEL_DB_USER_PASSWORD}@${PANEL_DB_HOST}:5432/${PANEL_DB_NAME}?schema=public\n",
			sets: map[string]string{"DATABASE_URL": "PART_URL postgresql://{user}:{password}@{host}:5432/{database}?schema=public"},
			file: []string{"DATABASE_URL=${DATABASE_URL}"},
		},
	} {
		rw, err := rewrite(needApp(tc.env, postgres()))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(rw.needs) != 1 {
			t.Errorf("%s: needs = %v, want one", name, rw.needs)
			continue
		}
		got := map[string]string{}
		for _, s := range rw.needs[0].GetSets() {
			got[s.GetName()] = s.GetPart().String() + " " + s.GetTemplate()
		}
		if !maps.Equal(got, tc.sets) {
			t.Errorf("%s: sets = %v, want %v", name, got, tc.sets)
		}
		for _, want := range tc.file {
			if !strings.Contains(string(rw.file), want) {
				t.Errorf("%s: the file doesn't say %s\n%s", name, want, rw.file)
			}
		}
	}
}

// What a need can't write leaves the app out, saying why: a value mixing a need's
// parts with another variable or another need's, a default on a part, a part used
// outside a service's environment, a need with no host, and a database need with no
// password, as an app signs in to a server as a user of its own.
func TestWhatANeedCantWrite(t *testing.T) {
	redis := NeedDraft{
		Name: "redis", Label: "Redis", Engines: []catalogv1.Engine{catalogv1.Engine_ENGINE_REDIS},
		Parts: map[string]catalogv1.Part{"PANEL_REDIS_HOST": catalogv1.Part_PART_HOST},
	}
	for name, tc := range map[string]struct {
		app App
		why string
	}{
		"another variable": {
			app: needApp("    environment:\n      - DSN=${DRIVER}://${PANEL_DB_HOST}\n      - DRIVER=${DRIVER:-pg}\n", postgres()),
			why: "builds DSN from another app's ${PANEL_DB_HOST} and ${DRIVER}",
		},
		"two apps": {
			app: needApp("    environment:\n      - BOTH=${PANEL_DB_HOST},${PANEL_REDIS_HOST}\n", postgres(), redis),
			why: "builds BOTH from two other apps' values",
		},
		"a default": {
			app: needApp("    environment:\n      - DB_HOST=${PANEL_DB_HOST:-db}\n", postgres()),
			why: "gives DB_HOST a default or a check",
		},
		"outside the environment": {
			app: needApp("    command: wait-for ${PANEL_DB_HOST}\n    environment:\n      - DB_PASS=${PANEL_DB_USER_PASSWORD}\n", postgres()),
			why: "outside a service's environment",
		},
		"no host": {
			app: needApp("    environment:\n      - DB_PASS=${PANEL_DB_USER_PASSWORD}\n", postgres()),
			why: "with no host to reach it at",
		},
		"no password": {
			app: needApp("    environment:\n      - DB_HOST=${PANEL_DB_HOST}\n", postgres()),
			why: "with no password of its own",
		},
		"a URL of an engine it isn't offered": {
			app: needApp("    environment:\n      - URL=mongodb://${PANEL_DB_USER}:${PANEL_DB_USER_PASSWORD}@${PANEL_DB_HOST}/${PANEL_DB_NAME}\n", postgres()),
			why: "its URL for Database names an engine its form doesn't offer",
		},
	} {
		_, err := rewrite(tc.app)
		if err == nil || !strings.Contains(err.Error(), tc.why) || classOf(err, ClassFile) != ClassAnotherApp {
			t.Errorf("%s: %v, want it left out as another app's, %q", name, err, tc.why)
		}
	}
}

// A draft the file doesn't use is no need, a need named as a service is renamed, and
// a variable the source fills with the engine's word is an engine part, with the app's
// word for each engine the need accepts, which the catalog's checks take.
func TestANeedsNameAndWhatItLeaves(t *testing.T) {
	unused := NeedDraft{
		Name: "redis", Label: "Redis", Engines: []catalogv1.Engine{catalogv1.Engine_ENGINE_REDIS},
		Parts: map[string]catalogv1.Part{"PANEL_REDIS_HOST": catalogv1.Part_PART_HOST},
	}
	typed := typedDraft()
	app := needApp("    environment:\n      - DB_HOST=${PANEL_DB_HOST}\n      - DB_PASS=${PANEL_DB_USER_PASSWORD}\n      - DB_TYPE=${PANEL_DB_TYPE}\n  db:\n    image: example/other:1.0\n", typed, unused)
	rw, err := rewrite(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(rw.needs) != 1 || rw.needs[0].GetName() != "db-need" {
		t.Fatalf("needs = %v, want db alone, renamed from the service db", rw.needs)
	}
	n := rw.needs[0]
	if len(n.GetAccepts()) != 2 {
		t.Errorf("accepts = %v, want PostgreSQL and MySQL", n.GetAccepts())
	}
	var words []string
	for _, s := range n.GetSets() {
		if s.GetPart() == catalogv1.Part_PART_ENGINE {
			for _, w := range s.GetWords() {
				words = append(words, w.GetEngine().String()+"="+w.GetWord())
			}
		}
	}
	if !slices.Equal(words, []string{"ENGINE_POSTGRESQL=pgsql", "ENGINE_MYSQL=mysql"}) {
		t.Errorf("the engine part's words = %v, want pgsql for PostgreSQL and mysql for MySQL", words)
	}
	if !strings.Contains(string(rw.file), "DB_TYPE=${DB_TYPE}") {
		t.Errorf("the file doesn't read the engine's word as the need sets it\n%s", rw.file)
	}

	// The engine's word inside a longer value, as a URL's scheme, is no part: the need
	// takes its first engine alone, with that engine's word written in.
	app = needApp("    environment:\n      - DB_HOST=${PANEL_DB_HOST}\n      - DB_PASS=${PANEL_DB_USER_PASSWORD}\n      - DB_TYPE=${PANEL_DB_TYPE}\n      - DRIVER=pdo_${PANEL_DB_TYPE}\n", typed)
	if rw, err = rewrite(app); err != nil {
		t.Fatal(err)
	}
	if a := rw.needs[0].GetAccepts(); len(a) != 1 || a[0].GetEngine() != catalogv1.Engine_ENGINE_POSTGRESQL {
		t.Errorf("accepts = %v, want PostgreSQL alone, as the word is part of DRIVER", a)
	}
	if file := string(rw.file); !strings.Contains(file, "DRIVER=pdo_pgsql") || !strings.Contains(file, "DB_TYPE=pgsql") {
		t.Errorf("PostgreSQL's word isn't written in\n%s", file)
	}
	if slices.ContainsFunc(rw.needs[0].GetSets(), func(s *catalogv1.NeedSet) bool { return s.GetPart() == catalogv1.Part_PART_ENGINE }) {
		t.Errorf("sets = %v, want no engine part", rw.needs[0].GetSets())
	}

	// A URL that names its engine takes that engine alone, and its word alone.
	app = needApp("    environment:\n      - DB_TYPE=${PANEL_DB_TYPE}\n      - URL=mysql://${PANEL_DB_USER}:${PANEL_DB_USER_PASSWORD}@${PANEL_DB_HOST}/${PANEL_DB_NAME}\n", typed)
	if rw, err = rewrite(app); err != nil {
		t.Fatal(err)
	}
	if a := rw.needs[0].GetAccepts(); len(a) != 1 || a[0].GetEngine() != catalogv1.Engine_ENGINE_MYSQL {
		t.Errorf("accepts = %v, want MySQL alone, as its URL is mysql://", a)
	}
}

// field is a form field of 1Panel's, by its variable, its type and its key.
func field(env, typ, key string) onePanelField {
	return onePanelField{EnvKey: env, Type: typ, Key: key}
}

// The fields of 1Panel's form that take another app's server are needs: an apps field
// offers servers by engine, with 1Panel's default first, unless the file reads the
// engine's word, when the default is the need's alone; a service field's key names
// the engine; a Redis is met by Redis or Valkey. Those 1Panel fills from the server
// aren't fields of the app's own.
func TestOnePanelsFormMakesNeeds(t *testing.T) {
	apps := onePanelField{EnvKey: "PANEL_DB_TYPE", Type: "apps", Default: "mariadb", Child: &onePanelField{EnvKey: "PANEL_DB_HOST", Type: "service"}}
	for _, v := range []string{"mysql", "mariadb", "postgres"} {
		apps.Values = append(apps.Values, struct {
			Label string `yaml:"label"`
			Value any    `yaml:"value"`
		}{Value: v})
	}
	fields := []onePanelField{apps, field("PANEL_DB_NAME", "text", ""), field("PANEL_DB_USER_PASSWORD", "password", ""), field("PANEL_REDIS_HOST", "service", "redis")}
	needs, filled, err := onePanelNeeds(fields, []byte("DB=${PANEL_DB_HOST}"), nil)
	if err != nil {
		t.Fatal(err)
	}
	engines := func(d NeedDraft) []string {
		var out []string
		for _, e := range d.Engines {
			out = append(out, e.String())
		}
		return out
	}
	if len(needs) != 2 || !slices.Equal(engines(needs[0]), []string{"ENGINE_MARIADB", "ENGINE_MYSQL", "ENGINE_POSTGRESQL"}) || needs[0].Parts["PANEL_DB_TYPE"] != catalogv1.Part_PART_UNSPECIFIED {
		t.Errorf("the database need = %v, want MariaDB first, then MySQL and PostgreSQL", needs)
	}
	if len(needs) == 2 && (!slices.Equal(engines(needs[1]), []string{"ENGINE_REDIS", "ENGINE_VALKEY"}) || needs[1].Parts["PANEL_REDIS_ROOT_PASSWORD"] != catalogv1.Part_PART_PASSWORD) {
		t.Errorf("the Redis need = %v, want Redis or Valkey, with its password", needs[1])
	}
	for _, v := range []string{"PANEL_DB_TYPE", "PANEL_DB_HOST", "PANEL_DB_NAME", "PANEL_DB_USER_PASSWORD", "PANEL_REDIS_HOST"} {
		if !filled[v] {
			t.Errorf("%s is left a field of the app's own", v)
		}
	}

	// The file reads the engine's word: an engine part, with 1Panel's word for each
	// engine, or the app's own where its init.sh turns 1Panel's into one.
	needs, _, err = onePanelNeeds(fields[:3], []byte("DB_TYPE=${PANEL_DB_TYPE}"), map[string]string{"mariadb": "mysql"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[catalogv1.Engine]string{catalogv1.Engine_ENGINE_MARIADB: "mysql", catalogv1.Engine_ENGINE_MYSQL: "mysql", catalogv1.Engine_ENGINE_POSTGRESQL: "postgres"}
	if !slices.Equal(engines(needs[0]), []string{"ENGINE_MARIADB", "ENGINE_MYSQL", "ENGINE_POSTGRESQL"}) || needs[0].Parts["PANEL_DB_TYPE"] != catalogv1.Part_PART_ENGINE || !maps.Equal(needs[0].Words, want) {
		t.Errorf("a typed need = %v, want every engine, with an engine part written as %v", needs[0], want)
	}
}

// What Dockwell can't meet leaves the app out, saying why: an app it has no engine
// for, a server's administrator, and two servers 1Panel names alike.
func TestWhatOnePanelsFormCantAsk(t *testing.T) {
	for name, tc := range map[string]struct {
		fields []onePanelField
		why    string
	}{
		"Kafka":         {fields: []onePanelField{field("KAFKA_HOST", "service", "kafka")}, why: "it uses kafka from another app, for its KAFKA_HOST, which Dockwell has no engine for"},
		"administrator": {fields: []onePanelField{field("PANEL_DB_HOST", "service", "mysql"), field("PANEL_DB_ROOT_PASSWORD", "password", "")}, why: "as its administrator (PANEL_DB_ROOT_PASSWORD)"},
		"two databases": {fields: []onePanelField{field("PANEL_DB_HOST", "service", "mysql"), field("OTHER_DB_HOST", "service", "postgresql")}, why: "two database servers"},
	} {
		_, _, err := onePanelNeeds(tc.fields, nil, nil)
		if err == nil || !strings.Contains(err.Error(), tc.why) || classOf(err, ClassFile) != ClassAnotherApp {
			t.Errorf("%s: %v, want %q", name, err, tc.why)
		}
	}
}
