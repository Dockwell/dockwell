// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
)

// What ADR 0029 asks of every entry holds of the ones built in, and each one's
// Compose file goes through what an import takes, with nothing a plan would
// have to warn of.
func TestTheBuiltInEntries(t *testing.T) {
	c, err := catalog.Load("abc123")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range c.All() {
		ids = append(ids, e.GetId())
		t.Run(e.GetId(), func(t *testing.T) {
			if e.GetSource().GetCommit() != "abc123" {
				t.Errorf("its commit = %q, not the build's", e.GetSource().GetCommit())
			}
			if e.GetAppLicense() == "" {
				t.Error("it doesn't say the app's licence")
			}
			given := map[string]string{}
			for _, in := range e.GetInputs() {
				if catalog.Asks(in) && in.GetDefaultValue() == "" {
					given[in.GetName()] = "someone@example.com"
					if catalog.Secret(in) {
						given[in.GetName()] = "typed-by-a-person-xyz"
					}
				}
			}
			filled, err := catalog.Fill(e, given, catalog.Suggest(e, nil))
			if err != nil {
				t.Fatal(err)
			}
			stack, err := compose.Read(t.Context(), compose.Input{File: []byte(e.GetCompose()), Variables: filled.Variables})
			if err != nil {
				t.Fatalf("its Compose file: %v", err)
			}
			if len(stack.Exposed) > 0 {
				t.Errorf("its Compose file writes a value in plain sight: %+v", stack.Exposed)
			}
			if w := e.GetWeb().GetService(); w != "" && !slices.ContainsFunc(stack.Services, func(s compose.Item[*resourcev1.Service]) bool { return s.Name == w }) {
				t.Errorf("it opens %s, which it doesn't run", w)
			}
			// A secret must be a whole value, as a rotation otherwise can't reach it.
			for name, value := range filled.Secrets {
				whole := false
				for _, s := range stack.Services {
					for k, v := range s.Spec.GetApp().GetEnv() {
						if v == value {
							whole = true
						} else if strings.Contains(v, value) {
							t.Errorf("%s holds %s inside a longer value", k, name)
						}
					}
				}
				if !whole {
					t.Errorf("no service has %s as a whole environment value", name)
				}
			}
		})
	}
	// The first set: a database with its admin tool, a reverse proxy's
	// replacement and a tunnel.
	for _, want := range []string{"postgres", "mysql", "nginx-proxy-manager", "cloudflared"} {
		if !slices.Contains(ids, want) {
			t.Errorf("the catalog has no %s: %v", want, ids)
		}
	}
}

// The catalog's page says its apps are open source, which each has to be. A licence
// new to this list needs a look before it goes on it, and an app that isn't open
// source needs the page's words changed.
func TestEveryAppIsOpenSource(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	open := []string{"Apache-2.0", "GPL-2.0-only", "MIT", "PostgreSQL"}
	for _, e := range c.All() {
		if !slices.Contains(open, e.GetAppLicense()) {
			t.Errorf("%s is under %s, and the catalog's page says it's open source", e.GetId(), e.GetAppLicense())
		}
	}
}

// An admin tool on a published port answers over plain HTTP, so a password typed into
// it crosses the network in clear. A person hears that before they apply, with what to
// do about it, and first, as the rest of the steps are signing in.
func TestAnAdminToolSaysItsPlainHTTP(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"postgres", "mysql"} {
		e, _ := c.Entry(id)
		steps := e.GetFirstSteps()
		at := strings.Index(steps, "plain HTTP")
		if at < 0 || !strings.Contains(steps, "in clear") || !strings.Contains(steps, "Add a domain") {
			t.Errorf("%s doesn't say its admin tool is plain HTTP, and what to do: %q", id, steps)
		}
		if signIn := strings.Index(steps, "sign in"); signIn >= 0 && signIn < at && !strings.HasPrefix(steps[signIn:], "signing") {
			t.Errorf("%s tells a person to sign in before it says that: %q", id, steps)
		}
	}
}

// These apps read their password from the environment on first start alone, so a
// rotation leaves the old one signing in, and the panel revealing a new one that
// doesn't. Each entry says so, and how to change the password instead.
func TestAPasswordReadOnFirstStartSaysSo(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for id, how := range map[string]string{
		"postgres":            "ALTER USER postgres PASSWORD",
		"mysql":               "ALTER USER",
		"nginx-proxy-manager": "change the password in the console",
	} {
		e, _ := c.Entry(id)
		if steps := e.GetFirstSteps(); !strings.Contains(steps, "on first start only") || !strings.Contains(steps, how) {
			t.Errorf("%s doesn't say its password is set once, and %q: %q", id, how, steps)
		}
	}
}

// Upstream's backend/setup.js writes the first password in the log, which takes no
// passkey to read, so this entry can't let the sheet call it sealed.
func TestNginxProxyManagersPasswordIsMarkedLogged(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := c.Entry("nginx-proxy-manager")
	for _, in := range e.GetInputs() {
		if want := in.GetName() == "ADMIN_PASSWORD"; in.GetLogged() != want {
			t.Errorf("%s has logged %t, want %t", in.GetName(), in.GetLogged(), want)
		}
	}
}

// The checks the catalog's CI will make, on what would get past a look.
func TestCheckRefuses(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := c.Entry("postgres")
	for name, tc := range map[string]struct {
		edit func(*catalogv1.Entry)
		want string
	}{
		"no licence": {func(e *catalogv1.Entry) { e.License = "" }, "no license"},
		"no source":  {func(e *catalogv1.Entry) { e.Source = nil }, "no source's repository"},
		"a tag alone": {func(e *catalogv1.Entry) {
			e.Compose = strings.Replace(e.GetCompose(), "@sha256:748a18a54d3842d43bc156f0840d78029b8d349e0feca98c12b9c4ce3404a6b3", "", 1)
		}, "isn't pinned by digest"},
		"no release":    {func(e *catalogv1.Entry) { e.Release = 0 }, "no release"},
		"unused input":  {func(e *catalogv1.Entry) { e.Inputs[0].Name = "NOT_USED" }, "doesn't use the input NOT_USED"},
		"shared secret": {func(e *catalogv1.Entry) { e.Inputs[1].DefaultValue = "hunter2" }, "everyone who installs it would share"},
		"short secret":  {func(e *catalogv1.Entry) { e.Inputs[1].Generate = 8 }, "at least 16"},
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

func TestParseRefusesAFieldTheFormatHasnt(t *testing.T) {
	if _, err := catalog.Parse([]byte("id: x\nnot_a_field: 1\n")); err == nil || !strings.Contains(err.Error(), "not_a_field") {
		t.Errorf("Parse = %v", err)
	}
}

// A port someone took moves on, and two inputs of one entry never land on one.
func TestSuggestMovesPastWhatsTaken(t *testing.T) {
	e := &catalogv1.Entry{Inputs: []*catalogv1.Input{
		{Name: "A", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "8080"},
		{Name: "B", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "8080"},
		{Name: "C", DefaultValue: "plain"},
		{Name: "D", Type: catalogv1.InputType_INPUT_TYPE_SECRET},
	}}
	got := catalog.Suggest(e, map[uint32]bool{8080: true, 8082: true})
	if got["A"] != "8081" || got["B"] != "8083" || got["C"] != "plain" {
		t.Errorf("Suggest = %v", got)
	}
	if _, ok := got["D"]; ok {
		t.Error("it suggests a secret")
	}
}

// Generated secrets differ each time, stay in the secrets, and aren't in what the
// pin keeps. What a person gives is checked by its type, and a refusal never
// quotes a value.
func TestFill(t *testing.T) {
	e := &catalogv1.Entry{Name: "Thing", Inputs: []*catalogv1.Input{
		{Name: "PORT", Label: "Port", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "8081"},
		{Name: "EMAIL", Label: "Your email", Type: catalogv1.InputType_INPUT_TYPE_EMAIL},
		{Name: "TOKEN", Label: "Token", Type: catalogv1.InputType_INPUT_TYPE_SECRET},
		{Name: "KEY", Label: "Key", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32},
		{Name: "NOTE", Label: "Note", Optional: true},
	}}
	given := func(over map[string]string) map[string]string {
		out := map[string]string{"EMAIL": "a@example.com", "TOKEN": "t0ken"}
		for k, v := range over {
			out[k] = v
		}
		return out
	}
	one, err := catalog.Fill(e, given(nil), catalog.Suggest(e, nil))
	if err != nil {
		t.Fatal(err)
	}
	two, err := catalog.Fill(e, given(nil), catalog.Suggest(e, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Secrets["KEY"]) != 32 || one.Secrets["KEY"] == two.Secrets["KEY"] {
		t.Errorf("generated %q and %q", one.Secrets["KEY"], two.Secrets["KEY"])
	}
	if _, ok := one.Plain["KEY"]; ok || one.Plain["TOKEN"] != "" {
		t.Errorf("the pin would keep a secret: %v", one.Plain)
	}
	if one.Variables["PORT"] != "8081" || one.Plain["PORT"] != "8081" || one.Secrets["TOKEN"] != "t0ken" {
		t.Errorf("Fill = %+v", one)
	}
	for name, tc := range map[string]struct {
		over map[string]string
		want string
	}{
		"bad port":   {map[string]string{"PORT": "99999"}, "Port takes a port"},
		"not a port": {map[string]string{"PORT": "web"}, "Port takes a port"},
		"bad email":  {map[string]string{"EMAIL": "not an address"}, "Your email takes an email address"},
		"no value":   {map[string]string{"TOKEN": " "}, "Token needs a value"},
		"two lines":  {map[string]string{"NOTE": "a\nb"}, "Note takes one line"},
		"no such":    {map[string]string{"OTHER": "x"}, "takes no input OTHER"},
		"generated":  {map[string]string{"KEY": "mine"}, "takes no value for KEY"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := catalog.Fill(e, given(tc.over), catalog.Suggest(e, nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Fill = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "t0ken") {
				t.Errorf("a refusal quotes a value: %v", err)
			}
		})
	}
}

// An upgrade gives what the install was given, a default to what's new, and reads no
// secret: one that's held stands for itself, and only a new one is made.
func TestCarry(t *testing.T) {
	e := &catalogv1.Entry{Name: "Thing", Inputs: []*catalogv1.Input{
		{Name: "PORT", Label: "Port", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "8081"},
		{Name: "ADDED", Label: "Added", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "9000"},
		{Name: "KEPT", Label: "Kept", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32},
		{Name: "NEW", Label: "New", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32},
		{Name: "TYPED", Label: "Typed", Type: catalogv1.InputType_INPUT_TYPE_SECRET},
	}}
	pin := &resourcev1.CatalogPin{Values: map[string]string{"PORT": "8090", "GONE": "x"}}
	filled, err := catalog.Carry(e, pin, nil, catalog.Suggest(e, nil), map[string]bool{"KEPT": true, "TYPED": true})
	if err != nil {
		t.Fatal(err)
	}
	if filled.Variables["PORT"] != "8090" || filled.Variables["ADDED"] != "9000" {
		t.Errorf("Carry gives %v", filled.Variables)
	}
	if _, ok := filled.Plain["GONE"]; ok {
		t.Errorf("the pin keeps an input the entry dropped: %v", filled.Plain)
	}
	if _, ok := filled.Secrets["KEPT"]; ok || filled.Held["KEPT"] == "" || filled.Held["TYPED"] == "" {
		t.Errorf("a held secret reads %+v", filled)
	}
	if len(filled.Secrets["NEW"]) != 32 {
		t.Errorf("a new secret reads %q", filled.Secrets["NEW"])
	}
	if got := filled.Stands(); len(got) != 3 || got["NEW"] == "" || got["KEPT"] != filled.Held["KEPT"] {
		t.Errorf("Stands = %v", got)
	}
	if _, err := catalog.Carry(e, pin, nil, catalog.Suggest(e, nil), map[string]bool{"KEPT": true}); err == nil || !strings.Contains(err.Error(), "Typed needs a value") {
		t.Errorf("a secret this release adds with no value = %v", err)
	}
}

// An install's old file reads again from its pin alone: what it kept, and a stand-in
// for a secret, which it never kept.
func TestGivenReadsAPinBack(t *testing.T) {
	variables, stands, err := catalog.Given(&resourcev1.CatalogPin{
		File:   []byte("a: ${PORT}\nb: ${PASSWORD}\nc: ${PASSWORD}\n"),
		Values: map[string]string{"PORT": "8081", "UNUSED": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if variables["PORT"] != "8081" || len(variables) != 2 || len(stands) != 1 {
		t.Errorf("variables %v, stands %v", variables, stands)
	}
	if s := stands["PASSWORD"]; len(s) != 32 || variables["PASSWORD"] != s {
		t.Errorf("the secret's stand-in is %q", s)
	}
}

// A directory of entries takes over from one built in, and adds to the rest.
func TestADirectoryOfEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	built, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := built.Entry("postgres")
	// A release of its own, in the entry's own words.
	raw, err := os.ReadFile("entries/postgres.yaml")
	if err != nil {
		t.Fatal(err)
	}
	newer := strings.Replace(string(raw), "release: 1\n", "release: 9\n", 1)
	if err := os.WriteFile(filepath.Join(dir, "postgres.yaml"), []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := c.Entry("postgres")
	if got.GetRelease() != 9 || len(c.All()) != len(built.All()) || got.GetName() != mine.GetName() {
		t.Errorf("Load gives release %d of %d entries", got.GetRelease(), len(c.All()))
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("id: broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Load(""); err == nil || !strings.Contains(err.Error(), "broken.yaml") {
		t.Errorf("a broken entry in the directory = %v", err)
	}
}
