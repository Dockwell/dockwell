// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"go.yaml.in/yaml/v4"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog/folder"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/registry"
)

// The same app in several catalogs (ADR 0078): WordPress from Dockwell's own, from
// 1Panel's app store and from Coolify's templates is one app, shown once, with the
// catalog to install it from chosen at install. Matching is conservative, as ADR
// 0029 asks: a wrong merge installs the wrong software, and a missed one costs a
// card.

// claims are the IDs the catalogs before a source have, by ID: whose each is, and
// its entry.
type claims = map[string]folder.Taken

// sameApp says two entries of one ID are the same app: the same name, in any case,
// and the same image for the service a person opens, by its repository. Anything
// less is another app, which a source leaves out, as one would take over from the
// other.
func sameApp(a, b *catalogv1.Entry) bool {
	if a == nil || b == nil || a.GetId() != b.GetId() || !strings.EqualFold(strings.TrimSpace(a.GetName()), strings.TrimSpace(b.GetName())) {
		return false
	}
	image := webImage(a)
	return image != "" && image == webImage(b)
}

// appKey is what sameApp compares of an entry, so a read of a source that depends on
// it is read again when it changes.
func appKey(e *catalogv1.Entry) string {
	if e == nil {
		return ""
	}
	return strings.ToLower(e.GetName()) + "\x00" + webImage(e)
}

// webImage is the repository of the image an entry's web service runs, as Docker
// names it, such as "wordpress" or "ghcr.io/acme/web", or of its one service when it
// names none. Empty when it can't tell.
func webImage(e *catalogv1.Entry) string {
	var file struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal([]byte(e.GetCompose()), &file) != nil {
		return ""
	}
	service := e.GetWeb().GetService()
	if service == "" && len(file.Services) == 1 {
		for name := range file.Services {
			service = name
		}
	}
	s, ok := file.Services[service]
	if !ok {
		return ""
	}
	ref, err := registry.Parse(strings.TrimSpace(s.Image))
	if err != nil {
		return ""
	}
	return ref.Name()
}

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
