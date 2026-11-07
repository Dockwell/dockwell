// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"
	"google.golang.org/protobuf/encoding/protojson"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// PinsFile is where a folder of converted entries keeps what the converter knows of
// each image's tag.
const PinsFile = "pins.json"

// ReportFile says what each run converted, and why it left the rest out.
const ReportFile = "REPORT.md"

// Encode writes an entry as the catalog reads one, after a header that says where
// it came from, its licence, and that it was modified and when.
func Encode(e *catalogv1.Entry, src Source, date string) ([]byte, error) {
	asJSON, err := protojson.Marshal(e)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(asJSON, &doc); err != nil {
		return nil, err
	}
	plain(&doc)
	if c := mapGet(doc.Content[0], "compose"); c != nil {
		c.Style = yaml.LiteralStyle
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "# SPDX-License-Identifier: %s\n", src.License)
	fmt.Fprintf(&b, "# Converted from %s, %s, %s,\n", src.Name, src.Repository, e.GetSource().GetPath())
	fmt.Fprintf(&b, "# and modified by Dockwell's converter on %s.\n", date)
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// plain clears the styles JSON gave each node, so the file reads as YAML.
func plain(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		plain(c)
	}
}

var modifiedOn = regexp.MustCompile(`(?m)^# and modified by Dockwell's converter on (\d{4}-\d{2}-\d{2})\.$`)

// ReadPrevious reads the entries an earlier run wrote to dir, with the date of each
// one's release, and what it kept of the images it pinned.
func ReadPrevious(dir string) (map[string]*catalogv1.Entry, map[string]string, map[string]Pinned, error) {
	entries, dates, pins := map[string]*catalogv1.Entry{}, map[string]string{}, map[string]Pinned{}
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, nil, nil, err
	}
	for _, file := range files {
		raw, err := os.ReadFile(file) //nolint:gosec // an earlier run's entry, in the folder the person running it names
		if err != nil {
			return nil, nil, nil, err
		}
		e, err := catalog.Decode(raw)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("the earlier run's %s: %w", filepath.Base(file), err)
		}
		entries[e.GetId()] = e
		if m := modifiedOn.FindSubmatch(raw); m != nil {
			dates[e.GetId()] = string(m[1])
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, PinsFile)) //nolint:gosec // in the folder the person running it names
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, nil, nil, err
	default:
		if err := json.Unmarshal(raw, &pins); err != nil {
			return nil, nil, nil, fmt.Errorf("the earlier run's %s: %w", PinsFile, err)
		}
	}
	return entries, dates, pins, nil
}

// Write writes a run into dir: each entry it converted, the report, and the pins. An
// entry an earlier run wrote stays when this one may convert it again later, and
// goes when it was left out for good.
func Write(dir string, src Source, results []Result, previous map[string]*catalogv1.Entry, pins map[string]Pinned) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a catalog is for anyone to read
		return err
	}
	stay, gone := Kept(results, previous)
	for _, r := range results {
		if r.Entry == nil {
			continue
		}
		out, err := Encode(r.Entry, src, r.Date)
		if err != nil {
			return fmt.Errorf("writing %s: %w", r.ID, err)
		}
		if err := os.WriteFile(filepath.Join(dir, r.ID+".yaml"), out, 0o644); err != nil { //nolint:gosec // a catalog is for anyone to read
			return err
		}
	}
	for _, id := range gone {
		if err := os.Remove(filepath.Join(dir, id+".yaml")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	pinned, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, PinsFile), append(pinned, '\n'), 0o644); err != nil { //nolint:gosec // a catalog is for anyone to read
		return err
	}
	return os.WriteFile(filepath.Join(dir, ReportFile), Report(src, results, stay, gone), 0o644) //nolint:gosec // a catalog is for anyone to read
}

// The reasons an app is left out, in the words the report counts them by.
var reasonClasses = []struct{ has, class string }{
	{"reaches its server", "Reaches its server, waiting for a person to look"},
	{"runs scripts", "Runs scripts on the server"},
	{"from another app", "Uses a database or service from another app"},
	{"ships ", "Ships config files beside its Compose file"},
	{"it needs a summary", "Converts once a person writes its summary or category"},
	{"it needs a category", "Converts once a person writes its summary or category"},
	{"days old", "Its version is under 14 days old"},
	{"isn't known", "Its image's age isn't known yet"},
	{"no version", "Its image names no version"},
	{"Dockwell's own entry", "Dockwell has its own entry"},
	{"its platform fills in", "Uses a value its platform fills in"},
	{"pinning", "Its image couldn't be pinned"},
}

func classOf(reason string) string {
	for _, c := range reasonClasses {
		if strings.Contains(reason, c.has) {
			return c.class
		}
	}
	return "Something else"
}

// Report says what a run converted and why it left the rest out, for a person to see
// what's missing and what to write.
func Report(src Source, results []Result, stay, gone []string) []byte {
	var b strings.Builder
	converted, counts := 0, map[string]int{}
	for _, r := range results {
		if r.Entry != nil {
			converted++
		} else {
			counts[classOf(r.LeftOut)]++
		}
	}
	commit := src.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	fmt.Fprintf(&b, "# %s, converted\n\n", upperFirst(src.Name))
	fmt.Fprintf(&b, "At [`%s`](%s/tree/%s): %d apps, %d converted, %d left out.\n\n", commit, src.Repository, src.Commit, len(results), converted, len(results)-converted)
	if len(counts) > 0 {
		b.WriteString("## Why apps are left out\n\n| Reason | Apps |\n| --- | ---: |\n")
		classes := make([]string, 0, len(counts))
		for c := range counts {
			classes = append(classes, c)
		}
		slices.SortFunc(classes, func(x, y string) int {
			if counts[x] != counts[y] {
				return counts[y] - counts[x]
			}
			return strings.Compare(x, y)
		})
		for _, c := range classes {
			fmt.Fprintf(&b, "| %s | %d |\n", c, counts[c])
		}
		b.WriteString("\n## Each app left out\n\n| App | Version | Why |\n| --- | --- | --- |\n")
		for _, r := range results {
			if r.Entry == nil {
				fmt.Fprintf(&b, "| %s | `%s` | %s |\n", r.ID, r.Path, cell(r.LeftOut))
			}
		}
		b.WriteString("\n")
	}
	if converted > 0 {
		b.WriteString("## Converted\n\n| App | Version | Release | What the converter changed beyond the rules |\n| --- | --- | ---: | --- |\n")
		for _, r := range results {
			if r.Entry != nil {
				fmt.Fprintf(&b, "| %s | %s | %d | %s |\n", r.ID, r.Entry.GetVersion(), r.Entry.GetRelease(), cell(strings.Join(r.Notes, "; ")))
			}
		}
		b.WriteString("\n")
	}
	if len(stay) > 0 {
		fmt.Fprintf(&b, "Kept as an earlier run wrote them, as a later run may convert them again: %s.\n\n", strings.Join(stay, ", "))
	}
	if len(gone) > 0 {
		fmt.Fprintf(&b, "Removed, as this run left them out: %s.\n", strings.Join(gone, ", "))
	}
	return []byte(b.String())
}

func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
