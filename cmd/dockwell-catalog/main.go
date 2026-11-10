// SPDX-License-Identifier: AGPL-3.0-only

// Command dockwell-catalog builds a catalog from other catalogs, as ADR 0029 sets
// out: it converts each source's apps into entries in Dockwell's format, once,
// where the catalog is built. It's a separate program from dockwell, which never
// reads another catalog's format.
//
//	dockwell-catalog convert --from 1panel --source ./appstore --overlays ./overlays/1panel --out ./entries/1panel
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/catalog/convert"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dockwell-catalog:", err)
		os.Exit(1)
	}
}

const usage = `Usage: dockwell-catalog convert --from 1panel --source <checkout> --out <folder> [--overlays <folder>]

Converts a catalog's apps into Dockwell's entries, one file each in --out, with a
report of what it left out and why, and what it knows of each image.

`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "convert" {
		fmt.Fprint(stderr, usage)
		return errors.New("name what to do: convert")
	}
	flags := flag.NewFlagSet("convert", flag.ContinueOnError)
	flags.SetOutput(stderr)
	from := flags.String("from", "", "the source's format: 1panel, dokploy or coolify")
	source := flags.String("source", "", "a checkout of the source")
	out := flags.String("out", "", "the folder converted entries go in, where an earlier run's are")
	overlays := flags.String("overlays", "", "a folder of what people wrote for each app, as <id>.yaml")
	commit := flags.String("commit", "", "the source's commit, read from its checkout when left out")
	only := flags.String("only", "", "the apps to convert, by ID, separated by commas; every app when left out")
	hubGets := flags.Int("hub-gets", 60, "how many manifest GETs to ask of Docker Hub, which counts each as a pull")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *source == "" || *out == "" {
		return errors.New("name the source's checkout with --source, and where entries go with --out")
	}
	if *commit == "" {
		head, err := exec.CommandContext(ctx, "git", "-C", *source, "rev-parse", "HEAD").Output() //nolint:gosec // the checkout the person running it names
		if err != nil {
			return fmt.Errorf("reading the commit of %s: %w. Name it with --commit", *source, err)
		}
		*commit = strings.TrimSpace(string(head))
	}
	var (
		src  convert.Source
		apps []convert.App
		err  error
	)
	switch *from {
	case "1panel":
		src = convert.OnePanelSource(*commit)
		apps, err = convert.OnePanel(*source)
	case "dokploy":
		src = convert.DokploySource(*commit)
		apps, err = convert.Dokploy(*source)
	case "coolify":
		src = convert.CoolifySource(*commit)
		apps, err = convert.Coolify(*source)
	default:
		return fmt.Errorf("the converter reads 1panel, dokploy and coolify, not %q", *from)
	}
	if err != nil {
		return err
	}
	if src.LicenseText, err = convert.ReadLicense(*source, src); err != nil {
		return err
	}
	all := *only == ""
	if !all {
		ids := strings.Split(*only, ",")
		apps = slices.DeleteFunc(apps, func(a convert.App) bool { return !slices.Contains(ids, a.ID) })
	}
	previous, dates, pins, err := convert.ReadPrevious(*out)
	if err != nil {
		return err
	}
	written, err := convert.ReadOverlays(*overlays)
	if err != nil {
		return err
	}
	// Dockwell's own entries are the binary's, and never a folder DOCKWELL_CATALOG_DIR
	// names, which may be this run's own output.
	own, err := catalog.BuiltIn("")
	if err != nil {
		return err
	}
	c := &convert.Converter{
		Source:        src,
		Registry:      &convert.OCI{Client: &http.Client{Timeout: 30 * time.Second}, HubGets: *hubGets},
		Overlays:      written,
		Previous:      previous,
		PreviousDates: dates,
		Pins:          pins,
		Own:           own.All(),
		Now:           time.Now(),
	}
	var results []convert.Result
	converted, kept := 0, 0
	for i := range apps {
		r := c.Convert(ctx, apps[i])
		switch {
		case r.Held != "":
			kept++
		case r.Entry != nil:
			converted++
		}
		results = append(results, r)
	}
	if err := convert.Write(*out, src, results, previous, c.Pins, all); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Converted %d of %d apps from %s into %s, and kept %d at an earlier release. %s says why the rest were left out.\n",
		converted, len(results), src.Name, *out, kept, convert.ReportFile)
	return nil
}
