// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
)

// Image is Caddy's image, by the digest of its multi-platform index, so
// whatever serves it, Docker checks it is this one (ADR 0024). Pin a new one
// to a release at least 14 days old. The tag is what the digest was read from,
// and not part of the reference, as Docker needs only one of them.
const (
	Repository = "caddy"
	Tag        = "2.11.7-alpine"
	Digest     = "sha256:e70b164da81ef698b3ad19af799ea6086ce6d9e48ed4079eb9fe26f6fbf14ee5"
	Image      = Repository + "@" + Digest
)

// ownLabel marks Caddy's container as Dockwell's, and versionLabel says which
// version it runs, for people, as a digest doesn't.
const (
	ownLabel     = docker.OwnPrefix + "ingress"
	versionLabel = docker.OwnPrefix + "ingress.version"
)

// memoryLimit is the most memory Caddy's container may use, with no swap. It
// reads 153 MB at the most routes a server may have (ingress.MaxRoutes), so this
// is far past what it needs: what it stops is a Caddy that runs away taking the
// server with it. Docker starts it again if it goes past this.
const memoryLimit = 1 << 30

// Where Caddy's directories are in its container: the image's own, and one for
// sockets.
const (
	containerData   = "/data"
	containerConfig = "/config"
	containerRun    = "/run/dockwell"
	containerBoot   = "/etc/dockwell"
)

// stopWait is how long Caddy gets to finish what it is doing as it stops,
// before Docker kills it.
const stopWait = 30 * time.Second

// Container runs Caddy as a container on the server's own network, which gives
// it the real address of every client, on IPv4 and IPv6, and HTTP/3 with
// nothing in between (ADR 0024).
type Container struct {
	Client *docker.Client
	// Dir is the ingress's directory on the server.
	Dir string
	// Mirror is the repository to pull from instead of Docker Hub's, such as
	// registry.example.com/library/caddy. Only the repository changes: the
	// digest is pinned, and Docker checks it.
	Mirror string
}

var _ Runtime = (*Container)(nil)

// Paths are Caddy's directories in its container.
func (*Container) Paths() Paths {
	return Paths{Data: containerData, Config: containerConfig, Run: containerRun, Boot: containerBoot}
}

// Reference is what to run: Caddy's image, by its digest, from the mirror if
// there is one.
func (c *Container) Reference() string {
	if c.Mirror != "" {
		return strings.TrimSuffix(c.Mirror, "/") + "@" + Digest
	}
	return Image
}

// found is Caddy's container, or nil. One of the name that isn't Dockwell's is
// refused, and left alone.
func (c *Container) found(ctx context.Context) (*docker.Container, error) {
	got, err := c.Client.ContainerNamed(ctx, ingress.ContainerName)
	switch {
	case docker.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	if got.Config.Labels[ownLabel] == "" {
		return nil, fmt.Errorf("a container named %[1]s is on this server, and Dockwell didn't make it, so the ingress can't run. Rename or remove it with docker rename %[1]s or docker rm %[1]s", ingress.ContainerName)
	}
	return got, nil
}

// Status says whether Caddy's container exists and runs.
func (c *Container) Status(ctx context.Context) (ingress.Status, error) {
	got, err := c.found(ctx)
	if err != nil || got == nil {
		return ingress.Status{}, err
	}
	return ingress.Status{
		Exists: true, Running: got.State.Running && !got.State.Restarting,
		Outdated: !strings.HasSuffix(got.Config.Image, "@"+Digest),
		Version:  got.Config.Labels[versionLabel], Pinned: Tag, Image: got.Config.Image,
	}, nil
}

// Pull gets Caddy's image, by its digest, if the server hasn't it. Docker
// checks what it gets against the digest, whatever serves it.
func (c *Container) Pull(ctx context.Context) error {
	ref := c.Reference()
	_, err := c.Client.InspectImage(ctx, ref)
	if !docker.IsNotFound(err) {
		return err
	}
	resource.Progress(ctx, "pulling Caddy")
	err = c.Client.PullImage(ctx, ref, func(p string) { resource.Progress(ctx, "pulling Caddy, "+p) })
	if err != nil && !docker.IsUnavailable(err) && ctx.Err() == nil {
		return fmt.Errorf("can't pull Caddy from %s: %w. If this server can't reach it, set registry-mirrors in /etc/docker/daemon.json, or DOCKWELL_INGRESS_IMAGE to a registry that has it", ref, err)
	}
	return err
}

// Run makes Caddy's container and starts it, replacing any there is.
func (c *Container) Run(ctx context.Context, config string) error {
	if err := c.Pull(ctx); err != nil {
		return err
	}
	ref := c.Reference()
	old, err := c.found(ctx)
	if err != nil {
		return err
	}
	if old != nil {
		resource.Progress(ctx, "stopping the old Caddy")
		if err := c.Client.StopContainer(ctx, old.ID, stopWait); err != nil && !docker.IsNotFound(err) {
			return fmt.Errorf("can't stop the old Caddy: %w", err)
		}
		if err := c.Client.RemoveContainer(ctx, old.ID, true); err != nil && !docker.IsNotFound(err) {
			return fmt.Errorf("can't remove the old Caddy: %w", err)
		}
	}
	id, err := c.Client.CreateContainer(ctx, ingress.ContainerName, c.request(ref, config))
	if err != nil {
		return fmt.Errorf("docker can't create %s: %w", ingress.ContainerName, err)
	}
	resource.Progress(ctx, "starting Caddy")
	if err := c.Client.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("docker can't start %s: %w", ingress.ContainerName, err)
	}
	return nil
}

// request is Caddy's container: on the server's own network, as a user that
// isn't root, with only the capability to bind low ports, and nothing writable
// but its own directories.
func (c *Container) request(ref, config string) *docker.CreateRequest {
	uid, gid, _ := proxyUser()
	// Docker takes an absolute path alone, and --data-dir may not be one.
	dir, err := filepath.Abs(c.Dir)
	if err != nil {
		dir = c.Dir
	}
	bind := func(name, target, options string) string {
		return filepath.Join(dir, name) + ":" + target + ":" + options
	}
	req := &docker.CreateRequest{Config: docker.Config{
		Image:  ref,
		Cmd:    []string{"caddy", "run", "--config", config, "--resume"},
		User:   strconv.Itoa(uid) + ":" + strconv.Itoa(gid),
		Labels: map[string]string{ownLabel: "caddy", versionLabel: Tag},
		Env:    []string{"XDG_DATA_HOME=" + containerData, "XDG_CONFIG_HOME=" + containerConfig, AdminEnv(c.Paths())},
	}}
	req.HostConfig = docker.HostConfig{
		NetworkMode:   "host",
		RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
		Binds: []string{
			bind("data", containerData, "Z"), bind("config", containerConfig, "Z"), bind("run", containerRun, "Z"),
			bind(bootDirName, containerBoot, "ro,Z"),
		},
		CapDrop:        []string{"ALL"},
		CapAdd:         []string{"NET_BIND_SERVICE"},
		ReadonlyRootfs: true,
		Tmpfs:          map[string]string{"/tmp": "rw,noexec,nosuid,size=16m"},
		LogConfig:      &docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
		Memory:         memoryLimit,
		MemorySwap:     memoryLimit,
		// An engine with userns-remap refuses the host network otherwise, and
		// nothing here is any use inside a user namespace.
		UsernsMode: "host",
	}
	return req
}

// Abandon stops a Caddy that failed to start from trying again, and leaves it
// for docker logs to say why.
func (c *Container) Abandon(ctx context.Context) error {
	got, err := c.found(ctx)
	if err != nil || got == nil {
		return err
	}
	if err := c.Client.UpdateRestartPolicy(ctx, got.ID, docker.RestartPolicy{Name: "no"}); err != nil && !docker.IsNotFound(err) {
		return err
	}
	if err := c.Client.StopContainer(ctx, got.ID, time.Second); err != nil && !docker.IsNotFound(err) {
		return err
	}
	return nil
}

// Remove stops Caddy gracefully and removes its container, and its data stays.
func (c *Container) Remove(ctx context.Context) error {
	got, err := c.found(ctx)
	if err != nil || got == nil {
		return err
	}
	if err := c.Client.StopContainer(ctx, got.ID, stopWait); err != nil && !docker.IsNotFound(err) {
		return fmt.Errorf("can't stop Caddy: %w", err)
	}
	if err := c.Client.RemoveContainer(ctx, got.ID, true); err != nil && !docker.IsNotFound(err) {
		return fmt.Errorf("can't remove Caddy: %w", err)
	}
	return nil
}
