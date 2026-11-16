// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// A value of a service's environment with a secret inside, as a connection URL holds
// a password, passes the catalog's check, and an install writes it as a secret of its
// own from a template of the environment's values: the password where it stands,
// encoded as a URL's userinfo, and the plain values written in once the install has
// them; a second service's value of the same name takes its service's name (#570).
func TestASecretInsideAValueIsWrittenFromATemplate(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := c.Entry("postgres")
	e := proto.CloneOf(base)
	e.Compose = strings.Replace(e.GetCompose(), "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}",
		"POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      DATABASE_URL: postgres://postgres:${POSTGRES_PASSWORD}@db:5432/app?sslmode=${MODE:-disable}\n      PRICE: $$5", 1)
	if err := catalog.Check(e); err != nil {
		t.Fatalf("an entry with a secret inside a value of its environment: %v", err)
	}
	needed, err := catalog.Need(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(needed.Entry.GetCompose(), "DATABASE_URL: ${DATABASE_URL}") || !strings.Contains(needed.Entry.GetCompose(), "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}") {
		t.Errorf("the install's Compose file:\n%s", needed.Entry.GetCompose())
	}
	var added bool
	for _, in := range needed.Entry.GetInputs() {
		if in.GetName() == "DATABASE_URL" {
			added = catalog.Secret(in) && in.GetGenerate() >= 16 && !catalog.Asks(in)
		}
	}
	if !added {
		t.Errorf("no generated secret DATABASE_URL among %v", needed.Entry.GetInputs())
	}
	if !needed.Inside["DATABASE_URL"] {
		t.Errorf("DATABASE_URL isn't marked inside: %v", needed.Inside)
	}
	tmpl := needed.Written["DATABASE_URL"]
	if got, want := tmpl.GetText(), "postgres://postgres:${POSTGRES_PASSWORD}@db:5432/app?sslmode=${MODE:-disable}"; got != want {
		t.Errorf("the template reads %q, want %q", got, want)
	}
	if len(tmpl.GetNames()) != 1 || tmpl.GetNames()[0].GetName() != "POSTGRES_PASSWORD" || !tmpl.GetNames()[0].GetSecret() ||
		tmpl.GetNames()[0].GetEncoding() != resourcev1.Encoding_ENCODING_USERINFO {
		t.Errorf("the template names %v, want POSTGRES_PASSWORD, a secret, as a URL's userinfo", tmpl.GetNames())
	}
	// Once the install has its values, the plain ones are written in, a $ of their own
	// as $$, and the secret stays named.
	needed.Fix(map[string]string{"POSTGRES_PASSWORD": "stand-in", "MODE": ""})
	if got, want := needed.Written["DATABASE_URL"].GetText(), "postgres://postgres:${POSTGRES_PASSWORD}@db:5432/app?sslmode=disable"; got != want {
		t.Errorf("fixed, the template reads %q, want %q", got, want)
	}
	needed.Fix(map[string]string{"MODE": "re$quire"})
	if got := needed.Written["DATABASE_URL"].GetText(); !strings.HasSuffix(got, "sslmode=disable") {
		t.Errorf("fixed again, the template reads %q, want it as it was", got)
	}
	// A plain value is encoded for where it stands, as a secret is (#570's review, L2).
	e2 := proto.CloneOf(base)
	e2.Compose = strings.Replace(e2.GetCompose(), "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}",
		"POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      DATABASE_URL: postgres://${DB_USER:-me@corp}:${POSTGRES_PASSWORD}@db:5432/${DB:-my db}?app=${APP:-a b}", 1)
	plain, err := catalog.Need(e2, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain.Fix(map[string]string{})
	if got, want := plain.Written["DATABASE_URL"].GetText(), "postgres://me%40corp:${POSTGRES_PASSWORD}@db:5432/my%20db?app=a%20b"; got != want {
		t.Errorf("plain values are written %q, want %q", got, want)
	}
	// A value with no secret inside, or a secret as the whole value, is left as it is.
	if _, ok := needed.Written["PRICE"]; ok {
		t.Error("PRICE, with no secret in it, is written from a template")
	}
}

// Two services with a value of one name, each with the secret inside, take a secret
// each, the second by its service's name, and an install made again names them the
// same, as an upgrade reads them (#570).
func TestValuesOfOneNameTakeASecretEach(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := c.Entry("postgres")
	e := proto.CloneOf(base)
	e.Compose = strings.Replace(e.GetCompose(), "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}",
		"POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      URL: x-${POSTGRES_PASSWORD}", 1)
	e.Compose = strings.Replace(e.GetCompose(), "services:\n", "services:\n  worker:\n    image: busybox:1.37@sha256:"+strings.Repeat("a", 64)+"\n    environment:\n      - URL=y-${POSTGRES_PASSWORD}\n", 1)
	var names []string
	for range 2 {
		needed, err := catalog.Need(e, nil)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for name := range needed.Inside {
			got = append(got, name)
		}
		if len(got) != 2 || !strings.Contains(needed.Entry.GetCompose(), "- URL=${") {
			t.Fatalf("names %v, file:\n%s", got, needed.Entry.GetCompose())
		}
		if names == nil {
			names = got
		}
		for _, n := range got {
			if !strings.Contains(strings.Join(names, " "), n) {
				t.Errorf("made again, a value is named %s, where it was one of %v", n, names)
			}
		}
	}
}

// An install that runs keeps the secret it names a value by, where a release names
// it otherwise, as a service the release adds before takes the name; the value the
// release adds takes a name nothing runs (#570's recheck, N1).
func TestNeedAsKeepsTheNamesThatRun(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := c.Entry("postgres")
	e := proto.CloneOf(base)
	e.Compose = strings.Replace(e.GetCompose(), "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}",
		"POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      URL: x-${POSTGRES_PASSWORD}", 1)
	first, err := catalog.Need(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	var key string
	for k, name := range first.InsideKeys {
		if name != "URL" {
			t.Fatalf("an install names %s %s", k, name)
		}
		key = k
	}
	e.Compose = strings.Replace(e.GetCompose(), "services:\n", "services:\n  worker:\n    image: busybox:1.37@sha256:"+strings.Repeat("a", 64)+"\n    environment:\n      - URL=y-${POSTGRES_PASSWORD}\n", 1)
	if fresh, err := catalog.Need(e, nil); err != nil || fresh.InsideKeys[key] == "URL" {
		t.Fatalf("a fresh install of the new release names %s URL, where the new service takes it: %v", key, err)
	}
	next, err := catalog.NeedAs(e, nil, map[string]string{key: "URL", "worker/OTHER": "WORKER_URL"})
	if err != nil {
		t.Fatal(err)
	}
	if got := next.InsideKeys[key]; got != "URL" {
		t.Errorf("what runs as URL is named %s", got)
	}
	if got := next.InsideKeys["worker/URL"]; got == "URL" || got == "WORKER_URL" || got == "" {
		t.Errorf("the value the release adds is named %q, a name that runs", got)
	}
}
