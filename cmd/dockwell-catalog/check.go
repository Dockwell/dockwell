// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/dockwell/dockwell/internal/catalog/folder"
	"github.com/dockwell/dockwell/internal/gitfetch"
)

// check checks a folder of entries as the control plane reads a catalog source's
// commit: within the same limits, each entry through the same checks, and each image
// by its first bytes. It names each file it would leave out and why, and fails if
// there's any, so a catalog's CI can't merge what a server would refuse.
func check(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("name the folder of entries to check, such as dockwell-catalog check entries")
	}
	dir := args[0]
	entries, err := readFolder(dir, folder.Limits, nil)
	if err != nil {
		return err
	}
	images, err := readFolder(filepath.Join(dir, folder.Media), folder.MediaLimits, folder.ImageName)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var refused []folder.Refusal
	passed := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(images)) {
		if _, err := folder.Image(name, images[name]); err != nil {
			refused = append(refused, folder.Refusal{File: folder.Media + "/" + name, Reason: err.Error()})
			continue
		}
		passed[name] = true
	}
	got := folder.Check(ctx, "this catalog", entries, passed, map[string]folder.Taken{}, folder.Options{})
	refused = append(refused, got.Refused...)
	for _, r := range refused {
		fmt.Fprintf(stdout, "%s: %s\n", r.File, r.Reason)
	}
	if len(refused) > 0 {
		return fmt.Errorf("a server would leave out or lose %d of what %s holds, as listed. Fix each, and check again", len(refused), dir)
	}
	fmt.Fprintf(stdout, "Checked %s and %s in %s, as a server reads them: each passes.\n", count(len(got.Entries), "entry", "entries"), count(len(passed), "image", "images"), dir)
	return nil
}

// count says how many of something there are, as "1 entry" or "2 entries".
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// readFolder reads the regular files in dir that want takes, or all of them for a nil
// want, by name, within lim, as gitfetch reads a commit's folder.
func readFolder(dir string, lim gitfetch.Limits, want func(string) bool) (map[string][]byte, error) {
	list, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	var total int64
	for _, e := range list {
		if !e.Type().IsRegular() || want != nil && !want(e.Name()) {
			continue
		}
		if len(out) == lim.Files {
			return nil, fmt.Errorf("the folder %s holds over %d files, more than a server reads", dir, lim.Files)
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // the folder the person running it names
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > lim.File {
			return nil, fmt.Errorf("%s is over %d KiB, more than a server reads of one file", filepath.Join(dir, e.Name()), lim.File>>10)
		}
		if total += int64(len(data)); lim.Total > 0 && total > lim.Total {
			return nil, fmt.Errorf("the folder %s holds over %d MiB of files, more than a server reads", dir, lim.Total>>20)
		}
		out[e.Name()] = data
	}
	return out, nil
}
