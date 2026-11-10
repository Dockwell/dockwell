// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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
	if line := copyrightOf(src); line != "" {
		fmt.Fprintf(&b, "# %s\n", line)
	}
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
			return nil, nil, nil, fmt.Errorf("reading the earlier run's %s: %w", filepath.Base(file), err)
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
		return nil, nil, nil, fmt.Errorf("reading the earlier run's %s: %w", PinsFile, err)
	default:
		if err := json.Unmarshal(raw, &pins); err != nil {
			return nil, nil, nil, fmt.Errorf("the earlier run's %s: %w", PinsFile, err)
		}
	}
	return entries, dates, pins, nil
}

// RowsFile keeps the report's rows, so a run over some apps alone merges into what
// the last run over them all said.
const RowsFile = "report.json"

// Row is what the report says of one app.
type Row struct {
	ID      string   `json:"id"`
	Path    string   `json:"path"`
	Version string   `json:"version,omitempty"`
	Release uint32   `json:"release,omitempty"`
	Class   Class    `json:"class,omitempty"`
	Why     string   `json:"why,omitempty"`
	Held    string   `json:"held,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

func rowOf(r *Result) Row {
	row := Row{ID: r.ID, Path: r.Path, Class: r.Class, Why: r.LeftOut, Held: r.Held, Notes: r.Notes}
	if r.Entry != nil {
		row.Version, row.Release, row.Why = r.Entry.GetVersion(), r.Entry.GetRelease(), ""
	}
	return row
}

// Write writes a run into dir: each entry it converted, the report, and the pins. An
// entry an earlier run wrote stays when this one may convert it again later, and
// goes when it was left out for good. A run over some apps alone, all false, removes
// only what it read, and its report merges into the last one.
func Write(dir string, src Source, results []Result, previous map[string]*catalogv1.Entry, pins map[string]Pinned, all bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a catalog is for anyone to read
		return fmt.Errorf("making %s: %w", dir, err)
	}
	stay, gone := Kept(results, previous, all)
	for i := range results {
		r := &results[i]
		if r.Entry == nil {
			continue
		}
		out, err := Encode(r.Entry, src, r.Date)
		if err != nil {
			return fmt.Errorf("writing %s: %w", r.ID, err)
		}
		file := filepath.Join(dir, r.ID+".yaml")
		if err := os.WriteFile(file, out, 0o644); err != nil { //nolint:gosec // a catalog is for anyone to read
			return fmt.Errorf("writing %s: %w", file, err)
		}
	}
	for _, id := range gone {
		file := filepath.Join(dir, id+".yaml")
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", file, err)
		}
	}
	if len(src.LicenseText) > 0 {
		file := filepath.Join(dir, "LICENSE")
		if err := os.WriteFile(file, src.LicenseText, 0o644); err != nil { //nolint:gosec // a catalog is for anyone to read
			return fmt.Errorf("writing %s: %w", file, err)
		}
	}
	rows := make([]Row, 0, len(results))
	for i := range results {
		rows = append(rows, rowOf(&results[i]))
	}
	if !all {
		earlier, err := readRows(dir)
		if err != nil {
			return err
		}
		read := map[string]bool{}
		for i := range rows {
			read[rows[i].ID] = true
		}
		for i := range earlier {
			if !read[earlier[i].ID] {
				rows = append(rows, earlier[i])
			}
		}
		slices.SortFunc(rows, func(a, b Row) int { return strings.Compare(a.ID, b.ID) })
	}
	for name, v := range map[string]any{PinsFile: pins, RowsFile: rows} {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, append(data, '\n'), 0o644); err != nil { //nolint:gosec // a catalog is for anyone to read
			return fmt.Errorf("writing %s: %w", file, err)
		}
	}
	file := filepath.Join(dir, ReportFile)
	if err := os.WriteFile(file, Report(src, rows, stay, gone), 0o644); err != nil { //nolint:gosec // a catalog is for anyone to read
		return fmt.Errorf("writing %s: %w", file, err)
	}
	return nil
}

func readRows(dir string) ([]Row, error) {
	raw, err := os.ReadFile(filepath.Join(dir, RowsFile)) //nolint:gosec // in the folder the person running it names
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading the earlier run's %s: %w", RowsFile, err)
	}
	var rows []Row
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("reading the earlier run's %s: %w", RowsFile, err)
	}
	return rows, nil
}

// Report says what a run converted and why it left the rest out, for a person to see
// what's missing and what to write.
func Report(src Source, rows []Row, stay, gone []string) []byte {
	var b strings.Builder
	converted, held, counts := 0, 0, map[Class]int{}
	for i := range rows {
		r := &rows[i]
		switch {
		case r.Held != "":
			held++
		case r.Version != "":
			converted++
		default:
			counts[cmp.Or(r.Class, ClassCantSay)]++
		}
	}
	commit := src.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	fmt.Fprintf(&b, "# %s, converted\n\n", upperFirst(src.Name))
	fmt.Fprintf(&b, "At [`%s`](%s/tree/%s): %d apps, %d converted, %d kept at an earlier release, %d left out.\n\n",
		commit, src.Repository, src.Commit, len(rows), converted, held, len(rows)-converted-held)
	if len(counts) > 0 {
		b.WriteString("## Why apps are left out\n\n| Reason | Apps |\n| --- | ---: |\n")
		classes := slices.SortedFunc(maps.Keys(counts), func(x, y Class) int {
			return cmp.Or(counts[y]-counts[x], strings.Compare(string(x), string(y)))
		})
		for _, c := range classes {
			fmt.Fprintf(&b, "| %s | %d |\n", cell(string(c)), counts[c])
		}
		b.WriteString("\n## Each app left out\n\n| App | Where | Why |\n| --- | --- | --- |\n")
		for i := range rows {
			r := &rows[i]
			if r.Version == "" && r.Held == "" {
				fmt.Fprintf(&b, "| %s | %s | %s |\n", cell(r.ID), code(r.Path), cell(r.Why))
			}
		}
		b.WriteString("\n")
	}
	if held > 0 {
		b.WriteString("## Kept at an earlier release\n\n| App | Version | Release | Why |\n| --- | --- | ---: | --- |\n")
		for i := range rows {
			r := &rows[i]
			if r.Held != "" {
				fmt.Fprintf(&b, "| %s | %s | %d | %s |\n", cell(r.ID), cell(r.Version), r.Release, cell(r.Held))
			}
		}
		b.WriteString("\n")
	}
	if converted > 0 {
		b.WriteString("## Converted\n\n| App | Version | Release | What the converter changed beyond the rules |\n| --- | --- | ---: | --- |\n")
		for i := range rows {
			r := &rows[i]
			if r.Version != "" && r.Held == "" {
				fmt.Fprintf(&b, "| %s | %s | %d | %s |\n", cell(r.ID), cell(r.Version), r.Release, cell(strings.Join(r.Notes, "; ")))
			}
		}
		b.WriteString("\n")
	}
	if len(stay) > 0 {
		fmt.Fprintf(&b, "Kept as an earlier run wrote them, as a later run may convert them again: %s.\n\n", cell(strings.Join(stay, ", ")))
	}
	if len(gone) > 0 {
		fmt.Fprintf(&b, "Removed, as this run left them out: %s.\n", cell(strings.Join(gone, ", ")))
	}
	return []byte(b.String())
}

// cell writes text as one cell of a Markdown table, whatever it holds.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\r", " ", "\n", " ").Replace(s)
}

// code writes text as code in a cell, with any backtick in it kept out of the way.
func code(s string) string {
	return "`` " + strings.ReplaceAll(cell(s), "``", "` `") + " ``"
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
