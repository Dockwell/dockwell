// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
)

// builtInPostgres is the built-in PostgreSQL, whose Adminer is a companion.
func builtInPostgres(t *testing.T) *catalogv1.Entry {
	t.Helper()
	c, err := catalog.BuiltIn("")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := c.Entry("postgres")
	if !ok {
		t.Fatal("the catalog has no postgres")
	}
	return e
}

// A database's admin tool is a companion, left out unless a person turns it on, and
// no other built-in service is one.
func TestADatabasesAdminToolIsACompanion(t *testing.T) {
	c, err := catalog.BuiltIn("")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"postgres": "adminer", "mysql": "phpmyadmin", "mariadb": "phpmyadmin"}
	for _, e := range c.All() {
		var got []string
		for _, comp := range e.GetCompanions() {
			got = append(got, comp.GetService())
			if comp.GetOn() {
				t.Errorf("%s's %s is on unless someone turns it off", e.GetId(), comp.GetService())
			}
		}
		if tool := want[e.GetId()]; !slices.Equal(got, slices.DeleteFunc([]string{tool}, func(s string) bool { return s == "" })) {
			t.Errorf("%s's companions are %v, want %q", e.GetId(), got, tool)
		}
	}
}

// Left out, a companion's service, its inputs and its notes go, and what's left reads
// with no value for them; on, the entry installs as it always did.
func TestChoosingCompanions(t *testing.T) {
	e := builtInPostgres(t)
	choice, err := catalog.Chosen(e, nil)
	if err != nil || !slices.Equal(catalog.Off(choice), []string{"adminer"}) {
		t.Fatalf("by default, %v, %v", choice, err)
	}
	chosen, err := catalog.Choose(e, choice)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(chosen.GetCompose(), "adminer") || strings.Contains(chosen.GetCompose(), "ADMIN_PORT") {
		t.Errorf("left out, the file still runs Adminer:\n%s", chosen.GetCompose())
	}
	if slices.ContainsFunc(chosen.GetInputs(), func(in *catalogv1.Input) bool { return in.GetName() == "ADMIN_PORT" }) {
		t.Error("left out, Adminer's port is still asked for")
	}
	if !strings.Contains(e.GetCompose(), "adminer") {
		t.Error("choosing changed the catalog's own entry")
	}
	filled, err := catalog.Fill(chosen, nil, catalog.Suggest(chosen, nil))
	if err != nil {
		t.Fatal(err)
	}
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(chosen.GetCompose()), Variables: filled.Variables})
	if err != nil {
		t.Fatal(err)
	}
	if len(stack.Services) != 1 || stack.Services[0].Name != "db" || len(stack.Volumes) != 1 {
		t.Errorf("left out, it runs %d services and %d volumes", len(stack.Services), len(stack.Volumes))
	}
	if words := catalog.FirstWords(e, choice); strings.Contains(words, "adminer") || strings.Contains(words, "Adminer") {
		t.Errorf("left out, the plan still says %q", words)
	}

	on, err := catalog.Chosen(e, map[string]bool{"adminer": true})
	if err != nil {
		t.Fatal(err)
	}
	if same, _ := catalog.Choose(e, on); same.GetCompose() != e.GetCompose() || len(same.GetInputs()) != len(e.GetInputs()) {
		t.Error("on, it doesn't install as it always did")
	}
	if words := catalog.FirstWords(e, on); !strings.Contains(words, "Sign in to adminer") || !strings.Contains(words, "Choose PostgreSQL as Adminer's system.") {
		t.Errorf("on, the plan says %q", words)
	}
	if _, err := catalog.Chosen(e, map[string]bool{"pgweb": true}); err == nil || !strings.Contains(err.Error(), "has no companion pgweb. It has adminer") {
		t.Errorf("a companion it hasn't = %v", err)
	}
	if err := catalog.LeftOut(e, choice, map[string]string{"ADMIN_PORT": "9000"}); err == nil || !strings.Contains(err.Error(), "ADMIN_PORT is Adminer's") {
		t.Errorf("a value for what's left out = %v", err)
	}
}

// A volume only a companion uses goes with it, and one another service uses stays.
func TestWithoutTakesAVolumeOnlyItUsed(t *testing.T) {
	file := "services:\n  db:\n    image: x\n    volumes:\n      - data:/data\n  tool:\n    image: y\n    volumes:\n      - data:/also\n      - type: volume\n        source: own\n        target: /own\n      - ./bind:/bind\nvolumes:\n  data: {}\n  own: {}\n"
	got, err := catalog.Without(file, []string{"tool"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "tool") || strings.Contains(got, "own") || !strings.Contains(got, "data: {}") {
		t.Errorf("Without gives\n%s", got)
	}
	only := "services:\n  db:\n    image: x\n  tool:\n    image: y\n    volumes:\n      - own:/own\nvolumes:\n  own: {}\n"
	if got, err := catalog.Without(only, []string{"tool"}); err != nil || strings.Contains(got, "volumes") {
		t.Errorf("with no volume left, Without gives %v\n%s", err, got)
	}
}

// An install from before companions keeps what it ran; a pin says what it chose; and
// a companion a newer release adds is as its entry says.
func TestResolvingAnInstallsCompanions(t *testing.T) {
	e := builtInPostgres(t)
	old := &resourcev1.CatalogPin{File: []byte(e.GetCompose())}
	if got := catalog.Resolve(e, old); !got["adminer"] {
		t.Errorf("an install from before companions resolves to %v", got)
	}
	chose := &resourcev1.CatalogPin{File: []byte(e.GetCompose()), Companions: map[string]bool{"adminer": false}}
	if got := catalog.Resolve(e, chose); got["adminer"] {
		t.Errorf("a pin that left it out resolves to %v", got)
	}
	without, err := catalog.Without(e.GetCompose(), []string{"adminer"})
	if err != nil {
		t.Fatal(err)
	}
	added := &resourcev1.CatalogPin{File: []byte(without)}
	if got := catalog.Resolve(e, added); got["adminer"] {
		t.Errorf("a companion a release adds, off by default, resolves to %v", got)
	}
}

// What a companion may be: a service the app runs without, whose inputs are its own
// and none a secret.
func TestCheckRefusesACompanion(t *testing.T) {
	base := builtInPostgres(t)
	for name, tc := range map[string]struct {
		edit func(*catalogv1.Entry)
		want string
	}{
		"a service it doesn't run": {func(e *catalogv1.Entry) { e.Companions[0].Service = "web" }, "doesn't run"},
		"its database":             {func(e *catalogv1.Entry) { e.Companions[0].Service, e.Databases = "db", []string{"db"} }, "one of its databases"},
		"twice": {func(e *catalogv1.Entry) {
			e.Companions = append(e.Companions, proto.CloneOf(e.Companions[0]))
		}, "twice"},
		"no label": {func(e *catalogv1.Entry) { e.Companions[0].Label = "" }, "needs a label"},
		"a long purpose": {func(e *catalogv1.Entry) {
			e.Companions[0].Purpose = strings.Repeat("words ", 12)
		}, "fewer words"},
		"one another depends on": {func(e *catalogv1.Entry) {
			e.Compose = strings.Replace(e.GetCompose(), "  db:\n    image:", "  db:\n    depends_on: [adminer]\n    image:", 1)
		}, "db depends on adminer"},
		"an input it doesn't name": {func(e *catalogv1.Entry) { e.Companions[0].Inputs = nil }, "only its companion adminer uses the input ADMIN_PORT"},
		"an input it hasn't":       {func(e *catalogv1.Entry) { e.Companions[0].Inputs = append(e.Companions[0].Inputs, "NOPE") }, "names the input NOPE"},
		"a secret":                 {func(e *catalogv1.Entry) { e.Companions[0].Inputs = append(e.Companions[0].Inputs, "POSTGRES_PASSWORD") }, "takes the secret POSTGRES_PASSWORD"},
		"an input used elsewhere": {func(e *catalogv1.Entry) {
			e.Compose = strings.Replace(e.GetCompose(), "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}", "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      PORT_TOO: ${ADMIN_PORT}", 1)
		}, "used beyond its companion adminer"},
		"every service": {func(e *catalogv1.Entry) {
			e.Compose = strings.Replace(e.GetCompose(), "    depends_on:\n      db:\n        condition: service_healthy\n", "", 1)
			e.Companions = append(e.Companions, &catalogv1.Companion{Service: "db", Label: "Database", Purpose: "Keeps data"})
		}, "could install nothing"},
		"a note of two lines": {func(e *catalogv1.Entry) { e.Companions[0].Notes = []string{"One.\nTwo."} }, "isn't one line"},
	} {
		t.Run(name, func(t *testing.T) {
			e := proto.CloneOf(base)
			tc.edit(e)
			if err := catalog.Check(e); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Check = %v, want %q", err, tc.want)
			}
		})
	}
}

// Turning a companion on later fills its inputs as an install would, from what's given
// or what's free, and takes nothing that isn't its own.
func TestCompanionValues(t *testing.T) {
	e := builtInPostgres(t)
	c, _ := catalog.Companion(e, "adminer")
	got, err := catalog.CompanionValues(e, c, nil, map[string]string{"ADMIN_PORT": "8082"})
	if err != nil || got["ADMIN_PORT"] != "8082" || len(got) != 1 {
		t.Errorf("suggested, = %v, %v", got, err)
	}
	if _, err := catalog.CompanionValues(e, c, map[string]string{"ADMIN_PORT": "99999"}, nil); err == nil || !strings.Contains(err.Error(), "takes a port") {
		t.Errorf("a bad port = %v", err)
	}
	if _, err := catalog.CompanionValues(e, c, map[string]string{"POSTGRES_PASSWORD": "x"}, nil); err == nil || !strings.Contains(err.Error(), "Adminer takes no input POSTGRES_PASSWORD") {
		t.Errorf("an input not its own = %v", err)
	}
}
