// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// tidyShown keeps a service's shown_env to names its app's environment has, sorted
// and once each, so specs that mean the same compare equal, and a value taken out of
// the environment doesn't come back shown should one of its name return (ADR 0081).
func tidyShown(svc *resourcev1.Service) {
	if len(svc.GetShownEnv()) == 0 {
		svc.ShownEnv = nil
		return
	}
	env := svc.GetApp().GetEnv()
	shown := slices.DeleteFunc(slices.Clone(svc.GetShownEnv()), func(name string) bool {
		_, ok := env[name]
		return !ok
	})
	slices.Sort(shown)
	shown = slices.Compact(shown)
	if len(shown) == 0 {
		shown = nil
	}
	svc.ShownEnv = shown
}

// newlyShown is the first of names that desired shows and current doesn't, where
// current has a value for it: what keeping it would show that nobody saw.
func newlyShown(current, desired *resourcev1.Service, names []string) (string, bool) {
	for _, name := range names {
		if _, had := current.GetApp().GetEnv()[name]; had && slices.Contains(desired.GetShownEnv(), name) && !slices.Contains(current.GetShownEnv(), name) {
			return name, true
		}
	}
	return "", false
}

// Shown is each of the app's own values its shown_env names, by the path a plan's
// change names it by, for a plan to show (resource.Shower).
func (*Driver) Shown(spec proto.Message) map[string]string {
	svc, _ := spec.(*resourcev1.Service)
	env := svc.GetApp().GetEnv()
	var out map[string]string
	for _, name := range svc.GetShownEnv() {
		if v, ok := env[name]; ok {
			if out == nil {
				out = make(map[string]string, len(svc.GetShownEnv()))
			}
			out["app.env["+name+"]"] = v
		}
	}
	return out
}

// keptShown is observed's shown_env from current's, for drift kept as the latest
// revision: a name stays shown only while the server has the value current showed.
// What someone set by hand on the server could be anything, so it is hidden.
func keptShown(current, observed *resourcev1.Service) []string {
	var out []string
	for _, name := range current.GetShownEnv() {
		was, ok := current.GetApp().GetEnv()[name]
		if now, there := observed.GetApp().GetEnv()[name]; ok && there && now == was {
			out = append(out, name)
		}
	}
	return out
}

// showsKept refuses an edit that keeps a value it newly shows.
func showsKept(current, desired *resourcev1.Service, names []string) error {
	if name, ok := newlyShown(current, desired, names); ok {
		return fmt.Errorf("the edit shows %s, and keeps its value without reading it, so it would show a value nobody saw. Give %s's value in the edit to show it, or leave it out of shown_env", name, name)
	}
	return nil
}
