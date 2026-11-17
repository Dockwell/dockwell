// SPDX-License-Identifier: AGPL-3.0-only

package composeapi

import (
	"maps"
	"slices"

	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
)

// Address is what a catalog app's address writes into its services' environments
// (ADR 0103): the value each variable that follows it takes, by service and
// variable, and on an upgrade, the value each took at the address Dockwell last
// wrote, by which a variable that holds neither that nor what the old file gives
// it holds a value a person set, and stays.
type Address struct {
	Values  map[string]map[string]string
	Written map[string]map[string]string
	// Past are the values each variable took at each domain a route ever sent to web,
	// which are Dockwell's too (#779's check, N1).
	Past []map[string]map[string]string
}

// setAddress writes what the address gives each variable into the file's services.
func setAddress(stack *compose.Stack, values map[string]map[string]string) {
	for _, s := range stack.Services {
		app := s.Spec.GetApp()
		for name, value := range values[s.Name] {
			if app == nil {
				continue
			}
			if app.Env == nil {
				app.Env = map[string]string{}
			}
			app.Env[name] = value
		}
	}
}

// followAddress sets each variable that follows the address in the release, and in
// the old file a variable that holds what Dockwell last wrote, so the merge reads it
// as unchanged by a person and takes the release's value. One that holds anything
// else is a person's, which the merge keeps, and the plan says so.
func followAddress(env string, old *compose.Stack, now runs, next *compose.Stack, a Address) []revision.Note {
	setAddress(next, a.Values)
	var notes []revision.Note
	for _, name := range slices.Sorted(maps.Keys(a.Values)) {
		have := now.services[name]
		item := find(old.Services, name)
		if have == nil || item == nil || item.Spec.GetApp() == nil {
			continue
		}
		was := item.Spec
		for _, variable := range slices.Sorted(maps.Keys(a.Values[name])) {
			current, ok := have.GetApp().GetEnv()[variable]
			if !ok {
				continue
			}
			// What the merge reads as the old file's value is Dockwell's own: what it
			// wrote, where the pin records the address, and the file's value only where
			// it doesn't, so a value a person set to the file's stays once there's a
			// record (#779's review, L2).
			written, wrote := a.Written[name][variable]
			fileHas := was.GetApp().GetEnv()[variable]
			base := fileHas
			past := slices.ContainsFunc(a.Past, func(p map[string]map[string]string) bool {
				v, ok := p[name][variable]
				return ok && current == v
			})
			switch {
			case catalog.Unrendered(current), wrote && current == written, past:
				base = current
			case wrote:
				base = written
			}
			if base != fileHas {
				if was.GetApp().Env == nil {
					was.GetApp().Env = map[string]string{}
				}
				was.GetApp().Env[variable] = base
			}
			if current != base && current != a.Values[name][variable] {
				notes = append(notes, revision.Note{
					Reason:  "address_kept",
					Message: variable + " holds a value you set, so it stays rather than follow the app's address",
					Ref:     resource.Ref{Kind: service.Kind, Scope: env, Name: name},
				})
			}
		}
	}
	return notes
}
