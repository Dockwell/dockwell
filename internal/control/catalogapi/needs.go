// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"fmt"
	"maps"
	"slices"

	"connectrpc.com/connect"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/authapi"
)

// An entry's needs are met as it's installed, and as its pin says when it's upgraded
// (ADR 0084). For now a need is met by a new one beside the app.

// lookup finds a catalog entry by its ID, as listed, for a need's new one from the
// catalog, with the source it's in.
func (s sourced) lookup() catalog.Lookup {
	return func(id string) (*catalogv1.Entry, string, bool) {
		e, from, err := s.entryIn(id, "")
		if err != nil {
			return nil, "", false
		}
		return e, from.catalog, true
	}
}

// needOptions are the ways to meet each of an entry's needs, the default first for
// each, for GetCatalogEntry.
func needOptions(e *catalogv1.Entry, ways map[string][]catalog.Way) []*dockwellv1.CatalogNeedOption {
	var out []*dockwellv1.CatalogNeedOption
	for _, n := range e.GetNeeds() {
		for _, w := range ways[n.GetName()] {
			o := &dockwellv1.CatalogNeedOption{Need: n.GetName(), Choice: w.Choice(), Engine: w.Engine, Version: w.Version}
			if w.From != nil {
				o.Entry, o.EntryName = w.From.GetId(), w.From.GetName()
			}
			out = append(out, o)
		}
	}
	return out
}

// meetNeeds is an entry as an install that meets its needs runs it: as asked, by
// need, or as the install's pin says, or else by each need's first way. It refuses a
// value given for a name a need sets, as Dockwell sets each from what meets it.
func (s sourced) meetNeeds(e *catalogv1.Entry, values, asked map[string]string, pinned map[string]*resourcev1.PinnedNeed) (*catalog.Needed, error) {
	names := catalog.NeedNames(e)
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if names[name] {
			return nil, authapi.ReasonError(connect.CodeInvalidArgument,
				fmt.Errorf("%s is set by what meets %s's needs, so it takes no value of its own. Leave it out", name, e.GetName()), "invalid")
		}
	}
	met, err := catalog.Meet(e, catalog.Ways(e, s.lookup()), asked, pinned)
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	needed, err := catalog.Need(e, met)
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	return needed, nil
}
