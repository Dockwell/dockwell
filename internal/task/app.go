// SPDX-License-Identifier: AGPL-3.0-only

package task

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
)

// LabelTask names the task a one-off container runs, on that container.
const LabelTask = docker.OwnPrefix + "task"

// runApp runs a command in an app's image, as docker compose run does: a one-off
// container from the image the app's container runs now, with its entrypoint,
// environment, mounts, networks, user and limits, and the task's command in place
// of the app's. It publishes nothing, never restarts, and goes when it ends.
func (d *Driver) runApp(ctx context.Context, ref resource.Ref, a *resourcev1.TaskApp, timeout time.Duration, out *output) attempt {
	if d.Client == nil {
		return attempt{err: errors.New("this server's agent can't reach Docker, so it can't run an app's task")}
	}
	project, service := docker.Project(ref.Scope), a.GetService()
	app, err := d.Client.ContainerOf(ctx, project, service, 1)
	switch {
	case docker.IsNotFound(err):
		return attempt{err: fmt.Errorf("%s has no container on this server, and its task runs in what it runs. Deploy %s here, or place the task where it runs", service, service)}
	case err != nil:
		return attempt{err: fmt.Errorf("can't read %s's container: %w", service, err)}
	case !app.State.Running:
		return attempt{err: fmt.Errorf("%s isn't running on this server, and its task runs in what it runs. Start %s, then run the task again", service, service)}
	}
	words, err := Words(a.GetCommand())
	if err != nil {
		return attempt{err: err}
	}
	labels := map[string]string{
		docker.LabelProject: project, docker.LabelService: service, docker.LabelOneoff: "True", LabelTask: ref.Name,
	}
	host := app.HostConfig
	host.PortBindings, host.RestartPolicy = nil, docker.RestartPolicy{Name: "no"}
	// The run reaches what the app reaches, under no name of the app's own, so
	// nothing sends it the app's traffic. Docker joins one network as it makes a
	// container, the one its network mode names, and the rest join after. An app
	// on the host's network, or another container's, shares that as it is.
	networks := slices.Sorted(maps.Keys(app.NetworkSettings.Networks))
	endpoints := map[string]*docker.EndpointSettings{}
	if mode := host.NetworkMode; len(networks) > 0 && mode != "host" && mode != "none" && !strings.HasPrefix(mode, "container:") {
		first := networks[0]
		if slices.Contains(networks, mode) {
			first = mode
		}
		host.NetworkMode = first
		endpoints[first] = &docker.EndpointSettings{}
		networks = slices.DeleteFunc(networks, func(n string) bool { return n == first })
	} else {
		networks = nil
	}
	cfg := app.Config
	cfg.Image, cfg.Cmd, cfg.Labels = app.Image, words, labels
	cfg.ExposedPorts, cfg.Healthcheck, cfg.Tty, cfg.OpenStdin, cfg.StdinOnce = nil, nil, false, false, false
	name := fmt.Sprintf("%s-%s-run-%s", project, service, strings.ToLower(strings.TrimPrefix(id.New(id.TaskRun), id.TaskRun+"_"))[:12])
	cid, err := d.Client.CreateContainer(ctx, name, &docker.CreateRequest{Config: cfg, HostConfig: host, NetworkingConfig: docker.NetworkingConfig{EndpointsConfig: endpoints}})
	if err != nil {
		return attempt{err: fmt.Errorf("can't make a container from %s's image to run the task in: %w", service, err)}
	}
	defer func() {
		rm, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		_ = d.Client.RemoveOneOff(rm, cid)
		cancel()
	}()
	for _, net := range networks {
		if err := d.Client.ConnectNetwork(ctx, net, cid, nil); err != nil {
			return attempt{err: fmt.Errorf("can't put the task's container on %s's network %s: %w", service, net, err)}
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, err := d.Client.RunAttached(runCtx, cid, docker.Streams{Stdout: out, Stderr: out})
	if err == nil {
		if code != 0 {
			return attempt{exitCode: int32(code), started: true, err: fmt.Errorf("it exited with %d", code)} //nolint:gosec // an exit status fits
		}
		return attempt{started: true}
	}
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	stop, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), stopGrace+5*time.Second)
	_ = d.Client.StopContainer(stop, cid, stopGrace)
	cancelStop()
	why := fmt.Errorf("can't run it in %s's image: %w", service, err)
	switch {
	case timedOut:
		why = fmt.Errorf("it took longer than its timeout, %s, and was stopped", timeout)
	case ctx.Err() != nil:
		why = errors.New("the agent stopped, and stopped it")
	}
	out.Line("Dockwell: " + why.Error() + ".")
	return attempt{exitCode: -1, timedOut: timedOut, started: true, err: why}
}
