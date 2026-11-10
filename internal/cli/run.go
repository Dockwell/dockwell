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

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/access"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/logs"
	"github.com/dockwell/dockwell/internal/agent/nftables"
	"github.com/dockwell/dockwell/internal/agent/onepanel"
	"github.com/dockwell/dockwell/internal/agent/terminal"
	"github.com/dockwell/dockwell/internal/agent/updates"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/health"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/ingress/caddy"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/task"
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
	cmd.Flags().BoolVar(&g.noStun, "no-stun", stunOff(stdio),
		"never ask a public STUN server how this server reaches the internet ("+envNoStun+")")
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
// has a domain; nil on a server that joined. ownPorts are the ports the control
// plane beside it listens on, which no route may send to; a server that joined
// has none, as its agent opens no ports.
func runAgent(ctx context.Context, stdio IO, g *globalFlags, logger *slog.Logger, dir datadir.Dir, id *enroll.Identity, build buildinfo.Info, panel *ingress.Panel, ownPorts []int) error {
	cfg := agentDrivers(ctx, stdio, logger, dir, panel, ownPorts)
	cfg.Logger, cfg.Dir, cfg.Identity, cfg.Build, cfg.Files = logger, dir.Agent(), id, build, serverFiles(stdio, logger, dir)
	cfg.Updates = serverUpdates(stdio, logger, dir.Agent())
	cfg.OnePanel = serverOnePanel(stdio)
	cfg.Terminals = terminal.New(terminal.Options{
		Logger: logger, Docker: cfg.Docker, AsGoodAsRoot: agent.ContainerAsGoodAsRoot(cfg.Drivers, cfg.Docker, (&container.Paths{Client: cfg.Docker, DataDir: dir.Root}).Follow, agent.ProcMounts),
	})
	lookups(&cfg, stdio, g)
	err := agent.Run(ctx, cfg)
	if cfg.Tasks != nil && ctx.Err() != nil {
		// Stopping stops the tasks' runs too; what each came to is kept before the
		// agent goes, and one that outlasts this is ended when it starts again.
		cfg.Tasks.Drain(task.DrainWait)
	}
	return err
}

// hostTools runs the server's own tools, such as ufw and nft. A test runs none
// of the machine's, only stand-ins from the folder DOCKWELL_TEST_TOOLS names,
// laid out as the paths they stand in for, such as <folder>/usr/sbin/nft.
func hostTools(stdio IO) *hosttool.Runner {
	if !isSet(stdio.Getenv(envTest)) {
		return &hosttool.Runner{}
	}
	if dir := stdio.Getenv(envTestTools); dir != "" {
		return &hosttool.Runner{Root: dir}
	}
	return &hosttool.Runner{Off: true}
}

// lookups says how this server's agent finds out how the internet sees it: what the
// flag or the variable says of STUN, where metadata is, and that a route out through a
// TUN is no way of saying where the server is, as a Tailscale exit node's isn't.
func lookups(cfg *agent.Config, stdio IO, g *globalFlags) {
	cfg.NoLookup = g.noStun
	if isSet(stdio.Getenv(envTest)) {
		cfg.NoLookup, cfg.Collector.Metadata = true, collect.ClosedMetadata
		cfg.Probe = agent.LoopbackProbe
	}
	cfg.Collector.Outbound = func(ctx context.Context) string {
		return ingress.OwnAddress(collect.OutboundAddress(ctx))
	}
}

// stunOff reports DOCKWELL_NO_STUN set to turn the lookup off.
func stunOff(stdio IO) bool {
	return isSet(stdio.Getenv(envNoStun))
}

// isSet reports a variable set to turn something on.
func isSet(v string) bool {
	return v != "" && v != "0" && v != "false"
}

// serverFiles reads this server's disk for a person. What it refuses takes this
// data directory, and wherever Docker says it keeps its own.
func serverFiles(stdio IO, logger *slog.Logger, dir datadir.Dir) *files.Server {
	socket, err := docker.SocketFrom(stdio.Getenv)
	client := docker.Unusable(err)
	if err == nil {
		client = docker.New(socket)
	}
	return files.New(dir.Root, logger, func(ctx context.Context) (string, error) {
		info, err := client.Info(ctx)
		if err != nil {
			return "", err
		}
		return info.DockerRootDir, nil
	})
}

// serverUpdates reads which package updates wait on this server (ADR 0043). A
// test reads nothing of the machine it runs on: with DOCKWELL_TEST set, only the
// stand-ins under DOCKWELL_TEST_PACKAGES, and none without it.
func serverUpdates(stdio IO, logger *slog.Logger, stateDir string) *updates.Reader {
	o := updates.Options{Root: "/", Logger: logger, StateDir: stateDir}
	if isSet(stdio.Getenv(envTest)) {
		o.Root = stdio.Getenv(envTestPackages)
		if o.Root == "" {
			o.Unavailable = "This is a test, so Dockwell reads no package updates here. Set " + envTestPackages + " to a folder of stand-ins"
		}
	}
	if socket, err := docker.SocketFrom(stdio.Getenv); err == nil {
		client := docker.New(socket)
		o.LiveRestore = func(ctx context.Context) bool {
			info, err := client.Info(ctx)
			return err == nil && info.LiveRestoreEnabled
		}
	}
	return updates.New(o)
}

// serverOnePanel reads what 1Panel keeps on this server (ADR 0056). A test reads
// nothing of the machine it runs on: with DOCKWELL_TEST set, only what's under
// DOCKWELL_TEST_ONEPANEL, and nothing without it.
func serverOnePanel(stdio IO) func(context.Context) (*agentv1.ReadOnePanelResult, error) {
	root := "/"
	if isSet(stdio.Getenv(envTest)) {
		root = stdio.Getenv(envTestOnePanel)
	}
	return func(ctx context.Context) (*agentv1.ReadOnePanelResult, error) {
		if root == "" {
			return &agentv1.ReadOnePanelResult{NotHere: "this is a test, so Dockwell reads no 1Panel here. Set " + envTestOnePanel + " to a folder laid out as a server's root"}, nil
		}
		return onepanel.Read(ctx, root)
	}
}

// ingressEvery is how often the agent puts the server's ingress as its routes
// need it, which starts a stopped proxy within this long.
const ingressEvery = time.Minute

// agentDrivers are the drivers this server's agent runs: containers,
// services, volumes, networks and routes on the Docker Engine, at the socket
// DOCKER_HOST names, as the docker CLI finds it. The agent offers them
// while Docker answers, and keeps pruning the volumes deleted 7 days ago,
// its ingress as its routes need it, and watching how its services are, until
// ctx is done. The collector reads the same Docker's version, and the log reader
// the same Docker's logs, with what the server kept of the containers a change
// removed.
func agentDrivers(ctx context.Context, stdio IO, logger *slog.Logger, dir datadir.Dir, panel *ingress.Panel, ownPorts []int) agent.Config {
	collector := collect.New()
	socket, err := docker.SocketFrom(stdio.Getenv)
	client := docker.Unusable(err)
	if err == nil {
		client = docker.New(socket)
	}
	client.Adopted = &docker.Adopted{Path: filepath.Join(dir.Agent(), "adopted-containers.json")}
	collector.DockerSocket = socket
	kept := &volume.Kept{Path: filepath.Join(dir.Agent(), "deleted-volumes.json")}
	// What no container may mount, as this server has it: its data
	// directory, Docker's, and where symlinks lead.
	paths := &container.Paths{Client: client, DataDir: dir.Root}
	proxy := &caddy.Container{Client: client, Dir: dir.Ingress(), Mirror: stdio.Getenv(envIngressImage)}
	routes := &ingress.Manager{
		Client: client, Dir: dir.Ingress(), Panel: panel, OwnPorts: ownPorts, Logger: logger, KeeperImage: proxy.Reference(),
		Proxy: &caddy.Caddy{
			Dir: dir.Ingress(), Runtime: proxy,
			ACMEDirectory: stdio.Getenv(envACMEDirectory), ACMERoots: stdio.Getenv(envACMERoots),
		},
	}
	archive := &logs.Archive{Dir: filepath.Join(dir.Agent(), "kept-logs"), Client: client, Logger: logger}
	containers := container.Driver{Runtime: &container.Docker{Client: client, Deleted: kept, Logger: logger, Paths: paths, Folders: container.HostFolders{}, Ingress: routes, Logs: archive}}
	volumes := &volume.Docker{Client: client, Kept: kept, Paths: paths}
	go volumes.KeepPruned(ctx, logger)
	backups := &backup.Runner{Client: client, Root: filepath.Join(dir.Agent(), "backups"), Paths: paths, Logger: logger}
	go backups.Keep(ctx)
	go archive.KeepPruned(ctx)
	go routes.KeepReconciled(ctx, ingressEvery)
	go routes.Watch(ctx)
	monitor := &health.Monitor{Client: client, Logger: logger}
	go monitor.Run(ctx)
	tasks := &task.Driver{Dir: dir.Agent(), Client: client, Logger: logger}
	go tasks.Keep(ctx)
	var reserved string
	if panel != nil {
		reserved = panel.Domain
	}
	tools := hostTools(stdio)
	firewalls := firewallAccess(ctx, logger, dir, tools, dockerOrNot(client, socket, err), ownPorts, panel == nil && len(ownPorts) > 0)
	return agent.Config{
		Drivers: resource.NewRegistry(&containers, &service.Driver{Containers: containers},
			&volume.Driver{Runtime: volumes, Restores: backups, BacksUp: backups}, &network.Driver{Client: client}, &route.Driver{Ingress: routes, Reserved: reserved},
			&database.UserDriver{Client: client, Dir: filepath.Join(dir.Agent(), "databases")}, tasks,
			&firewall.Driver{Backend: firewalls}, &firewall.PolicyDriver{Backend: firewalls}),
		Collector: collector, Domains: routes, Public: routes, Logs: &logs.Reader{Client: client, Archive: archive, Logger: logger}, Docker: client, Health: monitor,
		Backups: backup.Calls{Runner: backups}, Databases: database.Reader{Client: client}, Tasks: tasks, Tools: tools, Access: firewalls,
	}
}

// firewallAccess keeps who reaches the server (ADR 0052). Once the server can
// hold Dockwell's table, as it can at start on most, it puts back at once a
// change that waited for its confirmation when the agent stopped, and loads
// what was confirmed, before the agent connects anywhere. Until it can, the
// agent offers no firewall kinds.
// dockerOrNot reads Docker's networks for the firewall, or nothing where the
// agent can't reach any Docker, as a server without Docker has none.
func dockerOrNot(client *docker.Client, socket string, unusable error) func(context.Context) (access.DockerNetworks, error) {
	if unusable != nil {
		return nil
	}
	return access.DockerReader(client, socket)
}

func firewallAccess(ctx context.Context, logger *slog.Logger, dir datadir.Dir, tools *hosttool.Runner, networks func(context.Context) (access.DockerNetworks, error), ownPorts []int, panelOverSSH bool) *access.Manager {
	own := make([]uint16, 0, len(ownPorts))
	for _, p := range ownPorts {
		if p > 0 && p < 65536 {
			own = append(own, uint16(p))
		}
	}
	pins := &access.PinReader{Proc: os.DirFS("/proc"), Tools: tools, Own: own}
	m := &access.Manager{
		Dir: filepath.Join(dir.Agent(), "access"), Table: &nftables.Firewall{Run: tools}, Logger: logger,
		Pins: pins.Read, Docker: networks,
		PanelOverSSH: panelOverSSH,
	}
	if err := m.Ready(ctx); err != nil {
		logger.InfoContext(ctx, "the agent offers no firewall kinds until this server can hold Dockwell's firewall", slog.String("reason", err.Error()))
	}
	go m.KeepCurrent(ctx)
	return m
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
