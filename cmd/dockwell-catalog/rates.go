// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dockwell/dockwell/internal/catalog/convert"
	"github.com/dockwell/dockwell/internal/gitfetch"
)

// RatesFile is where the repository records what each store converts to, at a
// commit of each it pins, as `make store-rates` reads and writes it.
const RatesFile = "internal/catalog/convert/rates.json"

// A ratesRecord is what converts of each store at the commit it pins, as the
// control plane converts it.
type ratesRecord struct {
	Stores []storeRates `json:"stores"`
}

// storeRates is one store's: the commit it's read at, how many apps it holds, which
// convert, and how many are left out for each kind of reason, by the API's word for
// it, such as "scripts".
type storeRates struct {
	Format     string         `json:"format"`
	Repository string         `json:"repository"`
	Commit     string         `json:"commit"`
	Apps       int            `json:"apps"`
	Converted  []string       `json:"converted"`
	LeftOut    map[string]int `json:"leftOut"`
	// reasons are each left-out app's, by kind, for --list; not recorded.
	reasons map[string][]string
}

const ratesUsage = `Usage: dockwell-catalog rates [--record] [--pin] [--list] [--show <format>/<id>] [--file <record>]

rates converts each store the control plane reads, at the commit its record pins,
as a server converts it, and says how many of its apps install and why the rest
are left out. It fails when an app that converted no longer does, and when the
record doesn't say what converts, which --record writes. --pin moves each store
to its default branch's commit first. --show prints one app's converted entry,
or why it's left out, such as --show coolify/it-tools.
`

// A shown app is one --show names, by its store's format and its ID.
type shown struct {
	format, id string
}

// fetchStore fetches and converts a store's commit, as the control plane does. A
// fetch that fails is tried twice more, a few seconds apart, as a host can drop a
// connection now and then, and a CI job that reads three stores shouldn't fail for it.
var fetchStore = func(ctx context.Context, store convert.Store, repo, commit string) (convert.Run, error) {
	var got convert.Fetched
	var err error
	for try := range 3 {
		if try > 0 {
			select {
			case <-ctx.Done():
				return convert.Run{}, ctx.Err()
			case <-time.After(time.Duration(try) * 3 * time.Second):
			}
		}
		got, err = convert.Fetch(ctx, nil, store, repo, commit, "")
		if err == nil {
			break
		}
	}
	if err != nil {
		return convert.Run{}, err
	}
	defer got.Close()
	converting, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	return convert.OnServer(converting, store, got.Listing, got.Source, got.Time, 0)
}

// headOf is the commit a repository's default branch is at.
var headOf = func(ctx context.Context, repo string) (string, error) {
	l, err := gitfetch.List(ctx, nil, repo)
	if err != nil {
		return "", err
	}
	if at := l.Refs[l.Head]; at != "" {
		return at, nil
	}
	return "", fmt.Errorf("%s names no default branch", repo)
}

func rates(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("rates", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, ratesUsage) }
	file := flags.String("file", RatesFile, "the record of what each store converts to")
	record := flags.Bool("record", false, "write what converts into the record")
	pin := flags.Bool("pin", false, "move each store to its default branch's commit, and record")
	list := flags.Bool("list", false, "list each app left out, and why")
	show := flags.String("show", "", "print one app's converted entry, or why it's left out, as <format>/<id>")
	if err := flags.Parse(args); err != nil {
		return err
	}
	var want shown
	if *show != "" {
		var ok bool
		if want.format, want.id, ok = strings.Cut(*show, "/"); !ok {
			return fmt.Errorf("name the app to show as <format>/<id>, such as coolify/it-tools, not %q", *show)
		}
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("reading the record: %w. Run it from the repository's top, or name it with --file", err)
	}
	var was ratesRecord
	if err := json.Unmarshal(raw, &was); err != nil {
		return fmt.Errorf("reading %s: %w", *file, err)
	}
	var now ratesRecord
	for _, rec := range was.Stores {
		store, ok := convert.StoreOf(rec.Format)
		if !ok {
			return fmt.Errorf("%s names a format the converter doesn't read, %q", *file, rec.Format)
		}
		if want.format != "" && want.format != rec.Format {
			continue
		}
		commit := rec.Commit
		if *pin {
			if commit, err = headOf(ctx, rec.Repository); err != nil {
				return fmt.Errorf("reading %s's default branch: %w", store.Name, err)
			}
		}
		run, err := fetchStore(ctx, store, rec.Repository, commit)
		if err != nil {
			return fmt.Errorf("converting %s at %s: %w", store.Name, commit, err)
		}
		if want.format != "" {
			return showApp(stdout, store, rec, run, want.id)
		}
		now.Stores = append(now.Stores, ratesOf(rec.Format, rec.Repository, commit, run))
	}
	if want.format != "" {
		return fmt.Errorf("the record has no store of the format %q", want.format)
	}
	printRates(stdout, now, *list)
	if *record || *pin {
		out, err := json.MarshalIndent(now, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*file, append(out, '\n'), 0o644); err != nil { //nolint:gosec // a file in the repository, read by everyone
			return err
		}
		fmt.Fprintf(stdout, "\nRecorded in %s.\n", *file)
		return nil
	}
	return compareRates(was, now)
}

// showApp prints one app of a store's run: its entry as the control plane keeps it,
// or why it's left out.
func showApp(w io.Writer, store convert.Store, rec storeRates, run convert.Run, id string) error {
	for i := range run.Results {
		r := &run.Results[i]
		if r.ID != id {
			continue
		}
		if r.Entry == nil {
			fmt.Fprintf(w, "%s leaves out %s, as %s: %s\n", store.Name, id, r.Class.Key(), r.LeftOut)
			return nil
		}
		src := convert.Source{Name: store.SourceName(rec.Repository), Repository: rec.Repository, Commit: rec.Commit, License: r.Entry.GetLicense()}
		raw, err := convert.Encode(r.Entry, src, time.Now().UTC().Format(time.DateOnly))
		if err != nil {
			return err
		}
		_, err = w.Write(raw)
		return err
	}
	return fmt.Errorf("%s has no app %s at %s", store.Name, id, rec.Commit)
}

// ratesOf counts a store's run.
func ratesOf(format, repo, commit string, run convert.Run) storeRates {
	out := storeRates{Format: format, Repository: repo, Commit: commit, Apps: len(run.Results), Converted: []string{}, LeftOut: map[string]int{}, reasons: map[string][]string{}}
	for i := range run.Results {
		r := &run.Results[i]
		if r.Entry != nil {
			out.Converted = append(out.Converted, r.ID)
			continue
		}
		k := r.Class.Key()
		out.LeftOut[k]++
		out.reasons[k] = append(out.reasons[k], r.ID+": "+r.LeftOut)
	}
	slices.Sort(out.Converted)
	return out
}

// printRates says what each store converts to, and why the rest are left out, the
// most first.
func printRates(w io.Writer, rec ratesRecord, list bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "Store\tCommit\tApps\tInstall\tRate\t")
	for _, s := range rec.Stores {
		store, _ := convert.StoreOf(s.Format)
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t\n", store.Name, s.Commit[:min(len(s.Commit), 12)], s.Apps, len(s.Converted), percent(len(s.Converted), s.Apps))
	}
	_ = tw.Flush()
	for _, s := range rec.Stores {
		store, _ := convert.StoreOf(s.Format)
		fmt.Fprintf(w, "\n%s leaves out %d:\n", store.Name, s.Apps-len(s.Converted))
		for _, k := range byCount(s.LeftOut) {
			fmt.Fprintf(w, "%5d  %s (%s)\n", s.LeftOut[k], convert.ClassOfKey(k), k)
			if list {
				for _, why := range s.reasons[k] {
					fmt.Fprintf(w, "         %s\n", why)
				}
			}
		}
	}
}

// byCount are a count's keys, the most first, then by name.
func byCount(m map[string]int) []string {
	keys := slices.Collect(maps.Keys(m))
	slices.SortFunc(keys, func(a, b string) int { return cmp.Or(m[b]-m[a], strings.Compare(a, b)) })
	return keys
}

func percent(n, of int) string {
	if of == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", n*100/of)
}

// compareRates fails where a store converts less than its record says, naming each
// app that no longer converts, and where the record doesn't say what converts now.
func compareRates(was, now ratesRecord) error {
	var fell, moved []string
	for i, s := range now.Stores {
		rec := was.Stores[i]
		store, _ := convert.StoreOf(s.Format)
		var gone []string
		for _, id := range rec.Converted {
			if !slices.Contains(s.Converted, id) {
				gone = append(gone, id)
			}
		}
		if len(gone) > 0 {
			fell = append(fell, fmt.Sprintf("%s no longer converts %s", store.Name, strings.Join(gone, ", ")))
		}
		if s.Apps != rec.Apps || !slices.Equal(s.Converted, rec.Converted) || !maps.Equal(s.LeftOut, rec.LeftOut) {
			moved = append(moved, fmt.Sprintf("%s converts %d of %d, where the record says %d of %d", store.Name, len(s.Converted), s.Apps, len(rec.Converted), rec.Apps))
		}
	}
	switch {
	case len(fell) > 0:
		return errors.New(strings.Join(fell, "; ") + ". Bring them back, or record why with make store-rates RECORD=1 and say so in the pull request")
	case len(moved) > 0:
		return errors.New(strings.Join(moved, "; ") + ". Record it with make store-rates RECORD=1")
	}
	return nil
}
