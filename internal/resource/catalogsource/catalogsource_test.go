// SPDX-License-Identifier: AGPL-3.0-only

package catalogsource

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// A source names a public repository over HTTPS, a folder in it and a commit in
// full, written as a fetch reads them.
func TestValidate(t *testing.T) {
	commit := strings.Repeat("a", 40)
	s := &resourcev1.CatalogSource{Repository: "https://example.com/catalog/", Path: "/entries/converted/", Commit: commit}
	if err := (Type{}).Validate(s); err != nil {
		t.Fatal(err)
	}
	if s.GetRepository() != "https://example.com/catalog" || s.GetPath() != "entries/converted" {
		t.Errorf("Validate wrote %q and %q", s.GetRepository(), s.GetPath())
	}
	for name, tc := range map[string]struct {
		spec *resourcev1.CatalogSource
		want string
	}{
		"a branch":             {&resourcev1.CatalogSource{Repository: "https://example.com/c", Commit: "main"}, "A branch moves"},
		"a short commit":       {&resourcev1.CatalogSource{Repository: "https://example.com/c", Commit: commit[:12]}, "40 hex digits"},
		"plain HTTP":           {&resourcev1.CatalogSource{Repository: "http://example.com/c", Commit: commit}, "isn't over HTTPS"},
		"a folder outside":     {&resourcev1.CatalogSource{Repository: "https://example.com/c", Path: "entries/../..", Commit: commit}, "in its repository"},
		"no repository at all": {&resourcev1.CatalogSource{Commit: commit}, "isn't a repository's URL"},
	} {
		if err := (Type{}).Validate(tc.spec); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	// A folder whose name holds two dots is still a folder.
	if err := (Type{}).Validate(&resourcev1.CatalogSource{Repository: "https://example.com/c", Path: "v1..2", Commit: commit}); err != nil {
		t.Errorf("a folder named v1..2: %v", err)
	}
	if err := (Type{}).CheckName("community"); err != nil {
		t.Error(err)
	}
}
