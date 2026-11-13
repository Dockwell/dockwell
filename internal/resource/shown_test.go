// SPDX-License-Identifier: AGPL-3.0-only

package resource_test

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// A plan says a value only where every side of its change that has one shows it, so
// a change that shows a hidden value, or hides a shown one, never says it (ADR 0081).
func TestShowChanges(t *testing.T) {
	from := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Env: map[string]string{
		"HOST": "db", "NAME": "wp", "GONE": "old", "HIDDEN": "hunter2", "SHOWS": "x",
	}}}}
	to := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Env: map[string]string{
		"HOST": "db2", "NAME": "wp", "NEW": "fresh", "HIDDEN": "hunter3", "SHOWS": "y", "SECRET": "s",
	}}}}
	changes := resource.Diff(from, to)
	resource.ShowChanges(changes,
		map[string]string{"app.env[HOST]": "db", "app.env[NAME]": "wp", "app.env[GONE]": "old"},
		map[string]string{"app.env[HOST]": "db2", "app.env[NAME]": "wp", "app.env[NEW]": "fresh", "app.env[SHOWS]": "y"},
	)
	want := map[string]resource.Change{
		"app.env[HOST]":   {Path: "app.env[HOST]", Op: resource.OpModified, From: "db", To: "db2"},
		"app.env[GONE]":   {Path: "app.env[GONE]", Op: resource.OpRemoved, From: "old"},
		"app.env[NEW]":    {Path: "app.env[NEW]", Op: resource.OpAdded, To: "fresh"},
		"app.env[HIDDEN]": {Path: "app.env[HIDDEN]", Op: resource.OpModified, Sensitive: true},
		"app.env[SHOWS]":  {Path: "app.env[SHOWS]", Op: resource.OpModified, Sensitive: true},
		"app.env[SECRET]": {Path: "app.env[SECRET]", Op: resource.OpAdded, Sensitive: true},
	}
	if len(changes) != len(want) {
		t.Fatalf("changes = %+v, want %d of them", changes, len(want))
	}
	for _, c := range changes {
		if w, ok := want[c.Path]; !ok || c != w {
			t.Errorf("change %+v, want %+v", c, w)
		}
	}
}
