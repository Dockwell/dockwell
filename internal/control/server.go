// SPDX-License-Identifier: AGPL-3.0-only

// Package control is the control plane: the API every surface uses, the
// panel, the agent listener servers connect to, and later the revision
// store and the scheduler.
//
// The API listener serves the Connect, gRPC and gRPC-Web protocols over
// HTTP/1.1 and HTTP/2, REST paths under /v1/ through Vanguard with their
// OpenAPI document (ADR 0015), the panel at every other path, and /healthz
// for process supervisors. It stays on loopback. With a domain, the same
// handler also serves HTTPS on the panel's domain, and a plain-HTTP
// listener answers certificate challenges and redirects to HTTPS (ADR
// 0011). Every procedure but sign-in needs a session or an API token whose
// scope covers it (internal/auth, ADR 0016), whatever the protocol.
//
// The agent listener speaks only to agents, over TLS 1.3 with client
// certificates (ADR 0007).
package control

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"
	"connectrpc.com/vanguard"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/alerts"
	"github.com/dockwell/dockwell/internal/control/apidoc"
	"github.com/dockwell/dockwell/internal/control/audit"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/autoupdate"
	"github.com/dockwell/dockwell/internal/control/backups"
	"github.com/dockwell/dockwell/internal/control/builds"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/checks"
	"github.com/dockwell/dockwell/internal/control/composeapi"
	"github.com/dockwell/dockwell/internal/control/containers"
	"github.com/dockwell/dockwell/internal/control/databases"
	"github.com/dockwell/dockwell/internal/control/domains"
	"github.com/dockwell/dockwell/internal/control/events"
	"github.com/dockwell/dockwell/internal/control/files"
	"github.com/dockwell/dockwell/internal/control/firewall"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/health"
	"github.com/dockwell/dockwell/internal/control/history"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/control/images"
	"github.com/dockwell/dockwell/internal/control/importapi"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/control/members"
	"github.com/dockwell/dockwell/internal/control/opener"
	"github.com/dockwell/dockwell/internal/control/processes"
	"github.com/dockwell/dockwell/internal/control/resources"
	"github.com/dockwell/dockwell/internal/control/servers"
	serviceapi "github.com/dockwell/dockwell/internal/control/services"
	"github.com/dockwell/dockwell/internal/control/serviceupdates"
	"github.com/dockwell/dockwell/internal/control/settings"
	"github.com/dockwell/dockwell/internal/control/system"
	"github.com/dockwell/dockwell/internal/control/tasks"
	"github.com/dockwell/dockwell/internal/control/terminals"
	"github.com/dockwell/dockwell/internal/control/updates"
	"github.com/dockwell/dockwell/internal/control/variables"
	"github.com/dockwell/dockwell/internal/control/websites"
	"github.com/dockwell/dockwell/internal/gitref"
	"github.com/dockwell/dockwell/internal/https"
	"github.com/dockwell/dockwell/internal/panel"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/serve"
	"github.com/dockwell/dockwell/internal/store"
)

// Default listen addresses. The API stays on loopback until the operator
// chooses to expose it; agents on other servers must reach the agent
// listener, which only speaks mTLS and join tickets.
const (
	DefaultListen      = "127.0.0.1:7600"
	DefaultAgentListen = ":7443"
)

// Config configures a Server.
type Config struct {
	// Logger receives the server's logs. Required.
	Logger *slog.Logger
	// DNSLookup resolves names for DomainService.CheckDomain. Nil means the control
	// server's resolver.
	DNSLookup func(ctx context.Context, name string) ([]netip.Addr, error)
	// Sources reads what a build's branch names (ADR 0046). Nil asks each
	// repository over Git's smart HTTP protocol (package gitref).
	Sources revision.Sources
	// Updates says what newer image each service has, for an update that names
	// none and for automatic updates (ADR 0080), in place of ADR 0079's keeper,
	// which reads them otherwise. Nil takes the keeper's.
	Updates serviceapi.Updates
	// Build is reported by SystemService.GetVersion.
	Build buildinfo.Info
	// Panel is the panel build to serve. Nil means the one embedded in the binary.
	Panel fs.FS
	// Identity is the install's authority, and Fleet its servers' live state.
	// Without both there is no agent listener and no ServerService.
	Identity *identity.Authority
	Fleet    *fleet.Hub
	// Standings say whether an app is down, as the health recorder folds its
	// states, for alerts. Nil judges by the states alone.
	Standings alerts.Standings
	// AlertClient sends to alert channels, and tests them, in place of netguard's,
	// for a test whose channels' far ends listen on loopback. Nil is netguard's.
	AlertClient *http.Client
	// CheckClient runs checks of a URL in place of netguard's guarded client, for
	// a test whose addresses are on this machine (ADR 0066).
	CheckClient checks.Doer
	// ManualAlerts leaves evaluating alert rules to whoever calls the evaluator,
	// as a test does by its own clock. The install's first rules are made all the
	// same.
	ManualAlerts bool
	// AgentHosts are the names and addresses agents dial; the agent
	// listener's certificate is valid for them.
	AgentHosts []string
	// Auth decides who may use the API. Without it, only the procedures
	// that need no session answer.
	Auth *auth.Service
	// Public serves the panel on its domain over HTTPS. Nil keeps the panel
	// on loopback.
	Public https.Source
	// PanelListen is the loopback address the panel answers on, PanelURL its
	// address on Public's domain while Dockwell serves it, and PanelCA who
	// issues its certificate, for the settings to say how the panel is served.
	// PanelHandsOver is true when the server's ingress takes the public ports
	// while the server has a route, and serves the panel's domain then (ADR
	// 0024): the settings ask PublicPorts which holds them now, and where.
	PanelListen, PanelURL, PanelCA string
	PanelHandsOver                 bool
	// PanelDomain is what Settings needs to set the panel's domain (ADR 0063).
	PanelDomain PanelDomain
	// DataDir is the directory dockwell run keeps its files in, KeyFile the
	// install's key there, AgentListen where servers' agents reach it, and
	// ServiceManager what runs it as a service, for the settings to say what
	// the control plane is, and how to stop it for a backup.
	DataDir, KeyFile, AgentListen string
	ServiceManager                dockwellv1.ServiceManager
	// PublicAddress is this server's public IP address, shown on the claim
	// screen for pointing a domain at. Empty when it isn't known.
	PublicAddress string
	// ClaimCommand prints a new claim link on the control server, as it runs
	// there, such as "sudo dockwell claim", for the panel to show while the
	// install has no owner.
	ClaimCommand string
	// Changes is the revision engine, which ChangeService serves, and
	// Agents how it reaches each server's agent over its stream. Store is
	// the state they read. Without Changes there is no ChangeService.
	Changes *revision.Engine
	Agents  *agentapi.Agents
	Store   *store.DB
	// Secrets keeps what may be secret encrypted, and reveals a secret to
	// a person who confirms it with a passkey. With Changes and Auth, it
	// serves VariableService.
	Secrets *secrets.Keeper
	// Logs changes the limits of reading logs, as tests do.
	Logs logapi.Options
	// EventPoll is how often a watch of the timeline reads the store again,
	// for events another process recorded; events.DefaultPoll when zero.
	EventPoll time.Duration
	// StepGrace is how long the steps under way get to finish when the
	// control plane stops, before they are cancelled; DefaultStepGrace when
	// zero. Their changes finish when it starts again.
	StepGrace time.Duration
	// Releases is the URL of Dockwell's releases, in GitHub's layout, which
	// UpdateService reads the newest release from. Empty reads none.
	Releases string
	// CatalogSources is where the catalogs read from Git are kept, by commit
	// (ADR 0055). Empty keeps none between runs.
	CatalogSources string
	// CatalogClient is what catalog sources are read with: netguard's, which reads
	// only public addresses, unless a test passes one that reaches its own server.
	CatalogClient *http.Client
	// CatalogFollow looks at the branch each catalog source follows every hour,
	// and moves the source along it (ADR 0077). Off, a source moves only when a
	// person asks Dockwell to look, as a test asks no host it doesn't run.
	CatalogFollow bool
}

// DefaultStepGrace is how long the steps under way get to finish when the
// control plane stops. With the API's and the agent listener's own
// shutdown, it keeps a stop well within the 90 seconds systemd waits.
const DefaultStepGrace = 30 * time.Second

// Server is a configured control plane, ready to serve.
type Server struct {
	logger   *slog.Logger
	api      http.Handler
	agents   http.Handler
	agentTLS *tls.Config
	// addAgentHost has the agent listener's certificate name one more address.
	addAgentHost func(string)
	// logs reads service logs, and tells servers what deletions put out of reach.
	logs *logapi.Handler
	// alerts evaluates the alert rules, and sends what's due (ADR 0044), unless
	// manualAlerts leaves that to a test.
	alerts *alerts.Evaluator
	// autoupdates applies the updates each service takes on its own, in its window
	// (ADR 0080).
	autoupdates *autoupdate.Runner
	// history keeps each server's samples a minute at a time, and by the hour
	// (ADR 0065).
	history      *history.Recorder
	manualAlerts bool
	// backups takes backups, and those under way end with the control plane.
	backups *backups.Handler
	// tasks reads scheduled tasks' runs from the agents that run them.
	tasks *tasks.Handler
	// updates checks what's newer than what each service runs (ADR 0079).
	updates *serviceupdates.Keeper
	public  https.Source
	changes *revision.Engine
	// hosts gives each server its host (ADR 0064), once its agent runs the kind.
	hosts *Hosts
	// follower moves the catalog sources that follow a branch along it (ADR 0077),
	// when Config.CatalogFollow asks it to.
	follower *catalogapi.Follower
	// catalogs gives the install Dockwell's own catalog at its first start (ADR 0077).
	catalogs  *catalogapi.Follower
	store     *store.DB
	stepGrace time.Duration
	ports     *PublicPorts
}

// New builds a Server from cfg.
func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		return nil, errors.New("control: Config.Logger is required")
	}
	if cfg.Panel == nil {
		cfg.Panel = panel.Embedded()
	}
	ui, err := panel.New(cfg.Panel)
	if err != nil {
		return nil, err
	}
	if !ui.Built() {
		cfg.Logger.Warn("this binary has no panel built in; the API works, and make build adds the panel")
	}
	if cfg.Public != nil && (cfg.Auth == nil || cfg.Auth.Sites().Domain != cfg.Public.Domain()) {
		return nil, errors.New("control: Config.Public needs Config.Auth for the same domain")
	}
	var logs *logapi.Handler
	var backupHandler *backups.Handler
	var taskHandler *tasks.Handler
	// Made first, so the settings can ask it who holds the public ports now.
	ports := &PublicPorts{ready: make(chan struct{})}
	var evaluator *alerts.Evaluator
	var updateKeeper *serviceupdates.Keeper
	var autoupdates *autoupdate.Runner
	var follower *catalogapi.Follower
	api, err := apiRoutes(cfg, ui, ports, &logs, &backupHandler, &taskHandler, &evaluator, &updateKeeper, &autoupdates, &follower)
	if err != nil {
		return nil, err
	}
	if cfg.Changes != nil {
		// Every change through the API carries who makes it, so the engine
		// refuses a person's that comes without (ADR 0049).
		cfg.Changes.RequireRoles()
	}
	s := &Server{logger: cfg.Logger, api: api, logs: logs, backups: backupHandler, tasks: taskHandler, updates: updateKeeper, alerts: evaluator, manualAlerts: cfg.ManualAlerts, public: cfg.Public, changes: cfg.Changes, store: cfg.Store, stepGrace: cfg.StepGrace, autoupdates: autoupdates}
	s.catalogs = follower
	if cfg.CatalogFollow {
		s.follower = follower
	}
	ports.server = s
	s.ports = ports
	if s.stepGrace <= 0 {
		s.stepGrace = DefaultStepGrace
	}
	if (cfg.Identity == nil) != (cfg.Fleet == nil) {
		return nil, errors.New("control: Config.Identity and Config.Fleet go together")
	}
	if cfg.Changes != nil && (cfg.Agents == nil || cfg.Store == nil) {
		return nil, errors.New("control: Config.Changes needs Config.Agents and Config.Store")
	}
	// Each stream sends a step the keeper's values it needs, and asks the keeper
	// what on a server names one, of an agent that doesn't write them in. Without a
	// keeper, such an agent is refused all it would read (agentapi.Agents.CheckNaming).
	if cfg.Agents != nil && cfg.Secrets != nil {
		cfg.Agents.SendValues(cfg.Secrets)
		cfg.Agents.CheckNaming(cfg.Secrets)
	}
	// A service's containers are named after what people call its environment.
	if cfg.Changes != nil && cfg.Agents != nil && cfg.Store != nil {
		s.hosts = &Hosts{DB: cfg.Store, Engine: cfg.Changes, Agents: cfg.Agents, Logger: cfg.Logger}
	}
	if cfg.Agents != nil && cfg.Store != nil {
		cfg.Agents.NameScopes(scopeNames{db: cfg.Store})
	}
	if cfg.Identity != nil {
		opts := []agentapi.Option{agentapi.WithAgents(cfg.Agents)}
		if cfg.Store != nil && cfg.Fleet != nil {
			s.history = &history.Recorder{DB: cfg.Store, Servers: fleetIDs(cfg.Fleet), Logger: cfg.Logger}
			opts = append(opts, agentapi.WithHistory(s.history))
		}
		s.agents = agentapi.NewHandler(cfg.Identity, cfg.Fleet, cfg.Logger, opts...)
		s.agentTLS, s.addAgentHost = agentapi.TLSConfig(cfg.Identity, cfg.AgentHosts)
	}
	return s, nil
}

// Handler returns the API listener's root handler.
func (s *Server) Handler() http.Handler { return s.api }

func apiRoutes(cfg Config, ui http.Handler, ports *PublicPorts, logsOut **logapi.Handler, backupsOut **backups.Handler, tasksOut **tasks.Handler, alertsOut **alerts.Evaluator, updatesOut **serviceupdates.Keeper, autoupdates **autoupdate.Runner, followOut **catalogapi.Follower) (http.Handler, error) {
	opts := append(serve.HandlerOptions(cfg.Logger), connect.WithInterceptors(authapi.Interceptor(cfg.Auth, cfg.Logger)))
	mux := http.NewServeMux()
	var services []string
	var rest []*vanguard.Service
	guardREST := func(h http.Handler) http.Handler { return h }
	var filesHandler http.Handler
	var appOpener *opener.Opener
	// handle serves a service's procedures, and its REST paths through
	// Vanguard, which calls the same handler, interceptors and all.
	handle := func(path string, h http.Handler) {
		mux.Handle(path, h)
		rest = append(rest, vanguard.NewService(path, h))
		services = append(services, strings.Trim(path, "/"))
	}
	handle(dockwellv1connect.NewSystemServiceHandler(system.New(cfg.Build), opts...))
	if cfg.Store != nil {
		serving := settings.Serving{
			Listen: cfg.PanelListen, URL: cfg.PanelURL, CA: cfg.PanelCA,
			From: cfg.PanelDomain.From, Problem: cfg.PanelDomain.Problem, Certified: cfg.PanelDomain.Certified,
			Started: time.Now(),
		}
		if cfg.Public != nil {
			serving.Domain = cfg.Public.Domain()
			serving.HTTPS = func() (string, bool) {
				listen, ok := ports.Serving()
				// A panel on ports of its own, beside the ingress, never hands them over.
				return listen, ok || !cfg.PanelHandsOver
			}
		}
		plane := settings.Plane{
			Build:          cfg.Build,
			Started:        time.Now(),
			DataDir:        cfg.DataDir,
			KeyFile:        cfg.KeyFile,
			AgentListen:    cfg.AgentListen,
			ServiceManager: cfg.ServiceManager,
		}
		if cfg.Identity != nil {
			plane.AgentURLs = agentURLs(cfg.Identity)
		}
		domain := settings.Domain{
			Auth: cfg.Auth, Resolve: cfg.PanelDomain.Resolve, PortsHeld: cfg.PanelDomain.PortsHeld, Restart: cfg.PanelDomain.Restart, Saved: cfg.PanelDomain.Saved,
			PublicAddresses: func() []string { return controlAddresses(cfg.Fleet) },
		}
		if cfg.PanelDomain.PublicAddresses != nil {
			domain.PublicAddresses = cfg.PanelDomain.PublicAddresses
		}
		handle(dockwellv1connect.NewSettingsServiceHandler(settings.New(cfg.Store, serving, plane).WithDomain(domain), opts...))
		// For a person signed in alone, as its events name where people signed in from.
		if cfg.Auth != nil {
			handle(dockwellv1connect.NewAuditServiceHandler(audit.New(cfg.Store, cfg.Changes, cfg.Logger), opts...))
			handle(dockwellv1connect.NewMemberServiceHandler(members.New(cfg.Store, cfg.Auth, cfg.Logger), opts...))
		}
	}
	if cfg.Auth != nil {
		controlServer := func() *dockwellv1.Server { return nil }
		if cfg.Fleet != nil {
			controlServer = cfg.Fleet.ControlServer
		}
		h := authapi.New(authapi.Config{
			Auth: cfg.Auth, Logger: cfg.Logger, ControlServer: controlServer, PublicAddress: cfg.PublicAddress, ClaimCommand: cfg.ClaimCommand,
		})
		handle(dockwellv1connect.NewAuthServiceHandler(h, opts...))
		handle(dockwellv1connect.NewTokenServiceHandler(h, opts...))
	}
	if cfg.Fleet != nil {
		handle(dockwellv1connect.NewServerServiceHandler(servers.New(cfg.Fleet, cfg.Store, cfg.Agents).Tickets(cfg.Identity, cfg.Auth), opts...))
		handle(dockwellv1connect.NewImageServiceHandler(images.New(cfg.Fleet, cfg.Agents), opts...))
		handle(dockwellv1connect.NewFirewallServiceHandler(firewall.New(cfg.Fleet, cfg.Agents), opts...))
	}
	if cfg.Changes != nil {
		ch := changes.New(cfg.Changes, cfg.Auth, cfg.Store, cfg.Logger)
		handle(dockwellv1connect.NewChangeServiceHandler(ch, opts...))
		own := containers.New(cfg.Agents, ch, cfg.Store, cfg.Logger)
		imports := composeapi.New(ch, own, cfg.Changes, cfg.Store, cfg.Secrets, cfg.Logger)
		handle(dockwellv1connect.NewComposeServiceHandler(imports, opts...))
		catalogSources := &catalogapi.Sources{Dir: cfg.CatalogSources, Client: cfg.CatalogClient}
		catalogHandler := catalogapi.New(imports, ch, cfg.Store, cfg.Secrets, catalogSources, cfg.Build.Commit, cfg.Logger)
		*followOut = catalogHandler.Follow(cfg.Changes)
		follower := *followOut
		if cfg.Fleet != nil {
			// Another store's entry pins its images through the agent it goes on.
			catalogHandler.PinWith(images.New(cfg.Fleet, cfg.Agents))
		}
		handle(dockwellv1connect.NewCatalogServiceHandler(catalogHandler, opts...))
		// A source's images, for whoever may read the catalog, as an image's request
		// carries the session's cookie (ADR 0077).
		mux.Handle(catalogapi.MediaRoute, catalogSources.Media(func(r *http.Request) error {
			return authapi.Authorizer(cfg.Auth, cfg.Logger)(r.Context(), dockwellv1connect.CatalogServiceListCatalogEntriesProcedure)
		}))
		handle(dockwellv1connect.NewWebsiteServiceHandler(websites.New(ch, cfg.Changes, cfg.Store, cfg.Secrets, cfg.Agents, cfg.Logger), opts...))
		handle(dockwellv1connect.NewEventServiceHandler(events.New(cfg.Changes, cfg.Store, cfg.Logger, cfg.EventPoll), opts...))
		handle(dockwellv1connect.NewResourceServiceHandler(resources.New(cfg.Changes.Kinds(), cfg.Store, cfg.Logger), opts...))
		if cfg.Secrets != nil && cfg.Auth != nil {
			handle(dockwellv1connect.NewVariableServiceHandler(
				variables.New(cfg.Changes.Kinds(), cfg.Store, cfg.Secrets, cfg.Auth, cfg.Logger), opts...))
		}
		domainHandler := domains.New(cfg.Changes.Kinds(), cfg.Store, cfg.Agents, fleetOf(cfg), cfg.Logger)
		domainHandler.Lookup = cfg.DNSLookup
		handle(dockwellv1connect.NewDomainServiceHandler(domainHandler, opts...))
		if cfg.Fleet != nil {
			var releases *updates.Releases
			if cfg.Releases != "" {
				releases = updates.NewReleases(cfg.Releases, cfg.Build.Version, cfg.Logger)
			}
			handle(dockwellv1connect.NewUpdateServiceHandler(updates.New(cfg.Fleet, cfg.Agents, releases, cfg.Build, cfg.Logger), opts...))
		}
		// A person's, so it needs sign-in; the agent decides what it reads.
		if cfg.Fleet != nil && cfg.Auth != nil {
			path, h := dockwellv1connect.NewFileServiceHandler(files.New(cfg.Fleet, cfg.Agents, cfg.Auth, cfg.Store, cfg.Logger), opts...)
			handle(path, h)
			filesHandler = h
			// A person's too, as a command line can hold a password, and ending a process is root.
			handle(dockwellv1connect.NewProcessServiceHandler(processes.New(cfg.Fleet, cfg.Agents, cfg.Auth, cfg.Store, cfg.Logger), opts...))
			handle(dockwellv1connect.NewTerminalServiceHandler(terminals.New(cfg.Fleet, cfg.Agents, cfg.Auth, cfg.Store, cfg.Logger), opts...))
		}
		// What another panel keeps on a server, read through its agent (ADR 0056).
		if cfg.Fleet != nil {
			handle(dockwellv1connect.NewImportServiceHandler(importapi.New(cfg.Fleet, cfg.Agents, imports, ch, cfg.Logger), opts...))
		}
		sources := cfg.Sources
		if sources == nil {
			// A test's Git server is on this machine.
			sources = &gitref.Resolver{Local: testing.Testing()}
		}
		cfg.Changes.SetSources(sources)
		serviceHandler := serviceapi.New(ch, cfg.Changes, cfg.Store, cfg.Secrets, cfg.Logger)
		serviceHandler.Installs = catalogHandler
		serviceHandler.Sources = sources
		*autoupdates = &autoupdate.Runner{
			DB: cfg.Store, Keeper: cfg.Secrets, Planner: serviceHandler, Engine: cfg.Changes, Logger: cfg.Logger,
		}
		if cfg.Fleet != nil {
			(*autoupdates).Images = images.New(cfg.Fleet, cfg.Agents)
		}
		if cfg.Agents != nil {
			buildHandler := builds.New(cfg.Store, cfg.Agents, serviceHandler, cfg.Logger)
			buildHandler.SetSources(sources)
			cfg.Changes.SetDetector(buildHandler)
			serviceHandler.Builds = buildHandler
			handle(dockwellv1connect.NewBuildServiceHandler(buildHandler, opts...))
		}
		if cfg.Auth != nil && cfg.Agents != nil && cfg.DataDir != "" {
			key, err := opener.LoadKey(filepath.Join(cfg.DataDir, "opener.key"))
			if err != nil {
				return nil, err
			}
			appOpener = &opener.Opener{
				Dial: func(ctx context.Context, t opener.Target) (net.Conn, error) {
					s, ok := cfg.Agents.Session(t.Server)
					if !ok {
						return nil, fmt.Errorf("the server %s isn't connected, so its apps don't open", t.Server)
					}
					conn, err := s.Forward(ctx, t.Project, t.Service, t.Port)
					if errors.Is(err, agentapi.ErrForwardsFull) {
						return nil, fmt.Errorf("%w: %w", opener.ErrFull, err)
					}
					return conn, err
				},
				Live: cfg.Auth.SessionLive, Key: key, Logger: cfg.Logger,
			}
			serviceHandler.Opener = appOpener
			serviceHandler.Auth = cfg.Auth
		}
		// What's newer than what each service runs, checked through its server's
		// agent, and a catalog install's read from its catalog (ADR 0079).
		if cfg.Agents != nil && cfg.Fleet != nil && cfg.Secrets != nil {
			keeper := &serviceupdates.Keeper{
				DB: cfg.Store, Secrets: cfg.Secrets, Agents: serviceupdates.FromAgents(cfg.Agents), Sources: sources, Logger: cfg.Logger,
				Catalog: func(ctx context.Context) (map[string]serviceupdates.CatalogInstall, error) {
					upgrades, err := catalogHandler.InstallUpgrades(ctx)
					if err != nil {
						return nil, err
					}
					out := make(map[string]serviceupdates.CatalogInstall, len(upgrades))
					for project, u := range upgrades {
						out[project] = serviceupdates.CatalogInstall{FromVersion: u.FromVersion, ToVersion: u.ToVersion, ToRelease: u.ToRelease, Notes: serviceupdates.NotesOf(u.AppSource)}
					}
					return out, nil
				},
			}
			serviceHandler.UpdateChecks = keeper
			// An update that names no image takes what the keeper found (ADR 0080).
			serviceHandler.Updates = serviceapi.NewerFrom(keeper)
			handle(dockwellv1connect.NewServiceUpdateServiceHandler(serviceupdates.NewHandler(keeper, serviceHandler, serverID(cfg.Fleet), cfg.Logger), opts...))
			*updatesOut = keeper
		}
		// A test's own record of what's newer stands in for the keeper's.
		if cfg.Updates != nil {
			serviceHandler.Updates = cfg.Updates
		}
		// Automatic updates take what an update with no image would (ADR 0080).
		(*autoupdates).Updates = serviceHandler.Updates
		handle(dockwellv1connect.NewServiceServiceHandler(serviceHandler, opts...))
		logs := logapi.New(cfg.Agents, ch, cfg.Store, cfg.Logger, cfg.Logs)
		*logsOut = logs
		path, h := dockwellv1connect.NewLogServiceHandler(logs, opts...)
		mux.Handle(path, logs.Guard(h))
		rest = append(rest, vanguard.NewService(path, h))
		services = append(services, strings.Trim(path, "/"))
		guardREST = logs.GuardREST
		healthHandler := health.New(cfg.Store, cfg.Agents, cfg.Logger)
		handle(dockwellv1connect.NewHealthServiceHandler(healthHandler, opts...))
		*alertsOut = &alerts.Evaluator{
			DB: cfg.Store, Keeper: cfg.Secrets, Engine: cfg.Changes, Domains: cfg.Agents, Health: healthHandler,
			Standings: cfg.Standings, Panel: cfg.PanelURL, Logger: cfg.Logger,
			Sender: &alerts.Sender{Client: cfg.AlertClient},
			Checks: &checks.Runner{DB: cfg.Store, Keeper: cfg.Secrets, Client: cfg.CheckClient, Logger: cfg.Logger},
		}
		if cfg.Fleet != nil {
			(*alertsOut).Fleet = cfg.Fleet
		}
		if keeper := *updatesOut; keeper != nil {
			(*alertsOut).Updates = appUpdates(keeper)
		}
		(*alertsOut).Catalogs = func(ctx context.Context) []alerts.Following {
			var out []alerts.Following
			for _, s := range follower.Standings(ctx) {
				out = append(out, alerts.Following{Name: s.Name, Branch: s.Branch, Looked: s.Looked, Problem: s.Problem, Since: s.Since})
			}
			return out
		}
		handle(dockwellv1connect.NewAlertServiceHandler(alerts.New(*alertsOut, cfg.Logger), opts...))
		handle(dockwellv1connect.NewContainerServiceHandler(own, opts...))
		*backupsOut = backups.New(ch, cfg.Changes, cfg.Agents, cfg.Store, cfg.Secrets, cfg.Logger)
		handle(dockwellv1connect.NewBackupServiceHandler(*backupsOut, opts...))
		dbs := databases.New(ch, cfg.Agents, *backupsOut, cfg.Store, cfg.Secrets, cfg.Logger)
		cfg.Changes.FeaturesFrom(dbs.LastFeatures)
		handle(dockwellv1connect.NewDatabaseServiceHandler(dbs, opts...))
		*tasksOut = tasks.New(cfg.Changes, cfg.Agents, cfg.Store, cfg.Logger)
		handle(dockwellv1connect.NewTaskServiceHandler(*tasksOut, opts...))
	}
	authorize := authapi.Authorizer(cfg.Auth, cfg.Logger)
	restPaths, err := restHandler(rest, authorize)
	if err != nil {
		return nil, err
	}
	mux.Handle(authapi.RESTPrefix, guardREST(restPaths))
	if filesHandler != nil {
		streamTransfers(mux, filesHandler, guardREST, authorize)
	}
	mux.Handle("GET "+apidoc.Path, apidoc.Handler())

	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker(services...), opts...))
	reflector := grpcreflect.NewStaticReflector(services...)
	mux.Handle(grpcreflect.NewHandlerV1(reflector, opts...))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector, opts...))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})

	// Everything else is the panel; it answers 404 for paths it doesn't own.
	mux.Handle("/", ui)

	var sites auth.Sites
	if cfg.Auth != nil {
		sites = cfg.Auth.Sites()
	}
	// An app opened through the panel answers on an origin of its own, ahead of the
	// panel's guard, which answers none but the panel's (ADR 0061).
	return serve.Recover(cfg.Logger, opener.Dispatch(appOpener, authapi.Guard(sites, cfg.Logger, mux))), nil
}

// Serve serves the API and panel on ln until ctx is done.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	return serve.Serve(ctx, ln, s.api, serve.Options{Name: "control plane", Logger: s.logger})
}

// agentFrame is the largest HTTP/2 frame the agent listener takes.
const agentFrame = 16 << 10

// ServeAgents serves the agent listener on ln until ctx is done.
func (s *Server) ServeAgents(ctx context.Context, ln net.Listener) error {
	if s.agents == nil {
		return errors.New("control: the agent listener needs Config.Identity")
	}
	// HTTP/2's smallest frame, a TLS record's size: an agent writes each stream's body
	// through a buffer of the frame's size, and with Go's own, 1 MiB, that's 512 KiB
	// for each upload, download and forward under way (ADR 0061).
	return serve.Serve(ctx, ln, s.agents, serve.Options{Name: "agent listener", Logger: s.logger, TLS: s.agentTLS, MaxFrame: agentFrame})
}

// AddAgentHost has the agent listener's certificate name host too, from the next
// connection.
func (s *Server) AddAgentHost(host string) {
	if s.addAgentHost != nil {
		s.addAgentHost(host)
	}
}

// PublicPorts returns what serves the panel on the public ports, for this
// server's ingress to take them from and give them back to: the addresses are
// where it listens again.
func (s *Server) PublicPorts(httpsAddr, httpAddr string) *PublicPorts {
	s.ports.httpsAddr, s.ports.httpAddr = httpsAddr, httpAddr
	return s.ports
}

// PublicPorts serves the panel over HTTPS and redirects plain HTTP, until this
// server's proxy takes ports 80 and 443, and again once it gives them back.
// Only one process can hold them (ADR 0024).
type PublicPorts struct {
	server              *Server
	httpsAddr, httpAddr string
	ready               chan struct{}
	readyOnce           sync.Once

	mu     sync.Mutex
	base   context.Context
	failed func(error)
	stop   context.CancelFunc
	done   *sync.WaitGroup
	// listening is the address the HTTPS listener has while it serves, as
	// bound, whether at the start or taken back from the ingress.
	listening string
	// ended says the server has stopped, so nothing starts from now on, and
	// every is all that was started, for that stop to wait for.
	ended bool
	every sync.WaitGroup
}

// begin says the listeners may start, under base, and fail through failed.
func (p *PublicPorts) begin(base context.Context, failed func(error)) {
	p.mu.Lock()
	p.base, p.failed = base, failed
	p.mu.Unlock()
	p.readyOnce.Do(func() { close(p.ready) })
}

// end stops what serves, and returns once it has, for the server to stop.
func (p *PublicPorts) end() {
	p.mu.Lock()
	p.ended = true
	if p.stop != nil {
		p.stop()
	}
	p.mu.Unlock()
	p.every.Wait()
}

// serve serves the panel on the two listeners.
func (p *PublicPorts) serve(httpsLn, httpLn net.Listener) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		_ = httpsLn.Close()
		_ = httpLn.Close()
		return
	}
	ctx, stop := context.WithCancel(p.base)
	done := &sync.WaitGroup{}
	p.stop, p.done = stop, done
	if httpsLn != nil {
		p.listening = httpsLn.Addr().String()
	}
	s := p.server
	listeners := []struct {
		ln   net.Listener
		h    http.Handler
		opts serve.Options
	}{
		{httpsLn, s.api, serve.Options{Name: "panel over HTTPS", Logger: s.logger, TLS: s.public.TLSConfig()}},
		{httpLn, s.public.HTTPHandler(), serve.Options{Name: "HTTP redirect", Logger: s.logger}},
	}
	for _, l := range listeners {
		if l.ln == nil {
			continue
		}
		done.Add(1)
		p.every.Add(1)
		go func() {
			defer p.every.Done()
			defer done.Done()
			if err := serve.Serve(ctx, l.ln, l.h, l.opts); err != nil {
				p.failed(err)
			}
		}()
	}
}

// Serving reports whether the control plane holds the public ports now,
// rather than this server's proxy, and where it serves HTTPS on them.
func (p *PublicPorts) Serving() (httpsAddr string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listening, p.stop != nil
}

// Release stops serving on the public ports, and returns once they are free.
func (p *PublicPorts) Release(ctx context.Context) error {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	stop, done := p.stop, p.done
	p.stop, p.done, p.listening = nil, nil, ""
	p.mu.Unlock()
	if stop == nil {
		return nil
	}
	stop()
	stopped := make(chan struct{})
	go func() {
		done.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Retake takes the public ports again, and serves on them.
func (p *PublicPorts) Retake(ctx context.Context) error {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	serving := p.stop != nil
	p.mu.Unlock()
	if serving {
		return nil
	}
	httpsLn, err := serve.Listen(ctx, p.httpsAddr)
	if err != nil {
		return err
	}
	httpLn, err := serve.Listen(ctx, p.httpAddr)
	if err != nil {
		_ = httpsLn.Close()
		return err
	}
	p.serve(httpsLn, httpLn)
	return nil
}

// Listeners are where a Server listens. API is required; the others are
// optional.
type Listeners struct {
	// API serves the API and panel over plain HTTP. It must be on loopback.
	API net.Listener
	// HTTPS serves the same on the panel's domain. It needs Config.Public.
	HTTPS net.Listener
	// HTTP answers certificate challenges and redirects to HTTPS. It needs
	// Config.Public.
	HTTP net.Listener
	// Agents is the agent listener. It needs Config.Identity.
	Agents net.Listener
	// Ingress serves the panel's domain to this server's proxy, while the
	// proxy holds the public ports. It is a unix socket that only the proxy
	// and root reach, which is all that makes what arrives on it HTTPS (ADR
	// 0024). It needs Config.Public.
	Ingress net.Listener
}

// Run serves on every listener in ls until ctx is done or one of them
// fails. A failure stops the others too.
//
// It stops in order, so a change under way isn't failed by the stop
// itself: first the API, so no new change starts; then the revision
// engine, which lets the steps under way finish for StepGrace and leaves
// what remains of their changes for the next start; then the agent
// listener, whose streams those steps need.
func (s *Server) Run(ctx context.Context, ls Listeners) error {
	if (ls.HTTPS != nil || ls.HTTP != nil || ls.Ingress != nil) && s.public == nil {
		return errors.New("control: the HTTPS, HTTP and ingress listeners need Config.Public")
	}
	if s.backups != nil {
		s.backups.Recover(ctx)
	}
	// An install's first alert rules are there before the API answers, so what
	// lists rules or changes reads them from the first request (ADR 0044).
	if s.alerts != nil {
		if err := s.alerts.MakeDefaults(ctx); err != nil && ctx.Err() == nil {
			s.logger.ErrorContext(ctx, "can't make the install's first alert rules; it tries again at the next start", slog.String("error", err.Error()))
		}
	}
	// And Dockwell's own catalog, which the catalog lists from the first request too.
	if s.catalogs != nil {
		if err := s.catalogs.MakeDefault(ctx); err != nil && ctx.Err() == nil {
			s.logger.ErrorContext(ctx, "can't give the install Dockwell's own catalog; it tries again at the next start", slog.String("error", err.Error()))
		}
	}
	apiCtx, stopAPI := context.WithCancel(ctx)
	defer stopAPI()
	agentCtx, stopAgents := context.WithCancel(context.WithoutCancel(ctx))
	defer stopAgents()

	var api, agents sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	failed := func(err error) {
		if err != nil {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}
	}
	run := func(serveFn func() error) {
		api.Go(func() {
			failed(serveFn())
			stopAPI()
		})
	}
	run(func() error { return s.Serve(apiCtx, ls.API) })
	if s.logs != nil {
		api.Go(func() { s.logs.KeepTelling(apiCtx) })
	}
	if s.alerts != nil && !s.manualAlerts {
		api.Go(func() { s.alerts.Run(apiCtx) })
		if runner, ok := s.alerts.Checks.(*checks.Runner); ok {
			api.Go(func() { runner.Run(apiCtx) })
		}
	}
	if s.backups != nil {
		api.Go(func() { s.backups.KeepRunning(apiCtx) })
	}
	if s.hosts != nil {
		api.Go(func() { s.hosts.Run(apiCtx) })
	}
	if s.follower != nil {
		api.Go(func() { s.follower.Run(apiCtx) })
	}
	if s.history != nil {
		api.Go(func() { s.history.Run(apiCtx) })
	}
	if s.autoupdates != nil && !s.manualAlerts {
		api.Go(func() { s.autoupdates.Run(apiCtx) })
	}
	if s.tasks != nil {
		s.tasks.Start(apiCtx)
	}
	if s.updates != nil {
		api.Go(func() { s.updates.Run(apiCtx) })
	}
	if ls.Ingress != nil {
		run(func() error {
			return serve.Serve(apiCtx, ls.Ingress, authapi.ViaIngress(s.api), serve.Options{Name: "panel behind the ingress", Logger: s.logger})
		})
	}
	s.ports.begin(apiCtx, func(err error) {
		failed(err)
		stopAPI()
	})
	if ls.HTTPS != nil {
		s.ports.serve(ls.HTTPS, ls.HTTP)
	}
	if ls.Agents != nil {
		agents.Go(func() {
			failed(s.ServeAgents(agentCtx, ls.Agents))
			stopAPI()
		})
	}
	api.Wait()
	s.ports.end()
	if s.backups != nil {
		s.backups.Stop()
	}
	if s.changes != nil {
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.stepGrace)
		if err := s.changes.Stop(grace); err != nil {
			s.logger.WarnContext(ctx, "the revision engine didn't stop cleanly", slog.String("error", err.Error()))
		}
		cancel()
	}
	stopAgents()
	agents.Wait()
	return errors.Join(errs...)
}

// fleetOf is the fleet a handler reads servers from, as the interface it takes, which
// a nil *fleet.Hub wouldn't read as nil.
func fleetOf(cfg Config) domains.Servers {
	if cfg.Fleet == nil {
		return nil
	}
	return cfg.Fleet
}

// agentURLs says where agents reach the control plane, as its authority records it:
// on the control server's own network, and from elsewhere.
func agentURLs(a *identity.Authority) func(ctx context.Context) (string, []string, error) {
	return func(ctx context.Context) (string, []string, error) {
		local, err := a.AgentURL(ctx)
		if err != nil {
			return "", nil, err
		}
		public, err := a.PublicAgentURLs(ctx)
		if err != nil {
			return "", nil, err
		}
		urls := make([]string, 0, len(public))
		for _, u := range public {
			urls = append(urls, u.URL)
		}
		return local, urls, nil
	}
}

// PanelDomain is what Settings needs to set the panel's domain, beyond the store
// (ADR 0063).
type PanelDomain struct {
	// From says where the domain in use comes from.
	From dockwellv1.PanelDomainFrom
	// Problem is why the domain the setting holds isn't served, when it isn't.
	Problem string
	// Certified reports whether Dockwell holds the domain's certificate.
	Certified func() bool
	// Resolve looks a name's addresses up. Nil looks nothing up, as in a test.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// PortsHeld says what holds port 80 or 443 other than Dockwell or its Caddy.
	PortsHeld func(ctx context.Context) []*dockwellv1.PanelPortHolder
	// Restart starts the control plane again, as it was started.
	Restart func()
	// Saved hears a domain as it's saved, which no route may claim from then.
	Saved func(domain string)
	// PublicAddresses replaces the control server's own, as a test does.
	PublicAddresses func() []string
}

// controlAddresses are the control server's public addresses, as it knows them
// (ADR 0034), where the panel's domain should lead.
func controlAddresses(hub *fleet.Hub) []string {
	if hub == nil {
		return nil
	}
	srv := hub.ControlServer()
	if srv == nil {
		return nil
	}
	var out []string
	for _, a := range []*dockwellv1.PublicAddress{srv.GetNetwork().GetIpv4(), srv.GetNetwork().GetIpv6()} {
		if a.GetAddress() != "" {
			out = append(out, a.GetAddress())
		}
	}
	return out
}

// fleetIDs lists the fleet's servers by ID, for a history row each minute.
func fleetIDs(hub *fleet.Hub) func() []string {
	return func() []string {
		listed := hub.List()
		ids := make([]string, 0, len(listed))
		for _, srv := range listed {
			ids = append(ids, srv.GetId())
		}
		return ids
	}
}

// serverID finds a server a request names, by ID or name.
func serverID(hub *fleet.Hub) func(named string) (string, error) {
	return func(named string) (string, error) {
		if srv, err := hub.Get(named); err == nil {
			return srv.GetId(), nil
		}
		for _, srv := range hub.List() {
			if srv.GetName() == named {
				return srv.GetId(), nil
			}
		}
		return "", connect.NewError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", named))
	}
}

// updateWords say each kind of update in a message.
var updateWords = map[dockwellv1.UpdateKind]string{
	dockwellv1.UpdateKind_UPDATE_KIND_PATCH:   "a patch",
	dockwellv1.UpdateKind_UPDATE_KIND_MINOR:   "a minor update",
	dockwellv1.UpdateKind_UPDATE_KIND_MAJOR:   "a major update",
	dockwellv1.UpdateKind_UPDATE_KIND_DIGEST:  "a new build of the same tag",
	dockwellv1.UpdateKind_UPDATE_KIND_RELEASE: "a catalog release",
	dockwellv1.UpdateKind_UPDATE_KIND_COMMIT:  "new commits",
}

// appUpdates says which apps have an update that counts, as the alerts read it
// (ADR 0079).
func appUpdates(keeper *serviceupdates.Keeper) func(ctx context.Context) ([]alerts.AppUpdate, error) {
	return func(ctx context.Context) ([]alerts.AppUpdate, error) {
		counted, err := keeper.Counting(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]alerts.AppUpdate, 0, len(counted))
		for _, c := range counted {
			o := c.Offer
			out = append(out, alerts.AppUpdate{
				ServiceID: c.ServiceID, Name: c.Name, ServerID: c.ServerID, From: o.GetFromVersion(), To: o.GetToVersion(),
				Major: o.GetKind() == dockwellv1.UpdateKind_UPDATE_KIND_MAJOR, Kind: updateWords[o.GetKind()],
			})
		}
		return out, nil
	}
}
