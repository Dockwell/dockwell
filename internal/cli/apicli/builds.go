// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"fmt"
	"io"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/human"
)

// buildFollow prints what a follow of a build gained: its lines, and how it ended
// once it has, on a line of its own (ADR 0046).
func buildFollow(w io.Writer, m *dockwellv1.WatchBuildResponse) {
	buildLines(w, m.GetLines())
	if len(m.GetLines()) == 0 && buildEnded(m.GetBuild()) {
		fmt.Fprintln(w, buildSummary(m.GetBuild()))
	}
}

// buildLog prints what a build's log keeps, saying where it left lines out, and how
// many its server dropped.
func buildLog(w io.Writer, m *dockwellv1.ReadBuildLogResponse) {
	buildLines(w, m.GetLines())
	if n := m.GetDropped(); n > 0 {
		fmt.Fprintf(w, "… %s lines dropped, as the build printed faster than its log could take them …\n", count(uint64(n)))
	}
}

func buildLines(w io.Writer, lines []*dockwellv1.BuildLine) {
	var last int64
	for _, l := range lines {
		if last > 0 && l.GetNumber() > last+1 {
			fmt.Fprintf(w, "… %s lines left out …\n", count(uint64(l.GetNumber()-last-1))) //nolint:gosec // more than the last, so positive
		}
		last = l.GetNumber()
		fmt.Fprintln(w, terminalSafe(l.GetText(), false))
	}
}

func buildEnded(b *dockwellv1.Build) bool {
	s := b.GetState()
	return s != dockwellv1.BuildState_BUILD_STATE_UNSPECIFIED && s != dockwellv1.BuildState_BUILD_STATE_QUEUED && s != dockwellv1.BuildState_BUILD_STATE_BUILDING
}

// buildSummary says how a build ended, in a sentence.
func buildSummary(b *dockwellv1.Build) string {
	switch b.GetState() {
	case dockwellv1.BuildState_BUILD_STATE_DONE:
		s := "Built " + shortCommit(b.GetCommit())
		if d := buildTook(b); d > 0 {
			s += " in " + human.Duration(d.Round(time.Second))
		}
		if b.GetSteps() > 0 {
			s += fmt.Sprintf(", %d of %d steps from the cache", b.GetCachedSteps(), b.GetSteps())
		}
		return s + "."
	case dockwellv1.BuildState_BUILD_STATE_FAILED:
		return sentence(terminalSafe(b.GetError(), false))
	case dockwellv1.BuildState_BUILD_STATE_CANCELLED:
		if b.GetCancelledBy() != "" {
			return "Cancelled by " + terminalSafe(b.GetCancelledBy(), false) + "."
		}
		return "Cancelled."
	case dockwellv1.BuildState_BUILD_STATE_LOST:
		return b.GetServerName() + " went away during the build, so how it ended isn't known."
	case dockwellv1.BuildState_BUILD_STATE_UNSPECIFIED, dockwellv1.BuildState_BUILD_STATE_QUEUED, dockwellv1.BuildState_BUILD_STATE_BUILDING:
	}
	return ""
}

// buildTook is how long a build took, or has taken so far.
func buildTook(b *dockwellv1.Build) time.Duration {
	if b.GetStartTime() == nil || b.GetEndTime() == nil {
		return 0
	}
	return b.GetEndTime().AsTime().Sub(b.GetStartTime().AsTime())
}

// buildsTable lists builds, newest first.
func buildsTable(w io.Writer, m *dockwellv1.ListBuildsResponse, now time.Time) {
	if len(m.GetBuilds()) == 0 {
		fmt.Fprintln(w, "No builds yet.")
		return
	}
	tw := newTable(w)
	row(tw, "BUILD", "STATE", "COMMIT", "SUBJECT", "SERVER", "TOOK", "STARTED")
	for _, b := range m.GetBuilds() {
		took := "-"
		if d := buildTook(b); d > 0 {
			took = human.Duration(d.Round(time.Second))
		}
		row(tw, b.GetId(), enumWord(b.GetState().Descriptor(), b.GetState().Number()), orDash(shortCommit(b.GetCommit())),
			orDash(terminalSafe(cutWords(b.GetCommitSubject(), 48), false)), orDash(b.GetServerName()), took, when(b.GetCreateTime(), now))
	}
	_ = tw.Flush()
	if t := m.GetNextPageToken(); t != "" {
		fmt.Fprintf(w, "There are older builds: pass --page-token %s.\n", t)
	}
}

func shortCommit(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

// cutWords cuts s to n characters, saying so.
func cutWords(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// buildWords says what a step builds before it runs it, as a plan does.
func buildWords(b *dockwellv1.BuildPlan) string {
	repo := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(b.GetUrl(), "https://"), "http://"), ".git")
	at := shortCommit(b.GetCommit())
	if b.GetRef() != "" {
		at += " (" + shownRef(b.GetRef()) + ")"
	}
	where := ""
	if f := strings.Trim(b.GetContext()+"/"+cmpOr(b.GetDockerfile(), "Dockerfile"), "/"); f != "Dockerfile" {
		where = ", with " + f
	}
	return fmt.Sprintf("Builds %s at %s%s first. If the build fails, what runs stays as it is.", repo, at, where)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// shownRef is a branch or a tag as people name it: main for refs/heads/main.
func shownRef(ref string) string {
	return strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
}

// buildDetection says how a commit would build, as DetectBuild read it.
func buildDetection(w io.Writer, m *dockwellv1.DetectBuildResponse) {
	ref := ""
	if m.GetRef() != "" && m.GetRef() != m.GetCommit() {
		ref = " (" + terminalSafe(shownRef(m.GetRef()), false) + ")"
	}
	fmt.Fprintf(w, "%s read %s%s.\n", terminalSafe(m.GetServerName(), false), shortCommit(m.GetCommit()), ref)
	detectionView(w, m.GetDetection())
}

// detectionView says what reading a commit found: its Dockerfile, or what
// Railpack makes of it.
func detectionView(w io.Writer, d *dockwellv1.BuildDetection) {
	safe := func(s string) string { return terminalSafe(s, false) }
	if d.GetBuilder() == resourcev1.ImageBuild_BUILDER_DOCKERFILE {
		s := "Builds with its Dockerfile, " + safe(d.GetDockerfile())
		if d.GetBaseImage() != "" {
			s += ", from " + safe(d.GetBaseImage())
		}
		fmt.Fprintln(w, s+".")
		return
	}
	if d.GetError() != "" {
		fmt.Fprintln(w, "Railpack can't build it. It says:")
		for _, line := range strings.Split(strings.TrimSpace(d.GetError()), "\n") {
			fmt.Fprintln(w, "  "+safe(line))
		}
		fmt.Fprintln(w, "Add a Dockerfile, or set the build's start command.")
		return
	}
	s := "Builds with Railpack"
	if d.GetRailpackVersion() != "" {
		s += " " + safe(d.GetRailpackVersion())
	}
	if len(d.GetProviders()) > 0 {
		s += ", as " + safe(strings.Join(d.GetProviders(), ", "))
	}
	fmt.Fprintln(w, s+".")
	t := newTable(w)
	for _, p := range d.GetPackages() {
		from := ""
		if p.GetSource() != "" {
			from = "from " + safe(p.GetSource())
		}
		fmt.Fprintf(t, "  %s\t%s\t%s\n", safe(p.GetName()), safe(orDash(p.GetResolved())), from)
	}
	for _, st := range d.GetSteps() {
		for i, c := range st.GetCommands() {
			name := ""
			if i == 0 {
				name = safe(st.GetName())
			}
			fmt.Fprintf(t, "  %s\t%s\t\n", name, safe(c))
		}
	}
	start := d.GetStartCommand()
	if start == "" {
		start = "none found: set the build's start command"
	}
	fmt.Fprintf(t, "  starts\t%s\t\n", safe(start))
	_ = t.Flush()
	for _, warning := range d.GetWarnings() {
		fmt.Fprintln(w, "Railpack warns: "+sentence(safe(warning)))
	}
}
