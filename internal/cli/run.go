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
	"time"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/https"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/serve"
	"github.com/dockwell/dockwell/internal/store"
)

type runFlags struct {
	listen string
	// listenFromEnv is true when DOCKWELL_LISTEN set listen, not the flag.
	listenFromEnv bool
	agentListen   string
	advertise     string
	domain        string
	httpsListen   string
	httpListen    string
	acmeDirector  string
}

func newRunCmd(stdio IO, g *globalFlags) *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run Dockwell on this server",
		Long: "Runs the roles this server has until it is stopped with SIGINT or SIGTERM.\n\n" +
			"The first server runs the control plane, serving the API and panel on " + control.DefaultListen +
			" and listening for other servers' agents on " + control.DefaultAgentListen + ", and an agent of its own. " +
			"A server added with dockwell join runs only the agent.\n\n" +
			"The panel stays on loopback, reached through an SSH tunnel, until you give it a domain with --domain. " +
			"It is then also served over HTTPS on " + defaultHTTPSListen + ", with a certificate from Let's Encrypt, " +
			"and " + defaultHTTPListen + " redirects to it.\n\n" +
			"Until the install has an owner, dockwell run prints a one-time link that claims it.",
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

			role, id, err := decideRole(dir)
			if err != nil {
				return err
			}
			f.listenFromEnv = !cmd.Flags().Changed("listen") && stdio.Getenv(envListen) != ""
			if role == roleControl {
				err = runControl(cmd.Context(), stdio, logger, dir, build, f)
			} else {
				err = agent.Run(cmd.Context(), agent.Config{Logger: logger, Dir: dir.Agent(), Identity: id, Build: build})
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
		"loopback address for the API and panel, as host:port; to reach the panel from elsewhere, add --domain (env "+envListen+")")
	fl.StringVar(&f.agentListen, "agent-listen", envOr(stdio, envAgentListen, control.DefaultAgentListen),
		"address for other servers' agents, as host:port (env "+envAgentListen+")")
	fl.StringVar(&f.advertise, "advertise-url", envOr(stdio, envAdvertise, ""),
		"the agent listener's URL as other servers reach it, put in join tickets; defaults to this server's address (env "+envAdvertise+")")
	fl.StringVar(&f.domain, "domain", envOr(stdio, envDomain, ""),
		"serve the panel over HTTPS on this hostname, such as panel.example.com, with a certificate from Let's Encrypt; point its DNS A record here first (env "+envDomain+")")
	fl.StringVar(&f.httpsListen, "https-listen", envOr(stdio, envHTTPSListen, defaultHTTPSListen),
		"address for the panel over HTTPS when it has a --domain (env "+envHTTPSListen+")")
	fl.StringVar(&f.httpListen, "http-listen", envOr(stdio, envHTTPListen, defaultHTTPListen),
		"address that answers certificate challenges and redirects to HTTPS when the panel has a --domain (env "+envHTTPListen+")")
	fl.StringVar(&f.acmeDirector, "acme-directory-url", envOr(stdio, envACMEDirectory, ""),
		"the ACME CA's directory, such as Let's Encrypt's staging one; defaults to Let's Encrypt (env "+envACMEDirectory+")")
	return cmd
}

// Where the panel listens when it has a domain.
const (
	defaultHTTPSListen = ":443"
	defaultHTTPListen  = ":80"
)

// runControl runs the control plane and this server's own agent.
func runControl(ctx context.Context, stdio IO, logger *slog.Logger, dir datadir.Dir, build buildinfo.Info, f runFlags) error {
	if f.domain != "" {
		if err := auth.ValidDomain(f.domain); err != nil {
			return usageError{fmt.Errorf("--domain %w", err)}
		}
	}
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
	authority, err := identity.Bootstrap(ctx, db, key, logger)
	if err != nil {
		return err
	}

	var ls control.Listeners
	closeAll := func() {
		for _, ln := range []net.Listener{ls.API, ls.HTTPS, ls.HTTP, ls.Agents} {
			if ln != nil {
				_ = ln.Close()
			}
		}
	}
	if ls.API, err = serve.Listen(ctx, f.listen); err != nil {
		return withListenHint(err, "--listen")
	}
	if !onLoopback(ls.API.Addr()) {
		closeAll()
		return usageError{beyondLoopback(f)}
	}
	if ls.Agents, err = serve.Listen(ctx, f.agentListen); err != nil {
		closeAll()
		return withListenHint(err, "--agent-listen")
	}
	if f.domain != "" {
		if ls.HTTPS, err = serve.Listen(ctx, f.httpsListen); err != nil {
			closeAll()
			return withListenHint(err, "--https-listen")
		}
		if ls.HTTP, err = serve.Listen(ctx, f.httpListen); err != nil {
			closeAll()
			return withListenHint(err, "--http-listen")
		}
	}

	agentURL, err := advertiseURL(ctx, f.advertise, ls.Agents.Addr())
	if err != nil {
		closeAll()
		return err
	}
	if err := authority.SetAgentURL(ctx, agentURL); err != nil {
		closeAll()
		return err
	}
	logger.Info("other servers join through the agent listener",
		slog.String("url", agentURL), slog.String("install", authority.InstallID()), slog.String("ca_pin", authority.CA().Pin()))

	own, err := localIdentity(ctx, authority, dir, ls.Agents.Addr())
	if err != nil {
		closeAll()
		return err
	}
	hub, err := fleet.New(ctx, db, logger)
	if err != nil {
		closeAll()
		return err
	}
	hub.SetControlServer(own.ServerID)

	people, err := auth.New(auth.Config{
		DB:     db,
		Key:    key,
		Logger: logger,
		Sites:  auth.Sites{Domain: f.domain},
		ServerName: func() string {
			if s := hub.ControlServer(); s != nil {
				return s.GetName()
			}
			return ""
		},
	})
	if err != nil {
		closeAll()
		return err
	}
	var public https.Source
	if ls.HTTPS != nil {
		if public, err = acmeSource(f, dir, ls.HTTPS.Addr()); err != nil {
			closeAll()
			return err
		}
	}

	addr := panelAddress{Domain: f.domain, Host: ipOf(ls.API.Addr()), Port: portOf(ls.API.Addr()), PublicHost: hostOf(agentURL)}
	if ls.HTTPS != nil {
		addr.HTTPSPort = portOf(ls.HTTPS.Addr())
	}
	if err := announce(ctx, stdio, logger, db, people, addr); err != nil {
		closeAll()
		return err
	}

	srv, err := control.New(control.Config{
		Logger:        logger,
		Build:         build,
		Identity:      authority,
		Fleet:         hub,
		AgentHosts:    agentHosts(agentURL),
		Auth:          people,
		Public:        public,
		PublicAddress: addr.dnsAddress(),
	})
	if err != nil {
		closeAll()
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var controlErr, agentErr error
	wg.Go(func() {
		controlErr = srv.Run(ctx, ls)
		cancel()
	})
	wg.Go(func() {
		agentErr = agent.Run(ctx, agent.Config{Logger: logger, Dir: dir.Agent(), Identity: own, Build: build})
		cancel()
	})
	wg.Wait()
	return errors.Join(controlErr, agentErr)
}

// beyondLoopback explains why a --listen address beyond loopback is
// refused, and how to fix it as it was set: by the flag or DOCKWELL_LISTEN.
// A domain doesn't lift the rule: it adds an HTTPS listener, and the panel's
// own listener stays on loopback.
func beyondLoopback(f runFlags) error {
	setting, given, domain := "--listen", "--listen "+f.listen, "--domain"
	if f.listenFromEnv {
		setting, given, domain = envListen, envListen+"="+f.listen, envDomain
	}
	return fmt.Errorf("%[1]s would serve the panel over plain HTTP beyond this server, so Dockwell refuses it. "+
		"Remove %[2]s to keep the panel on %[3]s, or set it to another loopback address, such as 127.0.0.1:7700, and reach the panel through an SSH tunnel. "+
		"To reach the panel from elsewhere, keep %[2]s on loopback and add %[4]s, which serves it over HTTPS",
		given, setting, control.DefaultListen, domain)
}

// acmeSource gets the panel's certificates from an ACME CA. Plain HTTP
// redirects to the port the HTTPS listener is bound to, which --https-listen
// may not spell as a number, as in :https or :0.
func acmeSource(f runFlags, dir datadir.Dir, httpsAddr net.Addr) (*https.ACME, error) {
	return https.NewACME(https.ACMEConfig{
		Domain:       f.domain,
		CacheDir:     dir.ACME(),
		DirectoryURL: f.acmeDirector,
		HTTPSPort:    portOf(httpsAddr),
	})
}

func onLoopback(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

func portOf(addr net.Addr) string {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return strconv.Itoa(tcp.Port)
	}
	return "7600"
}

// ipOf is the address a listener is bound to.
func ipOf(addr net.Addr) string {
	if tcp, ok := addr.(*net.TCPAddr); ok && tcp.IP != nil {
		return tcp.IP.String()
	}
	return ""
}

func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Hostname()
	}
	return ""
}

// localIdentity loads the control server's own agent identity, enrolling it
// directly with the authority the first time: it shares the machine, and
// the data directory, with the CA, so it needs no ticket. On every start it
// points the agent at the listener's current address, and renews directly
// if the certificate is due, which also rescues one that expired while the
// control server was off.
func localIdentity(ctx context.Context, authority *identity.Authority, dir datadir.Dir, agentAddr net.Addr) (*enroll.Identity, error) {
	controlURL := loopbackURL(agentAddr)
	id, err := enroll.Load(dir.Agent())
	switch {
	case errors.Is(err, enroll.ErrNotJoined):
		key, csr, err := pki.NewServerKey()
		if err != nil {
			return nil, err
		}
		hostname, _ := os.Hostname()
		joined, err := authority.EnrollLocal(ctx, csr, hostname)
		if err != nil {
			return nil, err
		}
		id = enroll.New(joined.Server.ID, joined.Server.Name, controlURL, key, joined.Cert, authority.CA().Certificate())
		if err := id.Save(dir.Agent()); err != nil {
			return nil, err
		}
		return id, nil
	case err != nil:
		return nil, err
	}

	if id.ControlURL != controlURL {
		id.ControlURL = controlURL
		if err := id.Save(dir.Agent()); err != nil {
			return nil, err
		}
	}
	if id.NeedsRenewal(time.Now()) {
		next, err := id.NextKey()
		if err != nil {
			return nil, err
		}
		csr, err := pki.CSR(next)
		if err != nil {
			return nil, err
		}
		cert, err := authority.Renew(ctx, id.ServerID, pki.SerialString(id.Cert()), csr)
		if err != nil {
			return nil, fmt.Errorf("can't renew this server's own certificate: %w", err)
		}
		id.Replace(next, cert)
		if err := id.Save(dir.Agent()); err != nil {
			return nil, err
		}
	}
	return id, nil
}

// loopbackURL is how the control server's own agent reaches the agent
// listener: the address it is bound to, or loopback when it listens on all
// addresses.
func loopbackURL(addr net.Addr) string {
	host, port := "127.0.0.1", "7443"
	if tcp, ok := addr.(*net.TCPAddr); ok {
		port = strconv.Itoa(tcp.Port)
		switch {
		case tcp.IP == nil, tcp.IP.IsUnspecified():
		default:
			host = tcp.IP.String()
		}
	}
	return "https://" + net.JoinHostPort(host, port)
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
	example := control.DefaultListen
	switch flag {
	case "--agent-listen":
		example = control.DefaultAgentListen
	case "--https-listen":
		example = defaultHTTPSListen
	case "--http-listen":
		example = defaultHTTPListen
	}
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
		return fmt.Errorf("%w. Pass %s as host:port, such as %s", err, flag, example)
	default:
		return err
	}
}
