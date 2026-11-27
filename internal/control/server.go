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
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
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
	"github.com/dockwell/dockwell/internal/control/certificates"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/checks"
	"github.com/dockwell/dockwell/internal/control/cistatus"
	"github.com/dockwell/dockwell/internal/control/composeapi"
	"github.com/dockwell/dockwell/internal/control/connections"
	"github.com/dockwell/dockwell/internal/control/containers"
	"github.com/dockwell/dockwell/internal/control/databases"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/control/domains"
	"github.com/dockwell/dockwell/internal/control/events"
	"github.com/dockwell/dockwell/internal/control/files"
	"github.com/dockwell/dockwell/internal/control/firewall"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/githooks"
	"github.com/dockwell/dockwell/internal/control/github"
	"github.com/dockwell/dockwell/internal/control/gitkeys"
	"github.com/dockwell/dockwell/internal/control/gitlab"
	"github.com/dockwell/dockwell/internal/control/gitproviders"
	"github.com/dockwell/dockwell/internal/control/health"
	"github.com/dockwell/dockwell/internal/control/history"
	"github.com/dockwell/dockwell/internal/control/holds"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/control/images"
	"github.com/dockwell/dockwell/internal/control/importapi"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/control/members"
	"github.com/dockwell/dockwell/internal/control/opener"
	"github.com/dockwell/dockwell/internal/control/owners"
	"github.com/dockwell/dockwell/internal/control/processes"
	"github.com/dockwell/dockwell/internal/control/publish"
	"github.com/dockwell/dockwell/internal/control/registrylogins"
	"github.com/dockwell/dockwell/internal/control/resources"
	"github.com/dockwell/dockwell/internal/control/servers"
	serviceapi "github.com/dockwell/dockwell/internal/control/services"
	"github.com/dockwell/dockwell/internal/control/serviceupdates"
	"github.com/dockwell/dockwell/internal/control/settings"
	"github.com/dockwell/dockwell/internal/control/signinproviders"
	"github.com/dockwell/dockwell/internal/control/system"
	"github.com/dockwell/dockwell/internal/control/tasks"
	"github.com/dockwell/dockwell/internal/control/terminals"
	"github.com/dockwell/dockwell/internal/control/updates"
	"github.com/dockwell/dockwell/internal/control/variables"
	"github.com/dockwell/dockwell/internal/control/volumes"
	"github.com/dockwell/dockwell/internal/control/watchroutes"
	"github.com/dockwell/dockwell/internal/control/websites"
	"github.com/dockwell/dockwell/internal/controlbackup"
	"github.com/dockwell/dockwell/internal/gitref"
	"github.com/dockwell/dockwell/internal/https"
	"github.com/dockwell/dockwell/internal/panel"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/seal"
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
	// SigninClient reads issuers' discovery documents for DomainService's sign-in
	// providers. Nil means netguard's client, which a test with a provider on
	// loopback replaces.
	SigninClient signinproviders.Doer
	// Sources reads what a build's branch names (ADR 0046). Nil asks each
	// repository over Git's smart HTTP protocol (package gitref).
	Sources revision.Sources
	// GitKeysLocal allows SSH deploy keys to dial loopback hosts in tests and
	// under DOCKWELL_TEST=1 (ADR 0113).
	GitKeysLocal bool
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
	// GitHub is where GitHub's API and pages are, and the client that reaches
	// them, in place of github.com through netguard's, for tests (ADR 0098).
	GitHub GitHubConfig
	// GitLab is where GitLab calls go in tests, in place of netguard's client
	// (ADR 0111).
	GitLab GitLabConfig
	// ManualCIStatus leaves draining ci_status_outbox to a test calling
	// Server.CIStatus().DeliverOnce directly.
	ManualCIStatus bool
	// ManualAlerts leaves evaluating alert rules to whoever calls the evaluator,
	// as a test does by its own clock. The install's first rules are made all the
	// same.
	ManualAlerts bool
	// ManualBackups leaves looking at the backup schedule to whoever calls
	// RunDue, as a test does by its own clock: the control plane's own looks, as
	// it starts and every 30 s of real time, would run beside the test's. What
	// was running when it last stopped is failed all the same.
	ManualBackups bool
	// ManualConnectionChecks leaves checking each connection on its schedule to a
	// test, which calls the checker itself (#799).
	ManualConnectionChecks bool
	// ManualOwners leaves looking at catalog apps' owners to a test, which calls
	// the runner's Tick and Wait itself: the control plane's own look, as it
	// starts and every 10 s, would make an owner beside the test's (#854).
	ManualOwners bool
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
	ControlBackupKey              *seal.Key
	ControlBackupAddress          string
	ControlBackupLocalS3          bool
	ServiceManager                dockwellv1.ServiceManager
	// PublicAddress is this server's public IP address, shown on the claim
	// screen for pointing a domain at. Empty when it isn't known.
	PublicAddress string
	// ClaimCommand prints a new claim link on the control server, as it runs
	// there, such as "sudo dockwell claim", for the panel to show while the
	// install has no owner.
	ClaimCommand string
	// RecoverCommand prints a one-time sign-in link on the control server, as
	// ClaimCommand does a claim link, for the panel to show someone signed in as
	// their way back in (#801).
	RecoverCommand string
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
	// ECR gets Amazon ECR's sign-in tokens for registry logins (ADR 0086): AWS's
	// own unless a test passes one that reaches its own server.
	ECR *registrylogins.ECR
	// logins are the registry logins, read with Secrets from Store.
	logins *registrylogins.Logins
	// appHolds keeps held catalog installs held, and appOwners makes their owners
	// (ADR 0105).
	appHolds  *holds.Holds
	appOwners *owners.Runner
	// connections are the connections apps use (ADR 0099), and connectionChecks
	// where apiRoutes puts what checks them on a schedule (#799).
	connections      *connections.Connections
	connectionChecks **connections.Checker
	// ConnectionChecks checks a connection's bucket before an install links an app to
	// it: each server's agent, from a network made for the check, unless a test
	// passes one that stands in for a bucket on loopback, which no check reaches.
	ConnectionChecks catalogapi.ConnectionChecks
	// DNSAPI reaches a DNS provider's API with a token, for the DNS providers
	// (ADR 0088): Cloudflare's own unless a test passes one that reaches its own
	// server.
	DNSAPI func(token string) dnsproviders.API
	// dns are the DNS providers, read with Secrets from Store.
	dns *dnsproviders.Providers
	// publisher publishes routes' domains at Cloudflare, for the engine and the
	// live check (ADR 0104).
	publisher *publish.Publisher
	// CatalogDir is a folder of catalog entries that take over Dockwell's own of the
	// same ID, as catalog.DirEnv names one. Empty adds none.
	CatalogDir string
	// CertificateCA is the ACME directory certificates by DNS come from (ADR
	// 0088): Let's Encrypt's unless set. CertificateClient reaches it, with the
	// roots it's trusted by; CertificatePropagation waits for each challenge
	// record, and nil waits for none, as a test's CA reads its own DNS.
	CertificateCA          string
	CertificateClient      *http.Client
	CertificatePropagation certificates.Waiter
	// CertificatesOff says no certificate by DNS is got, as in a test with no CA
	// of its own.
	CertificatesOff bool
	// certificates gets, renews and keeps certificates by DNS.
	certificates *certificates.Manager
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
	// publisher reads what Dockwell published at Cloudflare on a timer (ADR 0104).
	publisher *publish.Publisher
	logger    *slog.Logger
	api       http.Handler
	agents    http.Handler
	agentTLS  *tls.Config
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
	history *history.Recorder
	// connectionChecks checks each connection on a schedule (#799), unless
	// manualConnectionChecks leaves that to a test.
	connectionChecks       *connections.Checker
	manualConnectionChecks bool
	manualAlerts           bool
	manualBackups          bool
	// manualOwners leaves the owners' looks to a test (#854).
	manualOwners bool
	// backups takes backups, and those under way end with the control plane.
	backups       *backups.Handler
	controlBackup *controlbackup.Manager
	// hooks deploys on push, and what deploys wait for ends with the control plane.
	hooks *githooks.Handler
	// apps are the GitHub Apps Dockwell made, which check their deliveries and
	// poll for pushes where GitHub can't reach the panel (ADR 0098).
	apps *gitproviders.Handler
	// ciStatus delivers durable CI deploy status transitions to GitHub and GitLab (ADR 0111).
	ciStatus       *cistatus.Worker
	manualCIStatus bool
	// tasks reads scheduled tasks' runs from the agents that run them.
	tasks *tasks.Handler
	// certificates gets and renews certificates by DNS (ADR 0088).
	certificates *certificates.Manager
	// updates checks what's newer than what each service runs (ADR 0079).
	updates *serviceupdates.Keeper
	public  https.Source
	changes *revision.Engine
	// hosts gives each server its host (ADR 0064), once its agent runs the kind.
	hosts *Hosts
	// owners makes catalog apps' owners (ADR 0105).
	owners *owners.Runner
	// follower moves the catalog sources that follow a branch along it (ADR 0077),
	// when Config.CatalogFollow asks it to.
	follower *catalogapi.Follower
	// connected reports a server whose agent is connected now, for what waits on one.
	connected func(server string) bool
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
	// Connecting GitHub posts the App's manifest to GitHub's page that makes it
	// (ADR 0098).
	ui.PostsTo(originOf(cmp.Or(cfg.GitHub.Web, github.Web)))
	if !ui.Built() {
		cfg.Logger.Warn("this binary has no panel built in; the API works, and make build adds the panel")
	}
	if cfg.Public != nil && (cfg.Auth == nil || cfg.Auth.Sites().Domain != cfg.Public.Domain()) {
		return nil, errors.New("control: Config.Public needs Config.Auth for the same domain")
	}
	var logs *logapi.Handler
	var backupHandler *backups.Handler
	var controlBackup *controlbackup.Manager
	var hooks *githooks.Handler
	var gitApps *gitproviders.Handler
	var ciStatusWorker *cistatus.Worker
	var taskHandler *tasks.Handler
	// Made first, so the settings can ask it who holds the public ports now.
	ports := &PublicPorts{ready: make(chan struct{})}
	var evaluator *alerts.Evaluator
	var updateKeeper *serviceupdates.Keeper
	var autoupdates *autoupdate.Runner
	var follower *catalogapi.Follower
	panelDomains := panelDomainsOf(cfg)
	// A pull signs in with the registry login its app's project may use (ADR 0086).
	if cfg.Store != nil && cfg.Secrets != nil {
		cfg.logins = registrylogins.New(cfg.Store, cfg.Secrets)
		cfg.connections = connections.NewConnections(cfg.Store, cfg.Secrets)
		cfg.connections.Logger = cfg.Logger
		if cfg.ECR != nil {
			cfg.logins.ECR = cfg.ECR
		}
		cfg.dns = dnsproviders.New(cfg.Store, cfg.Secrets)
		if cfg.DNSAPI != nil {
			cfg.dns.Cloudflare = cfg.DNSAPI
		}
		if !cfg.CertificatesOff {
			cfg.certificates = &certificates.Manager{
				DB: cfg.Store, Keeper: cfg.Secrets, DNS: cfg.dns, Directory: cfg.CertificateCA, HTTP: cfg.CertificateClient,
				Propagation: cfg.CertificatePropagation, Logger: cfg.Logger,
				PanelDomains: panelDomains,
				ControlServer: func() string {
					if cfg.Fleet != nil {
						if s := cfg.Fleet.ControlServer(); s != nil {
							return s.GetId()
						}
					}
					return ""
				},
			}
			if cfg.Agents != nil {
				cfg.certificates.Agents = certificateAgents{cfg.Agents}
				cfg.Agents.OnConnect(cfg.certificates.Reconnected)
			}
		}
	}
	// A server a change took out of the fleet: it leaves the fleet's list, its
	// stream ends now, and its certificates' keys are replaced and revoked at once
	// (ADR 0090).
	// Publishing a route's domain at Cloudflare is steps the engine takes (ADR 0104).
	if cfg.Changes != nil && cfg.dns != nil {
		cfg.publisher = publisher(&cfg)
		cfg.Changes.SetPublisher(cfg.publisher)
	}
	if cfg.Changes != nil {
		agents, certs, hub := cfg.Agents, cfg.certificates, cfg.Fleet
		cfg.Changes.OnRemoved(func(server string) {
			if hub != nil {
				hub.Removed(server)
			}
			if agents != nil {
				agents.Kick(server)
			}
			if certs != nil {
				certs.Wake()
			}
		})
	}
	// A held catalog install stays held through every write, and Dockwell makes its
	// owner where its entry says how (ADR 0105).
	if cfg.Changes != nil && cfg.Store != nil && cfg.Secrets != nil {
		cfg.appHolds = &holds.Holds{Keeper: cfg.Secrets}
		// Each routed domain gets an uptime check unless someone says not to (#835),
		// worked out from the edits as the holds left them.
		watch := &watchroutes.Watcher{Keeper: cfg.Secrets, Logger: cfg.Logger}
		cfg.Changes.SetEditHook(revision.ChainEditHooks(cfg.appHolds.Hook, watch.Hook))
		if cfg.Agents != nil {
			agents := cfg.Agents
			cfg.appOwners = &owners.Runner{
				DB: cfg.Store, Keeper: cfg.Secrets, Engine: cfg.Changes, Holds: cfg.appHolds, Logger: cfg.Logger,
				Agent: func(server string) (owners.Agent, bool) {
					session, ok := agents.Session(server)
					if !ok {
						return nil, false
					}
					return session, true
				},
			}
		}
	}
	cfg.connectionChecks = new(*connections.Checker)
	var histRecorder *history.Recorder
	if cfg.Identity != nil && cfg.Store != nil && cfg.Fleet != nil {
		histRecorder = &history.Recorder{DB: cfg.Store, Servers: fleetIDs(cfg.Fleet), Logger: cfg.Logger}
	}
	api, err := apiRoutes(cfg, ui, ports, histRecorder, &logs, &backupHandler, &controlBackup, &taskHandler, &evaluator, &updateKeeper, &autoupdates, &follower, &hooks, &gitApps, &ciStatusWorker)
	if err != nil {
		return nil, err
	}
	if cfg.Changes != nil {
		// Every change through the API carries who makes it, so the engine
		// refuses a person's that comes without (ADR 0049).
		cfg.Changes.RequireRoles()
	}
	s := &Server{logger: cfg.Logger, api: api, logs: logs, backups: backupHandler, controlBackup: controlBackup, hooks: hooks, apps: gitApps, ciStatus: ciStatusWorker, manualCIStatus: cfg.ManualCIStatus, tasks: taskHandler, updates: updateKeeper, alerts: evaluator, manualAlerts: cfg.ManualAlerts, manualBackups: cfg.ManualBackups, manualOwners: cfg.ManualOwners, public: cfg.Public, changes: cfg.Changes, store: cfg.Store, stepGrace: cfg.StepGrace, autoupdates: autoupdates, certificates: cfg.certificates, publisher: cfg.publisher, owners: cfg.appOwners}
	s.catalogs = follower
	s.connectionChecks, s.manualConnectionChecks = *cfg.connectionChecks, cfg.ManualConnectionChecks
	if cfg.Agents != nil {
		agents := cfg.Agents
		s.connected = func(server string) bool {
			_, ok := agents.Session(server)
			return ok
		}
	}
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
		if cfg.logins != nil {
			cfg.Agents.SendLogins(cfg.logins)
		}
	}
	// A service's containers are named after what people call its environment.
	if cfg.Changes != nil && cfg.Agents != nil && cfg.Store != nil {
		s.hosts = &Hosts{DB: cfg.Store, Engine: cfg.Changes, Agents: cfg.Agents, Logger: cfg.Logger, Fleet: cfg.Fleet}
	}
	if cfg.Agents != nil && cfg.Store != nil {
		cfg.Agents.NameScopes(scopeNames{db: cfg.Store})
	}
	if cfg.Identity != nil {
		opts := []agentapi.Option{agentapi.WithAgents(cfg.Agents)}
		if cfg.Store != nil {
			// What each container uses is placed by its service, for an app's figures (#797).
			opts = append(opts, agentapi.WithServices((&serviceIDs{db: cfg.Store}).find), agentapi.WithArtifactPlans(cfg.Store))
		}
		if histRecorder != nil {
			s.history = histRecorder
			opts = append(opts, agentapi.WithHistory(s.history))
		}
		s.agents = agentapi.NewHandler(cfg.Identity, cfg.Fleet, cfg.Logger, opts...)
		s.agentTLS, s.addAgentHost = agentapi.TLSConfig(cfg.Identity, cfg.AgentHosts)
	}
	return s, nil
}

// Handler returns the API listener's root handler.
func (s *Server) Handler() http.Handler { return s.api }

// CIStatus returns the CI deploy status delivery worker, if configured.
func (s *Server) CIStatus() *cistatus.Worker { return s.ciStatus }

func apiRoutes(cfg Config, ui http.Handler, ports *PublicPorts, hist *history.Recorder, logsOut **logapi.Handler, backupsOut **backups.Handler, controlBackupOut **controlbackup.Manager, tasksOut **tasks.Handler, alertsOut **alerts.Evaluator, updatesOut **serviceupdates.Keeper, autoupdates **autoupdate.Runner, followOut **catalogapi.Follower, hooksOut **githooks.Handler, appsOut **gitproviders.Handler, ciStatusOut **cistatus.Worker) (http.Handler, error) {
	opts := append(serve.HandlerOptions(cfg.Logger), connect.WithInterceptors(authapi.Interceptor(cfg.Auth, cfg.Logger)))
	mux := http.NewServeMux()
	var services []string
	var rest []*vanguard.Service
	// The tokens a check of the panel's domain behind a tunnel asks for.
	reach := settings.NewReach()
	guardREST := func(h http.Handler) http.Handler { return h }
	var filesHandler http.Handler
	var databasesHandler http.Handler
	var artifactHandler *containers.Handler
	var appOpener *opener.Opener
	var firewallHandler *firewall.Handler
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
		if d := cfg.PanelDomain.BehindProxy; d != "" {
			serving.Domain, serving.BehindProxy = d, true
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
			Reach:           reach, ReachClient: cfg.PanelDomain.ReachClient, Inside: cfg.PanelDomain.ReachInside,
		}
		if domain.ReachClient == nil {
			domain.ReachClient = settings.ReachClient()
		}
		if cfg.PanelDomain.PublicAddresses != nil {
			domain.PublicAddresses = cfg.PanelDomain.PublicAddresses
		}
		handle(dockwellv1connect.NewSettingsServiceHandler(settings.New(cfg.Store, serving, plane).WithDomain(domain), opts...))
		// For a person signed in alone, as its events name where people signed in from.
		if cfg.Auth != nil {
			handle(dockwellv1connect.NewAuditServiceHandler(audit.New(cfg.Store, cfg.Changes, cfg.Logger), opts...))
			// What someone removed had planned is discarded through the engine, where there is one.
			var planned members.Changes
			if cfg.Changes != nil {
				planned = cfg.Changes
			}
			handle(dockwellv1connect.NewMemberServiceHandler(members.New(cfg.Store, cfg.Auth, planned, cfg.Logger), opts...))
		}
	}
	if cfg.Auth != nil {
		controlServer := func() *dockwellv1.Server { return nil }
		if cfg.Fleet != nil {
			controlServer = cfg.Fleet.ControlServer
		}
		h := authapi.New(authapi.Config{
			Auth: cfg.Auth, Logger: cfg.Logger, ControlServer: controlServer, PublicAddress: cfg.PublicAddress, ClaimCommand: cfg.ClaimCommand, RecoverCommand: cfg.RecoverCommand,
		})
		handle(dockwellv1connect.NewAuthServiceHandler(h, opts...))
		handle(dockwellv1connect.NewTokenServiceHandler(h, opts...))
	}
	if cfg.Fleet != nil {
		serverHandler := servers.New(cfg.Fleet, cfg.Store, cfg.Agents).Tickets(cfg.Identity, cfg.Auth).History(hist)
		if cfg.Changes != nil {
			// A unit an action moved reads anew for whoever watches its server (ADR 0082).
			cfg.Changes.OnUnit(serverHandler.ReadSystemAgain)
		}
		handle(dockwellv1connect.NewServerServiceHandler(serverHandler, opts...))
		handle(dockwellv1connect.NewImageServiceHandler(images.New(cfg.Fleet, cfg.Agents, cfg.logins), opts...))
		firewallHandler = firewall.New(cfg.Fleet, cfg.Agents).WithSSHChanges(cfg.Auth, cfg.Store, cfg.Logger)
		handle(dockwellv1connect.NewFirewallServiceHandler(firewallHandler, opts...))
	}
	if cfg.Changes != nil {
		ch := changes.New(cfg.Changes, cfg.Auth, cfg.Store, cfg.Logger)
		ch.SetToolboxValues(cfg.Secrets)
		if firewallHandler != nil {
			firewallHandler.SetSSHTrialAccess(ch.AppliedFrom)
		}
		if cfg.certificates != nil {
			ch.SetRemovalNotes(cfg.certificates.RemovalNotes)
		}
		handle(dockwellv1connect.NewChangeServiceHandler(ch, opts...))
		own := containers.New(cfg.Agents, ch, cfg.Store, cfg.Logger)
		ch.DiscardDockerArchives = own.DiscardDockerArchives
		imports := composeapi.New(ch, own, cfg.Changes, cfg.Store, cfg.Secrets, cfg.Logger, cfg.CatalogClient)
		handle(dockwellv1connect.NewComposeServiceHandler(imports, opts...))
		catalogSources := &catalogapi.Sources{Dir: cfg.CatalogSources, Client: cfg.CatalogClient}
		catalogHandler := catalogapi.New(imports, ch, cfg.Store, cfg.Secrets, catalogSources, cfg.Build.Commit, cfg.Logger)
		catalogHandler.EntriesFrom(cfg.CatalogDir)
		if cfg.Auth != nil {
			// A setup link takes a person's passkey (ADR 0105).
			catalogHandler.SetStepUps(cfg.Auth)
		}
		// A route that moves a catalog app's address moves what follows it (ADR 0103).
		ch.Address = catalogHandler.FollowAddress
		if cfg.appHolds != nil {
			var retrier catalogapi.OwnerRetrier
			if cfg.appOwners != nil {
				retrier = cfg.appOwners
			}
			catalogHandler.SetHolds(cfg.appHolds, retrier)
			if cfg.appOwners != nil {
				// The steps that make an owner are its catalog entry's (ADR 0105).
				cfg.appOwners.SetRuns(catalogHandler)
			}
		}
		*followOut = catalogHandler.Follow(cfg.Changes)
		follower := *followOut
		if cfg.Fleet != nil {
			// Another store's entry pins its images through the agent it goes on.
			catalogHandler.PinWith(images.New(cfg.Fleet, cfg.Agents, cfg.logins))
		}
		handle(dockwellv1connect.NewCatalogServiceHandler(catalogHandler, opts...))
		// A source's images, for whoever may read the catalog, as an image's request
		// carries the session's cookie (ADR 0077).
		mux.Handle(catalogapi.MediaRoute, catalogSources.Media(func(r *http.Request) error {
			return authapi.Authorizer(cfg.Auth, cfg.Logger)(r.Context(), dockwellv1connect.CatalogServiceListCatalogEntriesProcedure)
		}))
		sites := websites.New(ch, cfg.Changes, cfg.Store, cfg.Secrets, cfg.Agents, cfg.Logger)
		sites.DNS = cfg.dns
		handle(dockwellv1connect.NewWebsiteServiceHandler(sites, opts...))
		handle(dockwellv1connect.NewEventServiceHandler(events.New(cfg.Changes, cfg.Store, cfg.Logger, cfg.EventPoll), opts...))
		handle(dockwellv1connect.NewResourceServiceHandler(resources.New(cfg.Changes.Kinds(), cfg.Store, cfg.Logger), opts...))
		if cfg.Secrets != nil {
			handle(dockwellv1connect.NewVolumeServiceHandler(volumes.New(cfg.Store, cfg.Secrets, cfg.Agents, cfg.Logger), opts...))
		}
		if cfg.Secrets != nil && cfg.Auth != nil {
			handle(dockwellv1connect.NewVariableServiceHandler(
				variables.New(cfg.Changes.Kinds(), cfg.Store, cfg.Secrets, cfg.Auth, cfg.Logger), opts...))
		}
		domainHandler := domains.New(cfg.Changes.Kinds(), cfg.Store, cfg.Agents, fleetOf(cfg), cfg.Logger)
		domainHandler.DNS, domainHandler.Certificates = cfg.dns, cfg.certificates
		domainHandler.Changes, domainHandler.Agents, domainHandler.Auth = ch, cfg.Agents, cfg.Auth
		domainHandler.PanelDomains = panelDomainsOf(cfg)
		if cfg.publisher != nil {
			domainHandler.Cloudflare = cfg.publisher
		}
		domainHandler.Lookup = cfg.DNSLookup
		if cfg.Secrets != nil {
			domainHandler.Providers = signinproviders.New(cfg.Store, cfg.Secrets)
			if cfg.SigninClient != nil {
				domainHandler.Providers.Client = cfg.SigninClient
			}
			// The same providers sign people in to the panel, where one says to (ADR 0095).
			if cfg.Auth != nil {
				cfg.Auth.UseProviders(panelProviders(domainHandler.Providers), domainHandler.Providers.Client)
			}
		}
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
		base := cfg.Sources
		if base == nil {
			// A test's Git server is on this machine.
			base = &gitref.Resolver{Local: testing.Testing()}
		}
		// A private repository reads through the GitHub App's installation or
		// project SSH deploy key its build names (ADR 0098, ADR 0113).
		appSources := &gitproviders.Sources{Base: base}
		keySources := &gitkeys.Sources{Base: appSources}
		var sources revision.Sources = keySources
		cfg.Changes.SetSources(sources)
		serviceHandler := serviceapi.New(ch, cfg.Changes, cfg.Store, cfg.Secrets, cfg.Logger)
		serviceHandler.Installs = catalogHandler
		serviceHandler.Sources = sources
		serviceHandler.CIImages = images.New(cfg.Fleet, cfg.Agents, cfg.logins)
		serviceHandler.Agents = cfg.Agents
		*autoupdates = &autoupdate.Runner{
			DB: cfg.Store, Keeper: cfg.Secrets, Planner: serviceHandler, Engine: cfg.Changes, Logger: cfg.Logger,
		}
		if cfg.Fleet != nil {
			(*autoupdates).Images = images.New(cfg.Fleet, cfg.Agents, cfg.logins)
		}
		var canceller githooks.Canceller
		var buildHandler *builds.Handler
		if cfg.Agents != nil {
			buildHandler = builds.New(cfg.Store, cfg.Agents, serviceHandler, cfg.Logger)
			buildHandler.SetSources(sources)
			own.SetBuilds(buildHandler)
			cfg.Changes.SetDetector(buildHandler)
			serviceHandler.Builds = buildHandler
			canceller = buildHandler
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
			// Until it has a domain, an app is reached at its Open address, which what
			// follows its address says, where the panel opens apps: served on localhost
			// alone (ADR 0061). Served at a domain, the panel offers a tunnel to the
			// app's own port instead, which the loopback address is (ADR 0103; #779's
			// review, H2).
			if cfg.Public == nil && cfg.PanelDomain.BehindProxy == "" {
				at := ".localhost"
				if _, port, err := net.SplitHostPort(cfg.PanelListen); err == nil && port != "" {
					at += ":" + port
				}
				catalogHandler.Open = func(serviceID string) string { return "http://" + appOpener.Label(serviceID) + at }
			}
		}
		// What's newer than what each service runs, checked through its server's
		// agent, and a catalog install's read from its catalog (ADR 0079).
		if cfg.Agents != nil && cfg.Fleet != nil && cfg.Secrets != nil {
			keeper := &serviceupdates.Keeper{
				DB: cfg.Store, Secrets: cfg.Secrets, Agents: serviceupdates.FromAgents(cfg.Agents), Sources: sources, Logger: cfg.Logger,
				Logins: loginsOrNil(cfg.logins),
				Catalog: func(ctx context.Context) (map[string]serviceupdates.CatalogInstall, error) {
					upgrades, err := catalogHandler.InstallUpgrades(ctx)
					if err != nil {
						return nil, err
					}
					out := make(map[string]serviceupdates.CatalogInstall, len(upgrades))
					for project, u := range upgrades {
						out[project] = serviceupdates.CatalogInstall{FromVersion: u.FromVersion, Release: u.Release, ToVersion: u.ToVersion, ToRelease: u.ToRelease, Notes: serviceupdates.NotesOf(u.AppSource), Since: u.Since, WithdrawnTo: u.WithdrawnTo, Unavailable: u.Unavailable}
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
		var apps *gitproviders.Handler
		glClient := &gitlab.Client{HTTP: cfg.GitLab.Client, TestBase: cfg.GitLab.Base}
		if cfg.Auth != nil && cfg.Secrets != nil {
			// GitHub as an App Dockwell makes: connecting it, its installations, and
			// the repositories a project's apps may build from (ADR 0098), plus GitLab
			// connections for CI deploy status reporting (ADR 0111).
			apps = gitproviders.New(gitproviders.Config{
				Auth: cfg.Auth, DB: cfg.Store, Keeper: cfg.Secrets, Changes: ch, Logger: cfg.Logger,
				API: cfg.GitHub.API, Web: cfg.GitHub.Web, Client: cfg.GitHub.Client, GitLab: glClient,
				Now: cfg.GitHub.Now, BeforeChange: cfg.GitHub.BeforeChange,
			})
			handle(dockwellv1connect.NewGitProviderServiceHandler(apps, opts...))
			cfg.Auth.SetDeployRepoCheck(apps.ValidateDeployRepositoryInTx)
			appSources.SetApps(apps)
			deployKeys := gitkeys.New(gitkeys.Config{
				Auth: cfg.Auth, DB: cfg.Store, Keeper: cfg.Secrets, Changes: ch, Logger: cfg.Logger,
				Local: cfg.GitKeysLocal || testing.Testing(),
			})
			handle(dockwellv1connect.NewGitDeployKeyServiceHandler(deployKeys, opts...))
			keySources.SetKeys(deployKeys)
			logins := &gitkeys.MultiGitLogins{Keys: deployKeys, Fallback: apps}
			ch.SetGitLogins(logins)
			if cfg.Agents != nil {
				cfg.Agents.SendGitAuth(apps.AuthorizationIn)
				cfg.Agents.SendDockerBuildGitAuth(apps.DockerBuildAuthorization)
				cfg.Agents.SendGitPack(deployKeys.FetchPackIn)
			}
			if buildHandler != nil {
				// A form's readings fetch a private repository as the plan will.
				buildHandler.SetGitLogins(logins)
			}
		}
		if cfg.Store != nil {
			panelFn := func() string { return livePanelLinkBase(cfg) }
			cfg.Changes.SetPanelURL(panelFn)
			var targets cistatus.Targets
			if apps != nil {
				targets = ciStatusTargets{apps: apps}
				ch.GitHubPermissionsURL = apps.GitHubPermissionsURL
				serviceHandler.GitHubPermissionsURL = apps.GitHubPermissionsURL
			}
			worker := cistatus.New(cistatus.Config{
				DB:       cfg.Store,
				Targets:  targets,
				GitLab:   glClient,
				Logger:   cfg.Logger,
				PanelURL: panelFn,
				Now:      cfg.GitHub.Now,
			})
			cfg.Changes.OnCIStatus(worker.Wake)
			ch.RetryCIStatus = worker.Retry
			*ciStatusOut = worker
		}
		if cfg.Auth != nil {
			// Deploy on push: the API that turns it on, and the receivers its hooks'
			// URLs name and the GitHub App's webhook names, which face the internet
			// on the panel's address (ADR 0046, ADR 0098).
			hooksConfig := githooks.Config{
				Auth: cfg.Auth, DB: cfg.Store, Changes: ch, Engine: cfg.Changes, Services: serviceHandler,
				Builds: canceller, Sources: sources, Logger: cfg.Logger,
			}
			if apps != nil {
				hooksConfig.Apps = apps
			}
			*hooksOut = githooks.New(hooksConfig)
			handle(dockwellv1connect.NewGitHookServiceHandler(*hooksOut, opts...))
			mux.Handle("POST "+githooks.Path+"{id}", *hooksOut)
			mux.Handle("POST "+githooks.AppPath+"{id}", http.HandlerFunc((*hooksOut).ServeGitHubApp))
			if apps != nil {
				// What polling finds deploys as a push would.
				apps.SetPusher(*hooksOut)
				*appsOut = apps
			}
		}
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
		if certs := cfg.certificates; certs != nil {
			// What servers that went had and isn't revoked yet (ADR 0090).
			(*alertsOut).Revocations = func(ctx context.Context) ([]alerts.Revocation, error) {
				list, err := certs.Revocations(ctx)
				out := make([]alerts.Revocation, 0, len(list))
				for i := range list {
					r := &list[i]
					out = append(out, alerts.Revocation{Serial: r.Serial, Names: r.Names, Servers: r.Servers, Message: r.Message, Since: r.Since})
				}
				return out, err
			}
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
		handle(dockwellv1connect.NewDockerServiceHandler(own, opts...))
		artifactHandler = own
		*backupsOut = backups.New(ch, cfg.Changes, cfg.Agents, cfg.Store, cfg.Secrets, cfg.Logger)
		// A backup restores into a new database the catalog way (ADR 0106).
		(*backupsOut).InstallWith(imports.PlanInstall)
		handle(dockwellv1connect.NewBackupServiceHandler(*backupsOut, opts...))
		var controlBackupHandler dockwellv1connect.ControlBackupServiceHandler = dockwellv1connect.UnimplementedControlBackupServiceHandler{}
		if cfg.ControlBackupKey != nil && cfg.Identity != nil && cfg.Auth != nil {
			manager := controlbackup.New(controlbackup.Config{
				DB: cfg.Store, Auth: cfg.Auth, Key: cfg.ControlBackupKey, KeyFile: cfg.KeyFile,
				Install: cfg.Identity.InstallID(), ControlID: cfg.Fleet.ControlServer().GetId(),
				Address: cfg.ControlBackupAddress,
				Build:   cfg.Build, Logger: cfg.Logger, Agents: cfg.Agents,
				Destination: (*backupsOut).ControlDestination, LocalS3: cfg.ControlBackupLocalS3,
			})
			if err := manager.Init(context.Background()); err != nil {
				return nil, fmt.Errorf("prepare control plane backups: %w", err)
			}
			*controlBackupOut = manager
			controlBackupHandler = manager
		}
		handle(dockwellv1connect.NewControlBackupServiceHandler(controlBackupHandler, opts...))
		dbs := databases.New(ch, cfg.Agents, *backupsOut, cfg.Store, cfg.Secrets, cfg.Logger)
		dbs.ImportWith(func(ctx context.Context, ref resource.Ref, source resource.RestoreSource, summary string) (*dockwellv1.Change, error) {
			return ch.Planned(ctx, func(who revision.By) (*revision.Plan, error) {
				return cfg.Changes.RestoreWith(ctx, ref, source, revision.RestoreAs{}, who, summary)
			})
		})
		cfg.Changes.FeaturesFrom(dbs.LastFeatures)
		// The database servers an app may share, as ways to meet its needs (ADR 0084).
		catalogHandler.ShareWith(dbs)
		// An app's database, alone, backs up and restores from the app's page.
		catalogHandler.BackUpWith(*backupsOut)
		dbPath, dbH := dockwellv1connect.NewDatabaseServiceHandler(dbs, opts...)
		handle(dbPath, dbH)
		databasesHandler = dbH
		if cfg.connections != nil && cfg.Fleet != nil {
			// The connections apps use, their check from where an app is, and an app
			// connected to one (ADR 0099).
			conns := connections.New(cfg.connections, cfg.Fleet, cfg.Agents, ch, cfg.Store)
			*cfg.connectionChecks = conns.Checks()
			handle(dockwellv1connect.NewConnectionServiceHandler(conns, opts...))
			// An install checks a connection's bucket from where the app is to run
			// before it links the app to it.
			var bucketChecks catalogapi.ConnectionChecks = conns
			if cfg.ConnectionChecks != nil {
				bucketChecks = cfg.ConnectionChecks
			}
			catalogHandler.CheckWith(bucketChecks)
		}
		*tasksOut = tasks.New(cfg.Changes, cfg.Agents, cfg.Store, cfg.Logger)
		handle(dockwellv1connect.NewTaskServiceHandler(*tasksOut, opts...))
	}
	authorize := authapi.Authorizer(cfg.Auth, cfg.Logger)
	restPaths, err := restHandler(rest, authorize)
	if err != nil {
		return nil, err
	}
	mux.Handle(authapi.RESTPrefix, guardREST(restPaths))
	if artifactHandler != nil {
		streamArtifacts(mux, artifactHandler, guardREST, authapi.TrackInstallStream(cfg.Auth, cfg.Logger))
	}
	if filesHandler != nil {
		streamTransfers(mux, filesHandler, guardREST, authorize)
	}
	if databasesHandler != nil {
		streamDatabaseImports(mux, databasesHandler, guardREST, authorize)
	}
	mux.Handle("GET "+apidoc.Path, apidoc.Handler())

	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker(services...), opts...))
	reflector := grpcreflect.NewStaticReflector(services...)
	mux.Handle(grpcreflect.NewHandlerV1(reflector, opts...))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector, opts...))

	if cfg.Auth != nil {
		mux.Handle("GET "+auth.ProviderCallbackPath, authapi.ProviderCallback(cfg.Auth, cfg.Logger))
	}

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
	// panel's guard, which answers none but the panel's (ADR 0061). Behind a tunnel
	// or a proxy that ends TLS, what it sends for the panel's domain reads as the
	// HTTPS it came over, and a check of the way in is answered whatever its host
	// (ADR 0063's amendment).
	return serve.Recover(cfg.Logger, reach.Serve(authapi.ViaProxySeen(cfg.PanelDomain.BehindProxy, cfg.PanelDomain.ProxiedPort,
		opener.Dispatch(appOpener, authapi.Guard(sites, cfg.Logger, mux))))), nil
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
	if s.hooks != nil {
		s.hooks.Recover(ctx)
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
	if s.backups != nil && !s.manualBackups {
		api.Go(func() { s.backups.KeepRunning(apiCtx) })
	}
	if s.controlBackup != nil && !s.manualBackups {
		api.Go(func() { s.controlBackup.Run(apiCtx) })
	}
	if s.hooks != nil {
		api.Go(func() { s.hooks.KeepForgetting(apiCtx) })
	}
	if s.apps != nil {
		api.Go(func() { s.apps.KeepWatching(apiCtx) })
	}
	if s.ciStatus != nil && !s.manualCIStatus {
		api.Go(func() { s.ciStatus.Run(apiCtx) })
	}
	if s.owners != nil && !s.manualOwners {
		api.Go(func() { s.owners.Run(apiCtx) })
	}
	if s.hosts != nil {
		api.Go(func() { s.hosts.Run(apiCtx) })
		api.Go(func() { s.hosts.FollowLabels(apiCtx) })
	}
	// What Dockwell published at Cloudflare is read every few minutes (ADR 0104).
	if s.publisher != nil && !s.manualAlerts {
		api.Go(func() { s.publisher.Watch(apiCtx, s.logger) })
	}
	if s.certificates != nil && !s.manualAlerts {
		api.Go(func() { s.certificates.Run(apiCtx) })
	}
	// Each connection is checked once it's saved or changed, and every 6 hours (#799).
	if s.connectionChecks != nil && !s.manualConnectionChecks {
		api.Go(func() { s.connectionChecks.Run(apiCtx) })
	}
	// Installs from before Dockwell showed an app's plain values show them, once its
	// servers' agents are back (ADR 0081).
	if s.catalogs != nil && s.connected != nil {
		api.Go(func() { s.catalogs.KeepShowing(apiCtx, s.connected) })
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
	if s.hooks != nil {
		s.hooks.Stop()
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

func panelDomainsOf(cfg Config) func() []string {
	return func() []string {
		var out []string
		if cfg.Auth != nil {
			out = append(out, cfg.Auth.Sites().Domain)
		}
		// And as it's saved, which the route kind holds (ADR 0024).
		if cfg.Changes != nil {
			if t, err := cfg.Changes.Kinds().Type("route"); err == nil {
				if p, ok := t.(revision.PanelDomains); ok {
					out = append(out, p.PanelDomains()...)
				}
			}
		}
		return out
	}
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
	// BehindProxy is the panel's domain where a tunnel or a proxy that ends TLS
	// serves it, in Public's place: requests for it come to the loopback listener
	// from the proxy on this server (ADR 0063's amendment).
	BehindProxy string
	// ProxiedPort hears the port of each request the proxy sends for BehindProxy,
	// as its Host names it, or 443: the firewall's plan counts it a way in
	// (#850's re-check, N1). Nil hears none.
	ProxiedPort func(port uint16)
	// ReachClient asks a domain behind a tunnel or proxy for the token its check
	// makes. Nil takes settings.ReachClient, guarded as what a person names is.
	ReachClient settings.Doer
	// ReachInside says which addresses that check reaches lead inside. Nil takes
	// settings.Inside.
	ReachInside func(netip.Addr) bool
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
				Major: o.GetKind() == dockwellv1.UpdateKind_UPDATE_KIND_MAJOR,
				Build: o.GetKind() == dockwellv1.UpdateKind_UPDATE_KIND_DIGEST, Kind: updateWords[o.GetKind()],
			})
		}
		return out, nil
	}
}

// loginsOrNil is logins as an interface that's nil for none, not a nil pointer.
func loginsOrNil(logins *registrylogins.Logins) serviceupdates.Logins {
	if logins == nil {
		return nil
	}
	return logins
}

// certificateAgents gives the certificate manager the agents' sessions (ADR 0088).
type certificateAgents struct{ agents *agentapi.Agents }

func (c certificateAgents) Connected() []string { return c.agents.Connected() }

func (c certificateAgents) Loader(server string) (certificates.Loader, bool) {
	s, ok := c.agents.Session(server)
	if !ok {
		return nil, false
	}
	return s, true
}

// panelProviders reads the saved sign-in providers as the panel's sign-in takes
// them, secrets and all, which go no further than the control plane (ADR 0095).
func panelProviders(p *signinproviders.Providers) func(context.Context) ([]auth.Provider, error) {
	return func(ctx context.Context) ([]auth.Provider, error) {
		saved, err := p.All(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]auth.Provider, 0, len(saved))
		for _, s := range saved {
			out = append(out, auth.Provider{
				ID: s.ID, Name: s.Name, DisplayName: s.Spec.GetDisplayName(),
				Issuer: s.Spec.GetIssuer(), ClientID: s.Spec.GetClientId(), ClientSecret: s.Spec.GetClientSecret(),
				Scopes: s.Spec.GetScopes(), GroupsClaim: s.Spec.GetGroupsClaim(), Panel: s.Spec.GetPanel(),
			})
		}
		return out, nil
	}
}

// originOf is the scheme and host of raw, with its port, or "" where raw isn't an
// http or https URL.
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// GitHubConfig points the GitHub App's calls elsewhere, for tests. Empty fields
// keep GitHub's own.
type GitHubConfig struct {
	API, Web string
	Client   github.Doer
	// Now is the App's clock: a test's moves, as its webhook's old secret ends.
	Now func() time.Time
	// BeforeChange runs between reading an App and keeping a change to it: a
	// test's, to land another change there.
	BeforeChange func()
}

// GitLabConfig points GitLab calls elsewhere, for tests. Empty fields use
// netguard against the configured GitLab URL.
type GitLabConfig struct {
	Base   string
	Client gitlab.Doer
}

// livePanelLinkBase returns the panel's public HTTPS base URL for outbound links
// (such as GitHub Check Run details_url and GitLab Commit Status target_url).
// It reads the live panel_domain setting when configured and returns "" when no
// public address is set, never leaking a private listen address.
func livePanelLinkBase(cfg Config) string {
	switch {
	case cfg.PanelURL != "":
		return strings.TrimRight(cfg.PanelURL, "/")
	case cfg.PanelDomain.BehindProxy != "":
		return "https://" + strings.TrimRight(cfg.PanelDomain.BehindProxy, "/")
	}
	if cfg.Store != nil {
		if d, err := settings.PanelDomain(context.Background(), cfg.Store); err == nil && strings.TrimSpace(d) != "" {
			return "https://" + strings.TrimRight(strings.TrimSpace(d), "/")
		}
	}
	return ""
}

type ciStatusTargets struct {
	apps *gitproviders.Handler
}

func (t ciStatusTargets) FindGitHubStatusTarget(ctx context.Context, repoURL, serviceID string) (*cistatus.GitHubTarget, string, error) {
	got, skip, err := t.apps.FindGitHubStatusTarget(ctx, repoURL, serviceID)
	if err != nil || got == nil {
		return nil, skip, err
	}
	return &cistatus.GitHubTarget{
		App:            got.App,
		AppID:          got.AppID,
		AppName:        got.AppName,
		InstallationID: got.InstallationID,
		RepositoryID:   got.RepositoryID,
		FullName:       got.FullName,
		CredentialRef:  got.CredentialRef,
	}, "", nil
}

func (t ciStatusTargets) VerifyChecksPermission(ctx context.Context, appID string, installationID int64) bool {
	return t.apps.VerifyChecksPermission(ctx, appID, installationID)
}

func (t ciStatusTargets) FindGitLabStatusTarget(ctx context.Context, repoURL, serviceID string) (*cistatus.GitLabTarget, string, error) {
	got, skip, err := t.apps.FindGitLabStatusTarget(ctx, repoURL, serviceID)
	if err != nil || got == nil {
		return nil, skip, err
	}
	return &cistatus.GitLabTarget{
		ConnectionID:  got.ConnectionID,
		Revision:      got.Revision,
		Name:          got.Name,
		BaseURL:       got.BaseURL,
		Token:         got.Token,
		ProjectPath:   got.ProjectPath,
		CredentialRef: got.CredentialRef,
	}, "", nil
}

func (t ciStatusTargets) NoteGitLabStatus(connID string, rev int, state dockwellv1.GitLabConnectionState, detail string) {
	t.apps.NoteGitLabStatus(connID, rev, state, detail)
}
