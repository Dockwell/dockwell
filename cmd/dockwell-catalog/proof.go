// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/catalog/bump"
	"github.com/dockwell/dockwell/internal/registry"
)

// earlierCommand says which releases an upgrade to an entry is proven from, as
// JSON, for scripts/catalog-e2e.sh (ADR 0100, slice 2).
func earlierCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("earlier", flag.ContinueOnError)
	flags.SetOutput(stderr)
	checkout := flags.String("catalog", "", "a git checkout of the catalog, with its history")
	at := flags.String("at", "HEAD", "the commit the entry is proven at")
	base := flags.String("base", "", "the pull request's base, which it upgrades from; the history's release before when left out")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *checkout == "" || flags.NArg() != 1 {
		return errors.New("name the catalog's checkout with --catalog, and one entry")
	}
	got, err := bump.EarlierReleases(ctx, *checkout, flags.Arg(0), *at, *base)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(got)
}

// whoseCommand says which pull request a commit is judged as, as JSON, for
// scope.yml (#726's slice 2 review, H1): --started is the pull request whose run it
// is, --open the open pull requests with the commit as their head, as GitHub's
// commits/<sha>/pulls lists them, and --branches this repository's heads, as git
// ls-remote prints them.
func whoseCommand(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("scope-whose", flag.ContinueOnError)
	flags.SetOutput(stderr)
	head := flags.String("head", "", "the commit")
	app := flags.String("app", "app/dockwell-catalog-bump", "the bump App, as GitHub names it")
	startedFile := flags.String("started", "", "a file of the pull request whose run it is, as GitHub's API gives it")
	openFile := flags.String("open", "", "a file of the pull requests with the commit, as GitHub's commits/<sha>/pulls gives them")
	branchesFile := flags.String("branches", "", "a file of this repository's heads, as git ls-remote prints them")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *head == "" || *startedFile == "" {
		return errors.New("name the commit with --head, and the pull request whose run it is with --started")
	}
	var started bump.Pull
	if err := readJSON(*startedFile, &started); err != nil {
		return err
	}
	var open []bump.Pull
	if *openFile != "" {
		if err := readJSON(*openFile, &open); err != nil {
			return err
		}
	}
	branches := map[string]string{}
	if *branchesFile != "" {
		raw, err := os.ReadFile(*branchesFile) //nolint:gosec // the file the workflow names
		if err != nil {
			return err
		}
		for line := range strings.SplitSeq(string(raw), "\n") {
			if sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
				branches[ref] = sha
			}
		}
	}
	return json.NewEncoder(stdout).Encode(bump.Whose(*head, *app, &started, open, branches))
}

func readJSON(file string, into any) error {
	raw, err := os.ReadFile(file) //nolint:gosec // the file the workflow names
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("reading %s: %w", file, err)
	}
	return nil
}

// scopeOutput is what scope prints: a commit status's state and description.
type scopeOutput struct {
	State       string   `json:"state"`
	Description string   `json:"description"`
	Why         []string `json:"why"`
}

// scopeDeps are what scope reads beyond git, which a test replaces.
type scopeDeps struct {
	sources bump.Sources
	now     time.Time
}

// scopeCommand is bump scope (ADR 0100): it reads a pull request's diff in a
// checkout of the base that has the head's objects, never checking the head out,
// and prints the commit status it earns. It runs from main's own definition.
func scopeCommand(ctx context.Context, args []string, stdout, stderr io.Writer, deps *scopeDeps) error {
	flags := flag.NewFlagSet("scope", flag.ContinueOnError)
	flags.SetOutput(stderr)
	checkout := flags.String("catalog", "", "a git checkout of the catalog's base, with the head's commit fetched")
	base := flags.String("base", "", "the pull request's base commit")
	head := flags.String("head", "", "the pull request's head commit")
	in := bump.ScopeInput{App: "app/dockwell-catalog-bump"}
	flags.StringVar(&in.Author, "author", "", "who opened it, as GitHub names it")
	flags.StringVar(&in.App, "app", in.App, "the bump App, as GitHub names it")
	flags.StringVar(&in.Branch, "branch", "", "its head branch")
	flags.BoolVar(&in.CrossRepository, "cross-repository", false, "it comes from a fork")
	flags.BoolVar(&in.Approved, "approved", false, "a maintainer approved its head")
	bodyFile := flags.String("body", "", "a file of its body, where its bump recorded when it first saw each version")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *checkout == "" || *base == "" || *head == "" {
		return errors.New("name the checkout with --catalog, and the pull request's --base and --head")
	}
	// What the head changes since it left main, not main's own later changes (#726's
	// slice 2 review, L1).
	forked, err := gitOut(ctx, *checkout, "merge-base", *base, *head)
	if err != nil {
		return err
	}
	from := strings.TrimSpace(string(forked))
	raw, err := gitOut(ctx, *checkout, "diff", "--name-status", "--no-renames", from, *head)
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if status, file, ok := strings.Cut(line, "\t"); ok {
			in.Changes = append(in.Changes, bump.Change{Status: status, Path: file})
		}
	}
	moved := false
	if len(in.Changes) == 1 {
		file := in.Changes[0].Path
		in.BaseEntry, _ = gitOut(ctx, *checkout, "show", from+":"+file)
		in.HeadEntry, _ = gitOut(ctx, *checkout, "show", *head+":"+file)
		now, _ := gitOut(ctx, *checkout, "show", *base+":"+file)
		moved = !bytes.Equal(now, in.BaseEntry)
	}
	r := bump.Scope(in)
	if r.Pass && moved && in.Author == in.App && !in.Approved && !in.CrossRepository {
		r.Pass, r.Why = false, []string{"Its entry moved on main since the bump was made, so the bump makes it again"}
	}
	if r.Pass && in.Author == in.App && !in.Approved && !in.CrossRepository {
		r.Why = append(r.Why, stepInScope(ctx, *checkout, *base, r.Entry, in.HeadEntry, *bodyFile, deps)...)
		r.Pass = len(r.Why) == 0
		if r.Pass {
			r.Why = []string{"The step the bump makes now, with nothing waiting for a person"}
		}
	}
	out := scopeOutput{State: "success", Why: r.Why}
	switch {
	case !r.Pass && slices.ContainsFunc(r.Why, func(w string) bool { return strings.HasPrefix(w, bump.Unread) }):
		// A registry that didn't answer says nothing of the bump: an error, run again.
		out.State = "error"
	case !r.Pass:
		out.State = "failure"
	}
	out.Description = r.Why[0]
	if !r.Pass && len(r.Why) > 1 {
		out.Description = fmt.Sprintf("%s, and %d more", r.Why[0], len(r.Why)-1)
	}
	// A commit status's description holds 140 characters.
	if len(out.Description) > 140 {
		out.Description = out.Description[:137] + "..."
	}
	return json.NewEncoder(stdout).Encode(out)
}

// stepInScope plans the entry again from its base, in a folder of the base's
// entry and policy, and checks the head is that step, with nothing waiting.
func stepInScope(ctx context.Context, checkout, base, id string, head []byte, bodyFile string, deps *scopeDeps) []string {
	work, err := os.MkdirTemp("", "scope")
	if err != nil {
		return []string{"Its base can't be read: " + err.Error()}
	}
	defer os.RemoveAll(work)
	for _, file := range []string{"entries/" + id + ".yaml", "updates/" + id + ".yaml"} {
		text, err := gitOut(ctx, checkout, "show", base+":"+file)
		if err != nil {
			continue
		}
		if err := os.MkdirAll(work+"/"+strings.Split(file, "/")[0], 0o750); err != nil {
			return []string{err.Error()}
		}
		if err := os.WriteFile(work+"/"+file, text, 0o600); err != nil {
			return []string{err.Error()}
		}
	}
	e, err := bump.ReadEntry(work, id)
	if err != nil {
		return []string{"Its base's entry can't be read: " + err.Error()}
	}
	policy, err := bump.ReadPolicy(work, id)
	if err != nil {
		return []string{"Its policy can't be read: " + err.Error()}
	}
	seen := map[string]time.Time{}
	if bodyFile != "" {
		body, err := os.ReadFile(bodyFile) //nolint:gosec // the file the workflow names
		if err != nil {
			return []string{"Its body can't be read: " + err.Error()}
		}
		line, _ := json.Marshal(map[string]string{"body": string(body)})
		var times bytes.Buffer
		if err := seenFromBodies(bytes.NewReader(line), &times); err == nil {
			var said map[string]time.Time
			if json.Unmarshal(times.Bytes(), &said) == nil {
				seen = said
			}
		}
	}
	if deps == nil {
		deps = &scopeDeps{sources: &bump.Live{Registry: registry.New(registry.Options{}), Token: os.Getenv("GITHUB_TOKEN"), Tries: 4, Budget: 5 * time.Minute}, now: time.Now()}
	}
	return bump.StepInScope(ctx, &bump.Planner{Sources: deps.sources, Now: deps.now, Seen: seen}, e, policy, head)
}

func gitOut(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // git, with what the workflow names
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return out, nil
}
