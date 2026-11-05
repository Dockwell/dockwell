// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// naming stands in for the keeper: what it lists names values on the server.
type naming struct {
	named []resource.Ref
	asked int
}

func (n *naming) NamingValues(_ context.Context, _ string, refs []resource.Ref) ([]resource.Ref, error) {
	n.asked++
	var out []resource.Ref
	for _, r := range refs {
		for _, m := range n.named {
			if r == m {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// An agent that doesn't say it writes references in reads what names values as
// Docker has it. So every way the control plane reads it is refused for what names
// one there, whatever spec it is asked about: reading it back, planning and applying.
// What names nothing goes, and an agent that says it writes them in is never asked.
func TestAnOlderAgentIsRefusedWhatNamesAValueAtEveryEntry(t *testing.T) {
	redis := resource.Ref{Kind: service.Kind, Scope: "env_1", Name: "redis"}
	web := resource.Ref{Kind: service.Kind, Scope: "env_1", Name: "web"}
	plain := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "redis:8", Command: []string{"redis-server"}}}}
	errSent := errors.New("the test stops the call here")
	open := func(writes bool, n *naming) *Session {
		a := NewAgents(resource.NewRegistry(&service.Driver{}), slog.New(slog.DiscardHandler))
		a.CheckNaming(n)
		return a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(service.Kind)}, WritesReferences: writes}, func(*agentv1.ConnectResponse) error { return errSent })
	}
	entries := map[string]func(*Session, resource.Ref) error{
		"reading back": func(s *Session, ref resource.Ref) error {
			_, err := s.Observe(t.Context(), []resource.Ref{web, ref})
			return err
		},
		"planning": func(s *Session, ref resource.Ref) error {
			_, err := s.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: plain}})
			return err
		},
		"applying": func(s *Session, ref resource.Ref) error { return s.Apply(t.Context(), resource.Step{Ref: ref}, plain) },
	}
	for name, entry := range entries {
		t.Run(name, func(t *testing.T) {
			n := &naming{named: []resource.Ref{redis}}
			err := entry(open(false, n), redis)
			if !errors.Is(err, resource.ErrAgentTooOld) || !strings.Contains(err.Error(), "until you update the agent there") {
				t.Errorf("an older agent, with what names a value = %v", err)
			}
			if err := entry(open(false, n), web); errors.Is(err, resource.ErrAgentTooOld) {
				t.Errorf("an older agent, with what names nothing = %v", err)
			}
			current := &naming{named: []resource.Ref{redis}}
			if err := entry(open(true, current), redis); errors.Is(err, resource.ErrAgentTooOld) || current.asked != 0 {
				t.Errorf("an agent that writes references in = %v, asked %d times", err, current.asked)
			}
		})
	}
}
