// SPDX-License-Identifier: AGPL-3.0-only

package resource_test

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/container/containertest"
)

func TestDiffNamesEachFieldAsThePanelShowsIt(t *testing.T) {
	from := &resourcev1.Container{
		Image:   "nginx:1.27",
		Command: []string{"nginx", "-g", "daemon off;"},
		Ports:   []*resourcev1.PortMapping{{HostPort: 80, ContainerPort: 8080}},
		Restart: resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED,
		Labels:  map[string]string{"team": "web", "tier": "edge"},
		Env:     map[string]string{"TOKEN": "old"},
	}
	to := &resourcev1.Container{
		Image:   "nginx:1.28",
		Ports:   []*resourcev1.PortMapping{{HostPort: 80, ContainerPort: 8081}, {HostPort: 443, ContainerPort: 8443}},
		Restart: resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS,
		Labels:  map[string]string{"team": "platform", "zone": ""},
		Env:     map[string]string{"TOKEN": "new"},
	}
	want := []resource.Change{
		{Path: "image", Op: resource.OpModified, From: "nginx:1.27", To: "nginx:1.28"},
		{Path: "command", Op: resource.OpRemoved, From: "nginx, -g, daemon off;"},
		{Path: "env[TOKEN]", Op: resource.OpModified, Sensitive: true},
		{Path: "ports[0].container_port", Op: resource.OpModified, From: "8080", To: "8081"},
		{Path: "ports[1].host_port", Op: resource.OpAdded, To: "443"},
		{Path: "ports[1].container_port", Op: resource.OpAdded, To: "8443"},
		{Path: "restart", Op: resource.OpModified, From: "unless-stopped", To: "always"},
		{Path: "labels[team]", Op: resource.OpModified, From: "web", To: "platform"},
		{Path: "labels[tier]", Op: resource.OpRemoved, From: "edge"},
		// An entry with an empty value is still there.
		{Path: "labels[zone]", Op: resource.OpAdded, To: ""},
	}
	got := resource.Diff(from, to)
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d:\n%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDiffOfNothing(t *testing.T) {
	spec := &resourcev1.FirewallRule{Port: 22, Sources: []string{"10.0.0.0/8"}}
	if c := resource.Diff(spec, spec); len(c) != 0 {
		t.Errorf("a spec differs from itself: %+v", c)
	}
	if c := resource.Diff(nil, nil); c != nil {
		t.Errorf("nothing differs from nothing: %+v", c)
	}
	created := resource.Diff(nil, spec)
	if len(created) != 2 || created[0].Path != "port" || created[1].To != "10.0.0.0/8" {
		t.Errorf("creating: %+v", created)
	}
}

func TestDiffSeesThroughAmbiguousLists(t *testing.T) {
	a := &resourcev1.Container{Command: []string{"sh -c", "x"}}
	b := &resourcev1.Container{Command: []string{"sh -c, x"}}
	got := resource.Diff(a, b)
	if len(got) != 2 || got[0].Path != "command[0]" || got[1].Path != "command[1]" || got[1].Op != resource.OpRemoved {
		t.Errorf("changes: %+v", got)
	}
}

// A rotation counts every sensitive value the new spec sets as changed,
// whatever the old one held, and never says which held the same: a map's
// keys still show, as added, kept or removed.
func TestDiffRotatingNeverComparesAValue(t *testing.T) {
	from := &resourcev1.Container{Image: "web:1", Env: map[string]string{"A": "same", "B": "old", "GONE": "x"}}
	to := &resourcev1.Container{Image: "web:1", Env: map[string]string{"A": "same", "B": "new", "NEW": "y"}}
	want := []resource.Change{
		{Path: "env[A]", Op: resource.OpModified, Sensitive: true},
		{Path: "env[B]", Op: resource.OpModified, Sensitive: true},
		{Path: "env[GONE]", Op: resource.OpRemoved, Sensitive: true},
		{Path: "env[NEW]", Op: resource.OpAdded, Sensitive: true},
	}
	if got := resource.DiffRotating(from, to); !slices.Equal(got, want) {
		t.Errorf("DiffRotating = %+v, want %+v", got, want)
	}
	if got := resource.Diff(from, to); len(got) != 3 {
		t.Errorf("Diff = %+v, want the three that differ", got)
	}
	same := &resourcev1.Secret{Value: "hunter2"}
	if got := resource.DiffRotating(same, same); !slices.Equal(got, []resource.Change{{Path: "value", Op: resource.OpModified, Sensitive: true}}) {
		t.Errorf("rotating a secret to its own value = %+v", got)
	}
	if got := resource.DiffRotating(&resourcev1.FirewallRule{Port: 22}, &resourcev1.FirewallRule{Port: 22}); len(got) != 0 {
		t.Errorf("a rotation of what holds no secret = %+v", got)
	}
	for m, want := range map[proto.Message]bool{
		&resourcev1.Secret{Value: "x"}:                         true,
		&resourcev1.Secret{}:                                   false,
		&resourcev1.Container{Env: map[string]string{"A": ""}}: true,
		&resourcev1.Container{Image: "web:1"}:                  false,
		&resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Env: map[string]string{"A": "1"}}}}: true,
		&resourcev1.Service{Secrets: map[string]string{"A": ""}}:                                                         false,
	} {
		if got := resource.SetsSensitive(m); got != want {
			t.Errorf("SetsSensitive(%v) = %v, want %v", m, got, want)
		}
	}
}

// A step that rotates a resource there plans as a change, with every value
// it sets changed, though the server has the very same.
func TestAStepThatRotatesIsNeverNothing(t *testing.T) {
	d := &container.Driver{Runtime: containertest.NewFake()}
	ref := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}
	spec := &resourcev1.Container{Image: "web:1", Env: map[string]string{"A": "same"}}
	plain, err := resource.PlanStep(d, resource.Desired{Ref: ref, Spec: spec}, spec)
	if err != nil || plain.Action != resource.ActionNone {
		t.Errorf("planning what the server has = %+v, %v, want nothing", plain, err)
	}
	rotated, err := resource.PlanStep(d, resource.Desired{Ref: ref, Spec: spec, Rotate: true}, spec)
	if err != nil || rotated.Action != resource.ActionReplace || len(rotated.Changes) != 1 || !rotated.Changes[0].Sensitive {
		t.Errorf("rotating what the server has = %+v, %v, want a replace", rotated, err)
	}
	created, err := resource.PlanStep(d, resource.Desired{Ref: ref, Spec: spec, Rotate: true}, nil)
	if err != nil || created.Action != resource.ActionCreate {
		t.Errorf("rotating what isn't there = %+v, %v, want a create", created, err)
	}
}

// A path names a sensitive field when any field along it is one, and a
// path the message doesn't have counts as sensitive. A map's key is a key,
// whatever it holds, such as the dots of a label like traefik.enable.
func TestSensitivePath(t *testing.T) {
	service := (&resourcev1.Service{}).ProtoReflect().Descriptor()
	container := (&resourcev1.Container{}).ProtoReflect().Descriptor()
	for path, want := range map[string]bool{
		"app.env[API_KEY]":                          true,
		"app.env[api.key]":                          true,
		"app.env[a]b.c]":                            true,
		"app.image":                                 false,
		"app.ports[0].host_port":                    false,
		"app.ports[12].container_port":              false,
		"app.command[1]":                            false,
		"app.labels[traefik.enable]":                false,
		"app.labels[traefik.http.routers.web.rule]": false,
		"app.labels[odd]key.with[brackets]":         false,
		"secrets[DATABASE_URL]":                     false,
		"revisions[DATABASE_URL]":                   false,
		"app.nonsense":                              true,
		"app.ports[0].host_port.x":                  true,
		"app.ports[x].host_port":                    true,
		"app.ports[0]x":                             true,
		"app.image[0]":                              true,
		"app.labels":                                false,
		"app.":                                      true,
		"":                                          true,
	} {
		if got := resource.SensitivePath(service, path); got != want {
			t.Errorf("resource.SensitivePath(Service, %q) = %v, want %v", path, got, want)
		}
	}
	if !resource.SensitivePath(container, "env[API_KEY]") || resource.SensitivePath(container, "image") ||
		resource.SensitivePath(container, "labels[com.example.team]") {
		t.Error("SensitivePath misreads a container's paths")
	}
	if !resource.SensitivePath((&resourcev1.Secret{}).ProtoReflect().Descriptor(), "value") {
		t.Error("a secret's value isn't sensitive")
	}
}
