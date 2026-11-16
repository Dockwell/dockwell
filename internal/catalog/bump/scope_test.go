// SPDX-License-Identifier: AGPL-3.0-only

package bump_test

import (
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/catalog/bump"
	"github.com/dockwell/dockwell/internal/catalog/bump/bumptest"
)

const app = "app/dockwell-catalog-bump"

// bumped is the shop's entry as a bump moves it: the app to v1.5.0, PostgreSQL to
// 17.3, the version and the release.
func bumped() string {
	text := shopEntry("1.4.2")
	text = strings.Replace(text, image("ghcr.io/acme/shop", "acme/shop", "v1.4.2"), image("ghcr.io/acme/shop", "acme/shop", "v1.5.0"), 1)
	text = strings.Replace(text, image("postgres", "library/postgres", "17.2"), image("postgres", "library/postgres", "17.3"), 1)
	text = strings.Replace(text, `version: "1.4.2"`, `version: "1.5.0"`, 1)
	return strings.Replace(text, "release: 3", "release: 4", 1)
}

func inScope(in bump.ScopeInput) bump.ScopeInput {
	if in.Author == "" {
		in.Author = app
	}
	in.App = app
	if in.Branch == "" {
		in.Branch = "bump/shop"
	}
	if in.Changes == nil {
		in.Changes = []bump.Change{{Status: "M", Path: "entries/shop.yaml"}}
	}
	if in.BaseEntry == nil {
		in.BaseEntry = []byte(shopEntry("1.4.2"))
	}
	if in.HeadEntry == nil {
		in.HeadEntry = []byte(bumped())
	}
	return in
}

// bump scope reads the diff alone (ADR 0100): a person's pull request passes, and
// the App's only where it changes one entry's images' tags and digests, version
// and release, by one, keeping each image's repository, or a maintainer approved it.
func TestScopeReadsTheDiffAlone(t *testing.T) {
	base := shopEntry("1.4.2")
	for name, tt := range map[string]struct {
		in   bump.ScopeInput
		pass bool
		says string
	}{
		"a bump":                  {in: inScope(bump.ScopeInput{}), pass: true},
		"a person's":              {in: inScope(bump.ScopeInput{Author: "mira", Changes: []bump.Change{{"M", ".github/workflows/check.yml"}}}), pass: true, says: "review rule"},
		"the App's, from a fork":  {in: inScope(bump.ScopeInput{CrossRepository: true}), pass: true, says: "review rule"},
		"a major":                 {in: inScope(bump.ScopeInput{Branch: "bump/shop-major"}), says: "beyond its policy's levels"},
		"a major, approved":       {in: inScope(bump.ScopeInput{Branch: "bump/shop-major", Approved: true}), pass: true, says: "maintainer approved"},
		"another branch":          {in: inScope(bump.ScopeInput{Branch: "main"}), says: "isn't a bump's"},
		"another entry's file":    {in: inScope(bump.ScopeInput{Changes: []bump.Change{{"M", "entries/other.yaml"}}}), says: "changes entries/other.yaml"},
		"its policy too":          {in: inScope(bump.ScopeInput{Changes: []bump.Change{{"M", "entries/shop.yaml"}, {"M", "updates/shop.yaml"}}}), says: "changes 2 files"},
		"a check":                 {in: inScope(bump.ScopeInput{Changes: []bump.Change{{"M", "entries/shop.yaml"}, {"A", "checks/shop.sh"}}}), says: "changes 2 files"},
		"the workflow":            {in: inScope(bump.ScopeInput{Changes: []bump.Change{{"M", ".github/dockwell-commit"}}}), says: "changes .github/dockwell-commit"},
		"a new file":              {in: inScope(bump.ScopeInput{Changes: []bump.Change{{"A", "entries/shop.yaml"}}}), says: "(A)"},
		"a deleted entry":         {in: inScope(bump.ScopeInput{Changes: []bump.Change{{"D", "entries/shop.yaml"}}}), says: "(D)"},
		"a release up by two":     {in: inScope(bump.ScopeInput{HeadEntry: []byte(strings.Replace(bumped(), "release: 4", "release: 5", 1))}), says: "not up by one"},
		"the same release":        {in: inScope(bump.ScopeInput{HeadEntry: []byte(strings.Replace(bumped(), "release: 4", "release: 3", 1))}), says: "not up by one"},
		"another repository":      {in: inScope(bump.ScopeInput{HeadEntry: []byte(strings.Replace(bumped(), "ghcr.io/acme/shop:v1.5.0", "ghcr.io/evil/shop:v1.5.0", 1))}), says: "another repository"},
		"another registry":        {in: inScope(bump.ScopeInput{HeadEntry: []byte(strings.Replace(bumped(), "ghcr.io/acme/shop:v1.5.0", "quay.io/acme/shop:v1.5.0", 1))}), says: "another repository"},
		"no digest":               {in: inScope(bump.ScopeInput{HeadEntry: []byte(strings.Replace(bumped(), "@"+bumptest.Digest("acme/shop", bumptest.Tag{Name: "v1.5.0"}), "", 1))}), says: "isn't pinned by its digest"},
		"a name changed":          {in: inScope(bump.ScopeInput{HeadEntry: []byte(strings.Replace(bumped(), "name: Shop", "name: Sh0p", 1))}), says: `"name: Shop" becomes "name: Sh0p"`},
		"a line added":            {in: inScope(bump.ScopeInput{HeadEntry: []byte(bumped() + "privileged: true\n")}), says: "a bump only replaces lines"},
		"a command in place":      {in: inScope(bump.ScopeInput{HeadEntry: []byte(strings.Replace(bumped(), "      # The app's own image.", "      command: sh -c evil", 1))}), says: "isn't an image's tag and digest"},
		"an image key in another": {in: inScope(bump.ScopeInput{HeadEntry: []byte(strings.Replace(bumped(), "name: Shop", "image: ghcr.io/acme/shop:v1", 1))}), says: "isn't an image's tag and digest"},
		"nothing moved":           {in: inScope(bump.ScopeInput{HeadEntry: []byte(base)}), says: "not up by one"},
	} {
		got := bump.Scope(tt.in)
		if got.Pass != tt.pass || tt.says != "" && !strings.Contains(strings.Join(got.Why, " | "), tt.says) {
			t.Errorf("%s: %+v, want pass %v saying %q", name, got, tt.pass, tt.says)
		}
	}
}

// What passes the diff is then checked as the step the bump would make now: past
// its wait, corroborated, keeping its platforms, and exactly that step.
func TestAStepInScopeIsTheStepTheBumpMakes(t *testing.T) {
	s := shopStand(t)
	dir := checkout(t, map[string]string{"shop": shopEntry("1.4.2")}, map[string]string{"shop": "gives: app\n"})
	e, err := bump.ReadEntry(dir, "shop")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := bump.ReadPolicy(dir, "shop")
	if err != nil {
		t.Fatal(err)
	}
	ref := mustParse(t, "ghcr.io/acme/shop:v1.4.2")
	seen := map[string]time.Time{bump.SeenKey(ref, "v1.5.0", bumptest.Digest("acme/shop", bumptest.Tag{Name: "v1.5.0"})): ago(10)}
	planner := func(seen map[string]time.Time) *bump.Planner {
		return &bump.Planner{Sources: s.Live(), Now: now, Seen: seen}
	}
	if why := bump.StepInScope(t.Context(), planner(seen), e, policy, []byte(bumped())); len(why) != 0 {
		t.Errorf("the bump's own step, past its wait, is out of scope: %v", why)
	}
	// Seen two days ago, it's within its week's wait.
	recent := map[string]time.Time{bump.SeenKey(ref, "v1.5.0", bumptest.Digest("acme/shop", bumptest.Tag{Name: "v1.5.0"})): ago(2)}
	if why := strings.Join(bump.StepInScope(t.Context(), planner(recent), e, policy, []byte(bumped())), " | "); !strings.Contains(why, "within its wait") {
		t.Errorf("within its wait, it reads %q", why)
	}
	// A digest the bump didn't read, as a tag pushed again since, isn't its step.
	other := strings.Replace(bumped(), bumptest.Digest("acme/shop", bumptest.Tag{Name: "v1.5.0"}), bumptest.Digest("acme/shop", bumptest.Tag{Name: "v1.5.0", Build: 2}), 1)
	if why := strings.Join(bump.StepInScope(t.Context(), planner(seen), e, policy, []byte(other)), " | "); !strings.Contains(why, "isn't the step the bump makes") {
		t.Errorf("another digest reads %q", why)
	}
	// A step that drops a platform waits for a person.
	s.Images["library/postgres"][1].Platforms = []string{"linux/amd64"}
	if why := strings.Join(bump.StepInScope(t.Context(), planner(seen), e, policy, []byte(bumped())), " | "); !strings.Contains(why, "drops linux/arm64") {
		t.Errorf("a dropped platform reads %q", why)
	}
}
