// SPDX-License-Identifier: AGPL-3.0-only

package bump

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/dockwell/dockwell/internal/registry"
)

// bump scope (ADR 0100, slice 2) says whether a pull request is one the catalog
// could take without a person: it reads the diff alone, never running the head. A
// person's pull request passes, as the review rule decides it. One from the bump
// App passes only where it's the bump the App would make now, with nothing waiting
// for a person, or a maintainer approved its head.

// Change is a file a pull request changes, as git diff --name-status --no-renames
// says it: its status letter and its path.
type Change struct {
	Status string
	Path   string
}

// ScopeInput is what bump scope reads of a pull request.
type ScopeInput struct {
	// Author is who opened it, as GitHub names it: "app/<slug>" for an App.
	Author string
	// App is the bump App, as GitHub names it, such as app/dockwell-catalog-bump.
	App string
	// Branch is its head branch, and CrossRepository says it's from a fork.
	Branch          string
	CrossRepository bool
	// Approved says a maintainer approved its head commit.
	Approved bool
	// Changes are what it changes against its base.
	Changes []Change
	// BaseEntry and HeadEntry are the file of the one entry it changes, as the
	// base and the head have it.
	BaseEntry, HeadEntry []byte
}

// ScopeResult says whether a pull request is in scope, and why.
type ScopeResult struct {
	Pass bool
	// Why says why it passes or what puts it out of scope, a line each.
	Why []string
	// Entry is the entry an App bump moves, for its step to be checked.
	Entry string
}

var bumpBranch = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^bump/([a-z0-9-]+?)(-major)?$`) })

// Scope checks a pull request's diff (ADR 0100): what Diff alone can tell. A bump
// that passes it still needs StepInScope, which reads the registries.
func Scope(in ScopeInput) ScopeResult {
	switch {
	case in.Author != in.App || in.CrossRepository:
		return ScopeResult{Pass: true, Why: []string{"A person's pull request: the review rule decides it"}}
	case in.Approved:
		return ScopeResult{Pass: true, Why: []string{"A maintainer approved its head"}}
	}
	var why []string
	m := bumpBranch().FindStringSubmatch(in.Branch)
	if m == nil {
		return ScopeResult{Why: []string{fmt.Sprintf("Its branch %s isn't a bump's", in.Branch)}}
	}
	id, major := m[1], m[2] != ""
	if major {
		why = append(why, "A step beyond its policy's levels waits for a person")
	}
	want := path.Join("entries", id+".yaml")
	switch {
	case len(in.Changes) != 1:
		var paths []string
		for _, c := range in.Changes {
			paths = append(paths, c.Path)
		}
		why = append(why, fmt.Sprintf("It changes %d files (%s), and a bump changes its entry's alone", len(in.Changes), strings.Join(paths, ", ")))
	case in.Changes[0].Path != want || in.Changes[0].Status != "M":
		why = append(why, fmt.Sprintf("It changes %s (%s), and a bump changes %s alone", in.Changes[0].Path, in.Changes[0].Status, want))
	default:
		why = append(why, lineChanges(in.BaseEntry, in.HeadEntry)...)
	}
	return ScopeResult{Pass: len(why) == 0, Why: why, Entry: id}
}

// lineChanges says what in an entry's change isn't a bump's: a line other than an
// image's tag and digest, the version and the release; an image that changes its
// repository; or a release that isn't the base's plus one.
func lineChanges(base, head []byte) []string {
	before := strings.Split(string(base), "\n")
	after := strings.Split(string(head), "\n")
	if len(before) != len(after) {
		return []string{fmt.Sprintf("It has %d lines where its base has %d, and a bump only replaces lines", len(after), len(before))}
	}
	var why []string
	was, err1 := Release(base)
	now, err2 := Release(head)
	if err1 != nil || err2 != nil || now != was+1 {
		why = append(why, fmt.Sprintf("Its release goes from %d to %d, not up by one", was, now))
	}
	for i := range before {
		b, a := before[i], after[i]
		if b == a {
			continue
		}
		switch {
		case strings.HasPrefix(b, "version:") && strings.HasPrefix(a, "version:"):
		case strings.HasPrefix(b, "release:") && strings.HasPrefix(a, "release:"):
			// Read as a whole below.
		default:
			if w := imageLineChange(b, a); w != "" {
				why = append(why, fmt.Sprintf("Line %d: %s", i+1, w))
			}
		}
	}
	return why
}

// imageLineChange says what's wrong with a changed line where only an image's tag
// and digest may change, or "" where that's all that did.
func imageLineChange(b, a string) string {
	bk, bv, bok := strings.Cut(b, "image:")
	ak, av, aok := strings.Cut(a, "image:")
	if !bok || !aok || bk != ak || strings.TrimSpace(bk) != "" {
		return fmt.Sprintf("%q becomes %q, which isn't an image's tag and digest, a version or a release", strings.TrimSpace(b), strings.TrimSpace(a))
	}
	from, err1 := registry.Parse(strings.Trim(strings.TrimSpace(bv), `"'`))
	to, err2 := registry.Parse(strings.Trim(strings.TrimSpace(av), `"'`))
	switch {
	case err1 != nil || err2 != nil:
		return fmt.Sprintf("the image %q can't be read as one", strings.TrimSpace(av))
	case from.Registry != to.Registry || from.Repository != to.Repository:
		return fmt.Sprintf("the image moves from %s to %s, another repository, which only a person moves", from.Name(), to.Name())
	case to.Digest == "":
		return fmt.Sprintf("the image %s isn't pinned by its digest", to.Name()+":"+to.Tag)
	}
	return ""
}

// StepInScope checks that a bump's head is the step the bump would make now from
// its base, within its policy's levels, past its wait, corroborated, and keeping its
// platforms, and says why it isn't, read again from the registries.
func StepInScope(ctx context.Context, p *Planner, base *Entry, policy *Policy, head []byte) []string {
	r := p.Plan(ctx, base, policy)
	if len(r.Unread) > 0 {
		return []string{"Its images can't all be read now: " + strings.Join(r.Unread, "; ")}
	}
	var within *Lane
	for i := range r.Lanes {
		if r.Lanes[i].Lane == LaneWithin {
			within = &r.Lanes[i]
		}
	}
	if within == nil {
		return []string{"The bump makes no step within its policy's levels from its base now"}
	}
	edit := Edit{Images: map[string]string{}, Release: within.Release, Version: within.Version}
	if within.Version == base.Version {
		edit.Version = ""
	}
	var why []string
	for _, s := range within.Steps {
		if s.To != "" {
			edit.Images[s.From] = s.To
		}
		for _, w := range s.Waits {
			why = append(why, s.Service+": "+w)
		}
	}
	after, err := Apply(base.Text, edit)
	if err != nil {
		return append(why, "The step the bump would make can't be written: "+err.Error())
	}
	if string(after) != string(head) {
		why = append(why, "Its change isn't the step the bump makes from its base now")
	}
	return why
}
