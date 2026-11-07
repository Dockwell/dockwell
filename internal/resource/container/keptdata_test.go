// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"context"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// planAndApply plans one resource, in ctx, and applies what the plan says.
func planApplyIn(ctx context.Context, t *testing.T, l resource.Local, ref resource.Ref, spec *resourcev1.Service) resource.Step {
	t.Helper()
	d := resource.Desired{Ref: ref}
	if spec != nil {
		d.Spec = spec
	}
	steps, err := l.Plan(ctx, []resource.Desired{d})
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if steps[0].Action != resource.ActionNone {
		var err error
		if spec != nil {
			err = l.Apply(ctx, steps[0], spec)
		} else {
			err = l.Apply(ctx, steps[0], nil)
		}
		if err != nil {
			t.Fatalf("applying %s: %v", steps[0].Action, err)
		}
	}
	return steps[0]
}

func named(t *testing.T, scope string) context.Context {
	return resource.WithScopeNames(t.Context(), map[string]string{env: scope})
}

// The data an image kept, which the server keeps for 7 days after a deletion,
// is the service's, whatever its containers are named: deleted before the
// upgrade, when they were named as Compose would, and undone after it, the plan
// restores it, and so does a step applied under another name than it was
// planned under (ADR 0035).
func TestKeptDataComesBackAcrossTheNameChange(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reg, _ := keptDrivers(e, &now)
	l := resource.Local{Drivers: reg}
	ref := resource.Ref{Kind: service.Kind, Scope: env, Name: "db"}

	planApplyIn(t.Context(), t, l, ref, postgres()) // before the upgrade: no names
	first := mountedAt(e.Container(service.ContainerName(ref)))
	if first == "" {
		t.Fatal("postgres has no volume of its image's")
	}
	planApplyIn(t.Context(), t, l, ref, nil)

	undo := planApplyIn(named(t, "default.production"), t, l, ref, postgres())
	c := e.Container("default.production.db-1")
	if !undo.Restores || c == nil || mountedAt(c) != first {
		t.Fatalf("undone after the upgrade: restores %v, and default.production.db-1 mounts %q, want %q", undo.Restores, mountedAt(c), first)
	}

	// Planned under one name and applied under another, as when the store
	// can't say what people call the environment for one of them.
	planApplyIn(named(t, "default.production"), t, l, ref, nil)
	steps, err := l.Plan(named(t, "default.production"), []resource.Desired{{Ref: ref, Spec: postgres()}})
	if err != nil {
		t.Fatal(err)
	}
	if !steps[0].Restores {
		t.Fatalf("the undo planned with names doesn't restore: %+v", steps[0])
	}
	if err := l.Apply(t.Context(), steps[0], postgres()); err != nil {
		t.Fatalf("applied with no names, the step that restores says: %v", err)
	}
	if got := mountedAt(e.Container(service.ContainerName(ref))); got != first {
		t.Errorf("applied with no names, db mounts %q, want %q", got, first)
	}
}
