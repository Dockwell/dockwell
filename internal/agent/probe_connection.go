// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/probe"
	"github.com/dockwell/dockwell/internal/resource"
)

// probeConnection checks a connection from where an app is, on its networks, with
// what the control plane sealed for it (ADR 0099). What the check may reach is the
// agent's to say, whatever the request held: the app's networks, and never the
// server's own addresses.
func (c *calls) probeConnection(ctx context.Context, call *agentv1.ProbeConnectionCall) (*agentv1.ProbeConnectionResult, error) {
	if c.cfg.Probes == nil {
		return nil, errors.New("this agent doesn't check connections")
	}
	if c.delivery == nil {
		return nil, errors.New("this agent has no delivery key to open what the check was sent with")
	}
	values, err := c.delivery.Open(c.server, resource.ProbeRef(call.GetEnvironment()), call.GetRequest())
	if err != nil {
		return nil, err
	}
	defer clear(values)
	var req probe.Request
	if err := json.Unmarshal([]byte(values["request"]), &req); err != nil {
		return nil, fmt.Errorf("can't read what the check was asked: %w", err)
	}
	req.Allow, req.Refuse = nil, nil
	networks, err := c.appNetworks(ctx, call.GetEnvironment(), call.GetService())
	if err != nil {
		return nil, err
	}
	res, err := c.cfg.Probes.Run(ctx, req, networks)
	if err != nil {
		return nil, err
	}
	return &agentv1.ProbeConnectionResult{
		Passed: res.Passed, Step: res.Step, Code: res.Code, Status: int32(res.Status), //nolint:gosec // an HTTP status
		Message: res.Message, Words: res.Words, Region: res.Region,
	}, nil
}

// appNetworks are the networks an app's containers are on, by name: none for no
// app, or for one on the host's network, whose network is the server's. The
// ingress network and a database copy's are left out: the ingress gives each
// container on it an address from its own table, which an address Docker picks for
// the check could take from a stopped service, and a copy's is Dockwell's for a
// moment (ADR 0024). Neither reaches anything an app's bucket is on.
func (c *calls) appNetworks(ctx context.Context, environment, service string) ([]string, error) {
	if service == "" || c.cfg.Docker == nil {
		return nil, nil
	}
	list, err := c.cfg.Docker.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + docker.Project(environment), docker.LabelService + "=" + service}})
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range list {
		for name := range list[i].NetworkSettings.Networks {
			switch {
			case name == "host":
				return nil, nil
			case name == "none" || name == docker.IngressNetwork || strings.HasPrefix(name, docker.CopyPrefix) || slices.Contains(out, name):
			default:
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}
