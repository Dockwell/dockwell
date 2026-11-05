// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/ingress/caddy"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// runRole runs the role dockwell run takes, until ctx is done.
type runRole func(ctx context.Context, logger *slog.Logger, dir datadir.Dir, build buildinfo.Info) error

// newRunCmd returns dockwell run. Which roles it can take depends on the
// build: runCommand, in run_control.go for the full build and in
// edition_agent.go for the agent build, says how it takes one, and adds
// the flags it needs.
func newRunCmd(stdio IO, g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run Dockwell on this server",
		Args:  noArgs,
	}
	start := runCommand(cmd, stdio, g)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		logger, err := g.logger(stdio.Err)
		if err != nil {
			return err
		}
		dir, err := g.dir(stdio)
		if err != nil {
			return err
		}
		build := buildinfo.Read()
		logger.Info("starting dockwell", "version", build.Version, "build", string(build.Edition),
			"commit", build.ShortCommit(), "data_dir", dir.Root)
		err = start(cmd.Context(), logger, dir, build)
		// Asked to stop while still starting: every write so far was a
		// transaction, so stopping now is clean, not a failure.
		if errors.Is(err, context.Canceled) && cmd.Context().Err() != nil {
			logger.Info("stopped while starting")
			return nil //nolint:nilerr // a requested stop isn't a failure
		}
		return err
	}
	return cmd
}

// runAgent runs this server's agent with its identity and its drivers: on
// its own, on a server that joined a control plane, or beside the control
// plane, on the control server. Both builds run it the same way.
//
// panel is the panel on this server, when the control plane beside this agent
// has a domain; nil on a server that joined.
func runAgent(ctx context.Context, stdio IO, logger *slog.Logger, dir datadir.Dir, id *enroll.Identity, build buildinfo.Info, panel *ingress.Panel) error {
	drivers, collector, routes := agentDrivers(ctx, stdio, logger, dir, panel)
	return agent.Run(ctx, agent.Config{
		Logger: logger, Dir: dir.Agent(), Identity: id, Build: build, Drivers: drivers, Collector: collector, Domains: routes,
	})
}

// ingressEvery is how often the agent puts the server's ingress as its routes
// need it, which starts a stopped proxy within this long.
const ingressEvery = time.Minute

// agentDrivers are the drivers this server's agent runs: containers,
// services, volumes, networks and routes on the Docker Engine, at the socket
// DOCKER_HOST names, as the docker CLI finds it. The agent offers them
// while Docker answers, and keeps pruning the volumes deleted 7 days ago,
// and its ingress as its routes need it, until ctx is done. The collector
// reads the same Docker's version.
func agentDrivers(ctx context.Context, stdio IO, logger *slog.Logger, dir datadir.Dir, panel *ingress.Panel) (*resource.Registry, *collect.Collector, *ingress.Manager) {
	collector := collect.New()
	socket, err := docker.SocketFrom(stdio.Getenv)
	client := docker.Unusable(err)
	if err == nil {
		client = docker.New(socket)
	}
	collector.DockerSocket = socket
	kept := &volume.Kept{Path: filepath.Join(dir.Agent(), "deleted-volumes.json")}
	// What no container may mount, as this server has it: its data
	// directory, Docker's, and where symlinks lead.
	paths := &container.Paths{Client: client, DataDir: dir.Root}
	proxy := &caddy.Container{Client: client, Dir: dir.Ingress(), Mirror: stdio.Getenv(envIngressImage)}
	routes := &ingress.Manager{
		Client: client, Dir: dir.Ingress(), Panel: panel, Logger: logger, KeeperImage: proxy.Reference(),
		Proxy: &caddy.Caddy{
			Dir: dir.Ingress(), Runtime: proxy,
			ACMEDirectory: stdio.Getenv(envACMEDirectory), ACMERoots: stdio.Getenv(envACMERoots),
		},
	}
	containers := container.Driver{Runtime: &container.Docker{Client: client, Deleted: kept, Logger: logger, Paths: paths, Ingress: routes}}
	volumes := &volume.Docker{Client: client, Kept: kept, Paths: paths}
	go volumes.KeepPruned(ctx, logger)
	go routes.KeepReconciled(ctx, ingressEvery)
	go routes.Watch(ctx)
	var reserved string
	if panel != nil {
		reserved = panel.Domain
	}
	return resource.NewRegistry(&containers, &service.Driver{Containers: containers},
		&volume.Driver{Runtime: volumes}, &network.Driver{Client: client}, &route.Driver{Ingress: routes, Reserved: reserved}), collector, routes
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
