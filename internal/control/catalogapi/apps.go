// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog/folder"
	"github.com/dockwell/dockwell/internal/control/authapi"
)

// claims are the IDs the catalogs before a source have, by ID: whose each is, and
// its entry. folder.SameApp says which of a source's are the same app (ADR 0078).
type claims = map[string]folder.Taken

// catalogsOf are the catalogs that have an app, the one it's listed from first, by
// their sources' names, empty for Dockwell's own.
func (s sourced) catalogsOf(id string) []string {
	if _, ok := s.catalog.Entry(id); !ok {
		return nil
	}
	out := []string{s.origins[id].catalog}
	for _, a := range s.also[id] {
		out = append(out, a.from.catalog)
	}
	return out
}

// entryIn is an app's entry in a catalog, by its source's name, or in the one it's
// listed from when that's empty, and where it comes from.
func (s sourced) entryIn(id, catalogName string) (*catalogv1.Entry, origin, error) {
	e, ok := s.catalog.Entry(id)
	if !ok {
		return nil, origin{}, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("the catalog has no app %s. Run dockwell catalog list to see what it has", id), "not_found")
	}
	if catalogName == "" || catalogName == s.origins[id].catalog {
		return e, s.origins[id], nil
	}
	for _, a := range s.also[id] {
		if a.from.catalog == catalogName {
			return a.entry, a.from, nil
		}
	}
	return nil, origin{}, authapi.ReasonError(connect.CodeNotFound,
		fmt.Errorf("the catalog %s has no %s. It comes from %s", catalogName, id, strings.Join(catalogWords(s.catalogsOf(id)), " and ")), "not_found")
}

// catalogWords say catalogs by their sources' names, Dockwell's own as such.
func catalogWords(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = origin{catalog: n}.words()
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
