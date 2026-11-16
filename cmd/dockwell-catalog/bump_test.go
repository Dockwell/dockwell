// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/catalog/bump"
	"github.com/dockwell/dockwell/internal/catalog/bump/bumptest"
)

// A bump reads a catalog checkout's entries and policies, with no network but the
// stand-in's, and writes a patch for each pull request and bumps.json, which the job
// that opens them reads with jq alone: each its branch, title, body and labels. The
// body records when the bump first saw a version its registry gives no push time of,
// which the next run reads back from the open pull requests.
func TestABumpWritesPatchesAndWhatEachPullRequestSays(t *testing.T) {
	s := bumptest.New(t)
	s.Images["acme/notes"] = []bumptest.Tag{{Name: "2.3.1"}, {Name: "2.3.2"}, {Name: "2.4.0"}, {Name: "3.0.0"}}
	s.Images["library/redis"] = []bumptest.Tag{{Name: "7.4.1", Pushed: time.Now().Add(-90 * 24 * time.Hour)}, {Name: "7.4.2", Pushed: time.Now().Add(-4 * 24 * time.Hour)}}
	s.Hub["library/redis"] = true
	s.Releases["acme/notes@v2.4.0"] = bumptest.Release{}
	s.Official["redis"] = "Tags: 7.4.2, 7.4, 7\n"
	digest := func(repo, tag string) string { return bumptest.Digest(repo, bumptest.Tag{Name: tag}) }
	dir := t.TempDir()
	entry := `id: notes
name: Notes
summary: Notes
category: productivity
version: "2.3.1"
release: 7
license: AGPL-3.0-only
appLicense: MIT
appSource: https://github.com/acme/notes
compose: |
  services:
    app:
      image: ghcr.io/acme/notes:2.3.1@` + digest("acme/notes", "2.3.1") + `
    cache:
      image: redis:7.4.1@` + digest("library/redis", "7.4.1") + `
`
	for path, text := range map[string]string{"entries/notes.yaml": entry, "updates/notes.yaml": "gives: app\n"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "init", "-q")
	git(t, dir, "add", ".")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "base")
	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	deps := &bumpDeps{sources: s.Live(), now: time.Now()}
	if err := bumpCommand(t.Context(), []string{"--catalog", dir, "--out", out}, &stdout, &stderr, deps); err != nil {
		t.Fatalf("bump: %v\n%s", err, stderr.String())
	}
	raw, err := os.ReadFile(filepath.Join(out, BumpsFile))
	if err != nil {
		t.Fatal(err)
	}
	var got bumpOutput
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Bumps) != 2 || len(got.Unread) != 0 {
		t.Fatalf("bumps.json reads %s", raw)
	}
	within := got.Bumps[0]
	if within.Branch != "bump/notes" || within.Title != "Move notes to 2.4.0" || !slices.Contains(within.Labels, "within its wait") ||
		!slices.Contains(within.Labels, "bump") || !strings.Contains(within.Body, "Moves notes from 2.3.1 to 2.4.0, as release 8.") ||
		!strings.Contains(within.Body, "| cache | `7.4.1` | `7.4.2` | patch | pushed ") ||
		!strings.Contains(within.Body, "Nothing in Dockwell's catalog merges by itself yet") {
		t.Errorf("the bump within reads %+v", within)
	}
	if major := got.Bumps[1]; major.Branch != "bump/notes-major" || !slices.Contains(major.Labels, "major") || !strings.Contains(major.Body, "`3.0.0` | major") {
		t.Errorf("the major bump reads %+v", major)
	}
	// Each patch applies with git, as the job that opens the pull request applies it.
	for _, b := range got.Bumps {
		git(t, dir, "apply", "--check", filepath.Join(out, b.Patch))
	}

	// The next run reads when it first saw 2.4.0 back from the open pull request.
	lines, _ := json.Marshal(map[string]string{"body": within.Body})
	var seen bytes.Buffer
	if err := seenFromBodies(bytes.NewReader(append(lines, '\n')), &seen); err != nil {
		t.Fatal(err)
	}
	var times map[string]time.Time
	if err := json.Unmarshal(seen.Bytes(), &times); err != nil {
		t.Fatalf("seen reads %s: %v", seen.String(), err)
	}
	if at, ok := times["ghcr.io/acme/notes:2.4.0"]; !ok || time.Since(at) > time.Hour {
		t.Errorf("seen reads %s", seen.String())
	}
	seenFile := filepath.Join(t.TempDir(), "seen.json")
	week := map[string]time.Time{"ghcr.io/acme/notes:2.4.0": time.Now().Add(-8 * 24 * time.Hour)}
	weekRaw, _ := json.Marshal(week)
	if err := os.WriteFile(seenFile, weekRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	again := t.TempDir()
	if err := bumpCommand(t.Context(), []string{"--catalog", dir, "--out", again, "--seen", seenFile, "--only", "notes"}, &stdout, &stderr, deps); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(again, BumpsFile))
	var later bumpOutput
	if err := json.Unmarshal(raw, &later); err != nil {
		t.Fatal(err)
	}
	// Past its week, and corroborated by its release, it waits on nothing.
	if w := later.Bumps[0]; w.Branch != "bump/notes" || !slices.Equal(w.Labels, []string{"bump"}) || !strings.Contains(w.Body, "first seen") {
		t.Errorf("seen a week ago, the bump within reads %+v", w)
	}
}

// The schema in updates.schema.json names the fields a policy has, and no others.
func TestTheUpdatesSchemaMatchesThePolicy(t *testing.T) {
	var schema struct {
		Properties map[string]any `json:"properties"`
		Defs       struct {
			Service struct {
				Properties map[string]struct {
					Properties map[string]any `json:"properties"`
				} `json:"properties"`
			} `json:"service"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(updatesSchema, &schema); err != nil {
		t.Fatal(err)
	}
	fields := func(v any) []string {
		var out []string
		ty := reflect.TypeOf(v)
		for i := range ty.NumField() {
			out = append(out, ty.Field(i).Tag.Get("yaml"))
		}
		slices.Sort(out)
		return out
	}
	names := func(m map[string]any) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	service := map[string]any{}
	for k := range schema.Defs.Service.Properties {
		service[k] = nil
	}
	if got, want := names(schema.Properties), fields(bump.Policy{}); !slices.Equal(got, want) {
		t.Errorf("the schema's fields are %v, the policy's %v", got, want)
	}
	if got, want := names(service), fields(bump.ServicePolicy{}); !slices.Equal(got, want) {
		t.Errorf("the schema's service fields are %v, the policy's %v", got, want)
	}
	if got, want := names(schema.Defs.Service.Properties["signedBy"].Properties), fields(bump.Signer{}); !slices.Equal(got, want) {
		t.Errorf("the schema's signedBy fields are %v, the policy's %v", got, want)
	}
}

// check refuses a policy beside the entries that a bump couldn't follow.
func TestCheckRefusesAPolicyABumpCantFollow(t *testing.T) {
	dir := t.TempDir()
	entry, err := os.ReadFile(filepath.Join("..", "..", "internal", "catalog", "snapshot", "entries", "gotify.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for path, text := range map[string]string{
		"entries/gotify.yaml":  string(entry),
		"updates/gotify.yaml":  "services:\n  web:\n    level: patch\n",
		"updates/nothing.yaml": "gives: app\n",
		"updates/README.md":    "notes",
	} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	err = check(t.Context(), []string{filepath.Join(dir, "entries")}, &out)
	for _, want := range []string{"updates/gotify.yaml: it names the service web, which the entry doesn't have", "updates/nothing.yaml: there's no entry nothing", "updates/README.md: it isn't a policy"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("check says %q, not %q", out.String(), want)
		}
	}
	if err == nil {
		t.Error("check passes a policy a bump can't follow")
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
