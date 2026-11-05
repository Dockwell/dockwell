// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/control"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	controlhealth "github.com/dockwell/dockwell/internal/control/health"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/https"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/ingress/caddy"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/serve"
	"github.com/dockwell/dockwell/internal/store"
)

type runFlags struct {
	// database is where the control plane keeps its state: a SQLite file or
	// a postgres:// URL.
	database string
	listen   string
	// listenFromEnv is true when DOCKWELL_LISTEN set listen, not the flag.
	listenFromEnv bool
	agentListen   string
	advertise     string
	domain        string
	httpsListen   string
	httpListen    string
	acmeDirector  string
}

// runCommand makes dockwell run take the role the data directory gives
// this server: the control plane with its own agent, or only the agent, of
// the control plane it joined. It adds the control plane's flags to cmd.
func runCommand(cmd *cobra.Command, stdio IO, g *globalFlags) runRole {
	var f runFlags
	cmd.Long = "Runs the roles this server has until it is stopped with SIGINT or SIGTERM.\n\n" +
		"The first server runs the control plane, serving the API and panel on " + control.DefaultListen +
		" and listening for other servers' agents on " + control.DefaultAgentListen + ", and an agent of its own. " +
		"A server added with dockwell join runs only the agent.\n\n" +
		"The panel stays on loopback, reached through an SSH tunnel, until you give it a domain with --domain. " +
		"It is then also served over HTTPS on " + defaultHTTPSListen + ", with a certificate from Let's Encrypt, " +
		"and " + defaultHTTPListen + " redirects to it.\n\n" +
		"Until the install has an owner, dockwell run prints a one-time link that claims it."
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

	return func(ctx context.Context, logger *slog.Logger, dir datadir.Dir, build buildinfo.Info) error {
		role, id, err := decideRole(dir)
		if err != nil {
			return err
		}
		if role == roleAgent {
			return runAgent(ctx, stdio, logger, dir, id, build, nil, nil)
		}
		f.listenFromEnv = !cmd.Flags().Changed("listen") && stdio.Getenv(envListen) != ""
		if f.database, err = g.databaseOf(stdio, dir); err != nil {
			return err
		}
		return runControl(ctx, stdio, logger, dir, build, f)
	}
}

// Where the panel listens when it has a domain.
const (
	defaultHTTPSListen = ":443"
	defaultHTTPListen  = ":80"
)

// runControl runs the control plane and this server's own agent.
func runControl(ctx context.Context, stdio IO, logger *slog.Logger, dir datadir.Dir, build buildinfo.Info, f runFlags) error {
	compose.UseGuard()
	if f.domain != "" {
		if err := auth.ValidDomain(f.domain); err != nil {
			return usageError{fmt.Errorf("--domain %w", err)}
		}
	}
	if err := datadir.Ensure(dir.Control()); err != nil {
		return err
	}
	db, authority, key, err := openState(ctx, logger, dir, f.database)
	if err != nil {
		return err
	}
	defer db.Close()

	if f.domain != "" {
		if err := refuseClaimedDomain(ctx, db, f.domain); err != nil {
			return err
		}
	}

	var ls control.Listeners
	closeAll := func() {
		for _, ln := range []net.Listener{ls.API, ls.HTTPS, ls.HTTP, ls.Agents, ls.Ingress} {
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
	// With routes on this server, its proxy holds the public ports and sends
	// the panel's domain here over a socket only it reaches, so the panel
	// leaves them alone, whether the proxy is up or not (ADR 0024).
	handsOver := f.domain != "" && onPublicPorts(f)
	proxied := handsOver && ingress.Active(dir.Ingress())
	if f.domain != "" && !proxied {
		if ls.HTTPS, err = serve.Listen(ctx, f.httpsListen); err != nil {
			closeAll()
			return withListenHint(err, "--https-listen")
		}
		if ls.HTTP, err = serve.Listen(ctx, f.httpListen); err != nil {
			closeAll()
			return withListenHint(err, "--http-listen")
		}
	}
	if handsOver {
		if ls.Ingress, err = listenForIngress(ctx, dir); err != nil {
			closeAll()
			return err
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
	if f.domain != "" {
		if public, err = acmeSource(f, dir, httpsAddrOf(ls.HTTPS, f.httpsListen)); err != nil {
			closeAll()
			return err
		}
	}

	addr := panelAddress{Domain: f.domain, Host: ipOf(ls.API.Addr()), Port: portOf(ls.API.Addr()), PublicHost: hostOf(agentURL)}
	if ls.HTTPS != nil {
		addr.HTTPSPort = portOf(ls.HTTPS.Addr())
	} else if proxied {
		addr.HTTPSPort = "443"
	}
	if err := announce(ctx, stdio, logger, db, people, addr); err != nil {
		closeAll()
		return err
	}
	if proxied {
		logger.Info("this server's routes hold ports 80 and 443, and Caddy serves the panel's domain. If it's down, the panel still answers through an SSH tunnel",
			slog.String("domain", f.domain), slog.String("tunnel", "ssh -L "+addr.Port+":"+addr.tunnelHost()+":"+addr.Port+" "+sshTarget(addr, stdio.Getenv)))
	}

	// The revision engine reaches agents over their streams. A change left
	// applying, by a stop partway or a rollback waiting for a server, is
	// resumed once serving starts, whenever a server reconnects, and
	// whenever one can run a kind it couldn't, as when Docker starts after
	// its agent (ADR 0009, ADR 0021). The engine's runs outlive the request that started them;
	// stopping lets them finish their steps or leaves them for the next
	// start, and the store closes only after (srv.Run).
	kinds := controlKinds(f.domain)
	keeper := secrets.New(db, key, kinds.With(scope.Types()...))
	// control.New has the agents' streams send the keeper's values, and ask it what
	// names them.
	agents := agentapi.NewAgents(kinds, logger)
	engine := revision.New(db, kinds, agents, keeper, logger)
	resumeCtx := context.WithoutCancel(ctx)
	resume := func() {
		if err := engine.Resume(resumeCtx); err != nil {
			logger.Warn("can't finish a change left applying yet; Dockwell tries again when a server reconnects", slog.String("error", err.Error()))
		}
	}
	agents.OnConnect(func(string) { resume() })
	// Tells the timeline when a service turns unhealthy or comes back, for as
	// long as the control plane runs, and it stops with it.
	recorder := &controlhealth.Recorder{DB: db, Logger: logger}
	agents.OnHealth(recorder.Heard)

	srv, err := control.New(control.Config{
		Logger:        logger,
		Build:         build,
		Identity:      authority,
		Fleet:         hub,
		AgentHosts:    agentHosts(agentURL),
		Auth:          people,
		Public:        public,
		PublicAddress: addr.dnsAddress(),
		Changes:       engine,
		Agents:        agents,
		Store:         db,
		Secrets:       keeper,
	})
	if err != nil {
		closeAll()
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// This server's own agent stops after the control plane, which stops
	// the revision engine before its agent listener: a step under way on
	// this server then finishes, or is left for the next start, rather
	// than failing because its agent left first.
	agentCtx, stopAgent := context.WithCancel(context.WithoutCancel(ctx))
	defer stopAgent()
	go resume()
	var wg sync.WaitGroup
	var controlErr, agentErr error
	wg.Go(func() {
		controlErr = srv.Run(ctx, ls)
		cancel()
		stopAgent()
	})
	wg.Go(func() { recorder.Run(ctx) })
	var panel *ingress.Panel
	if f.domain != "" {
		panel = &ingress.Panel{Domain: f.domain}
		if handsOver {
			ports := srv.PublicPorts(f.httpsListen, f.httpListen)
			panel.Release, panel.Retake = ports.Release, ports.Retake
			if acme, ok := public.(*https.ACME); ok {
				panel.Certificate = func(ctx context.Context) (*ingress.PanelCert, error) {
					chain, key, err := acme.Current(ctx)
					if err != nil || chain == nil {
						return nil, err
					}
					return &ingress.PanelCert{Chain: chain, Key: key}, nil
				}
			}
		}
	}
	wg.Go(func() {
		agentErr = runAgent(agentCtx, stdio, logger, dir, own, build, panel, ownPorts(ls, f))
		cancel()
	})
	wg.Wait()
	return errors.Join(controlErr, agentErr)
}

// ownPorts are the TCP ports the control plane listens on, or will when it takes
// its public ports back, none of which a route may send to: one that reached the
// panel's loopback listener would open it to the network, which is what ADR 0011
// refuses.
func ownPorts(ls control.Listeners, f runFlags) []int {
	var out []int
	for _, ln := range []net.Listener{ls.API, ls.Agents, ls.HTTPS, ls.HTTP} {
		if ln == nil {
			continue
		}
		if addr, ok := ln.Addr().(*net.TCPAddr); ok {
			out = append(out, addr.Port)
		}
	}
	if f.domain == "" {
		return out
	}
	for _, listen := range []string{f.httpsListen, f.httpListen} {
		if _, port, err := net.SplitHostPort(listen); err == nil {
			if n, err := strconv.Atoi(port); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// onPublicPorts reports a panel on ports 80 or 443, which only one process can
// hold, so that a proxy on this server takes them over. A panel on others, as
// with --https-listen :8443, stays as it is beside one.
func onPublicPorts(f runFlags) bool {
	public := func(addr string, names ...string) bool {
		_, port, err := net.SplitHostPort(addr)
		return err == nil && slices.Contains(names, port)
	}
	return public(f.httpsListen, "443", "https") || public(f.httpListen, "80", "http")
}

// httpsAddrOf is where the panel's HTTPS is for redirects: its listener, or
// the address it would listen on, while its proxy holds the port.
func httpsAddrOf(ln net.Listener, listen string) net.Addr {
	if ln != nil {
		return ln.Addr()
	}
	addr, err := net.ResolveTCPAddr("tcp", listen)
	if err != nil {
		return &net.TCPAddr{Port: 443}
	}
	return addr
}

// listenForIngress listens on the socket the proxy sends the panel's domain
// to. Only root and the proxy's user reach it, which is why what arrives on it
// counts as HTTPS: no header decides that (ADR 0024).
func listenForIngress(ctx context.Context, dir datadir.Dir) (net.Listener, error) {
	if err := caddy.EnsureDirs(dir.Ingress()); err != nil {
		return nil, err
	}
	path := caddy.PanelSocket(dir.Ingress())
	if err := caddy.ClearSocket(path); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("can't listen on %s for what the ingress sends the panel: %w", path, err)
	}
	if err := caddy.OwnSocket(path); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// refuseClaimedDomain refuses to give the panel a domain a route serves, or
// one a route is under: no route can claim the panel's, and none that does
// already should find itself under the panel's passkeys (ADR 0024).
func refuseClaimedDomain(ctx context.Context, db *store.DB, domain string) error {
	return db.View(ctx, func(tx *store.Tx) error {
		routes, err := tx.ListKind(ctx, string(route.Kind))
		if err != nil {
			return err
		}
		for _, r := range routes {
			spec := &resourcev1.Route{}
			if err := proto.Unmarshal(r.Spec, spec); err != nil {
				return fmt.Errorf("can't read the route %s: %w", r.Name, err)
			}
			if route.UnderOrIs(spec.GetDomain(), domain) {
				return usageError{fmt.Errorf("the route %s serves %s, so the panel can't take %s: a passkey for that name works under it too. Delete the route first, or give the panel another domain", r.Name, spec.GetDomain(), domain)}
			}
		}
		return nil
	})
}

// controlKinds are the kinds the control plane knows: it checks their specs
// and plans them for a server that is offline. Servers run them only where
// their agents have drivers, which they report in their hello: the Docker
// kinds while Docker answers. Variables and secrets run on no server. domain
// is the panel's domain, which no route may claim. The firewall's driver comes
// later.
func controlKinds(domain string) *resource.Registry {
	return resource.NewRegistry(&container.Driver{}, &firewall.Driver{}, &service.Driver{}, &volume.Driver{}, &network.Driver{}, &route.Driver{Reserved: domain}, variable.Type{}, variable.SecretType{})
}

// openState opens the control plane's key, its state and its identity,
// and makes the install on the first start. It refuses to make a second
// install beside an existing one: a data directory whose key belongs to an
// install must find that install's state, or agents enrolled with it
// would no longer be recognized.
//
// The first start is all or nothing. Its key waits at NewSealKey until the
// install is on record, and only then moves to SealKey. So a first start
// that fails, such as with the database out of reach, leaves no key that
// belongs to an install, and the next start carries on with the key it
// left.
func openState(ctx context.Context, logger *slog.Logger, dir datadir.Dir, location string) (*store.DB, *identity.Authority, *seal.Key, error) {
	installed := exists(dir.SealKey())
	postgres := store.IsPostgres(location)
	switch {
	case postgres && exists(dir.StateDB()):
		return nil, nil, nil, fmt.Errorf("this control plane keeps its state in %s, and moving it to Postgres isn't supported yet. "+
			"Unset --database and %s to keep using it", dir.StateDB(), envDatabase)
	case !postgres && installed && !exists(location):
		return nil, nil, nil, fmt.Errorf("this data directory holds a control plane's key, but its state isn't in %s. "+
			"If it keeps its state in Postgres, pass --database or set %s to its URL; otherwise restore %s from a backup",
			location, envDatabase, filepath.Base(location))
	}
	keyPath := dir.SealKey()
	if !installed {
		keyPath = dir.NewSealKey()
	}
	key, err := seal.LoadOrCreate(keyPath)
	if err != nil {
		return nil, nil, nil, err
	}
	db, err := store.Open(ctx, location)
	if err != nil {
		return nil, nil, nil, err
	}
	logger.Info("opened the control plane's state", slog.String("backend", string(db.Backend())), slog.String("database", db.Where()))
	var authority *identity.Authority
	if installed {
		authority, err = identity.Open(ctx, db, key, logger)
		if errors.Is(err, identity.ErrNoInstall) {
			err = fmt.Errorf("%s holds no Dockwell install, but this data directory's key belongs to one. "+
				"Point --database or %s at that install's database", db.Where(), envDatabase)
		}
	} else {
		authority, err = identity.Bootstrap(ctx, db, key, logger)
	}
	if err == nil {
		_, err = revision.Setup(ctx, db, logger)
	}
	if err == nil && !installed {
		err = seal.Commit(keyPath, dir.SealKey())
	}
	if err != nil {
		_ = db.Close()
		return nil, nil, nil, err
	}
	return db, authority, key, nil
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
