// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"fmt"
	"maps"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/ownerstep"
	"github.com/dockwell/dockwell/internal/resource"
)

// AppOwnerTimeout is how long making an app's owner may take on its agent: the wait
// for its service to be healthy, and its steps' own bounds.
const AppOwnerTimeout = 5*time.Minute + 4*ownerstep.CommandWait + 30*time.Second

// MakesAppOwners reports an agent that makes a catalog app's owner (ADR 0105).
func (s *Session) MakesAppOwners() bool { return s.makesAppOwners }

// MakeAppOwner asks the agent to run the steps that make an app's owner, in its
// environment's service, with values sealed to it for this call alone: they never
// enter the agent's desired state, and the agent keeps none of them. With checkFirst,
// it reads whether the owner is made first.
func (s *Session) MakeAppOwner(ctx context.Context, environment, service string, owner *resourcev1.OwnerRun, values map[string]string, checkFirst bool) (*agentv1.MakeAppOwnerResult, error) {
	if !s.makesAppOwners {
		return nil, fmt.Errorf("%s's agent can't make an app's owner yet. Update its agent, then try again", s.name)
	}
	call := &agentv1.MakeAppOwnerCall{Environment: environment, Service: service, Owner: owner, CheckFirst: checkFirst}
	seal := func(on *Session) error {
		v := maps.Clone(values)
		defer clear(v)
		sealed, err := on.seal(resource.AppOwnerRef(environment), v)
		call.Values = sealed
		return err
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_MakeAppOwner{MakeAppOwner: call}}, AppOwnerTimeout, seal)
	if err != nil {
		return nil, err
	}
	if r.GetMakeAppOwner() == nil {
		return nil, ErrOlderAgent
	}
	return r.GetMakeAppOwner(), nil
}

// ReadAppOwner asks the agent whether an app's owner is made, by its done, or its
// sign-ups closed, by their check.
func (s *Session) ReadAppOwner(ctx context.Context, environment, service string, owner *resourcev1.OwnerRun, values map[string]string, signups bool) (*agentv1.ReadAppOwnerResult, error) {
	if !s.makesAppOwners {
		return nil, fmt.Errorf("%s's agent can't read an app's owner yet. Update its agent, then try again", s.name)
	}
	call := &agentv1.ReadAppOwnerCall{Environment: environment, Service: service, Owner: owner, Signups: signups}
	seal := func(on *Session) error {
		v := maps.Clone(values)
		defer clear(v)
		sealed, err := on.seal(resource.AppOwnerRef(environment), v)
		call.Values = sealed
		return err
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ReadAppOwner{ReadAppOwner: call}}, AppOwnerTimeout, seal)
	if err != nil {
		return nil, err
	}
	if r.GetReadAppOwner() == nil {
		return nil, ErrOlderAgent
	}
	return r.GetReadAppOwner(), nil
}
