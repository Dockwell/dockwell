// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
)

// Logins picks the registry login a resource's pull signs in with, opened for one
// call (ADR 0086): registrylogins.Logins. planned, when set, is the login a plan
// picked, which its apply uses and no other.
type Logins interface {
	For(ctx context.Context, ref resource.Ref, spec proto.Message, planned string) (registry.Auth, bool, error)
	// BuildNames names the logins ref's build may pull its base images with, for
	// its plan, and ForBuild opens those a plan named, for its step.
	BuildNames(ctx context.Context, ref resource.Ref, spec proto.Message) (names, versions []string, err error)
	ForBuild(ctx context.Context, ref resource.Ref, spec proto.Message, planned, versions []string) ([]registry.Auth, error)
}

// SendLogins has plans and steps carry the registry login their image is pulled
// with, sealed to the stream they go out on. Without it, no login is sent.
func (a *Agents) SendLogins(logins Logins) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logins = logins
}

// login is the login ref's pull signs in with, if any, for this agent: one that
// can't sign in with a login is refused one, as it would pull without it.
func (s *Session) login(ctx context.Context, ref resource.Ref, spec proto.Message, planned string) (registry.Auth, bool, error) {
	s.agents.mu.Lock()
	logins := s.agents.logins
	s.agents.mu.Unlock()
	if logins == nil || spec == nil {
		if planned != "" {
			return registry.Auth{}, false, fault.New(ErrNoLogins, "the plan pulls %s with the registry login %s, which this control plane can't read. Plan it again", ref.Name, planned)
		}
		return registry.Auth{}, false, nil
	}
	a, ok, err := logins.For(ctx, ref, spec, planned)
	if err != nil || !ok {
		return registry.Auth{}, false, err
	}
	if err := s.takesLogins(a.Name); err != nil {
		return registry.Auth{}, false, err
	}
	if len(s.deliveryKey) == 0 {
		return registry.Auth{}, false, s.noDeliveryKey(ref)
	}
	return a, true, nil
}

// ErrNoLogins is a plan that named a login this control plane can't read.
var ErrNoLogins = errors.New("no registry logins")

// takesLogins refuses an agent from before registry logins, naming the login.
func (s *Session) takesLogins(name string) error {
	if s.registryLogins {
		return nil
	}
	return fault.New(ErrOlderAgent, "%s's agent is older than the control plane, and can't sign in to a registry with the login %s. "+
		"Update its agent by running the install script on the server again", s.name, name)
}

// sealLogin seals a login for ref to this stream's delivery key, apart from ref's
// own values.
func (s *Session) sealLogin(ref resource.Ref, a registry.Auth) ([]byte, error) {
	values := a.Values()
	defer clear(values)
	return s.seal(resource.LoginRef(ref), values)
}

// ImageAs asks the agent to read an image signed in with a login, sealed to the
// stream the call goes out on.
func (s *Session) ImageAs(ctx context.Context, call *agentv1.ImageCall, a registry.Auth) (*agentv1.ImageResult, error) {
	if err := s.takesLogins(a.Name); err != nil {
		return nil, err
	}
	seal := func(on *Session) error {
		sealed, err := on.sealLogin(resource.Ref{}, a)
		call.RegistryAuth = sealed
		return err
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Image{Image: call}}, ImageTimeout, seal)
	if err != nil {
		return nil, err
	}
	if r.GetImage() == nil {
		return nil, ErrOlderAgent
	}
	return r.GetImage(), nil
}

// ImageUpdatesAs asks the agent what's newer than an image, signed in with a login.
func (s *Session) ImageUpdatesAs(ctx context.Context, call *agentv1.ImageUpdatesCall, a registry.Auth) (*agentv1.ImageUpdatesResult, error) {
	if err := s.takesLogins(a.Name); err != nil {
		return nil, err
	}
	seal := func(on *Session) error {
		sealed, err := on.sealLogin(resource.Ref{}, a)
		call.RegistryAuth = sealed
		return err
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ImageUpdates{ImageUpdates: call}}, ImageUpdatesTimeout, seal)
	if err != nil {
		return nil, err
	}
	if r.GetImageUpdates() == nil {
		return nil, ErrOlderAgent
	}
	return r.GetImageUpdates(), nil
}

// CheckRegistryLogin asks the agent to sign in at a login's registry, as its pull
// would, and save nothing. first says the login is as typed, and may go to the
// token service the registry names now, wherever that is.
func (s *Session) CheckRegistryLogin(ctx context.Context, a registry.Auth, first bool) (*agentv1.CheckRegistryLoginResult, error) {
	if err := s.takesLogins(a.Name); err != nil {
		return nil, err
	}
	call := &agentv1.CheckRegistryLoginCall{First: first}
	seal := func(on *Session) error {
		sealed, err := on.sealLogin(resource.Ref{}, a)
		call.RegistryAuth = sealed
		return err
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_CheckRegistryLogin{CheckRegistryLogin: call}}, ImageTimeout, seal)
	if err != nil {
		return nil, err
	}
	if r.GetCheckRegistryLogin() == nil {
		return nil, ErrOlderAgent
	}
	return r.GetCheckRegistryLogin(), nil
}

// ProbeRegistry asks the agent to ask a registry's /v2/ with no login, and send
// nothing that signs in.
func (s *Session) ProbeRegistry(ctx context.Context, host string) (*agentv1.CheckRegistryLoginResult, error) {
	if err := s.takesLogins(host); err != nil {
		return nil, err
	}
	call := &agentv1.CheckRegistryLoginCall{Probe: host}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_CheckRegistryLogin{CheckRegistryLogin: call}}, ImageTimeout, nil)
	if err != nil {
		return nil, err
	}
	if r.GetCheckRegistryLogin() == nil {
		return nil, ErrOlderAgent
	}
	return r.GetCheckRegistryLogin(), nil
}

// buildLoginNames names the logins ref's build may pull its base images with, for a
// plan through this agent: one that can't hand them to BuildKit is refused them.
func (s *Session) buildLoginNames(ctx context.Context, ref resource.Ref, spec proto.Message) (names, versions []string, err error) {
	s.agents.mu.Lock()
	logins := s.agents.logins
	s.agents.mu.Unlock()
	if logins == nil || spec == nil {
		return nil, nil, nil
	}
	names, versions, err = logins.BuildNames(ctx, ref, spec)
	if err != nil || len(names) == 0 {
		return nil, nil, err
	}
	if err := s.takesBuildLogins(names); err != nil {
		return nil, nil, err
	}
	return names, versions, nil
}

// takesBuildLogins refuses an agent that doesn't hand a build's logins to BuildKit,
// which would build without them.
func (s *Session) takesBuildLogins(names []string) error {
	if s.handsBuildLogins {
		return nil
	}
	return fault.New(ErrOlderAgent, "%s's agent is older than the control plane, and can't pull a build's base images with the registry logins %s. "+
		"Update its agent by running the install script on the server again", s.name, strings.Join(names, ", "))
}

// buildAuths opens the logins a step's build pulls its base images with, as its
// plan named them.
func (s *Session) buildAuths(ctx context.Context, step resource.Step, spec proto.Message) ([]registry.Auth, error) {
	if step.Build == nil || len(step.BuildLogins) == 0 {
		return nil, nil
	}
	s.agents.mu.Lock()
	logins := s.agents.logins
	s.agents.mu.Unlock()
	if logins == nil {
		return nil, fault.New(ErrNoLogins, "the plan builds %s with the registry logins %s, which this control plane can't read. Plan it again", step.Ref.Name, strings.Join(step.BuildLogins, ", "))
	}
	if err := s.takesBuildLogins(step.BuildLogins); err != nil {
		return nil, err
	}
	if len(s.deliveryKey) == 0 {
		return nil, s.noDeliveryKey(step.Ref)
	}
	return logins.ForBuild(ctx, step.Ref, spec, step.BuildLogins, step.BuildLoginVersions)
}

// sealBuildLogins seals a build's logins for ref to this stream's delivery key.
func (s *Session) sealBuildLogins(ref resource.Ref, auths []registry.Auth) ([]byte, error) {
	values := registry.AuthsValues(auths)
	defer clear(values)
	return s.seal(resource.BuildLoginRef(ref), values)
}
