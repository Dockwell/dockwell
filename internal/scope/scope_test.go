// SPDX-License-Identifier: AGPL-3.0-only

package scope

import (
	"errors"
	"slices"
	"testing"
)

func defaults() Tree {
	return Tree{Projects: []Project{{ID: "prj_1", Name: "default", Environments: []Environment{{ID: "env_1", Name: "production", Project: "prj_1"}}}}}
}

func TestOneOfEachShowsNothingAndNeedsNoNames(t *testing.T) {
	tr := defaults()
	if s := tr.Shown(""); s != (Shown{}) {
		t.Errorf("Shown = %+v, want nothing", s)
	}
	for _, args := range [][2]string{{"", ""}, {"default", ""}, {"", "production"}, {"prj_1", "env_1"}, {"", "env_1"}} {
		p, e, err := tr.Resolve(args[0], args[1])
		if err != nil || p.ID != "prj_1" || e.ID != "env_1" {
			t.Errorf("Resolve(%q, %q) = %s, %s, %v", args[0], args[1], p.ID, e.ID, err)
		}
	}
}

func TestALevelShowsOnceItOffersAChoice(t *testing.T) {
	tr := defaults()
	tr.Projects[0].Environments = append(tr.Projects[0].Environments, Environment{ID: "env_2", Name: "staging", Project: "prj_1"})
	tr.Projects = append(tr.Projects, Project{ID: "prj_2", Name: "shop", Environments: []Environment{{ID: "env_3", Name: "production", Project: "prj_2"}}})

	if s := tr.Shown(""); !s.Project || !s.Environment || s.Organization {
		t.Errorf("across projects = %+v, want project and environment", s)
	}
	if s := tr.Shown("shop"); !s.Project || s.Environment {
		t.Errorf("in shop, which has one environment = %+v, want project only", s)
	}
	if s := tr.Shown("prj_1"); !s.Environment {
		t.Errorf("in default, which has two = %+v, want the environment", s)
	}

	var choose *ChoiceError
	if _, _, err := tr.Resolve("", "production"); !errors.As(err, &choose) || choose.Level != ProjectKind ||
		err.Error() != "this organization has 2 projects, default and shop, so say which one you mean" {
		t.Errorf("no project = %v", err)
	}
	if _, _, err := tr.Resolve("default", ""); !errors.As(err, &choose) || !slices.Equal(choose.Options, []string{"production", "staging"}) ||
		err.Error() != "project default has 2 environments, production and staging, so say which one you mean" {
		t.Errorf("no environment = %v", err)
	}
	if _, e, err := tr.Resolve("shop", ""); err != nil || e.ID != "env_3" {
		t.Errorf("shop has one environment, so it needs no name: %+v, %v", e, err)
	}
	if _, e, err := tr.Resolve("", "env_2"); err != nil || e.Name != "staging" {
		t.Errorf("an environment's ID needs no project: %+v, %v", e, err)
	}
	for _, args := range [][2]string{{"nope", ""}, {"default", "nope"}, {"shop", "env_2"}, {"", "env_9"}} {
		if _, _, err := tr.Resolve(args[0], args[1]); !errors.Is(err, ErrNotFound) {
			t.Errorf("Resolve(%q, %q) = %v, want ErrNotFound", args[0], args[1], err)
		}
	}
}

func TestNamesFitURLs(t *testing.T) {
	for name, ok := range map[string]bool{"staging": true, "eu-west-2": true, "p": true, "": false, "Staging": false, "2nd": false, "a b": false, "trailing-": false} {
		if err := (EnvironmentType{}).CheckName(name); (err == nil) != ok {
			t.Errorf("CheckName(%q) = %v", name, err)
		}
	}
}
