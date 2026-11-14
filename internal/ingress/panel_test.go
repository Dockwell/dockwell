// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// A panel's proxy holds ports 80 and 443 until its domains come over, and the change
// that brings them stops it first (ADR 0089): a route planned in that change isn't
// refused for what the proxy holds, and one planned without it still is.
func TestAPanelsProxyThatGoesFirstHoldsNothing(t *testing.T) {
	s := newSetup(t)
	c := s.engine.AddContainer(docker.Container{Name: "/coolify-proxy"})
	s.proc.listen("tcp", 80, "0A", "31")
	s.proc.listen("tcp", 443, "0A", "32")
	s.proc.process(4321, "traefik", "0::/system.slice/docker-"+c.ID+".scope", "31", "32")
	ref := routeRef("blog")
	route := resource.Desired{Ref: ref, Spec: spec("blog.example.com", "", "web", 80)}
	if err := s.Check(t.Context(), ref, []resource.Desired{route}); err == nil || !strings.Contains(err.Error(), "coolify-proxy") {
		t.Errorf("a route with the proxy holding the ports = %v", err)
	}
	stop := resource.Desired{Ref: resource.Ref{Kind: "server.host", Name: "host"}, Panel: &resource.PanelAsked{Kind: resource.PanelCoolify, Proxy: true}}
	if err := s.Check(t.Context(), ref, []resource.Desired{stop, route}); err != nil {
		t.Errorf("a route in the change that stops the proxy = %v", err)
	}
	// Another panel's proxy going leaves this one holding them.
	other := resource.Desired{Ref: stop.Ref, Panel: &resource.PanelAsked{Kind: resource.PanelDokploy, Proxy: true}}
	if err := s.Check(t.Context(), ref, []resource.Desired{other, route}); err == nil {
		t.Error("Dokploy's proxy going let Coolify's go too")
	}
}
