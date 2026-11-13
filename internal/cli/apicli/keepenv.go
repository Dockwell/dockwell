// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// keepOwnEnv has an update of a service keep every value of its own environment
// that the update doesn't set and remove names: one that reads back hidden by name,
// as no response carries it (keep_env), and one it shows by its value, still shown.
// It reads the service where the update places it, and plans from the revision it
// read, so a change that landed meanwhile makes the plan stale rather than undone.
// Before it, an update emptied every value it didn't set, hidden ones included,
// which nobody could set back without knowing them (ADR 0081).
func (r *runner) keepOwnEnv(ctx context.Context, edit *dockwellv1.Edit, remove []string) error {
	app := edit.GetSpec().GetService().GetApp()
	if app == nil {
		return nil // nothing runs, which the plan refuses in its own words
	}
	if err := r.connect(ctx); err != nil {
		return err
	}
	name := edit.GetResource().GetName()
	var found []*dockwellv1.ServiceInfo
	token := ""
	for {
		res, err := r.client.Services().ListServices(ctx, connect.NewRequest(&dockwellv1.ListServicesRequest{
			Project: edit.GetProject(), Environment: edit.GetResource().GetScope(), PageSize: 1000, PageToken: token,
		}))
		if err != nil {
			return r.explain(err)
		}
		for _, s := range res.Msg.GetServices() {
			if s.GetResource().GetName() == name {
				found = append(found, s)
			}
		}
		if token = res.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	switch {
	case len(found) == 0:
		return nil // there's none to keep from, which the plan says
	case len(found) > 1:
		return usage(fmt.Errorf("there's a service %s in %d environments, so it's not clear whose values to keep. Name its environment with --environment", name, len(found)))
	}
	current := found[0]
	for _, n := range remove {
		if _, ok := current.GetSpec().GetApp().GetEnv()[n]; !ok {
			return usage(fmt.Errorf("%s has no environment variable %s of its own to remove. Its own are %v", name, n, slices.Sorted(maps.Keys(current.GetSpec().GetApp().GetEnv()))))
		}
	}
	if app.Env == nil {
		app.Env = map[string]string{}
	}
	shown := current.GetSpec().GetShownEnv()
	spec := edit.GetSpec().GetService()
	for _, n := range slices.Sorted(maps.Keys(current.GetSpec().GetApp().GetEnv())) {
		if _, set := app.GetEnv()[n]; set || slices.Contains(remove, n) {
			continue
		}
		if slices.Contains(shown, n) {
			app.Env[n] = current.GetSpec().GetApp().GetEnv()[n]
			if !slices.Contains(spec.ShownEnv, n) {
				spec.ShownEnv = append(spec.ShownEnv, n)
			}
			continue
		}
		edit.KeepEnv = append(edit.KeepEnv, n)
	}
	edit.BaseRevision = current.GetRevision()
	return nil
}
