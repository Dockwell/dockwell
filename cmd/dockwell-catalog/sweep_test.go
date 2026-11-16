// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/catalog/convert"
	"github.com/dockwell/dockwell/internal/resource/variable"
)

// Every converted app of every store, at the commits the record pins, as an install
// with its needs' first ways makes it: no secret stays inside a longer value of a
// service's environment, and each template, written with a password of every
// character a URL treats apart, parses back to it (#570's review). It reads the
// stores over the network, so it runs only with CATALOG_SWEEP naming a file for its
// report: CATALOG_SWEEP=/tmp/sweep.txt go test -run TestEveryConvertedApp ./cmd/dockwell-catalog
func TestEveryConvertedAppsSecretsAreWritten(t *testing.T) {
	if os.Getenv("CATALOG_SWEEP") == "" {
		t.Skip("CATALOG_SWEEP names no file for its report")
	}
	raw, err := os.ReadFile(RatesFile)
	if err != nil {
		raw, err = os.ReadFile("../../" + RatesFile)
	}
	if err != nil {
		t.Fatal(err)
	}
	var rec ratesRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	own, err := catalog.BuiltIn("")
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(os.Getenv("CATALOG_SWEEP"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	const nasty = "p@ss:w/rd?#%+&= é$x"
	for _, s := range rec.Stores {
		store, _ := convert.StoreOf(s.Format)
		run, err := fetchStore(t.Context(), store, s.Repository, s.Commit)
		if err != nil {
			t.Fatal(err)
		}
		besideOwn(run, own)
		for i := range run.Results {
			r := &run.Results[i]
			if r.Entry == nil {
				continue
			}
			e := r.Entry
			id := s.Format + "/" + r.ID
			met, err := catalog.Meet(e, catalog.Ways(e, func(id string) (*catalogv1.Entry, string, bool) { x, ok := own.Entry(id); return x, "", ok }), nil, nil)
			if err != nil {
				fmt.Fprintf(out, "%s: MEET %v\n", id, err)
				continue
			}
			needed, err := catalog.Need(e, met)
			if err != nil {
				fmt.Fprintf(out, "%s: NEED %v\n", id, err)
				continue
			}
			secret := map[string]bool{}
			for _, in := range needed.Entry.GetInputs() {
				if catalog.Secret(in) {
					secret[in.GetName()] = true
				}
			}
			for _, v := range envValues(t, needed.Entry.GetCompose()) {
				for name := range secret {
					re := regexp.MustCompile(`\$\{?` + regexp.QuoteMeta(name) + `\b`)
					if re.MatchString(v) && v != "${"+name+"}" {
						t.Errorf("%s: %s stays inside %q", id, name, v)
					}
				}
			}
			for _, name := range slices.Sorted(func(yield func(string) bool) {
				for k := range needed.Inside {
					if !yield(k) {
						return
					}
				}
			}) {
				tmpl := needed.Written[name]
				fmt.Fprintf(out, "%s: %s = %q", id, name, tmpl.GetText())
				for _, n := range tmpl.GetNames() {
					fmt.Fprintf(out, " [%s %s]", n.GetName(), strings.TrimPrefix(n.GetEncoding().String(), "ENCODING_"))
				}
				fmt.Fprintln(out)
			}
			if len(needed.Inside) == 0 {
				continue
			}
			suggested := map[string]string{}
			for _, in := range needed.Entry.GetInputs() {
				if in.GetDefaultValue() != "" {
					suggested[in.GetName()] = in.GetDefaultValue()
					continue
				}
				if !in.GetOptional() && in.GetGenerate() == 0 {
					suggested[in.GetName()] = "18000"
					if in.GetType().String() != "INPUT_TYPE_PORT" {
						suggested[in.GetName()] = "plain-value"
					}
				}
			}
			filled, err := catalog.Fill(needed.Entry, nil, suggested)
			if err != nil {
				fmt.Fprintf(out, "%s: FILL %v\n", id, err)
				continue
			}
			if err := filled.Link(needed.Plain); err != nil {
				t.Fatal(err)
			}
			needed.Fix(filled.Variables)
			values := map[string]string{}
			for name := range secret {
				values[name] = nasty
			}
			for name := range needed.Inside {
				tmpl := needed.Written[name]
				if bad := regexp.MustCompile(`\$\$|\$\{[A-Za-z_][A-Za-z0-9_]*\}`).ReplaceAllString(tmpl.GetText(), ""); strings.Contains(bad, "$") {
					t.Errorf("%s: %s's template %q has a $ a template refuses", id, name, tmpl.GetText())
					continue
				}
				got, err := variable.Write(tmpl, values)
				if err != nil {
					t.Errorf("%s: %s: %v", id, name, err)
					continue
				}
				for stand := range strings.SplitSeq(strings.Join(mapValues(filled.LinkStands), " "), " ") {
					if stand != "" && strings.Contains(got, stand) {
						t.Errorf("%s: %s holds a link's stand-in: %s", id, name, got)
					}
				}
				if !strings.Contains(got, "://") {
					if !strings.Contains(got, nasty) {
						t.Errorf("%s: %s written as %q, without the password as it is", id, name, got)
					}
					continue
				}
				// A URL in one pair of quotes is one URL (L1).
				if len(got) >= 2 && (got[0] == '"' || got[0] == '\'') && got[len(got)-1] == got[0] {
					got = got[1 : len(got)-1]
				}
				u, err := url.Parse(got)
				if err != nil {
					t.Errorf("%s: %s written as %q doesn't parse: %v", id, name, got, err)
					continue
				}
				found := false
				if p, ok := u.User.Password(); ok && p == nasty || u.User.Username() == nasty {
					found = true
				}
				for _, vs := range u.Query() {
					if slices.Contains(vs, nasty) {
						found = true
					}
				}
				if strings.Contains(u.Path, nasty) {
					found = true
				}
				if !found {
					t.Errorf("%s: %s written as %q parses without the password (user %v, query %v, path %q)", id, name, got, u.User, u.Query(), u.Path)
				}
			}
		}
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func envValues(t *testing.T, compose string) []string {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	services, _ := doc["services"].(map[string]any)
	for _, s := range services {
		svc, _ := s.(map[string]any)
		switch env := svc["environment"].(type) {
		case map[string]any:
			for _, v := range env {
				if s, ok := v.(string); ok {
					out = append(out, s)
				}
			}
		case []any:
			for _, v := range env {
				if s, ok := v.(string); ok {
					_, val, _ := strings.Cut(s, "=")
					out = append(out, val)
				}
			}
		}
	}
	return out
}

var _ = resourcev1.Encoding_ENCODING_UNSPECIFIED
