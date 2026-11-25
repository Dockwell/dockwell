// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type toolboxServiceDropins struct {
	paths    []string
	digest   []byte
	typeWide bool
}

// An unloaded unit can have empty DropInPaths even when the unit search path
// contains overrides. Read the lookup paths in the manager's precedence order.
func (t *hostToolbox) planServiceDropins(ctx context.Context, unit string, typeWide bool) (toolboxServiceDropins, error) {
	dropins := toolboxServiceDropins{typeWide: typeWide}
	raw, err := t.run(ctx, toolboxSystem, "show", "--property=UnitPath", "--value")
	if err != nil || len(strings.Fields(string(raw))) == 0 {
		raw, err = t.run(ctx, toolboxTool("systemd-analyze"), "unit-paths")
		if err != nil {
			return dropins, err
		}
	}
	lookup := strings.Fields(string(raw))
	if len(lookup) == 0 {
		return dropins, errors.New("systemd didn't report its unit search paths")
	}
	subdirs := []string{unit + ".d"}
	stem := strings.TrimSuffix(unit, ".service")
	for at := strings.LastIndexByte(stem, '-'); at >= 0; at = strings.LastIndexByte(stem[:at], '-') {
		subdirs = append(subdirs, stem[:at+1]+".service.d")
	}
	if typeWide {
		subdirs = append(subdirs, "service.d")
	}
	digest := sha256.New()
	seen := map[string]bool{}
	for _, dir := range lookup {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return dropins, errors.New("systemd reported an invalid unit search path")
		}
		for _, subdir := range subdirs {
			resolvedDir, err := filepath.EvalSymlinks(t.path(filepath.Join(dir, subdir)))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return dropins, err
			}
			folder, err := t.serviceDropinPath(resolvedDir)
			if err != nil {
				return dropins, err
			}
			entries, err := os.ReadDir(resolvedDir)
			if err != nil {
				return dropins, err
			}
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".conf") {
					continue
				}
				path := filepath.Join(folder, name)
				if subdir != "service.d" {
					return dropins, fmt.Errorf("%s changes this service. Remove the drop-in or choose another name", path)
				}
				if seen[name] {
					continue
				}
				seen[name] = true
				// A mask shadows the same basename in lower-priority directories.
				resolved, err := filepath.EvalSymlinks(t.path(path))
				dropins.paths = append(dropins.paths, path)
				if resolved == "/dev/null" {
					fmt.Fprintf(digest, "%q masked\n", path)
					continue
				}
				if err != nil {
					return dropins, err
				}
				file, err := t.serviceDropinPath(resolved)
				if err != nil {
					return dropins, err
				}
				contents, err := t.read(file)
				if err != nil {
					return dropins, err
				}
				fmt.Fprintf(digest, "%q %x\n", path, sha256.Sum256(contents))
			}
		}
	}
	// systemd reports these in basename order, including /dev/null masks.
	slices.SortFunc(dropins.paths, func(a, b string) int {
		return strings.Compare(filepath.Base(a), filepath.Base(b))
	})
	dropins.digest = digest.Sum(nil)
	return dropins, nil
}

func (t *hostToolbox) serviceDropinPath(resolved string) (string, error) {
	rel, err := filepath.Rel(t.root(), resolved)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%s resolves outside the host", resolved)
	}
	return "/" + rel, nil
}

func (t *hostToolbox) checkLoadedServiceDropins(ctx context.Context, unit string, reviewed toolboxServiceDropins) error {
	raw, err := t.serviceDropins(ctx, unit)
	if err != nil {
		return err
	}
	current, err := t.planServiceDropins(ctx, unit, reviewed.typeWide)
	if err != nil {
		return err
	}
	if raw != strings.Join(reviewed.paths, " ") || !bytes.Equal(current.digest, reviewed.digest) {
		return errors.New("systemd's drop-ins changed or differ from the reviewed service. Plan the operation again")
	}
	return nil
}
