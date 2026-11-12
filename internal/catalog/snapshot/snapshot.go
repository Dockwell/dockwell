// SPDX-License-Identifier: AGPL-3.0-only

// Package snapshot is Dockwell/catalog's entries, with the images and notices
// beside them, at one commit, built into the binary. It's the commit Dockwell's
// own catalog source starts at, which the control plane reads from here, so a
// first boot needs no network, and an install that can't reach GitHub still has a
// catalog (docs/adr/0077-dockwells-catalog-in-its-own-repository.md). make
// catalog-snapshot writes it from the commit in the file commit, and CI fails if
// it isn't what that commit holds: the repository is where entries are written.
package snapshot

import (
	"embed"
	"io/fs"
	"strings"
)

// Dockwell's own catalog, as a source names it.
const (
	// Repository is Dockwell/catalog's URL.
	Repository = "https://github.com/Dockwell/catalog"
	// Path is the folder of entries in it.
	Path = "entries"
	// Branch is the branch every install follows.
	Branch = "main"
)

//go:embed commit
var commit string

//go:embed all:entries
var files embed.FS

// Commit is the commit of Dockwell/catalog this build holds, in full.
func Commit() string { return strings.TrimSpace(commit) }

// Entries is the folder of entries at Commit: each <id>.yaml, and the folder media
// beside them.
func Entries() fs.FS {
	sub, err := fs.Sub(files, Path)
	if err != nil {
		panic(err) // the folder is embedded, by the directive above
	}
	return sub
}
