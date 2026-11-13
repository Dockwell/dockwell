// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"errors"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/variable"
)

// tidyShown keeps a service's shown_env to names its app's environment has, sorted
// and once each, so specs that mean the same compare equal, and a value taken out of
// the environment doesn't come back shown should one of its name return. A value
// shaped like a secret is never shown, whoever names it: a client that sends
// shown_env back as it read it, with a new value such as redis://:pw@redis, would
// otherwise show that (ADR 0081).
func tidyShown(svc *resourcev1.Service) {
	if len(svc.GetShownEnv()) == 0 {
		svc.ShownEnv = nil
		return
	}
	env := svc.GetApp().GetEnv()
	shown := slices.DeleteFunc(slices.Clone(svc.GetShownEnv()), func(name string) bool {
		v, ok := env[name]
		return !ok || SecretShape(v)
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

// MoveValues takes each of the app's own environment variables names names out of
// desired, which uses a secret of the same name in its place, and says what each
// held in current, for the secret to be made with (resource.ValueMover). A value
// the edit sets or keeps too is refused, as is one the app hasn't got or that's
// empty, which no secret can be.
func (*Driver) MoveValues(current, desired proto.Message, names []string) (resource.Kind, map[string]string, error) {
	cur, _ := current.(*resourcev1.Service)
	want, _ := desired.(*resourcev1.Service)
	if cur == nil || want.GetApp() == nil {
		return "", nil, errors.New("it moves values into secrets, but runs no image of its own to take them from. Leave env_to_secrets out")
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		v, ok := cur.GetApp().GetEnv()[name]
		_, set := want.GetApp().GetEnv()[name]
		switch {
		case out[name] != "":
			return "", nil, fmt.Errorf("env_to_secrets names %s twice. Name it once", name)
		case !ok:
			return "", nil, fmt.Errorf("it has no environment variable %s of its own to make a secret. Read it again, and leave %s out", name, name)
		case set:
			return "", nil, fmt.Errorf("the edit both sets %s and makes it a secret. Do one", name)
		case v == "":
			return "", nil, fmt.Errorf("%s is empty, and a secret can't be. Set a value instead", name)
		}
		out[name] = v
		binding.Use(want, resource.Use{As: name, Kind: variable.SecretKind, Name: name})
		want.ShownEnv = slices.DeleteFunc(want.ShownEnv, func(n string) bool { return n == name })
	}
	return variable.SecretKind, out, nil
}
