// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/ownerstep"
	"github.com/dockwell/dockwell/internal/resource"
)

// OwnerWait is how long making an app's owner waits for its service to be healthy,
// as a new install's is just after its change applies, and ownerEvery how often it
// looks again.
const (
	OwnerWait  = 5 * time.Minute
	ownerEvery = 2 * time.Second
)

// makeAppOwner runs the steps that make a catalog app's owner, with what the control
// plane sealed for this call alone, once the service is healthy (ADR 0105). What the
// app says back is scrubbed before it leaves.
func (c *calls) makeAppOwner(ctx context.Context, call *agentv1.MakeAppOwnerCall) (*agentv1.MakeAppOwnerResult, error) {
	values, t, err := c.ownerTarget(ctx, call.GetEnvironment(), call.GetService(), call.GetValues())
	if err != nil {
		return nil, err
	}
	defer clear(values)
	res := ownerstep.Run(ctx, t, call.GetOwner(), values, call.GetCheckFirst())
	return &agentv1.MakeAppOwnerResult{Made: res.Made, Waiting: res.Waiting, Step: int32(res.Step), Said: res.Said}, nil //nolint:gosec // a step's place, a few at most
}

// readAppOwner reads whether a catalog app's owner is made, or its sign-ups closed.
func (c *calls) readAppOwner(ctx context.Context, call *agentv1.ReadAppOwnerCall) (*agentv1.ReadAppOwnerResult, error) {
	values, t, err := c.ownerTarget(ctx, call.GetEnvironment(), call.GetService(), call.GetValues())
	if err != nil {
		return nil, err
	}
	defer clear(values)
	o := call.GetOwner()
	check := o.GetDone()
	if call.GetSignups() {
		check = o.GetSignupsCheck()
	}
	yes, said := ownerstep.Read(ctx, t, o, check, values)
	return &agentv1.ReadAppOwnerResult{Yes: yes, Said: said}, nil
}

// ownerTarget opens what was sealed for the call, and finds the service's healthy
// container, waiting for it.
func (c *calls) ownerTarget(ctx context.Context, environment, service string, sealed []byte) (map[string]string, *ownerContainer, error) {
	if c.cfg.Docker == nil {
		return nil, nil, errors.New("this agent has no Docker to make an app's owner in")
	}
	if c.delivery == nil {
		return nil, nil, errors.New("this agent has no delivery key to open what the owner's steps were sent with")
	}
	values, err := c.delivery.Open(c.server, resource.AppOwnerRef(environment), sealed)
	if err != nil {
		return nil, nil, err
	}
	t, err := healthyContainer(ctx, c.cfg.Docker, environment, service)
	if err != nil {
		clear(values)
		return nil, nil, err
	}
	return values, t, nil
}

// healthyContainer waits, for OwnerWait at most, for one of a service's containers
// to run and pass its health check, where it has one.
func healthyContainer(ctx context.Context, client *docker.Client, environment, service string) (*ownerContainer, error) {
	ctx, cancel := context.WithTimeout(ctx, OwnerWait)
	defer cancel()
	said := "no container of it runs yet"
	for {
		// Dockwell's own, by the revision it deploys, never a container that only
		// says the service's Compose labels.
		list, err := client.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + docker.Project(environment), docker.LabelService + "=" + service, docker.LabelRevision}})
		if err != nil && ctx.Err() == nil {
			return nil, err
		}
		for i := range list {
			if list[i].State != "running" {
				continue
			}
			got, err := client.InspectContainer(ctx, list[i].ID)
			if err != nil {
				continue
			}
			if h := got.State.Health; h != nil && h.Status != "healthy" {
				said = "its health check hasn't passed yet"
				continue
			}
			return &ownerContainer{client: client, c: got}, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s didn't come up healthy within %s, so its owner wasn't made: %s", service, OwnerWait, said)
		case <-time.After(ownerEvery):
		}
	}
}

// ownerContainer is a service's running container, as an owner's steps reach it.
type ownerContainer struct {
	client *docker.Client
	c      *docker.Container
}

// Exec runs words in the container, as user, with env added, and keeps what it
// prints, up to what's ever kept of it.
func (t *ownerContainer) Exec(ctx context.Context, words []string, user string, env []string) ([]byte, int, error) {
	var out capped
	code, err := t.client.ExecWith(ctx, t.c.ID, words, docker.ExecOptions{User: user, Env: env}, docker.Streams{Stdout: &out, Stderr: &out})
	return out.Bytes(), code, err
}

// Address is the container's address on one of its networks, as the agent reaches
// a container for an HTTP health check: the ingress's last, as its addresses are
// the ingress's own table's.
func (t *ownerContainer) Address(_ context.Context, port uint32) (string, error) {
	nets := t.c.NetworkSettings.Networks
	names := slices.Sorted(maps.Keys(nets))
	slices.SortStableFunc(names, func(a, b string) int {
		switch {
		case a == docker.IngressNetwork && b != docker.IngressNetwork:
			return 1
		case b == docker.IngressNetwork && a != docker.IngressNetwork:
			return -1
		}
		return 0
	})
	for _, name := range names {
		if e := nets[name]; e != nil && e.IPAddress != "" {
			return net.JoinHostPort(e.IPAddress, strconv.FormatUint(uint64(port), 10)), nil
		}
	}
	return "", errors.New("its container has no address the agent can reach, as on the server's own network")
}

// capped keeps the first 64 KiB written to it, which is more than is ever kept of
// what a step prints.
type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := 64<<10 - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
