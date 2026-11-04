// SPDX-License-Identifier: AGPL-3.0-only

package resource_test

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
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
