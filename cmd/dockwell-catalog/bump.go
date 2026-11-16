// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/catalog/bump"
	"github.com/dockwell/dockwell/internal/registry"
)

// updatesSchema is the schema of a policy in a catalog's updates/ folder (ADR
// 0100), for an editor to check one as it's written; dockwell-catalog reads them
// to the same rules, strictly.
//
//go:embed updates.schema.json
var updatesSchema []byte

// BumpsFile is the list of a bump's pull requests, which the job that opens them
// reads with nothing but jq: each its branch, title, body, labels and patch.
const BumpsFile = "bumps.json"

// bumpOutput is what BumpsFile holds.
type bumpOutput struct {
	Bumps []bumpPR `json:"bumps"`
	// Notes say why an entry, or an image of it, gets no bump.
	Notes []entryNote `json:"notes"`
	// Unread are the images that couldn't be read this run.
	Unread []entryNote `json:"unread"`
}

type bumpPR struct {
	Entry  string   `json:"entry"`
	Lane   string   `json:"lane"`
	Branch string   `json:"branch"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
	// Patch is the patch's file, in the output folder.
	Patch string `json:"patch"`
}

type entryNote struct {
	Entry string `json:"entry"`
	Said  string `json:"said"`
}

// seenMarker starts the line of a bump's body that records when it first saw each
// version its registry gives no push time of, which the next run reads back.
const seenMarker = "<!-- dockwell-bump-seen "

// bumpDeps are what a bump reads, which a test replaces.
type bumpDeps struct {
	sources bump.Sources
	now     time.Time
}

func bumpCommand(ctx context.Context, args []string, stdout, stderr io.Writer, deps *bumpDeps) error {
	flags := flag.NewFlagSet("bump", flag.ContinueOnError)
	flags.SetOutput(stderr)
	checkout := flags.String("catalog", "", "a checkout of the catalog: entries/ and updates/ in it")
	out := flags.String("out", "", "the folder the patches and "+BumpsFile+" go in")
	seenFile := flags.String("seen", "", "a file of when each version was first seen, as the open bumps' bodies record it")
	only := flags.String("only", "", "the entries to bump, by ID, separated by commas; every entry when left out")
	schema := flags.Bool("schema", false, "print the schema of a policy in updates/, and do nothing else")
	deadline := flags.Duration("deadline", 25*time.Minute, "how long the run reads before it writes what it has, leaving the rest unread; below the job's own limit")
	fromBodies := flags.Bool("seen-from-bodies", false, "read the open bumps' bodies, as JSON lines of {\"body\": ...}, on standard input, and print when each version was first seen")
	if err := flags.Parse(args); err != nil {
		return err
	}
	switch {
	case *schema:
		_, err := stdout.Write(updatesSchema)
		return err
	case *fromBodies:
		return seenFromBodies(os.Stdin, stdout)
	}
	if *checkout == "" || *out == "" {
		return errors.New("name the catalog's checkout with --catalog, and where the patches go with --out")
	}
	seen, err := readSeen(*seenFile)
	if err != nil {
		return err
	}
	if deps == nil {
		deps = &bumpDeps{
			sources: &bump.Live{
				Registry: registry.New(registry.Options{}),
				Token:    os.Getenv("GITHUB_TOKEN"),
				Tries:    4,
				// Waiting on Docker Hub's 429s takes at most a third of the run.
				Budget: *deadline / 3,
			},
			now: time.Now(),
		}
	}
	ids, err := entryIDs(*checkout, *only)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o750); err != nil {
		return err
	}
	// The run writes what it has before the job's limit, naming what it didn't read.
	ctx, cancel := context.WithTimeout(ctx, *deadline)
	defer cancel()
	p := &bump.Planner{Sources: deps.sources, Now: deps.now, Seen: seen}
	result := bumpOutput{Bumps: []bumpPR{}, Notes: []entryNote{}, Unread: []entryNote{}}
	for _, id := range ids {
		if ctx.Err() != nil {
			result.Unread = append(result.Unread, entryNote{id, fmt.Sprintf("the run's %s ran out before it was read", *deadline)})
			continue
		}
		e, err := bump.ReadEntry(*checkout, id)
		if err != nil {
			result.Notes = append(result.Notes, entryNote{id, "Its file can't be read: " + err.Error() + "."})
			continue
		}
		policy, err := bump.ReadPolicy(*checkout, id)
		if err != nil {
			result.Notes = append(result.Notes, entryNote{id, "Its policy in updates/" + id + ".yaml can't be read: " + err.Error() + "."})
			continue
		}
		r := p.Plan(ctx, e, policy)
		for _, n := range r.Notes {
			result.Notes = append(result.Notes, entryNote{id, n})
		}
		for _, n := range r.Unread {
			result.Unread = append(result.Unread, entryNote{id, n})
		}
		for i := range r.Lanes {
			l := &r.Lanes[i]
			file := strings.ReplaceAll(l.Branch, "/", "-") + ".patch"
			if err := os.WriteFile(filepath.Join(*out, file), l.Patch, 0o600); err != nil {
				return err
			}
			result.Bumps = append(result.Bumps, bumpPR{
				Entry: id, Lane: l.Lane, Branch: l.Branch, Title: l.Title, Body: body(e, l, deps.now),
				Labels: append(slices.Clone(l.Labels), "bump"), Patch: file,
			})
		}
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, BumpsFile), append(raw, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Read %d entries: %d bumps to open, %d notes, %d images unread. %s says each.\n",
		len(ids), len(result.Bumps), len(result.Notes), len(result.Unread), filepath.Join(*out, BumpsFile))
	return nil
}

// entryIDs are the entries of a checkout, by ID, or those only names.
func entryIDs(checkout, only string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(checkout, "entries", "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s has no entries/*.yaml. Name a checkout of the catalog", checkout)
	}
	var ids []string
	for _, f := range files {
		ids = append(ids, strings.TrimSuffix(filepath.Base(f), ".yaml"))
	}
	if only == "" {
		return ids, nil
	}
	var picked []string
	for id := range strings.SplitSeq(only, ",") {
		if !slices.Contains(ids, id) {
			return nil, fmt.Errorf("the catalog has no entry %s", id)
		}
		picked = append(picked, id)
	}
	return picked, nil
}

// readSeen reads a file of when each version was first seen: JSON of an image's
// repository, tag and digest to a time, as bump.SeenKey names them. No file is none seen.
func readSeen(file string) (map[string]time.Time, error) {
	seen := map[string]time.Time{}
	if file == "" {
		return seen, nil
	}
	raw, err := os.ReadFile(file) //nolint:gosec // a file the job running it names
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &seen); err != nil {
		return nil, fmt.Errorf("reading %s: %w", file, err)
	}
	return seen, nil
}

// body is a bump's pull request's body: what moves, each step's age and what
// corroborates it, why it waits for a person, and, on a line of its own, when it
// first saw each version that has no push time to read.
func body(e *bump.Entry, l *bump.Lane, now time.Time) string {
	var b strings.Builder
	if l.Version != e.Version {
		fmt.Fprintf(&b, "Moves %s from %s to %s, as release %d.\n\n", e.ID, e.Version, l.Version, l.Release)
	} else {
		fmt.Fprintf(&b, "Moves %s's images, as release %d.\n\n", e.ID, l.Release)
	}
	b.WriteString("| Service | From | To | Step | Out | Corroborated by |\n|---|---|---|---|---|---|\n")
	seen := map[string]string{}
	var waits []string
	for i := range l.Steps {
		s := &l.Steps[i]
		out := "first seen " + s.Out.UTC().Format("2006-01-02")
		if s.Pushed {
			out = "pushed " + s.Out.UTC().Format("2006-01-02")
		}
		// Every version the run read of the image, so one passed over keeps its time.
		for k, at := range s.Saw {
			seen[k] = at.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "| %s | `%s` | `%s` | %s | %s, %s | %s |\n", s.Service, s.FromTag, s.ToTag, s.Kind, out,
			age(now.Sub(s.Out)), cmpOr(s.Corroborated, "nothing"))
		for _, w := range s.Waits {
			waits = append(waits, s.Service+": "+w+".")
		}
	}
	if len(waits) > 0 {
		b.WriteString("\nIt waits for a person:\n")
		for _, w := range waits {
			b.WriteString("- " + w + "\n")
		}
	}
	b.WriteString("\nNothing in Dockwell's catalog merges by itself yet (ADR 0100, slice 1): a person reviews and merges this.\n")
	if len(seen) > 0 {
		raw, _ := json.Marshal(seen)
		b.WriteString("\n" + seenMarker + string(raw) + " -->\n")
	}
	return b.String()
}

// age says how long ago something was, in days: "today" within the first.
func age(d time.Duration) string {
	switch n := int(d / (24 * time.Hour)); n {
	case 0:
		return "today"
	case 1:
		return "1 day ago"
	default:
		return fmt.Sprintf("%d days ago", n)
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// seenFromBodies reads when each version was first seen from open bumps' bodies,
// as the read job gives them: JSON values of {"body": ...}, as gh pr list --json
// body writes them through jq -c '.[]'. The earliest each was seen stands.
func seenFromBodies(r io.Reader, w io.Writer) error {
	dec := json.NewDecoder(r)
	seen := map[string]string{}
	for {
		var pr struct {
			Body string `json:"body"`
		}
		if err := dec.Decode(&pr); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("reading the open bumps: %w", err)
		}
		for line := range strings.SplitSeq(pr.Body, "\n") {
			rest, ok := strings.CutPrefix(strings.TrimSpace(line), seenMarker)
			if !ok {
				continue
			}
			var said map[string]string
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "-->"))), &said) != nil {
				continue
			}
			for k, v := range said {
				if _, err := time.Parse(time.RFC3339, v); err != nil {
					continue
				}
				if have, ok := seen[k]; !ok || v < have {
					seen[k] = v
				}
			}
		}
	}
	raw, err := json.MarshalIndent(seen, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(raw, '\n'))
	return err
}
