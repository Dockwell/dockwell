// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// An agent from before references would run ${NAME} as the words it is: a password anyone
// who can read the plan knows. So one that doesn't say it writes them in gets no such
// spec, to plan or to apply, and is told to update.
func TestAnAgentThatDoesntWriteReferencesGetsNone(t *testing.T) {
	names := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "redis:8", Command: []string{"redis-server", "--requirepass", "${PW}"},
		Env: map[string]string{"PW": "x"}, References: []string{"command"},
	}}}
	plain := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "redis:8", Command: []string{"$HOME"}}}}
	ref := resource.Ref{Kind: service.Kind, Scope: "env_1", Name: "redis"}
	errSent := errors.New("the test stops the call here")
	open := func(writes bool) *Session {
		a := NewAgents(resource.NewRegistry(&service.Driver{}), slog.New(slog.DiscardHandler))
		// Nothing on the server names values yet, so only what goes out is checked.
		a.CheckNaming(&naming{})
		return a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(service.Kind)}, WritesReferences: writes}, func(*agentv1.ConnectResponse) error { return errSent })
	}
	calls := map[string]func(*Session, proto.Message) error{
		"to plan": func(s *Session, spec proto.Message) error {
			_, err := s.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
			return err
		},
		"to apply": func(s *Session, spec proto.Message) error {
			return s.Apply(t.Context(), resource.Step{Ref: ref}, spec)
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			const says = "the agent on hz-1 can't write a value into a command, and redis names one there. Update the agent on hz-1"
			// What goes out fails as the stream does, which the refusal comes before.
			refused := func(on *Session, spec proto.Message) bool {
				t.Helper()
				err := call(on, spec)
				return err != nil && strings.Contains(err.Error(), says)
			}
			if !refused(open(false), names) {
				t.Error("an old agent got a spec that names values")
			}
			if err := call(open(false), plain); !errors.Is(err, resource.ErrNotConnected) {
				t.Errorf("an old agent was refused a spec that names nothing: %v", err)
			}
			if refused(open(true), names) {
				t.Error("a new agent was refused a spec that names values")
			}
		})
	}
}
