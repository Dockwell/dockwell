// SPDX-License-Identifier: AGPL-3.0-only

package bump_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/catalog/bump"
	"github.com/dockwell/dockwell/internal/catalog/bump/bumptest"
	"github.com/dockwell/dockwell/internal/registry"
)

var now = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)

func ago(days int) time.Time { return now.Add(-time.Duration(days) * 24 * time.Hour) }

// shopStand is a stand-in with an app on GHCR, which says no push times, and a
// Docker Official PostgreSQL beside it, which Docker Hub says when each was pushed.
func shopStand(t *testing.T) *bumptest.Stand {
	t.Helper()
	s := bumptest.New(t)
	s.Images["acme/shop"] = []bumptest.Tag{
		{Name: "v1.4.2"}, {Name: "v1.4.3"}, {Name: "v1.5.0"}, {Name: "v2.0.0"}, {Name: "v2.1.0"}, {Name: "v3.0.0"}, {Name: "v1.6.0-rc1"},
	}
	s.Images["library/postgres"] = []bumptest.Tag{
		{Name: "17.2", Pushed: ago(60)}, {Name: "17.3", Pushed: ago(5)}, {Name: "17.4", Pushed: ago(1)}, {Name: "18.1", Pushed: ago(20)},
	}
	s.Hub["library/postgres"] = true
	s.Releases["acme/shop@v1.4.3"] = bumptest.Release{}
	s.Releases["acme/shop@v1.5.0"] = bumptest.Release{}
	s.Releases["acme/shop@v2.1.0"] = bumptest.Release{Prerelease: true}
	s.Tags["acme/shop@v2.0.0"] = true
	s.Official["postgres"] = "Maintainers: someone\n\nTags: 17.3, 17\nArchitectures: amd64, arm64v8\n\nTags: 16.9, 16\n"
	return s
}

func image(s, repo, tag string) string {
	return s + ":" + tag + "@" + bumptest.Digest(repo, bumptest.Tag{Name: tag})
}

// shopEntry is an entry of the shop on PostgreSQL, as the catalog writes one, with
// a comment the bump keeps.
func shopEntry(version string) string {
	return `# SPDX-License-Identifier: AGPL-3.0-only
id: shop
name: Shop
summary: A shop
category: cms
version: "` + version + `"
release: 3
license: AGPL-3.0-only
appLicense: MIT
appSource: https://github.com/acme/shop
source:
  repository: https://github.com/Dockwell/catalog
  path: entries/shop.yaml
compose: |
  services:
    app:
      # The app's own image.
      image: ` + image("ghcr.io/acme/shop", "acme/shop", "v1.4.2") + `
    db:
      image: ` + image("postgres", "library/postgres", "17.2") + `
`
}

// checkout writes a catalog checkout with entries and policies, by ID.
func checkout(t *testing.T, entries, policies map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for id, text := range entries {
		write(t, filepath.Join(dir, "entries", id+".yaml"), text)
	}
	for id, text := range policies {
		write(t, filepath.Join(dir, bump.PolicyDir, id+".yaml"), text)
	}
	return dir
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func plan(t *testing.T, s *bumptest.Stand, dir, id string, seen map[string]time.Time) bump.Result {
	t.Helper()
	e, err := bump.ReadEntry(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	p, err := bump.ReadPolicy(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	return (&bump.Planner{Sources: s.Live(), Now: now, Seen: seen}).Plan(t.Context(), e, p)
}

func lane(r bump.Result, name string) *bump.Lane {
	for i := range r.Lanes {
		if r.Lanes[i].Lane == name {
			return &r.Lanes[i]
		}
	}
	return nil
}

func step(l *bump.Lane, svc string) *bump.Step {
	if l == nil {
		return nil
	}
	for i := range l.Steps {
		if l.Steps[i].Service == svc {
			return &l.Steps[i]
		}
	}
	return nil
}

func says(waits []string, part string) bool {
	return slices.ContainsFunc(waits, func(w string) bool { return strings.Contains(w, part) })
}

// An entry gets two lanes (ADR 0100): the newest step within each image's level
// that's aged past its wait, and the next step beyond, never two majors at once.
// A version Docker Hub says was pushed counts its wait from then; elsewhere from
// when the bump first saw it, which a version it never saw hasn't started. What
// gives the version is corroborated by the app's GitHub release, a Docker Official
// Image by docker-library's file of it. A database series move is a major.
func TestTwoLanesEachTheirNextStep(t *testing.T) {
	s := shopStand(t)
	dir := checkout(t, map[string]string{"shop": shopEntry("1.4.2")}, map[string]string{"shop": "gives: app\n"})
	key := bump.SeenKey(mustParse(t, "ghcr.io/acme/shop:v1.4.2"), "v1.5.0", bumptest.Digest("acme/shop", bumptest.Tag{Name: "v1.5.0"}))

	// Never seen: the newest minor is offered, and waits, as its wait hasn't begun.
	r := plan(t, s, dir, "shop", nil)
	within := lane(r, bump.LaneWithin)
	app, db := step(within, "app"), step(within, "db")
	if app == nil || app.ToTag != "v1.5.0" || app.Kind != registry.KindMinor || !says(app.Waits, "within its wait") {
		t.Fatalf("the app's step within its level is %+v", app)
	}
	// PostgreSQL's 17.4 is a day old: 17.3, five days old, is past a patch's three.
	if db == nil || db.ToTag != "17.3" || !db.Pushed || len(db.Waits) != 0 || !strings.Contains(db.Corroborated, "official-images") {
		t.Fatalf("the database's step within its level is %+v", db)
	}
	if within.Version != "1.5.0" || within.Release != 4 || within.Branch != "bump/shop" || !slices.Contains(within.Labels, "within its wait") {
		t.Errorf("the lane within reads %+v", within)
	}

	// Seen ten days ago, the minor is past its week's wait and corroborated.
	r = plan(t, s, dir, "shop", map[string]time.Time{key: ago(10)})
	if app := step(lane(r, bump.LaneWithin), "app"); app == nil || len(app.Waits) != 0 || !strings.Contains(app.Corroborated, "release v1.5.0") {
		t.Errorf("the app's step, seen ten days ago, is %+v", app)
	}

	// Beyond: the next major alone, never 3.0, and PostgreSQL 18, a series move.
	beyond := lane(r, bump.LaneBeyond)
	app, db = step(beyond, "app"), step(beyond, "db")
	if app == nil || app.ToTag != "v2.1.0" || app.Kind != registry.KindMajor || !says(app.Waits, "past app's level, minor") ||
		!says(app.Waits, "pre-release") {
		t.Errorf("the app's next step beyond is %+v", app)
	}
	if db == nil || db.ToTag != "18.1" || db.Kind != registry.KindMajor || !says(db.Waits, "names no postgres:18.1") {
		t.Errorf("the database's next step beyond is %+v", db)
	}
	if beyond.Branch != "bump/shop-major" || beyond.Release != 4 || !slices.Contains(beyond.Labels, "major") {
		t.Errorf("the lane beyond reads %+v", beyond)
	}
	// Each lane's patch applies to the entry as it is, and keeps its comment.
	for _, l := range r.Lanes {
		after := applyPatch(t, dir, l.Patch)
		if !strings.Contains(after, "# The app's own image.") || !strings.Contains(after, "release: 4\n") {
			t.Errorf("the %s lane's entry reads\n%s", l.Lane, after)
		}
	}
	if after := applyPatch(t, dir, lane(r, bump.LaneWithin).Patch); !strings.Contains(after, `version: "1.5.0"`) ||
		!strings.Contains(after, "ghcr.io/acme/shop:v1.5.0@"+bumptest.Digest("acme/shop", bumptest.Tag{Name: "v1.5.0"})) ||
		!strings.Contains(after, "postgres:17.3@"+bumptest.Digest("library/postgres", bumptest.Tag{Name: "17.3"})) {
		t.Errorf("the lane within's entry reads\n%s", after)
	}
}

// A held version is skipped, an image that stays never moves, and a next major that
// isn't the one right after waits where majors go in order.
func TestHoldsStaysAndMajorsInOrder(t *testing.T) {
	s := shopStand(t)
	s.Images["acme/shop"] = []bumptest.Tag{{Name: "v1.4.2"}, {Name: "v1.4.3"}, {Name: "v1.5.0"}, {Name: "v3.0.0"}}
	dir := checkout(t, map[string]string{"shop": shopEntry("1.4.2")}, map[string]string{"shop": `gives: app
services:
  app:
    majorsInOrder: true
    hold:
      - version: 1.5.0
        why: It loses its uploads on the way up.
  db:
    level: stays
`})
	r := plan(t, s, dir, "shop", nil)
	if app := step(lane(r, bump.LaneWithin), "app"); app == nil || app.ToTag != "v1.4.3" {
		t.Errorf("with 1.5.0 held, the app moves to %+v", app)
	}
	if db := step(lane(r, bump.LaneWithin), "db"); db != nil {
		t.Errorf("an image that stays moves to %+v", db)
	}
	if app := step(lane(r, bump.LaneBeyond), "app"); app == nil || !says(app.Waits, "majors go in order, and its next one among the tags read is 3, not 2") {
		t.Errorf("a skipped major reads %+v", app)
	}
}

// An entry with more images than one and nothing saying which gives its version gets
// no bumps, and neither does one whose version and tag disagree, each with why.
func TestWhatGetsNoBumpSaysWhy(t *testing.T) {
	s := shopStand(t)
	dir := checkout(t, map[string]string{
		"shop": shopEntry("1.4.2"),
		"odd":  strings.ReplaceAll(shopEntry("1.4"), "id: shop", "id: odd"),
	}, map[string]string{"odd": "gives: app\n"})
	if r := plan(t, s, dir, "shop", nil); len(r.Lanes) != 0 || !says(r.Notes, "no gives in its policy") {
		t.Errorf("two images and no gives: %+v", r)
	}
	if r := plan(t, s, dir, "odd", nil); len(r.Lanes) != 0 || !says(r.Notes, "Its version is 1.4 and app's tag says 1.4.2. Make them agree first.") {
		t.Errorf("a version and a tag that disagree: %+v", r)
	}
	if r := plan(t, s, checkout(t, map[string]string{"shop": shopEntry("1.4.2")}, map[string]string{"shop": "gives: web\n"}), "shop", nil); !says(r.Notes, "no such service") {
		t.Errorf("a policy naming a service the entry hasn't: %+v", r)
	}
}

// A new digest that drops a platform the old ran on waits, and so does one whose
// signer a policy names, as its signature isn't checked yet. Docker Hub's 429 is
// asked again within the run.
func TestPlatformsSignaturesAndARateLimit(t *testing.T) {
	s := shopStand(t)
	s.Images["library/postgres"][1].Platforms = []string{"linux/amd64"}
	s.Limited = 2
	dir := checkout(t, map[string]string{"shop": shopEntry("1.4.2")}, map[string]string{"shop": `gives: app
services:
  app:
    signedBy:
      repository: acme/shop
      workflow: .github/workflows/release.yml
`})
	r := plan(t, s, dir, "shop", nil)
	if len(r.Unread) != 0 {
		t.Fatalf("after two 429s it reads %v", r.Unread)
	}
	if db := step(lane(r, bump.LaneWithin), "db"); db == nil || !says(db.Waits, "It drops linux/arm64") {
		t.Errorf("a step that drops arm64 reads %+v", db)
	}
	if app := step(lane(r, bump.LaneWithin), "app"); app == nil || !says(app.Waits, "Its signature by acme/shop's workflow .github/workflows/release.yml isn't checked yet") {
		t.Errorf("a step of a signed image reads %+v", app)
	}
}

// A policy is read strictly, and its defaults come from code.
func TestPolicies(t *testing.T) {
	for raw, says := range map[string]string{
		"gives: app\nlevel: patch\n":                                                               "field level not found",
		"services:\n  app:\n    level: major\n":                                                    "a major never merges by itself",
		"services:\n  app:\n    signedBy:\n      issuer: x\n":                                      "Give one pair",
		"services:\n  app:\n    hold:\n      - version: 1.2\n":                                     "without saying which or why",
		"services:\n  app:\n    signedBy:\n      issuer: x\n      subject: y\n      workflow: z\n": "Give one pair",
		// A second document isn't read past (#726's review, L5).
		"gives: app\n---\nservices: {app: {level: banana}}\n": "more than one YAML document",
	} {
		if _, err := bump.ParsePolicy([]byte(raw)); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("ParsePolicy(%q) = %v, want it refused, saying %q", raw, err, says)
		}
	}
	p := &bump.Policy{}
	for img, want := range map[string]bump.Level{
		"postgres:17.2":              bump.LevelPatch,
		"valkey/valkey:8.1":          bump.LevelPatch,
		"ghcr.io/acme/tool:0.9.1":    bump.LevelPatch,
		"ghcr.io/acme/cal:2026.11.1": bump.LevelPatch,
		"ghcr.io/acme/shop:v1.4.2":   bump.LevelMinor,
		"wordpress:7.1.3-apache":     bump.LevelMinor,
	} {
		if got := p.For("x", mustParse(t, img)).Level; got != want {
			t.Errorf("%s's default level is %s, want %s", img, got, want)
		}
	}
	if !p.For("x", mustParse(t, "postgres:17.2")).Rebuilds || p.For("x", mustParse(t, "ghcr.io/acme/shop:v1")).Rebuilds {
		t.Error("rebuilds isn't on for a Docker Official Image alone")
	}
}

// An edit is refused, and nothing edited, where a line isn't there exactly once.
func TestAnEditNeedsEachLineOnce(t *testing.T) {
	text := []byte(shopEntry("1.4.2"))
	twice := []byte(strings.Replace(string(text), "release: 3\n", "release: 3\nrelease: 3\n", 1))
	from := image("postgres", "library/postgres", "17.2")
	for name, c := range map[string]struct {
		text []byte
		edit bump.Edit
		says string
	}{
		"an image not there": {text, bump.Edit{Images: map[string]string{"redis:7@sha256:x": "redis:8"}, Release: 4}, "isn't on a line of its own"},
		"a release twice":    {twice, bump.Edit{Images: map[string]string{from: "postgres:17.3"}, Release: 4}, "release is on 2"},
		"no new line at end": {[]byte(strings.TrimSuffix(string(text), "\n")), bump.Edit{Release: 4}, "new line"},
		"an image twice":     {[]byte(string(text) + "    other:\n      image: " + from + "\n"), bump.Edit{Images: map[string]string{from: "x"}, Release: 4}, "more than one line"},
	} {
		if _, err := bump.Apply(c.text, c.edit); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: Apply = %v, want it refused, saying %q", name, err, c.says)
		}
	}
}

func mustParse(t *testing.T, s string) registry.Ref {
	t.Helper()
	r, err := registry.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// applyPatch applies a patch with git, as the job that opens the pull request does,
// to a copy of the checkout, and returns the entry after.
func applyPatch(t *testing.T, dir string, patch []byte) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.CopyFS(repo, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "bump.patch")
	if err := os.WriteFile(file, patch, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "apply", file)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply: %v\n%s\n%s", err, out, patch)
	}
	after, err := os.ReadFile(filepath.Join(repo, "entries", "shop.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(after)
}

// mariaStand is the shop on MariaDB, which moves its data by series (12.3 to 12.4
// is a series move within one first number).
func mariaStand(t *testing.T, tags ...string) (*bumptest.Stand, string) {
	t.Helper()
	s := shopStand(t)
	var list []bumptest.Tag
	for _, tag := range tags {
		list = append(list, bumptest.Tag{Name: tag, Pushed: ago(30)})
	}
	s.Images["library/mariadb"] = list
	s.Hub["library/mariadb"] = true
	s.Official["mariadb"] = "Tags: " + strings.Join(tags, ", ") + "\n"
	entry := strings.Replace(shopEntry("1.4.2"), image("postgres", "library/postgres", "17.2"), image("mariadb", "library/mariadb", "12.3.3"), 1)
	return s, checkout(t, map[string]string{"shop": entry}, map[string]string{"shop": "gives: app\n"})
}

// A series move within one first number, the only step past the level, is offered
// beyond, as the next major it is (#726's review, M1).
func TestASeriesMoveAloneIsOfferedBeyond(t *testing.T) {
	s, dir := mariaStand(t, "12.3.3", "12.3.4", "12.4.1")
	r := plan(t, s, dir, "shop", nil)
	if db := step(lane(r, bump.LaneBeyond), "db"); db == nil || db.ToTag != "12.4.1" {
		t.Errorf("MariaDB 12.3 to 12.4, a series move, is offered beyond as %+v; notes %v", db, r.Notes)
	}
}

// With a later first number there too, the lane beyond offers the series right
// after, never two series moves at once.
func TestTheNextSeriesComesFirst(t *testing.T) {
	s, dir := mariaStand(t, "12.3.3", "12.3.4", "12.4.1", "13.0.2")
	r := plan(t, s, dir, "shop", nil)
	if db := step(lane(r, bump.LaneBeyond), "db"); db == nil || db.ToTag != "12.4.1" {
		t.Errorf("the next series is 12.4, and the lane beyond offers %+v", db)
	}
}

// First-seen goes by the digest: a tag pushed again with another image after its
// wait doesn't inherit the old time, and says so (#726's review, M4).
func TestATagPushedAgainStartsItsWaitAgain(t *testing.T) {
	s := shopStand(t)
	dir := checkout(t, map[string]string{"shop": shopEntry("1.4.2")}, map[string]string{"shop": "gives: app\n"})
	ref := mustParse(t, "ghcr.io/acme/shop:v1.4.2")
	current := bumptest.Digest("acme/shop", bumptest.Tag{Name: "v1.5.0"})
	// Seen ten days ago at what it was then; it now points elsewhere.
	r := plan(t, s, dir, "shop", map[string]time.Time{bump.SeenKey(ref, "v1.5.0", "sha256:"+strings.Repeat("0", 64)): ago(10)})
	l := lane(r, bump.LaneWithin)
	app := step(l, "app")
	if app == nil || !says(app.Waits, "pushed again") || !says(app.Waits, "within its wait") || !slices.Contains(l.Labels, "tag pushed again") {
		t.Errorf("a tag seen at another digest reads %+v, labels %v", app, l.Labels)
	}
	if at, ok := app.Saw[bump.SeenKey(ref, "v1.5.0", current)]; !ok || !at.Equal(now) {
		t.Errorf("it's first seen now at its digest: %v", app.Saw)
	}
	// Seen at the same digest, its time stands.
	r = plan(t, s, dir, "shop", map[string]time.Time{bump.SeenKey(ref, "v1.5.0", current): ago(10)})
	if app := step(lane(r, bump.LaneWithin), "app"); app == nil || says(app.Waits, "pushed again") || says(app.Waits, "within its wait") {
		t.Errorf("a tag seen at its digest reads %+v", app)
	}
}

// A comment after the version stays, as every comment does (#726's review, L1).
func TestAVersionsCommentStays(t *testing.T) {
	text := strings.Replace(shopEntry("1.4.2"), `version: "1.4.2"`, `version: "1.4.2" # the app's own tag`, 1)
	after, err := bump.Apply([]byte(text), bump.Edit{Version: "1.5.0", Release: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "# the app's own tag") {
		t.Errorf("the version's line reads %q", strings.Split(string(after), "\n")[5])
	}
}

// A hold names a version whole or by its first parts: 1.5 holds 1.5.0 and 1.5.3,
// not 1.50.0 (#726's review, L6).
func TestAHoldTakesAVersionOrItsFirstParts(t *testing.T) {
	e := bump.Effective{Hold: []bump.Hold{{Version: "1.5", Why: "breaks logins"}, {Version: "v2.0.1", Why: "bad image"}}}
	for version, held := range map[string]bool{"1.5.0": true, "1.5.3": true, "1.50.0": false, "1.4.9": false, "2.0.1": true, "2.0.10": false} {
		if got := e.Held(version) != ""; got != held {
			t.Errorf("Held(%s) = %v, want %v", version, got, held)
		}
	}
}

// Waiting on Docker Hub's 429s has a budget for the run: once it's spent, an image
// it refuses is left unread, saying why, rather than the run going past the job's
// time with nothing written (#726's review, L3).
func TestALimitThatHoldsLeavesItsImagesUnread(t *testing.T) {
	s := shopStand(t)
	s.Limited = 1 << 20
	dir := checkout(t, map[string]string{"shop": shopEntry("1.4.2")}, map[string]string{"shop": "gives: app\n"})
	e, err := bump.ReadEntry(dir, "shop")
	if err != nil {
		t.Fatal(err)
	}
	p, err := bump.ReadPolicy(dir, "shop")
	if err != nil {
		t.Fatal(err)
	}
	live := s.Live()
	live.Tries, live.Wait, live.Budget = 100, 10*time.Millisecond, 25*time.Millisecond
	start := time.Now()
	r := (&bump.Planner{Sources: live, Now: now}).Plan(t.Context(), e, p)
	if time.Since(start) > 5*time.Second || len(r.Unread) == 0 || !strings.Contains(strings.Join(r.Unread, " "), "past the run's 25ms of waiting") {
		t.Errorf("with the limit held, after %s it reads unread %v", time.Since(start), r.Unread)
	}
}

// A version whose digest can't be read in a run keeps what was seen of it, so a
// flaky registry doesn't start its wait again (#726's re-check, L-N3).
func TestAVersionUnreadThisRunKeepsWhenItWasSeen(t *testing.T) {
	s := shopStand(t)
	dir := checkout(t, map[string]string{"shop": shopEntry("1.4.2")}, map[string]string{"shop": "gives: app\n"})
	ref := mustParse(t, "ghcr.io/acme/shop:v1.4.2")
	key := bump.SeenKey(ref, "v1.4.3", bumptest.Digest("acme/shop", bumptest.Tag{Name: "v1.4.3"}))
	s.Broken["acme/shop:v1.4.3"] = true
	r := plan(t, s, dir, "shop", map[string]time.Time{key: ago(2)})
	var kept bool
	for i := range r.Lanes {
		for _, st := range r.Lanes[i].Steps {
			if at, ok := st.Saw[key]; ok && at.Equal(ago(2)) {
				kept = true
			}
		}
	}
	if !kept {
		t.Errorf("v1.4.3, unread this run, lost when it was first seen: %+v", r.Lanes)
	}
}
