// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
)

// builtIn is a built-in entry by its ID.
func builtIn(t *testing.T, id string) *catalogv1.Entry {
	t.Helper()
	c, err := catalog.BuiltIn("")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := c.Entry(id)
	if !ok {
		t.Fatalf("the catalog has no %s", id)
	}
	return e
}

// lookupBuiltIn finds a built-in entry, as a need's new one from the catalog would.
func lookupBuiltIn(t *testing.T) catalog.Lookup {
	t.Helper()
	c, err := catalog.BuiltIn("")
	if err != nil {
		t.Fatal(err)
	}
	return func(id string) (*catalogv1.Entry, string, bool) {
		e, ok := c.Entry(id)
		return e, "dockwell", ok
	}
}

// wordpressNeeds is the built-in WordPress as ADR 0084's example rewrites it: its
// database a need its own MariaDB meets, or a MySQL, by the names it sets.
func wordpressNeeds(t *testing.T) *catalogv1.Entry {
	t.Helper()
	e := proto.CloneOf(builtIn(t, "wordpress"))
	r := strings.NewReplacer(
		"WORDPRESS_DB_HOST: db", "WORDPRESS_DB_HOST: ${DB_ADDRESS}",
		"WORDPRESS_DB_USER: wordpress", "WORDPRESS_DB_USER: ${DB_USER}",
		"WORDPRESS_DB_NAME: wordpress", "WORDPRESS_DB_NAME: ${DB_NAME}",
		"MARIADB_DATABASE: wordpress", "MARIADB_DATABASE: ${DB_NAME}",
		"MARIADB_USER: wordpress", "MARIADB_USER: ${DB_USER}",
	)
	e.Compose = r.Replace(e.GetCompose())
	e.Inputs = slices.DeleteFunc(e.Inputs, func(in *catalogv1.Input) bool { return in.GetName() == "MARIADB_PASSWORD" })
	e.Needs = []*catalogv1.Need{{
		Name: "db", Label: "Database", Service: "db", Database: "wordpress", User: "wordpress",
		Accepts: []*catalogv1.NeedAccept{
			{Engine: catalogv1.Engine_ENGINE_MYSQL, Versions: ">=8.0"},
			{Engine: catalogv1.Engine_ENGINE_MARIADB, Versions: ">=10.6"},
		},
		Sets: []*catalogv1.NeedSet{
			{Name: "DB_ADDRESS", Part: catalogv1.Part_PART_ADDRESS},
			{Name: "DB_NAME", Part: catalogv1.Part_PART_DATABASE},
			{Name: "DB_USER", Part: catalogv1.Part_PART_USER},
			{Name: "MARIADB_PASSWORD", Part: catalogv1.Part_PART_PASSWORD},
		},
	}}
	return e
}

// fromAnotherStore is an app as a converted 1Panel entry would be: it brings no
// database, and reads one by the names its need sets.
func fromAnotherStore(t *testing.T) *catalogv1.Entry {
	t.Helper()
	e := proto.CloneOf(builtIn(t, "wordpress"))
	e.Id, e.Name, e.Databases, e.Inputs = "blog", "Blog", nil, []*catalogv1.Input{e.GetInputs()[0]} // PORT
	file, err := catalog.Without(e.GetCompose(), []string{"db"})
	if err != nil {
		t.Fatal(err)
	}
	e.Compose = strings.NewReplacer(
		"WORDPRESS_DB_HOST: db", "WORDPRESS_DB_HOST: ${PANEL_DB_HOST}",
		"WORDPRESS_DB_USER: wordpress", "WORDPRESS_DB_USER: ${PANEL_DB_USER}",
		"WORDPRESS_DB_NAME: wordpress", "WORDPRESS_DB_NAME: ${PANEL_DB_NAME}",
		"WORDPRESS_DB_PASSWORD: ${MARIADB_PASSWORD}", "WORDPRESS_DB_PASSWORD: ${PANEL_DB_USER_PASSWORD}",
	).Replace(file)
	e.Compose = removeDependsOn(e.GetCompose())
	e.Needs = []*catalogv1.Need{{
		Name: "db", Label: "Database", Database: "blog",
		Accepts: []*catalogv1.NeedAccept{{Engine: catalogv1.Engine_ENGINE_MYSQL}, {Engine: catalogv1.Engine_ENGINE_MARIADB}},
		Sets: []*catalogv1.NeedSet{
			{Name: "PANEL_DB_HOST", Part: catalogv1.Part_PART_HOST},
			{Name: "PANEL_DB_NAME", Part: catalogv1.Part_PART_DATABASE},
			{Name: "PANEL_DB_USER", Part: catalogv1.Part_PART_USER},
			{Name: "PANEL_DB_USER_PASSWORD", Part: catalogv1.Part_PART_PASSWORD},
		},
	}}
	return e
}

// removeDependsOn takes WordPress's wait for its database out, as an entry that
// brings none waits for nothing.
func removeDependsOn(file string) string {
	var out []string
	skip := false
	for line := range strings.Lines(file) {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "depends_on:":
			skip = true
			continue
		case skip && (strings.HasPrefix(trimmed, "db:") || strings.HasPrefix(trimmed, "condition:")):
			continue
		}
		skip = false
		out = append(out, line)
	}
	return strings.Join(out, "")
}

func TestCheckTakesNeeds(t *testing.T) {
	for _, e := range []*catalogv1.Entry{wordpressNeeds(t), fromAnotherStore(t)} {
		if err := catalog.Check(e); err != nil {
			t.Errorf("%s: %v", e.GetId(), err)
		}
	}
}

func TestCheckRefusesANeed(t *testing.T) {
	for want, change := range map[string]func(*catalogv1.Entry){
		"label on one line": func(e *catalogv1.Entry) { e.Needs[0].Label = "" },
		"doesn't know":      func(e *catalogv1.Entry) { e.Needs[0].Accepts[0].Engine = catalogv1.Engine(99) },
		"twice":             func(e *catalogv1.Entry) { e.Needs[0].Accepts[1].Engine = catalogv1.Engine_ENGINE_MYSQL },
		"compare with":      func(e *catalogv1.Entry) { e.Needs[0].Accepts[0].Versions = "8 or later" },
		"names no engine":   func(e *catalogv1.Entry) { e.Needs[0].Accepts = nil },
		"is an input too": func(e *catalogv1.Entry) {
			e.Inputs = append(e.Inputs, &catalogv1.Input{Name: "DB_NAME", Label: "Database", Type: catalogv1.InputType_INPUT_TYPE_TEXT, DefaultValue: "x"})
		},
		"whole value of a service's environment": func(e *catalogv1.Entry) {
			e.Compose = strings.Replace(e.GetCompose(), "WORDPRESS_DB_HOST: ${DB_ADDRESS}", "WORDPRESS_DB_HOST: ${DB_ADDRESS}x", 1)
		},
		"only a URL takes":   func(e *catalogv1.Entry) { e.Needs[0].Sets[0].Template = "{host}" },
		"isn't one of":       func(e *catalogv1.Entry) { e.Databases = nil },
		"doesn't accept":     func(e *catalogv1.Entry) { e.Needs[0].Accepts = e.Needs[0].Accepts[:1] },
		"isn't one the need": func(e *catalogv1.Entry) { e.Needs[0].Accepts[1].Versions = "<12" },
		"doesn't read": func(e *catalogv1.Entry) {
			e.Compose = strings.Replace(e.GetCompose(), "MARIADB_USER: ${DB_USER}", "MARIADB_USER: wordpress", 1)
		},
		"set once":    func(e *catalogv1.Entry) { e.Needs[0].SetOnce = true; e.Needs[0].Sets = e.Needs[0].Sets[:3] },
		"doesn't use": func(e *catalogv1.Entry) { e.Needs[0].Sets[0].Name = "DB_ELSEWHERE" },
		"words for on and off": func(e *catalogv1.Entry) {
			e.Needs[0].Sets = append(e.Needs[0].Sets, &catalogv1.NeedSet{Name: "DB_TLS", Part: catalogv1.Part_PART_TLS})
			e.Compose = strings.Replace(e.GetCompose(), "WORDPRESS_DB_NAME: ${DB_NAME}", "WORDPRESS_DB_NAME: ${DB_NAME}\n      WORDPRESS_DB_TLS: ${DB_TLS}", 1)
		},
		"only a TLS part": func(e *catalogv1.Entry) {
			e.Needs[0].Sets[0].Tls = &catalogv1.NeedTls{On: "true", Off: "false"}
		},
	} {
		e := wordpressNeeds(t)
		change(e)
		err := catalog.Check(e)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	// A need that brings no service of its own takes its name for the one it adds.
	e := fromAnotherStore(t)
	e.Needs[0].Name = "wordpress"
	if err := catalog.Check(e); err == nil || !strings.Contains(err.Error(), "has a service called wordpress") {
		t.Errorf("a clash with a service: got %v", err)
	}
}

// The entry's own service comes first, then each other engine the catalog brings,
// where its image makes a user who isn't its administrator.
func TestWays(t *testing.T) {
	ways := catalog.Ways(wordpressNeeds(t), lookupBuiltIn(t))["db"]
	var got []string
	for _, w := range ways {
		got = append(got, w.Choice())
	}
	if !slices.Equal(got, []string{"new:mariadb", "new:mysql"}) {
		t.Fatalf("ways %v", got)
	}
	if ways[0].From != nil || ways[1].From.GetId() != "mysql" {
		t.Fatalf("own first, then the catalog's mysql: %+v", ways)
	}
	// An entry that brings none starts from the catalog, in the need's order.
	got = nil
	for _, w := range catalog.Ways(fromAnotherStore(t), lookupBuiltIn(t))["db"] {
		got = append(got, w.Choice())
	}
	if !slices.Equal(got, []string{"new:mysql", "new:mariadb"}) {
		t.Fatalf("ways %v", got)
	}
	// PostgreSQL makes its first user its administrator, so none comes from the
	// catalog until Dockwell makes the app's user itself.
	e := fromAnotherStore(t)
	e.Needs[0].Accepts = []*catalogv1.NeedAccept{{Engine: catalogv1.Engine_ENGINE_POSTGRESQL}}
	if ways := catalog.Ways(e, lookupBuiltIn(t))["db"]; len(ways) != 0 {
		t.Fatalf("a PostgreSQL from the catalog: %+v", ways)
	}
	if _, err := catalog.Meet(e, catalog.Ways(e, lookupBuiltIn(t)), nil, nil); err == nil || !strings.Contains(err.Error(), "nothing here can make one yet") {
		t.Fatalf("an unmet need: %v", err)
	}
}

func TestMeet(t *testing.T) {
	e := wordpressNeeds(t)
	ways := catalog.Ways(e, lookupBuiltIn(t))
	for asked, want := range map[string]string{"": "new:mariadb", "new": "new:mariadb", "new:mysql": "new:mysql"} {
		met, err := catalog.Meet(e, ways, map[string]string{"db": asked}, nil)
		if err != nil || met["db"].Choice() != want {
			t.Errorf("asked %q: %v, %v", asked, met["db"].Choice(), err)
		}
	}
	if _, err := catalog.Meet(e, ways, map[string]string{"db": "new:postgresql"}, nil); err == nil || !strings.Contains(err.Error(), "takes new:mariadb or new:mysql") {
		t.Errorf("an engine it doesn't take: %v", err)
	}
	if _, err := catalog.Meet(e, ways, map[string]string{"cache": "new"}, nil); err == nil || !strings.Contains(err.Error(), "has no need cache") {
		t.Errorf("a need it hasn't: %v", err)
	}
}

// A new MySQL in place of WordPress's own MariaDB: the catalog's service, under the
// need's name, publishing nothing, with its volume and its root password named for
// the need, and its image making the app's database and user from the need's names.
func TestNeedFromTheCatalog(t *testing.T) {
	e := wordpressNeeds(t)
	met, err := catalog.Meet(e, catalog.Ways(e, lookupBuiltIn(t)), map[string]string{"db": "new:mysql"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := catalog.Need(e, met)
	if err != nil {
		t.Fatal(err)
	}
	file := needed.Entry.GetCompose()
	for _, want := range []string{"image: mysql:8", "MYSQL_DATABASE: ${DB_NAME}", "MYSQL_USER: ${DB_USER}", "MYSQL_PASSWORD: ${MARIADB_PASSWORD}", "${DB_MYSQL_ROOT_PASSWORD}", "db-data:/var/lib/mysql", "depends_on"} {
		if !strings.Contains(file, want) {
			t.Errorf("its file has no %q:\n%s", want, file)
		}
	}
	for _, gone := range []string{"image: mariadb", "phpmyadmin", "ADMIN_PORT", "MARIADB_ROOT_PASSWORD"} {
		if strings.Contains(file, gone) {
			t.Errorf("its file still has %q:\n%s", gone, file)
		}
	}
	inputs := map[string]*catalogv1.Input{}
	for _, in := range needed.Entry.GetInputs() {
		inputs[in.GetName()] = in
	}
	if _, ok := inputs["MARIADB_ROOT_PASSWORD"]; ok {
		t.Error("the MariaDB it left out still asks for its root password")
	}
	if in := inputs["DB_MYSQL_ROOT_PASSWORD"]; in.GetGenerate() == 0 || !catalog.Secret(in) {
		t.Errorf("the new MySQL's root password: %v", in)
	}
	for name, want := range map[string]string{"DB_ADDRESS": "db:3306", "DB_NAME": "wordpress", "DB_USER": "wordpress"} {
		if got := inputs[name].GetDefaultValue(); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if in := inputs["MARIADB_PASSWORD"]; in.GetGenerate() != 32 || !catalog.Secret(in) {
		t.Errorf("the app's password: %v", in)
	}
	if !slices.Equal(needed.Plain, []string{"DB_ADDRESS", "DB_NAME", "DB_USER"}) {
		t.Errorf("plain %v", needed.Plain)
	}
	p := needed.Pinned["db"]
	if p.GetEntry() != "mysql" || p.GetService() != "db" || p.GetEngine() != resourcev1.LinkEngine_LINK_ENGINE_MYSQL || len(p.GetFile()) == 0 || p.GetCatalog() != "dockwell" {
		t.Errorf("pinned %v", p)
	}
	l := needed.Links["db"]
	if l.GetService() != "db" || l.GetEngine() != resourcev1.LinkEngine_LINK_ENGINE_MYSQL || l.GetDatabaseName() != "wordpress" || len(l.GetSets()) != 4 || len(l.GetAccepts()) != 2 {
		t.Errorf("link %v", l)
	}
	// It reads as an install reads it.
	readsAsInstalled(t, needed.Entry)
	// An upgrade reads the file it ran from its pin alone.
	again, err := catalog.MetFile(e.GetCompose(), needed.Pinned)
	if err != nil {
		t.Fatal(err)
	}
	if again != file {
		t.Errorf("MetFile gave another file:\n%s\nwant:\n%s", again, file)
	}
	// And meets it again as the pin says, with nothing asked.
	repeat, err := catalog.Meet(e, catalog.Ways(e, lookupBuiltIn(t)), nil, needed.Pinned)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := catalog.Need(e, repeat)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Entry.GetCompose() != file || !proto.Equal(upgraded.Pinned["db"], p) {
		t.Errorf("as the pin says, it reads otherwise:\n%s", upgraded.Entry.GetCompose())
	}
}

// Met by the entry's own service, the file is the entry's, and the pin says so.
func TestNeedByItsOwnService(t *testing.T) {
	e := wordpressNeeds(t)
	met, err := catalog.Meet(e, catalog.Ways(e, lookupBuiltIn(t)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := catalog.Need(e, met)
	if err != nil {
		t.Fatal(err)
	}
	if needed.Entry.GetCompose() != e.GetCompose() {
		t.Error("its own service changed the file")
	}
	if p := needed.Pinned["db"]; p.GetEntry() != "" || len(p.GetFile()) != 0 || p.GetEngine() != resourcev1.LinkEngine_LINK_ENGINE_MARIADB {
		t.Errorf("pinned %v", p)
	}
	readsAsInstalled(t, needed.Entry)
}

// An entry that brings no database gets one under its need's name.
func TestNeedForAnEntryThatBringsNone(t *testing.T) {
	e := fromAnotherStore(t)
	met, err := catalog.Meet(e, catalog.Ways(e, lookupBuiltIn(t)), map[string]string{"db": "new:mariadb"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := catalog.Need(e, met)
	if err != nil {
		t.Fatal(err)
	}
	file := needed.Entry.GetCompose()
	for _, want := range []string{"image: mariadb:", "MARIADB_DATABASE: ${PANEL_DB_NAME}", "MARIADB_PASSWORD: ${PANEL_DB_USER_PASSWORD}"} {
		if !strings.Contains(file, want) {
			t.Errorf("its file has no %q:\n%s", want, file)
		}
	}
	var host string
	for _, in := range needed.Entry.GetInputs() {
		if in.GetName() == "PANEL_DB_HOST" {
			host = in.GetDefaultValue()
		}
	}
	if host != "db" {
		t.Errorf("PANEL_DB_HOST = %q, want db", host)
	}
	readsAsInstalled(t, needed.Entry)
}

// Its plain values become the environment's variables: what the file reads stands
// for each, as a secret's does, and Linked keeps the value.
func TestFilledLink(t *testing.T) {
	e := wordpressNeeds(t)
	met, _ := catalog.Meet(e, catalog.Ways(e, lookupBuiltIn(t)), nil, nil)
	needed, err := catalog.Need(e, met)
	if err != nil {
		t.Fatal(err)
	}
	filled, err := catalog.Fill(needed.Entry, nil, catalog.Suggest(needed.Entry, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := filled.Link(needed.Plain); err != nil {
		t.Fatal(err)
	}
	if filled.Linked["DB_ADDRESS"] != "db:3306" || filled.Plain["DB_ADDRESS"] != "db:3306" {
		t.Errorf("linked %v, plain %v", filled.Linked, filled.Plain)
	}
	if stand := filled.LinkStands["DB_ADDRESS"]; stand == "" || filled.Variables["DB_ADDRESS"] != stand {
		t.Errorf("what stands for DB_ADDRESS: %q, %q", stand, filled.Variables["DB_ADDRESS"])
	}
}

// A reading of an entry with needs, to check it or list what it installs, values
// what its needs set as its default meets them.
func TestStanding(t *testing.T) {
	for _, e := range []*catalogv1.Entry{wordpressNeeds(t), fromAnotherStore(t)} {
		readsAsInstalled(t, catalog.Standing(e))
	}
	if e := builtIn(t, "postgres"); catalog.Standing(e) != e {
		t.Error("an entry with no needs changed")
	}
}

// readsAsInstalled fills an entry's inputs and reads its file, as an install would.
func readsAsInstalled(t *testing.T, e *catalogv1.Entry) {
	t.Helper()
	filled, err := catalog.Fill(e, nil, catalog.Suggest(e, nil))
	if err != nil {
		t.Fatalf("%s's form: %v", e.GetId(), err)
	}
	if _, err := compose.Read(context.Background(), compose.Input{File: []byte(e.GetCompose()), Variables: filled.Variables}); err != nil {
		t.Fatalf("%s's file: %v", e.GetId(), err)
	}
}

// A part a need's engine has none of, as a user on Redis, is set empty, and the
// install reads.
func TestAPartTheEngineHasNoneOf(t *testing.T) {
	redis := builtIn(t, "redis")
	image := ""
	for line := range strings.Lines(redis.GetCompose()) {
		if strings.Contains(line, "image: redis:") {
			image = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "image:"))
		}
	}
	e := proto.CloneOf(redis)
	e.Id, e.Name, e.Inputs, e.Companions, e.Web = "queue", "Queue", nil, nil, nil
	e.Databases = []string{"redis"}
	e.Compose = "services:\n" +
		"  app:\n    image: " + image + "\n    environment:\n      CACHE_HOST: ${CACHE_HOST}\n      CACHE_USER: ${CACHE_USER}\n      CACHE_PASSWORD: ${CACHE_PASSWORD}\n" +
		"  redis:\n    image: " + image + "\n    command: [\"redis-server\", \"--requirepass\", \"${CACHE_PASSWORD}\"]\n"
	e.Needs = []*catalogv1.Need{{
		Name: "cache", Label: "Cache", Service: "redis",
		Accepts: []*catalogv1.NeedAccept{{Engine: catalogv1.Engine_ENGINE_REDIS}},
		Sets: []*catalogv1.NeedSet{
			{Name: "CACHE_HOST", Part: catalogv1.Part_PART_HOST},
			{Name: "CACHE_USER", Part: catalogv1.Part_PART_USER},
			{Name: "CACHE_PASSWORD", Part: catalogv1.Part_PART_PASSWORD},
		},
	}}
	if err := catalog.Check(e); err != nil {
		t.Fatal(err)
	}
	met, err := catalog.Meet(e, catalog.Ways(e, lookupBuiltIn(t)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := catalog.Need(e, met)
	if err != nil {
		t.Fatal(err)
	}
	readsAsInstalled(t, needed.Entry)
	if l := needed.Links["cache"]; l.GetUser() != "" || l.GetDatabaseName() != "" || l.GetService() != "redis" {
		t.Errorf("a Redis link says %v", l)
	}
}
