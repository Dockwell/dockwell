// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

// AppPortsTimeout is how long reading a service's ports may take on its agent: a
// few containers read, and each of up to 24 ports asked for 2 seconds, four at once.
const AppPortsTimeout = 30 * time.Second

// ReadsAppPorts reports an agent that reads a service's ports and asks each (#791).
func (s *Session) ReadsAppPorts() bool { return s.readsAppPorts }

// AppPorts asks the agent which TCP ports a service's running containers expose,
// publish and listen on, and what answers on each at the container's own address.
func (s *Session) AppPorts(ctx context.Context, environment, service string) (*agentv1.AppPortsResult, error) {
	if !s.readsAppPorts {
		return nil, fmt.Errorf("%s's agent can't read an app's ports yet: %w", s.name, ErrOlderAgent)
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_AppPorts{AppPorts: &agentv1.AppPortsCall{Environment: environment, Service: service}}}, AppPortsTimeout, nil)
	if err != nil {
		return nil, err
	}
	if r.GetAppPorts() == nil {
		return nil, ErrOlderAgent
	}
	return r.GetAppPorts(), nil
}
