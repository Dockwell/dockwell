// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/scope"
)

// A service's name goes in its URL and its container's name, so it is a DNS
// label.
func TestServiceNamesAreDNSLabels(t *testing.T) {
	for name, ok := range map[string]bool{
		"web": true, "api-v2": true, "a": true, strings.Repeat("a", 63): true,
		"": false, "My App": false, "api/v2": false, "Web": false, "2web": false, "web-": false, "web.v2": false,
		"web_v2": false, strings.Repeat("a", 64): false,
	} {
		err := (&service.Driver{}).CheckName(name)
		if (err == nil) != ok {
			t.Errorf("CheckName(%q) = %v", name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "such as api-v2") {
			t.Errorf("CheckName(%q) should give an example: %v", name, err)
		}
	}
}

// What a service only reads, a new version can't have changed, so it's left out of
// what a plan says a rollback doesn't undo.
func TestKeptDataIsWhatAServiceWritesTo(t *testing.T) {
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Mounts: []*resourcev1.Mount{
		{Source: &resourcev1.Mount_Volume{Volume: "uploads"}, Target: "/uploads"},
		{Source: &resourcev1.Mount_Volume{Volume: "assets"}, Target: "/assets", ReadOnly: true},
		{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/state"}, Target: "/state"},
		{Source: &resourcev1.Mount_HostPath{HostPath: "/etc/app"}, Target: "/etc/app", ReadOnly: true},
	}}}}
	if got, want := (&service.Driver{}).KeptData(spec), []string{"uploads", "/srv/state"}; !slices.Equal(got, want) {
		t.Errorf("KeptData = %v, want %v", got, want)
	}
}

// A container run by hand can never take the name of a service's
// container, so neither manages the other's.
func TestContainersCantTakeAServicesName(t *testing.T) {
	svc := service.ContainerName(resource.Ref{Kind: service.Kind, Scope: "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M", Name: "web"})
	if svc != "env_01kat4zr6e3v0y5xbkzb5d9q6m-web-1" {
		t.Errorf("the service's container is %s, want it named as Compose names a service's first", svc)
	}
	for name, ok := range map[string]bool{
		"web": true, "my_app.v2": true, "Web-1": true, "web.env": true, "db.env_X": true, "my-env_1": true,
		svc: false, "env_X-db-1": false, "ENV_x": false, "My App": false, "api/v2": false, "-web": false, "": false,
	} {
		if err := (&container.Driver{}).CheckName(name); (err == nil) != ok {
			t.Errorf("CheckName(%q) = %v", name, err)
		}
	}
}

// The kinds whose specs may hold a secret are the ones the revision store
// keeps sealed: a secret, a service or a container, whose environment may
// hold one, and a volume, whose options hold a CIFS password. A variable, a
// network, a project, an environment and a firewall rule hold none.
func TestWhichKindsAreSealed(t *testing.T) {
	for _, c := range []struct {
		t    resource.Type
		want bool
	}{
		{variable.SecretType{}, true},
		{&service.Driver{}, true},
		{&container.Driver{}, true},
		{&volume.Driver{}, true},
		{variable.Type{}, false},
		{&network.Driver{}, false},
		{scope.ProjectType{}, false},
		{scope.EnvironmentType{}, false},
		{&firewall.Driver{}, false},
	} {
		if got := resource.Sealed(c.t); got != c.want {
			t.Errorf("resource.Sealed(%s) = %t, want %t", c.t.Kind(), got, c.want)
		}
	}
}
