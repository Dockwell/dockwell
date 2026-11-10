// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

// MaxFile is the most the converter reads of one file. A Compose file, a form or an
// overlay is a few KiB, and a link to /dev/zero would otherwise read forever.
const MaxFile = 1 << 20

// maxEntries bounds what the converter lists of one app's folder.
const maxEntries = 2000

// tree reads a checkout through an os.Root, so nothing it reads is outside it, and it
// reads only regular files and folders: a link, which a checkout can hold, is never
// followed, even to somewhere inside it.
type tree struct{ root *os.Root }

func openTree(dir string) (*tree, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &tree{root: r}, nil
}

func (t *tree) Close() error { return t.root.Close() }

// kind says what a path is: a regular file, a folder, or neither, which is as good as
// absent, with why.
func (t *tree) kind(name string) (fs.FileMode, error) {
	fi, err := t.root.Lstat(name)
	if err != nil {
		return 0, err
	}
	switch m := fi.Mode(); {
	case m.IsRegular(), m.IsDir():
		return m.Type(), nil
	case m&fs.ModeSymlink != 0:
		return 0, fmt.Errorf("%s is a link, which the converter doesn't follow", name)
	default:
		return 0, fmt.Errorf("%s is neither a file nor a folder", name)
	}
}

// isDir says a path is a folder, and not a link to one.
func (t *tree) isDir(name string) bool {
	m, err := t.kind(name)
	return err == nil && m.IsDir()
}

// isFile says a path is a regular file, and not a link to one.
func (t *tree) isFile(name string) bool {
	m, err := t.kind(name)
	return err == nil && m.IsRegular()
}

// readFile reads a regular file, up to MaxFile.
func (t *tree) readFile(name string) ([]byte, error) {
	m, err := t.kind(name)
	if err != nil {
		return nil, err
	}
	if !m.IsRegular() {
		return nil, fmt.Errorf("%s is a folder", name)
	}
	f, err := t.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readCapped(f, name)
}

func readCapped(r io.Reader, name string) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(r, MaxFile+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if len(out) > MaxFile {
		return nil, fmt.Errorf("%s is over %d KiB, more than any file the converter reads", name, MaxFile>>10)
	}
	return out, nil
}

// readDir lists a folder's regular files and folders, sorted, leaving out links.
func (t *tree) readDir(name string) ([]fs.DirEntry, error) {
	if !t.isDir(name) {
		return nil, fmt.Errorf("%s isn't a folder", name)
	}
	f, err := t.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ents, err := f.ReadDir(maxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(ents) > maxEntries {
		return nil, fmt.Errorf("%s holds over %d files, more than the converter reads", name, maxEntries)
	}
	ents = slices.DeleteFunc(ents, func(e fs.DirEntry) bool { return !e.Type().IsRegular() && !e.IsDir() })
	slices.SortFunc(ents, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return ents, nil
}

// ReadLicense reads a source's licence's text from its checkout, as a file there.
func ReadLicense(root string, src Source) ([]byte, error) {
	if src.LicenseFile == "" {
		return nil, nil
	}
	t, err := openTree(root)
	if err != nil {
		return nil, err
	}
	defer t.Close()
	text, err := t.readFile(src.LicenseFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s's licence: %w. What's converted from it goes with its licence's text", src.Name, err)
	}
	return text, nil
}

// placeholder says a file is only there to keep its folder in Git.
func placeholder(name string) bool {
	return name == ".gitkeep" || name == ".keep"
}

// shipped lists what a folder ships, relative to it: each regular file, and each
// folder that holds one, but not a placeholder such as data/.gitkeep, which keeps a
// folder the app fills as it runs.
func (t *tree) shipped(dir string) (map[string]bool, error) {
	out := map[string]bool{}
	count := 0
	var walk func(rel string) (bool, error)
	walk = func(rel string) (bool, error) {
		ents, err := t.readDir(path.Join(dir, rel))
		if err != nil {
			return false, err
		}
		any := false
		for _, e := range ents {
			count++
			if count > maxEntries {
				return false, fmt.Errorf("%s holds over %d files, more than the converter reads", dir, maxEntries)
			}
			child := path.Join(rel, e.Name())
			if e.IsDir() {
				if holds, err := walk(child); err != nil {
					return false, err
				} else if holds {
					out[child], any = true, true
				}
				continue
			}
			if !placeholder(e.Name()) {
				out[child], any = true, true
			}
		}
		return any, nil
	}
	if _, err := walk("."); err != nil {
		return nil, err
	}
	return out, nil
}
