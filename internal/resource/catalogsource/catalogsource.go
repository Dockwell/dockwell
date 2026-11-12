// SPDX-License-Identifier: AGPL-3.0-only

// Package catalogsource is the "catalog.source" kind: a catalog Dockwell reads apps
// from, beside its own, from a public Git repository at one commit. It lives in the
// organization, and the control plane keeps it, as it applies on no server. See
// docs/adr/0055-catalogs-from-a-git-url.md.
package catalogsource

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/gitfetch"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the catalog source kind.
const Kind resource.Kind = "catalog.source"

// Type is the kind.
type Type struct{}

var _ resource.Type = Type{}

// Kind returns "catalog.source".
func (Type) Kind() resource.Kind { return Kind }

// Level is the organization: every project installs from the same catalog.
func (Type) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "cts".
func (Type) IDPrefix() string { return "cts" }

// New returns an empty CatalogSource.
func (Type) New() proto.Message { return &resourcev1.CatalogSource{} }

// CheckName takes what fits in a URL, as its entries show it beside them.
func (Type) CheckName(name string) error {
	return resource.CheckLabel("catalog source", name, "community")
}

// Validate refuses a source that couldn't be read as one commit, and writes its
// URL and folder as a fetch reads them.
func (Type) Validate(spec proto.Message) error {
	s := spec.(*resourcev1.CatalogSource)
	u, err := gitfetch.CheckURL(s.GetRepository())
	if err != nil {
		return err
	}
	s.Repository = u.String()
	if !gitfetch.Commit.MatchString(s.GetCommit()) {
		return fmt.Errorf("a catalog source is read at a commit, named in full as 40 hex digits, and %q isn't one. A branch moves, so name the commit it's at", s.GetCommit())
	}
	if slices.Contains(strings.Split(s.GetPath(), "/"), "..") {
		return errors.New("a catalog source's folder is in its repository. Name it from the repository's top, such as entries")
	}
	s.Path = strings.Trim(path.Clean("/"+s.GetPath()), "/")
	if _, ok := resourcev1.CatalogFormat_name[int32(s.GetFormat())]; !ok {
		return fmt.Errorf("a catalog source's format is one Dockwell reads, and %d isn't one. Leave it out for Dockwell to tell", s.GetFormat())
	}
	return nil
}
