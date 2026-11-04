// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
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
func runAgent(ctx context.Context, stdio IO, logger *slog.Logger, dir datadir.Dir, id *enroll.Identity, build buildinfo.Info) error {
	drivers, collector := agentDrivers(ctx, stdio, logger, dir)
	return agent.Run(ctx, agent.Config{
		Logger: logger, Dir: dir.Agent(), Identity: id, Build: build, Drivers: drivers, Collector: collector,
	})
}

// agentDrivers are the drivers this server's agent runs: containers,
// services, volumes and networks on the Docker Engine, at the socket
// DOCKER_HOST names, as the docker CLI finds it. The agent offers them
// while Docker answers, and keeps pruning the volumes deleted 7 days ago
// until ctx is done. The collector reads the same Docker's version.
func agentDrivers(ctx context.Context, stdio IO, logger *slog.Logger, dir datadir.Dir) (*resource.Registry, *collect.Collector) {
	collector := collect.New()
	socket, err := docker.SocketFrom(stdio.Getenv)
	client := docker.Unusable(err)
	if err == nil {
		client = docker.New(socket)
	}
	collector.DockerSocket = socket
	kept := &volume.Kept{Path: filepath.Join(dir.Agent(), "deleted-volumes.json")}
	containers := container.Driver{Runtime: &container.Docker{Client: client, Deleted: kept, Logger: logger}}
	volumes := &volume.Docker{Client: client, Kept: kept}
	go volumes.KeepPruned(ctx, logger)
	return resource.NewRegistry(&containers, &service.Driver{Containers: containers},
		&volume.Driver{Runtime: volumes}, &network.Driver{Client: client}), collector
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
