// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"context"
	"regexp"
	"slices"
	"strconv"
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
		"user would be root":       func(e *catalogv1.Entry) { e.Needs[0].User = "root" },
		"user would be pg_monitor": func(e *catalogv1.Entry) { e.Needs[0].User = "pg_monitor" },
		"database would be mysql":  func(e *catalogv1.Entry) { e.Needs[0].Database, e.Needs[0].User = "mysql", "blog" },
		"user would be postgres":   func(e *catalogv1.Entry) { e.Needs[0].Name, e.Needs[0].Database, e.Needs[0].User = "postgres", "", "" },
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
	// MongoDB makes its first user its administrator, so none comes from the catalog
	// until Dockwell makes the app's user on it.
	e := fromAnotherStore(t)
	e.Needs[0].Accepts = []*catalogv1.NeedAccept{{Engine: catalogv1.Engine_ENGINE_MONGODB}}
	if ways := catalog.Ways(e, lookupBuiltIn(t))["db"]; len(ways) != 0 {
		t.Fatalf("a MongoDB from the catalog: %+v", ways)
	}
	if _, err := catalog.Meet(e, catalog.Ways(e, lookupBuiltIn(t)), nil, nil); err == nil || !strings.Contains(err.Error(), "nothing here can make a new one yet") {
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

// An upgrade keeps each need met as it was, but not where the release no longer takes
// what runs: another engine, a version outside the release's, or the release's own
// service on another engine, whose data the one that runs keeps in a form it can't
// read.
func TestAnUpgradeRefusesAWayTheReleaseDoesntTake(t *testing.T) {
	e := wordpressNeeds(t)
	lookup := lookupBuiltIn(t)
	installed, err := catalog.Meet(e, catalog.Ways(e, lookup), map[string]string{"db": "new:mysql"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fromCatalog, err := catalog.Need(e, installed)
	if err != nil {
		t.Fatal(err)
	}
	own, err := catalog.Need(e, mustMeet(t, e, lookup))
	if err != nil {
		t.Fatal(err)
	}
	for want, c := range map[string]struct {
		pinned  map[string]*resourcev1.PinnedNeed
		release func(*catalogv1.Entry)
	}{
		"takes MySQL >=": {fromCatalog.Pinned, func(e *catalogv1.Entry) {
			e.Needs[0].Accepts = []*catalogv1.NeedAccept{{Engine: catalogv1.Engine_ENGINE_MYSQL, Versions: ">=" + bumped(fromCatalog.Pinned["db"].GetVersion())}}
		}},
		"takes MariaDB": {fromCatalog.Pinned, func(e *catalogv1.Entry) {
			e.Needs[0].Accepts = e.Needs[0].Accepts[1:]
		}},
		"runs MySQL for its database, where the install runs MariaDB": {own.Pinned, func(e *catalogv1.Entry) {
			e.Compose = regexp.MustCompile(`image: mariadb:\S+`).ReplaceAllString(e.GetCompose(), "image: mysql:8.4.6")
		}},
	} {
		release := proto.CloneOf(e)
		c.release(release)
		_, err := catalog.Meet(release, catalog.Ways(release, lookup), nil, c.pinned)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "stays on its release") {
			t.Errorf("%s: %v", want, err)
		}
	}
	// What still takes it upgrades as it ran.
	if _, err := catalog.Meet(e, catalog.Ways(e, lookup), nil, fromCatalog.Pinned); err != nil {
		t.Errorf("the same release: %v", err)
	}
}

func mustMeet(t *testing.T, e *catalogv1.Entry, lookup catalog.Lookup) map[string]catalog.Way {
	t.Helper()
	met, err := catalog.Meet(e, catalog.Ways(e, lookup), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return met
}

// bumped is a version one major release past v.
func bumped(v string) string {
	major, _, _ := strings.Cut(v, ".")
	n, _ := strconv.Atoi(major)
	return strconv.Itoa(n+1) + ".0"
}

// An input of the app's own that a new one from the catalog would keep its own secret
// in is refused, not taken as that secret.
func TestANewOneDoesntTakeAnAppsInput(t *testing.T) {
	e := wordpressNeeds(t)
	e.Inputs = append(e.Inputs, &catalogv1.Input{Name: "DB_MYSQL_ROOT_PASSWORD", Label: "Admin password", Type: catalogv1.InputType_INPUT_TYPE_SECRET})
	e.Compose = strings.Replace(e.GetCompose(), "WORDPRESS_DB_NAME: ${DB_NAME}", "WORDPRESS_DB_NAME: ${DB_NAME}\n      WORDPRESS_ADMIN_PASSWORD: ${DB_MYSQL_ROOT_PASSWORD}", 1)
	lookup := lookupBuiltIn(t)
	met, err := catalog.Meet(e, catalog.Ways(e, lookup), map[string]string{"db": "new:mysql"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Need(e, met); err == nil || !strings.Contains(err.Error(), "has an input DB_MYSQL_ROOT_PASSWORD") {
		t.Errorf("an input the new MySQL's root password would take: %v", err)
	}
}

// What waited for the entry's own database waits for the new one once it's healthy,
// and the new one joins the networks the entry's own was on, or its readers are.
func TestANewOneIsWaitedForAndJoinsItsReadersNetworks(t *testing.T) {
	lookup := lookupBuiltIn(t)
	e := wordpressNeeds(t)
	e.Needs[0].Name = "database"
	e.Compose = strings.NewReplacer(
		"image: wordpress:", "networks: [backend]\n    image: wordpress:",
		"image: mariadb:", "networks:\n      backend:\n        aliases: [mysql]\n    image: mariadb:",
	).Replace(e.GetCompose()) + "networks:\n  backend: {}\n"
	if err := catalog.Check(e); err != nil {
		t.Fatal(err)
	}
	met, err := catalog.Meet(e, catalog.Ways(e, lookup), map[string]string{"database": "new:mysql"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := catalog.Need(e, met)
	if err != nil {
		t.Fatal(err)
	}
	file := needed.Entry.GetCompose()
	if !strings.Contains(file, "depends_on:\n      database:\n        condition: service_healthy") ||
		!regexp.MustCompile(`(?s)\n  database:\n.*networks:\n      backend:\n        aliases: \[mysql\]\n`).MatchString(file) {
		t.Errorf("WordPress doesn't wait for the new one, or it isn't where the old one was:\n%s", file)
	}
	if strings.Contains(file, "\n      db:\n") {
		t.Errorf("WordPress still waits for its own database:\n%s", file)
	}

	// From another store, the new one joins each network a reader is on.
	other := fromAnotherStore(t)
	other.Compose = strings.Replace(other.GetCompose(), "image: wordpress:", "networks: [front, back]\n    image: wordpress:", 1) + "networks:\n  front: {}\n  back: {}\n"
	met, err = catalog.Meet(other, catalog.Ways(other, lookup), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	needed, err = catalog.Need(other, met)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?s)\n {2}db:\n.*networks:\n {6}- back\n {6}- front\n`).MatchString(needed.Entry.GetCompose()) {
		t.Errorf("the new one isn't on the blog's networks:\n%s", needed.Entry.GetCompose())
	}
}

// An install's pin keeps the names its needs set that hold no password, so a change
// that reads the file it ran, as a change of companions does, uses them as it ran,
// whatever a newer release names (the review of #577, its L4). A pin from before it
// kept them reads the entry's.
func TestThePinKeepsTheNamesANeedSets(t *testing.T) {
	e := wordpressNeeds(t)
	needed, err := catalog.Need(e, mustMeet(t, e, lookupBuiltIn(t)))
	if err != nil {
		t.Fatal(err)
	}
	if got := needed.Pinned["db"].GetPlain(); !slices.Equal(got, []string{"DB_ADDRESS", "DB_NAME", "DB_USER"}) {
		t.Fatalf("the pin keeps %v", got)
	}
	release := proto.CloneOf(e)
	release.Needs[0].Sets[0].Name = "DB_HOST_AND_PORT"
	if got := catalog.PinnedPlain(needed.Pinned, release); !slices.Equal(got, []string{"DB_ADDRESS", "DB_NAME", "DB_USER"}) {
		t.Errorf("with a release that renames one, the names are %v", got)
	}
	old := map[string]*resourcev1.PinnedNeed{"db": {Engine: resourcev1.LinkEngine_LINK_ENGINE_MARIADB}}
	if got := catalog.PinnedPlain(old, e); !slices.Equal(got, catalog.PlainNames(e)) {
		t.Errorf("a pin from before reads %v", got)
	}
}

// A server Dockwell runs already is a way to meet a need where it can, listed after
// the new ones, the usable first; any other is listed with why, and never chosen.
// New stays the default.
func TestASharedServer(t *testing.T) {
	e := wordpressNeeds(t)
	good := catalog.Candidate{
		ServiceID: "svc_1", Service: "db", Project: "data", Environment: "production", EnvironmentID: "env_DATA",
		Engine: catalogv1.Engine_ENGINE_MARIADB, Version: "11.4.4", Servers: []string{"srv_1"}, JoinsInPlace: true,
		Databases: []string{"wordpress"}, Users: []string{"wordpress"},
	}
	old := good
	old.Service, old.ServiceID, old.Version = "old", "svc_2", "10.5.29"
	admin := good
	admin.Service, admin.ServiceID, admin.AdministeredBy = "nc-db", "svc_3", "Nextcloud"
	elsewhere := good
	elsewhere.Service, elsewhere.ServiceID, elsewhere.Servers = "far", "svc_4", []string{"srv_2"}
	pg := good
	pg.Service, pg.Engine = "pg", catalogv1.Engine_ENGINE_POSTGRESQL
	ways := catalog.WaysWith(e, lookupBuiltIn(t), []catalog.Candidate{old, admin, good, elsewhere, pg}, "srv_1")["db"]
	var got []string
	for _, w := range ways {
		got = append(got, w.Choice()+" "+w.Why)
	}
	want := []string{
		"new:mariadb ", "new:mysql ",
		"service:data/production/db ",
		"service:data/production/old Runs MariaDB 10.5.29; WordPress takes 10.6 or later",
		"service:data/production/nc-db Nextcloud signs in to it as its administrator",
		"service:data/production/far Runs on another server",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ways\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	met, err := catalog.Meet(e, catalog.WaysWith(e, lookupBuiltIn(t), []catalog.Candidate{good}, "srv_1"), nil, nil)
	if err != nil || met["db"].Shares() {
		t.Fatalf("with nothing asked, a server is shared: %v, %v", met["db"].Choice(), err)
	}
	if _, err := catalog.Meet(e, map[string][]catalog.Way{"db": ways}, map[string]string{"db": "service:data/production/nc-db"}, nil); err == nil || !strings.Contains(err.Error(), "signs in to it as its administrator") {
		t.Errorf("a greyed server chosen: %v", err)
	}
	met, err = catalog.Meet(e, map[string][]catalog.Way{"db": ways}, map[string]string{"db": "service:data/production/db"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := met["db"]
	w.Database, w.User = catalog.SharedNames(e.GetNeeds()[0], "blog", good)
	if w.Database != "blog" || w.User != "blog" {
		t.Errorf("on a server that has wordpress, the names are %s and %s, want the project's", w.Database, w.User)
	}
	w.Password = "BLOG_DATABASE_PASSWORD"
	needed, err := catalog.Need(e, map[string]catalog.Way{"db": w})
	if err != nil {
		t.Fatal(err)
	}
	file := needed.Entry.GetCompose()
	if strings.Contains(file, "image: mariadb") || strings.Contains(file, "depends_on") {
		t.Errorf("sharing a server, the file still runs its own or waits for it:\n%s", file)
	}
	defaults := map[string]string{}
	for _, in := range needed.Entry.GetInputs() {
		defaults[in.GetName()] = in.GetDefaultValue()
	}
	if defaults["DB_ADDRESS"] != "db:3306" || defaults["DB_NAME"] != "blog" || defaults["DB_USER"] != "blog" {
		t.Errorf("sharing a server, the app reads %v", defaults)
	}
	p := needed.Pinned["db"]
	if p.GetEnvironment() != "env_DATA" || p.GetMeets() != "db" || p.GetDatabase() != "blog" || p.GetPassword() != "BLOG_DATABASE_PASSWORD" || p.GetService() != "db" {
		t.Errorf("pinned %v", p)
	}
	if l := needed.Links["db"]; l.GetMeetsEnvironment() != "env_DATA" || l.GetService() != "db" || l.GetDatabaseName() != "blog" {
		t.Errorf("link %v", l)
	}
	if len(needed.Shared) != 1 {
		t.Errorf("shared %v", needed.Shared)
	}
	readsAsInstalled(t, needed.Entry)
	again, err := catalog.MetFile(e.GetCompose(), needed.Pinned)
	if err != nil || again != file {
		t.Errorf("MetFile reads otherwise: %v\n%s", err, again)
	}
}

// A new PostgreSQL from the catalog keeps the image's own user its administrator, with
// a password of the server's own, and the app's user, with its database, is one the
// plan makes, its password the need's secret (ADR 0084).
func TestANewPostgreSQLGetsAUserThePlanMakes(t *testing.T) {
	e := fromAnotherStore(t)
	e.Needs[0].Accepts = []*catalogv1.NeedAccept{{Engine: catalogv1.Engine_ENGINE_POSTGRESQL, Versions: ">=13"}}
	ways := catalog.Ways(e, lookupBuiltIn(t))
	if got := ways["db"]; len(got) != 1 || got[0].Choice() != "new:postgresql" || got[0].From.GetId() != "postgres" {
		t.Fatalf("ways %+v", got)
	}
	met, err := catalog.Meet(e, ways, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := catalog.Need(e, met)
	if err != nil {
		t.Fatal(err)
	}
	u := needed.Users["blog"]
	if len(needed.Users) != 1 || u.GetService() != "db" || u.GetPassword() != "PANEL_DB_USER_PASSWORD" || u.GetDatabaseName() != "" {
		t.Fatalf("users %v", needed.Users)
	}
	file := needed.Entry.GetCompose()
	// The app, which brought no database and waited for none, waits for this one.
	for _, want := range []string{"image: postgres:17", "POSTGRES_PASSWORD: ${DB_POSTGRES_PASSWORD}", "db-data:/var/lib/postgresql/data", "depends_on:\n      db:\n        condition: service_healthy"} {
		if !strings.Contains(file, want) {
			t.Errorf("the file has no %q:\n%s", want, file)
		}
	}
	for _, not := range []string{"POSTGRES_USER", "POSTGRES_DB", "adminer"} {
		if strings.Contains(file, not) {
			t.Errorf("the file has %q:\n%s", not, file)
		}
	}
	admin := slices.IndexFunc(needed.Entry.GetInputs(), func(in *catalogv1.Input) bool { return in.GetName() == "DB_POSTGRES_PASSWORD" })
	if admin < 0 || needed.Entry.GetInputs()[admin].GetGenerate() == 0 {
		t.Errorf("the administrator's password isn't one the install generates: %v", needed.Entry.GetInputs())
	}
	if l := needed.Links["db"]; l.GetUser() != "blog" || l.GetDatabaseName() != "blog" || l.GetService() != "db" {
		t.Errorf("link %v", l)
	}

	// An upgrade reads the service the install added from its pin, and makes the same user.
	pinned, err := catalog.Meet(e, catalog.Ways(e, lookupBuiltIn(t)), nil, needed.Pinned)
	if err != nil {
		t.Fatal(err)
	}
	again, err := catalog.Need(e, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(again.Users["blog"], u) {
		t.Errorf("an upgrade's user %v, the install's %v", again.Users["blog"], u)
	}
}

// A URL a need sets is a secret Dockwell writes from the need's parts, each encoded for
// where it stands: the engine's own form, or the entry's template. A part the need
// doesn't set gets a name of the app's environment of its own, named for the need.
func TestAURLIsWrittenFromItsParts(t *testing.T) {
	e := fromAnotherStore(t)
	indent := regexp.MustCompile(`\n( *)WORDPRESS_DB_NAME: \$\{PANEL_DB_NAME\}`).FindStringSubmatch(e.GetCompose())[1]
	e.Compose = strings.Replace(e.GetCompose(), "WORDPRESS_DB_NAME: ${PANEL_DB_NAME}", "WORDPRESS_DB_NAME: ${PANEL_DB_NAME}\n"+indent+"DATABASE_URL: ${DATABASE_URL}\n"+indent+"CACHE_URL: ${CACHE_URL}", 1)
	e.Needs[0].Accepts = []*catalogv1.NeedAccept{{Engine: catalogv1.Engine_ENGINE_POSTGRESQL}}
	e.Needs[0].Sets = append(e.Needs[0].Sets,
		&catalogv1.NeedSet{Name: "DATABASE_URL", Part: catalogv1.Part_PART_URL},
		&catalogv1.NeedSet{Name: "CACHE_URL", Part: catalogv1.Part_PART_URL, Template: "postgresql://{user}:{password}@{address}/{database}?sslmode={tls}&application_name=$blog", Tls: &catalogv1.NeedTls{On: "require", Off: "disable"}},
	)
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
	type name struct {
		name     string
		secret   bool
		encoding resourcev1.Encoding
	}
	read := func(tm *resourcev1.ValueTemplate) []name {
		var out []name
		for _, n := range tm.GetNames() {
			out = append(out, name{n.GetName(), n.GetSecret(), n.GetEncoding()})
		}
		return out
	}
	url := needed.Written["DATABASE_URL"]
	if url.GetText() != "postgres://${PANEL_DB_USER}:${PANEL_DB_USER_PASSWORD}@${PANEL_DB_HOST}:${DB_PORT}/${PANEL_DB_NAME}" {
		t.Errorf("DATABASE_URL is written as %q", url.GetText())
	}
	if got := read(url); !slices.Equal(got, []name{
		{"PANEL_DB_USER", false, resourcev1.Encoding_ENCODING_USERINFO},
		{"PANEL_DB_USER_PASSWORD", true, resourcev1.Encoding_ENCODING_USERINFO},
		{"PANEL_DB_HOST", false, resourcev1.Encoding_ENCODING_UNSPECIFIED},
		{"DB_PORT", false, resourcev1.Encoding_ENCODING_UNSPECIFIED},
		{"PANEL_DB_NAME", false, resourcev1.Encoding_ENCODING_PATH},
	}) {
		t.Errorf("DATABASE_URL names %v", got)
	}
	cache := needed.Written["CACHE_URL"]
	if cache.GetText() != "postgresql://${PANEL_DB_USER}:${PANEL_DB_USER_PASSWORD}@${PANEL_DB_HOST}:${DB_PORT}/${PANEL_DB_NAME}?sslmode=${DB_TLS}&application_name=$$blog" {
		t.Errorf("CACHE_URL is written as %q", cache.GetText())
	}
	if got := read(cache); got[len(got)-1] != (name{"DB_TLS", false, resourcev1.Encoding_ENCODING_QUERY}) {
		t.Errorf("CACHE_URL names %v", got)
	}
	// The parts it added are the environment's variables, as the need's own are.
	values := map[string]string{}
	for _, in := range needed.Entry.GetInputs() {
		values[in.GetName()] = in.GetDefaultValue()
	}
	if values["DB_PORT"] != "5432" || values["DB_TLS"] != "disable" || !slices.Contains(needed.Plain, "DB_PORT") || !slices.Contains(needed.Plain, "DB_TLS") {
		t.Errorf("inputs %v, plain %v", values, needed.Plain)
	}
	if slices.Contains(catalog.PlainNames(e), "DATABASE_URL") {
		t.Error("a URL reads as a plain name")
	}
}

// A need that takes Redis is met new by the catalog's Redis, which is the app's alone:
// most apps send a Redis no user, so the app signs in with the server's one password,
// the need's own secret. It publishes nothing, and keeps its data on a volume named for
// the need.
func TestANewRedisIsTheAppsAlone(t *testing.T) {
	e := fromAnotherStore(t)
	indent := regexp.MustCompile(`\n( *)WORDPRESS_DB_NAME: \$\{PANEL_DB_NAME\}`).FindStringSubmatch(e.GetCompose())[1]
	e.Compose = strings.Replace(e.GetCompose(), "WORDPRESS_DB_NAME: ${PANEL_DB_NAME}", "WORDPRESS_DB_NAME: ${PANEL_DB_NAME}\n"+indent+"REDIS_HOST: ${REDIS_HOST}\n"+indent+"REDIS_PASSWORD: ${REDIS_PASSWORD}\n"+indent+"REDIS_URL: ${REDIS_URL}", 1)
	e.Needs = append(e.Needs, &catalogv1.Need{
		Name: "cache", Label: "Cache",
		Accepts: []*catalogv1.NeedAccept{{Engine: catalogv1.Engine_ENGINE_REDIS, Versions: ">=6"}},
		Sets: []*catalogv1.NeedSet{
			{Name: "REDIS_HOST", Part: catalogv1.Part_PART_HOST},
			{Name: "REDIS_PASSWORD", Part: catalogv1.Part_PART_PASSWORD},
			{Name: "REDIS_URL", Part: catalogv1.Part_PART_URL},
		},
	})
	if err := catalog.Check(e); err != nil {
		t.Fatal(err)
	}
	ways := catalog.Ways(e, lookupBuiltIn(t))
	if got := ways["cache"]; len(got) != 1 || got[0].Choice() != "new:redis" {
		t.Fatalf("ways %+v", got)
	}
	met, err := catalog.Meet(e, ways, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := catalog.Need(e, met)
	if err != nil {
		t.Fatal(err)
	}
	file := needed.Entry.GetCompose()
	for _, want := range []string{"image: redis:8", `"--requirepass", "${REDIS_PASSWORD}"`, "REDISCLI_AUTH: ${REDIS_PASSWORD}", "cache-data:/data"} {
		if !strings.Contains(file, want) {
			t.Errorf("the file has no %s:\n%s", want, file)
		}
	}
	if strings.Contains(file, "CACHE_REDIS_PASSWORD") || strings.Contains(file, "6379:6379") || strings.Contains(file, "${PORT}:6379") {
		t.Errorf("the Redis keeps a password of its own or publishes its port:\n%s", file)
	}
	if len(needed.Users) != 0 {
		t.Errorf("users %v", needed.Users)
	}
	if url := needed.Written["REDIS_URL"].GetText(); url != "redis://${CACHE_USER}:${REDIS_PASSWORD}@${REDIS_HOST}:${CACHE_PORT}/${CACHE_DATABASE}" {
		t.Errorf("REDIS_URL is written as %q", url)
	}
	values := map[string]string{}
	for _, in := range needed.Entry.GetInputs() {
		values[in.GetName()] = in.GetDefaultValue()
	}
	if values["REDIS_HOST"] != "cache" || values["CACHE_USER"] != "" || values["CACHE_DATABASE"] != "0" {
		t.Errorf("inputs %v", values)
	}
}

// An entry's own database, pinned apart from its app as a person first shares it,
// stays as it ran through the app's upgrades: a release that moves its own database
// to another image leaves the shared one alone (ADR 0084).
func TestAnOwnServerPinnedApartStaysAsItRan(t *testing.T) {
	e := wordpressNeeds(t)
	lookup := lookupBuiltIn(t)
	installed, err := catalog.Need(e, mustMeet(t, e, lookup))
	if err != nil {
		t.Fatal(err)
	}
	ran, err := catalog.MetFile(e.GetCompose(), installed.Pinned)
	if err != nil {
		t.Fatal(err)
	}
	apart, err := catalog.PinApart(ran, "db", installed.Pinned["db"], "12.3.3")
	if err != nil {
		t.Fatal(err)
	}
	if apart.GetEntry() != "mariadb" || apart.GetService() != "db" || !strings.Contains(string(apart.GetFile()), "image: mariadb:12.3.3") || !strings.Contains(string(apart.GetFile()), "data:") {
		t.Fatalf("pinned apart as %v:\n%s", apart, apart.GetFile())
	}
	pinned := map[string]*resourcev1.PinnedNeed{"db": apart}
	release := proto.CloneOf(e)
	release.Compose = regexp.MustCompile(`image: mariadb:\S+`).ReplaceAllString(release.GetCompose(), "image: mariadb:12.4.1")
	met, err := catalog.Meet(release, catalog.Ways(release, lookup), nil, pinned)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := catalog.Need(release, met)
	if err != nil {
		t.Fatal(err)
	}
	file := upgraded.Entry.GetCompose()
	if !strings.Contains(file, "image: mariadb:12.3.3") || strings.Contains(file, "mariadb:12.4.1") {
		t.Errorf("the app's upgrade moved the shared MariaDB:\n%s", file)
	}
	if got := upgraded.Links["db"].GetService(); got != "db" {
		t.Errorf("the app reaches its database as %q", got)
	}
}
