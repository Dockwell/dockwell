// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
)

// An agent that doesn't say it serves redirects would write a route that sends to
// nothing, so a redirect is refused it, planned or applied, with how to put it right
// (ADR 0070). A route to a service still goes, and an agent that says it serves
// redirects gets one.
func TestAnOlderAgentIsRefusedARedirect(t *testing.T) {
	www := resource.Ref{Kind: route.Kind, Scope: "env_1", Name: "www-example-com"}
	redirect := &resourcev1.Route{Domain: "www.example.com", Redirect: &resourcev1.RouteRedirect{To: "https://example.com"}}
	toWeb := &resourcev1.Route{Domain: "www.example.com", Service: "web", Port: 8080}
	errSent := errors.New("the test stops the call here")
	open := func(serves bool) *Session {
		a := NewAgents(resource.NewRegistry(&route.Driver{}), slog.New(slog.DiscardHandler))
		hello := &agentv1.Hello{Kinds: []string{string(route.Kind)}, WritesReferences: true, ReadsTakenReferences: true, ServesRedirects: serves}
		return a.open("srv_1", "hz-2", hello, func(*agentv1.ConnectResponse) error { return errSent })
	}
	entries := map[string]func(*Session, *resourcev1.Route) error{
		"planning": func(s *Session, spec *resourcev1.Route) error {
			_, err := s.Plan(t.Context(), []resource.Desired{{Ref: www, Spec: spec}})
			return err
		},
		"applying": func(s *Session, spec *resourcev1.Route) error {
			return s.Apply(t.Context(), resource.Step{Ref: www}, spec)
		},
	}
	for name, entry := range entries {
		t.Run(name, func(t *testing.T) {
			err := entry(open(false), redirect)
			if !errors.Is(err, resource.ErrAgentTooOld) || !strings.Contains(err.Error(), "hz-2's agent is older than this release, so it can't serve www.example.com's redirect. Update its agent") {
				t.Errorf("an older agent, with a redirect = %v", err)
			}
			if err := entry(open(false), toWeb); errors.Is(err, resource.ErrAgentTooOld) {
				t.Errorf("an older agent, with a route to a service = %v", err)
			}
			if err := entry(open(true), redirect); errors.Is(err, resource.ErrAgentTooOld) {
				t.Errorf("an agent that serves redirects = %v", err)
			}
		})
	}
}
