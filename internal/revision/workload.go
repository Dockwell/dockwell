// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

func (e *Engine) workloadNotes(ctx context.Context, items []item, notes []Note) ([]Note, error) {
	stopped := resourcev1.DesiredRunState_DESIRED_RUN_STATE_STOPPED
	for i := range items {
		it := &items[i]
		before, after := resource.RunIntent(it.current), resource.RunIntent(it.spec)
		if (before == stopped) == (after == stopped) || it.spec == nil || it.current == nil || resource.Workload(it.current) == nil {
			continue
		}
		var routes, dependants []string
		if err := e.db.View(ctx, func(tx *store.Tx) error {
			list, err := tx.ListResources(ctx, it.ref.Scope, "route")
			if err != nil {
				return err
			}
			for _, other := range list {
				spec, err := e.decode(ctx, tx, store.InRevision(other.ID, other.Kind, other.Revision), other.Spec)
				if err != nil {
					return err
				}
				if route, ok := spec.(*resourcev1.Route); ok && route.GetService() == it.ref.Name {
					routes = append(routes, other.Name)
				}
			}
			links, err := e.allLinks(ctx, tx, items)
			if err != nil {
				return err
			}
			for _, link := range links {
				if link.meets() == it.ref.Scope && link.GetService() == it.ref.Name {
					dependants = append(dependants, link.ref.Name)
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
		reason := "workload_starts"
		message := "Starts this workload on all its placed servers and resumes its health checks. Existing routes can serve it again, and dependent workloads keep running."
		if after == stopped {
			reason = "workload_stops"
			message = "Stops this workload on all its placed servers and pauses its container health checks. Its routes have no running backend. Dependent workloads stay up and may lose access to it."
		}
		notes = append(notes, Note{Reason: reason, Ref: it.ref, Message: message})
		if len(routes) > 0 {
			notes = append(notes, Note{Reason: "workload_routes", Ref: it.ref, Names: routes, Message: fmt.Sprintf("Routes: %s.", human.List(routes))})
		}
		if len(dependants) > 0 {
			notes = append(notes, Note{Reason: "workload_dependants", Ref: it.ref, Names: dependants, Message: fmt.Sprintf("Dependency links: %s.", human.List(dependants))})
		}
	}
	return notes, nil
}

// stoppedRestoreDatabase finds a database saved as stopped on the volume a
// whole-database dump restores. Restoring a single database on a shared server
// requires that server to be running already.
func (e *Engine) stoppedRestoreDatabase(ctx context.Context, server string, step *resource.Step) (string, error) {
	r := step.Restore
	if r.Method != resource.BackupDump || r.OneDatabase != "" || r.IntoNew {
		return "", nil
	}
	var name string
	err := e.db.View(ctx, func(tx *store.Tx) error {
		placed, err := tx.ListPlaced(ctx, server)
		if err != nil {
			return err
		}
		for _, candidate := range placed {
			if candidate.Scope != step.Ref.Scope || candidate.Kind != string(serviceKind) && candidate.Kind != "container" {
				continue
			}
			spec, err := e.decode(ctx, tx, store.InRevision(candidate.ID, candidate.Kind, candidate.Revision), candidate.Spec)
			if err != nil {
				return err
			}
			if resource.RunIntent(spec) != resourcev1.DesiredRunState_DESIRED_RUN_STATE_STOPPED {
				continue
			}
			app := &resourcev1.Service{Runs: &resourcev1.Service_App{App: resource.Workload(spec)}}
			if db, ok := database.FromSpec(app); ok && db.Engine.Name == r.Database && !db.External && db.Volume == step.Ref.Name {
				name = candidate.Name
				return nil
			}
		}
		return nil
	})
	return name, err
}

// SpecWithRunIntent reads the current spec and changes only its run intent,
// without moving its values through the API. The edit still carries its base
// revision, which the engine rechecks while planning and applying.
func (e *Engine) SpecWithRunIntent(ctx context.Context, tx *store.Tx, ref resource.Ref, base int, intent resourcev1.DesiredRunState) (proto.Message, error) {
	it, err := e.load(ctx, tx, ref)
	if err != nil {
		return nil, err
	}
	if it.base != base || resource.Workload(it.current) == nil {
		return nil, fault.New(ErrStale, "%s changed. Refresh the workload and plan again", ref.Name)
	}
	out := proto.Clone(it.current)
	resource.Workload(out).DesiredRunState = intent
	return out, nil
}
