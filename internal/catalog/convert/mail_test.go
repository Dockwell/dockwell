// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// A store app whose form declares a mail server's host, port, sign-in and how it's
// secured, as the reviewed table reads them, converts with an optional need of a
// mail server in place of those inputs, its security in the app's words; its
// sender stays an input, as the table maps none (ADR 0099).
func TestAStoreAppsMailServerIsANeed(t *testing.T) {
	store := storeOf(t, FormatRuntipi)
	src := Source{Name: store.Name, Repository: "https://example.com/appstore", Commit: strings.Repeat("a", 40), License: store.License}
	run, err := OnServer(t.Context(), store, listingOf(t, store, "testdata/runtipi", nil), src, committed, 0)
	if err != nil {
		t.Fatal(err)
	}
	r := resultsByID(run)["plausible-ce"]
	if r.Entry == nil {
		t.Fatalf("plausible-ce was left out: %s", r.LeftOut)
	}
	e := r.Entry
	if err := catalog.CheckTagged(e); err != nil {
		t.Fatalf("its entry fails the catalog's check: %v", err)
	}
	if len(e.GetNeeds()) != 1 {
		t.Fatalf("its needs are %v", e.GetNeeds())
	}
	n := e.GetNeeds()[0]
	if n.GetName() != "mail" || n.GetKind() != catalogv1.Kind_KIND_SMTP || !n.GetOptional() {
		t.Errorf("its need is %v", n)
	}
	parts := map[string]catalogv1.Part{}
	for _, s := range n.GetSets() {
		parts[s.GetName()] = s.GetPart()
	}
	want := map[string]catalogv1.Part{
		"PLAUSIBLE_SMTP_HOST_ADDR": catalogv1.Part_PART_HOST, "PLAUSIBLE_SMTP_HOST_PORT": catalogv1.Part_PART_PORT,
		"PLAUSIBLE_SMTP_USER_NAME": catalogv1.Part_PART_USER, "PLAUSIBLE_SMTP_USER_PWD": catalogv1.Part_PART_PASSWORD,
		"PLAUSIBLE_SMTP_HOST_SSL_ENABLED": catalogv1.Part_PART_SECURITY,
	}
	for name, p := range want {
		if parts[name] != p {
			t.Errorf("%s is %v, want %v", name, parts[name], p)
		}
	}
	// It takes TLS alone: it can't be told to require STARTTLS, nor to go without.
	for security, why := range map[string]string{"tls": "", "starttls": "require STARTTLS", "none": "go without TLS"} {
		if got := catalog.SecurityWhy(n, security); why == "" && got != "" || why != "" && !strings.Contains(got, why) {
			t.Errorf("a %s connection: %q", security, got)
		}
	}
	var inputs []string
	for _, in := range e.GetInputs() {
		inputs = append(inputs, in.GetName())
	}
	for name := range want {
		if slices.Contains(inputs, name) {
			t.Errorf("%s is an input too: %v", name, inputs)
		}
	}
	if !slices.Contains(inputs, "PLAUSIBLE_MAILER_EMAIL") {
		t.Errorf("the sender isn't an input: %v", inputs)
	}
	if c := e.GetCompose(); !strings.Contains(c, "SMTP_HOST_SSL_ENABLED=${PLAUSIBLE_SMTP_HOST_SSL_ENABLED}\n") {
		t.Errorf("its security keeps its default, which a need can't set:\n%s", c)
	}
}

// A mail server's set short of any part, or used inside another value, stays inputs.
func TestAMailSetShortOfAPartStaysInputs(t *testing.T) {
	t.Parallel()
	draft := mailFor("runtipi", "plausible-ce")
	fields := []Field{}
	for _, v := range []string{"PLAUSIBLE_SMTP_HOST_ADDR", "PLAUSIBLE_SMTP_HOST_PORT", "PLAUSIBLE_SMTP_USER_NAME", "PLAUSIBLE_SMTP_USER_PWD", "PLAUSIBLE_SMTP_HOST_SSL_ENABLED"} {
		fields = append(fields, Field{Name: v, Kind: Text, Optional: true})
	}
	whole := "services:\n  app:\n    image: example/app:1\n    environment:\n" +
		"      - A=${PLAUSIBLE_SMTP_HOST_ADDR}\n      - B=${PLAUSIBLE_SMTP_HOST_PORT}\n      - C=${PLAUSIBLE_SMTP_USER_NAME}\n" +
		"      - D=${PLAUSIBLE_SMTP_USER_PWD}\n      - E=${PLAUSIBLE_SMTP_HOST_SSL_ENABLED}\n"
	for name, c := range map[string]struct {
		file   string
		fields []Field
	}{
		"whole":                  {whole, fields},
		"no security field":      {whole, fields[:4]},
		"a password, no user":    {whole, slices.Delete(slices.Clone(fields), 2, 3)},
		"a host inside a URL":    {strings.Replace(whole, "A=${PLAUSIBLE_SMTP_HOST_ADDR}", "A=smtp://${PLAUSIBLE_SMTP_HOST_ADDR}", 1), fields},
		"a host outside the env": {whole + "    command: \"run ${PLAUSIBLE_SMTP_HOST_ADDR}\"\n", fields},
	} {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(c.file), &doc); err != nil {
			t.Fatal(err)
		}
		n, names := applyMail(doc.Content[0], []string{"app"}, App{Fields: c.fields, Mail: draft}, nil, map[string]bool{})
		if (n != nil) != (name == "whole") {
			t.Errorf("%s: need %v, names %v", name, n, names)
		}
	}
}
