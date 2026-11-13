// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// bundledEntry is an entry as the converter has it before needs, with a stand-in
// summary and category, as an overlay gives them.
func bundledEntry(file string, inputs ...*catalogv1.Input) *catalogv1.Entry {
	return &catalogv1.Entry{
		Id: "tracker", Name: "Tracker", Version: "1.0.0", Release: 1, Summary: "Stands in", Category: "ai",
		License: "MIT", Compose: file, Inputs: inputs,
		Source: &catalogv1.Source{Repository: "https://example.com/stores/tracker", Path: "tracker", Commit: "0123456789abcdef0123456789abcdef01234567"},
	}
}

func generated(name string) *catalogv1.Input {
	return &catalogv1.Input{Name: name, Label: name, Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32}
}

func text(name, value string) *catalogv1.Input {
	return &catalogv1.Input{Name: name, Label: name, Type: catalogv1.InputType_INPUT_TYPE_TEXT, DefaultValue: value}
}

// setsOf is a need's sets as name=part, with the template of a URL.
func setsOf(n *catalogv1.Need) []string {
	var out []string
	for _, s := range n.GetSets() {
		w := s.GetName() + "=" + strings.TrimPrefix(s.GetPart().String(), "PART_")
		if s.GetTemplate() != "" {
			w += " " + s.GetTemplate()
		}
		out = append(out, w)
	}
	return out
}

// checkNeeded holds an entry with needs to the catalog's checks, and to what an
// install of it does as the catalog meets needs today: wait for a URL Dockwell
// writes, until it can.
func checkNeeded(t *testing.T, e *catalogv1.Entry) {
	t.Helper()
	// As the control plane converts a store, its images by their tags (ADR 0078).
	if err := catalog.CheckTagged(e); err != nil {
		t.Fatalf("the catalog refuses it: %v\n%s", err, e.GetCompose())
	}
	err := needsWait(e)
	if writesURLs() != (err == nil) {
		t.Errorf("an install that writes URLs is %v, and it waits as %v", writesURLs(), err)
	}
	if err != nil && classOf(err, "") != ClassURL {
		t.Errorf("it waits as %s", classOf(err, ""))
	}
}

// Dokploy's shape: the database's user and name written out, its password made up,
// and the URL naming all three. The database meets the app's need, which sets what
// it's made with and the URL, as a template of them, which the app reads whole.
func TestADatabaseAURLReachesIsTheAppsNeed(t *testing.T) {
	e := bundledEntry(`services:
  app:
    image: example.com/tracker:1.0.0
    environment:
      DATABASE_URL: postgresql://tracker:${DB_PASSWORD}@db:5432/tracker?sslmode=disable
      DB_PASSWORD: ${DB_PASSWORD}
    depends_on:
      db:
        condition: service_healthy
  db:
    image: postgres:17-alpine
    environment:
      POSTGRES_DB: tracker
      POSTGRES_USER: tracker
      POSTGRES_PASSWORD: ${DB_PASSWORD}
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U $${POSTGRES_USER} -d $${POSTGRES_DB}"]
    volumes:
      - db-data:/var/lib/postgresql/data
volumes:
  db-data: {}
`, generated("DB_PASSWORD"))
	if err := bundledNeeds(e); err != nil {
		t.Fatal(err)
	}
	if len(e.GetNeeds()) != 1 {
		t.Fatalf("its needs are %v", e.GetNeeds())
	}
	n := e.GetNeeds()[0]
	want := []string{"POSTGRES_USER=USER", "DB_PASSWORD=PASSWORD", "POSTGRES_DB=DATABASE", "DATABASE_URL=URL postgresql://{user}:{password}@{host}:{port}/{database}?sslmode=disable"}
	if n.GetName() != "db" || n.GetService() != "db" || n.GetUser() != "tracker" || n.GetDatabase() != "tracker" ||
		n.GetAccepts()[0].GetEngine() != catalogv1.Engine_ENGINE_POSTGRESQL || !slices.Equal(setsOf(n), want) {
		t.Errorf("its need is %v, setting %q", n, setsOf(n))
	}
	for _, s := range []string{"DATABASE_URL: ${DATABASE_URL}", "DB_PASSWORD: ${DB_PASSWORD}", "POSTGRES_USER: ${POSTGRES_USER}", "POSTGRES_DB: ${POSTGRES_DB}", "pg_isready -U $$POSTGRES_USER -d $$POSTGRES_DB"} {
		if !strings.Contains(e.GetCompose(), s) {
			t.Errorf("its file doesn't say %q:\n%s", s, e.GetCompose())
		}
	}
	if len(e.GetInputs()) != 0 || !slices.Equal(e.GetDatabases(), []string{"db"}) {
		t.Errorf("its inputs are %v, and its databases %v", e.GetInputs(), e.GetDatabases())
	}
	checkNeeded(t, e)
}

// Coolify's shape: the user and the database are inputs with defaults, the user one
// PostgreSQL keeps for itself, and the check names them in exec form. The need
// takes them over, with a user of the app's own, and the check reads them from the
// database's environment.
func TestAStoresOwnUserAndDatabaseInputsAreTheNeeds(t *testing.T) {
	e := bundledEntry(`services:
  tracker:
    image: example.com/tracker:1.0.0
    environment:
      - DATABASE_URL=postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@postgresql:5432/${POSTGRES_DB}
      - DB_NAME=${POSTGRES_DB:-tracker}
  postgresql:
    image: postgres:16-alpine
    environment:
      - POSTGRES_USER=${POSTGRES_USER}
      - POSTGRES_PASSWORD=${POSTGRES_PASSWORD}
      - POSTGRES_DB=${POSTGRES_DB:-tracker}
    healthcheck:
      test: ["CMD", "pg_isready", "-U", "${POSTGRES_USER}", "-d", "${POSTGRES_DB:-tracker}"]
`, text("POSTGRES_USER", "postgres"), generated("POSTGRES_PASSWORD"), text("POSTGRES_DB", "tracker"))
	if err := bundledNeeds(e); err != nil {
		t.Fatal(err)
	}
	if len(e.GetNeeds()) != 1 {
		t.Fatalf("its needs are %v", e.GetNeeds())
	}
	n := e.GetNeeds()[0]
	want := []string{"POSTGRES_USER=USER", "POSTGRES_PASSWORD=PASSWORD", "POSTGRES_DB=DATABASE", "DATABASE_URL=URL postgres://{user}:{password}@{host}:{port}/{database}"}
	if n.GetUser() != "tracker" || n.GetDatabase() != "tracker" || !slices.Equal(setsOf(n), want) {
		t.Errorf("its need is %v, setting %q", n, setsOf(n))
	}
	for _, s := range []string{"DATABASE_URL=${DATABASE_URL}", "POSTGRES_DB=${POSTGRES_DB}", "DB_NAME=${POSTGRES_DB}\n", "[CMD-SHELL, ", "pg_isready -U $$POSTGRES_USER -d $$POSTGRES_DB"} {
		if !strings.Contains(e.GetCompose(), s) {
			t.Errorf("its file doesn't say %q:\n%s", s, e.GetCompose())
		}
	}
	if len(e.GetInputs()) != 0 {
		t.Errorf("its inputs are %v", e.GetInputs())
	}
	checkNeeded(t, e)
}

// A Redis the app reaches by a URL with its password is its need too, and a URL in
// an extension, which only gave the services their anchors, goes with it.
func TestARedisAURLReachesIsTheAppsNeed(t *testing.T) {
	e := bundledEntry(`x-env: &env
  REDIS_URL: redis://:${REDIS_PASSWORD}@redis:6379/0
services:
  app:
    image: example.com/tracker:1.0.0
    environment:
      REDIS_URL: redis://:${REDIS_PASSWORD}@redis:6379/0
  worker:
    image: example.com/tracker:1.0.0
    environment:
      QUEUE: redis://:${REDIS_PASSWORD}@redis:6379/0
  redis:
    image: redis:7-alpine
    command: redis-server --requirepass ${REDIS_PASSWORD}
`, generated("REDIS_PASSWORD"))
	if err := bundledNeeds(e); err != nil {
		t.Fatal(err)
	}
	if len(e.GetNeeds()) != 1 {
		t.Fatalf("its needs are %v", e.GetNeeds())
	}
	n := e.GetNeeds()[0]
	if n.GetLabel() != "Cache" || !slices.Equal(setsOf(n), []string{"REDIS_PASSWORD=PASSWORD", "REDIS_URL=URL redis://:{password}@{host}:{port}/0"}) {
		t.Errorf("its need is %v, setting %q", n, setsOf(n))
	}
	for _, s := range []string{"REDIS_URL: ${REDIS_URL}", "QUEUE: ${REDIS_URL}"} {
		if !strings.Contains(e.GetCompose(), s) {
			t.Errorf("its file doesn't say %q:\n%s", s, e.GetCompose())
		}
	}
	if strings.Contains(e.GetCompose(), "x-env") {
		t.Errorf("its extension stays:\n%s", e.GetCompose())
	}
	checkNeeded(t, e)
}

// A URL that doesn't read as the database's own leaves it as it is, with the
// secret inside a longer value, which is left out as that.
func TestAURLThatIsntTheDatabasesOwnStaysOut(t *testing.T) {
	const file = `services:
  app:
    image: example.com/tracker:1.0.0
    environment:
      DATABASE_URL: URL
  db:
    image: postgres:17-alpine
    environment:
      POSTGRES_USER: tracker
      POSTGRES_DB: tracker
      POSTGRES_PASSWORD: ${DB_PASSWORD}
`
	for name, url := range map[string]string{
		"another host":     "postgres://tracker:${DB_PASSWORD}@elsewhere:5432/tracker",
		"another port":     "postgres://tracker:${DB_PASSWORD}@db:6543/tracker",
		"another user":     "postgres://admin:${DB_PASSWORD}@db:5432/tracker",
		"another database": "postgres://tracker:${DB_PASSWORD}@db:5432/other",
		"no URL":           "user=tracker password=${DB_PASSWORD} host=db",
	} {
		t.Run(name, func(t *testing.T) {
			e := bundledEntry(strings.Replace(file, "URL\n", url+"\n", 1), generated("DB_PASSWORD"))
			was := proto.Clone(e)
			if err := bundledNeeds(e); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(e, was) {
				t.Errorf("it became %v", e)
			}
			if err := secretInside(e); err == nil || classOf(err, "") != ClassInside || !strings.Contains(err.Error(), "its app's DATABASE_URL puts the secret DB_PASSWORD inside a longer value") {
				t.Errorf("it's left out as %v", err)
			}
		})
	}
}

// What an install does is the catalog's to say: an entry whose need is a URL with
// its password waits for Dockwell to write one, and one with none doesn't.
func TestANeedWaitsOnlyForWhatAnInstallCantDo(t *testing.T) {
	plain := &catalogv1.Entry{Needs: []*catalogv1.Need{{Name: "db", Service: "db", Sets: []*catalogv1.NeedSet{{Name: "PW", Part: catalogv1.Part_PART_PASSWORD}}}}}
	if err := needsWait(plain); err != nil {
		t.Errorf("a need with a password alone waits: %v", err)
	}
	if writesURLs() {
		t.Skip("an install writes a URL with a password now")
	}
	url := proto.CloneOf(plain)
	url.Needs[0].Sets = append(url.Needs[0].Sets, &catalogv1.NeedSet{Name: "URL", Part: catalogv1.Part_PART_URL, Template: "postgres://{user}:{password}@{host}/{database}"})
	if err := needsWait(url); err == nil || classOf(err, "") != ClassURL || !strings.Contains(err.Error(), "its database db by a URL with its password in it, URL") {
		t.Errorf("a need with a URL with its password waits as %v", err)
	}
}

// A password a store writes out, in a database's own value and in its URL, becomes a
// secret Dockwell generates, which the URL names, so the database may be the app's
// need. Anywhere else, the value stays, and the app is left out as before.
func TestAWrittenOutPasswordInAURLNamesItsSecret(t *testing.T) {
	for s, want := range map[string]string{
		"postgres://app:hunter2@db:5432/app":               "postgres://app:${DB_PASSWORD}@db:5432/app",
		"DATABASE_URL=postgresql://app:hunter2@db/app?x=1": "DATABASE_URL=postgresql://app:${DB_PASSWORD}@db/app?x=1",
		"user=app password=hunter2 host=db":                "user=app password=hunter2 host=db",
		"postgres://app:hunter2@db:5432/app?pass=hunter2":  "postgres://app:hunter2@db:5432/app?pass=hunter2",
		"redis://:hunter2@cache:6379/0":                    "redis://:${DB_PASSWORD}@cache:6379/0",
		"http://not:a:url:hunter2@host":                    "http://not:a:url:hunter2@host",
	} {
		if got := urlPassword(s, "hunter2", "DB_PASSWORD"); got != want {
			t.Errorf("urlPassword(%q) = %q, want %q", s, got, want)
		}
	}
}
