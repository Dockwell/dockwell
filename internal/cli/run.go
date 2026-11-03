// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/serve"
	"github.com/dockwell/dockwell/internal/store"
)

type runFlags struct {
	listen      string
	agentListen string
	advertise   string
}

func newRunCmd(stdio IO, g *globalFlags) *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run Dockwell on this server",
		Long: "Runs the roles this server has until it is stopped with SIGINT or SIGTERM.\n\n" +
			"The first server runs the control plane, serving the API and panel on " + control.DefaultListen +
			" and listening for other servers' agents on " + control.DefaultAgentListen + ", and an agent of its own. " +
			"A server added with dockwell join runs only the agent.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger, err := g.logger(stdio.Err)
			if err != nil {
				return err
			}
			dir, err := g.dir(stdio)
			if err != nil {
				return err
			}
			build := buildinfo.Read()
			logger.Info("starting dockwell", "version", build.Version, "commit", build.ShortCommit(), "data_dir", dir.Root)

			id, err := enroll.Load(dir.Agent())
			hasControl := exists(dir.StateDB())
			switch {
			case errors.Is(err, enroll.ErrNotJoined) || hasControl:
				err = runControl(cmd.Context(), logger, dir, build, f)
			case err != nil:
				return err
			default:
				err = agent.Run(cmd.Context(), agent.Config{Logger: logger, Dir: dir.Agent(), Identity: id})
			}
			// Asked to stop while still starting: every write so far was a
			// transaction, so stopping now is clean, not a failure.
			if errors.Is(err, context.Canceled) && cmd.Context().Err() != nil {
				logger.Info("stopped while starting")
				return nil //nolint:nilerr // a requested stop isn't a failure
			}
			return err
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.listen, "listen", envOr(stdio, envListen, control.DefaultListen),
		"address for the API and panel, as host:port (env "+envListen+")")
	fl.StringVar(&f.agentListen, "agent-listen", envOr(stdio, envAgentListen, control.DefaultAgentListen),
		"address for other servers' agents, as host:port (env "+envAgentListen+")")
	fl.StringVar(&f.advertise, "advertise-url", envOr(stdio, envAdvertise, ""),
		"the agent listener's URL as other servers reach it, put in join tickets; defaults to this server's address (env "+envAdvertise+")")
	return cmd
}

// runControl runs the control plane and this server's own agent.
func runControl(ctx context.Context, logger *slog.Logger, dir datadir.Dir, build buildinfo.Info, f runFlags) error {
	if err := datadir.Ensure(dir.Control()); err != nil {
		return err
	}
	key, err := seal.LoadOrCreate(dir.SealKey())
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, dir.StateDB())
	if err != nil {
		return err
	}
	defer db.Close()
	auth, err := identity.Bootstrap(ctx, db, key, logger)
	if err != nil {
		return err
	}

	apiLn, err := serve.Listen(ctx, f.listen)
	if err != nil {
		return withListenHint(err, "--listen")
	}
	agentLn, err := serve.Listen(ctx, f.agentListen)
	if err != nil {
		_ = apiLn.Close()
		return withListenHint(err, "--agent-listen")
	}

	agentURL, err := advertiseURL(ctx, f.advertise, agentLn.Addr())
	if err != nil {
		_, _ = apiLn.Close(), agentLn.Close()
		return err
	}
	if err := auth.SetAgentURL(ctx, agentURL); err != nil {
		_, _ = apiLn.Close(), agentLn.Close()
		return err
	}
	logger.Info("other servers join through the agent listener",
		slog.String("url", agentURL), slog.String("install", auth.InstallID()), slog.String("ca_pin", auth.CA().Pin()))

	srv, err := control.New(control.Config{
		Logger:     logger,
		Build:      build,
		Identity:   auth,
		AgentHosts: agentHosts(agentURL),
	})
	if err != nil {
		_, _ = apiLn.Close(), agentLn.Close()
		return err
	}

	own, err := localIdentity(ctx, auth, dir, agentLn.Addr())
	if err != nil {
		_, _ = apiLn.Close(), agentLn.Close()
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var controlErr, agentErr error
	wg.Go(func() {
		controlErr = srv.Run(ctx, apiLn, agentLn)
		cancel()
	})
	wg.Go(func() {
		agentErr = agent.Run(ctx, agent.Config{Logger: logger, Dir: dir.Agent(), Identity: own})
		cancel()
	})
	wg.Wait()
	return errors.Join(controlErr, agentErr)
}

// localIdentity loads the control server's own agent identity, enrolling it
// directly with the authority the first time: it shares the machine, and
// the data directory, with the CA, so it needs no ticket.
func localIdentity(ctx context.Context, auth *identity.Authority, dir datadir.Dir, agentAddr net.Addr) (*enroll.Identity, error) {
	id, err := enroll.Load(dir.Agent())
	if !errors.Is(err, enroll.ErrNotJoined) {
		return id, err
	}
	key, csr, err := pki.NewServerKey()
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	joined, err := auth.EnrollLocal(ctx, csr, hostname)
	if err != nil {
		return nil, err
	}
	id = &enroll.Identity{
		ServerID:   joined.Server.ID,
		ServerName: joined.Server.Name,
		ControlURL: loopbackURL(agentAddr),
		Key:        key,
		Cert:       joined.Cert,
		CA:         auth.CA().Certificate(),
	}
	if err := id.Save(dir.Agent()); err != nil {
		return nil, err
	}
	return id, nil
}

// loopbackURL is how the control server's own agent reaches the agent listener.
func loopbackURL(addr net.Addr) string {
	port := "7443"
	if tcp, ok := addr.(*net.TCPAddr); ok {
		port = strconv.Itoa(tcp.Port)
	}
	return "https://" + net.JoinHostPort("127.0.0.1", port)
}

// advertiseURL is where other servers reach the agent listener: the given
// URL, or this server's outbound address and the listener's port.
func advertiseURL(ctx context.Context, given string, addr net.Addr) (string, error) {
	if given != "" {
		u, err := url.Parse(given)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return "", usageError{fmt.Errorf("--advertise-url %q must be an https URL, such as https://203.0.113.7:7443", given)}
		}
		return given, nil
	}
	tcp, _ := addr.(*net.TCPAddr)
	host := ""
	if tcp != nil && !tcp.IP.IsUnspecified() {
		host = tcp.IP.String()
	} else {
		host = outboundIP(ctx)
	}
	port := 7443
	if tcp != nil {
		port = tcp.Port
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// outboundIP is the address this server uses to reach the internet. A UDP
// "connection" sends nothing; it only picks the route.
func outboundIP(ctx context.Context) string {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", "192.0.2.1:9")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	if udp, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return udp.IP.String()
	}
	return "127.0.0.1"
}

// agentHosts are the names the agent listener's certificate covers: the
// advertised host, loopback, and this server's hostname.
func agentHosts(agentURL string) []string {
	hosts := []string{"127.0.0.1", "::1", "localhost"}
	if u, err := url.Parse(agentURL); err == nil && u.Hostname() != "" {
		hosts = append(hosts, u.Hostname())
	}
	if h, err := os.Hostname(); err == nil {
		hosts = append(hosts, h)
	}
	return hosts
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// withListenHint adds the fix to listen errors an operator can resolve.
func withListenHint(err error, flag string) error {
	var addrErr *net.AddrError
	var dnsErr *net.DNSError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EADDRINUSE):
		return fmt.Errorf("%w. Stop the process using that address, or pass a different %s address", err, flag)
	case errors.Is(err, syscall.EACCES):
		return fmt.Errorf("%w. Ports below 1024 need root or the CAP_NET_BIND_SERVICE capability; pass a higher port with %s", err, flag)
	case errors.As(err, &addrErr), errors.As(err, &dnsErr):
		return fmt.Errorf("%w. Pass %s as host:port, such as %s", err, flag, control.DefaultListen)
	default:
		return err
	}
}
