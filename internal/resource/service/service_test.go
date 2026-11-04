// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
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

// A container run by hand can never take the name of a service's
// container, so neither manages the other's.
func TestContainersCantTakeAServicesName(t *testing.T) {
	svc := service.ContainerName(resource.Ref{Kind: service.Kind, Scope: "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M", Name: "web"})
	for name, ok := range map[string]bool{
		"web": true, "my_app.v2": true, "Web-1": true, "web.env": true,
		svc: false, "db.env_X": false, "My App": false, "api/v2": false, "-web": false, "": false,
	} {
		if err := (&container.Driver{}).CheckName(name); (err == nil) != ok {
			t.Errorf("CheckName(%q) = %v", name, err)
		}
	}
}
