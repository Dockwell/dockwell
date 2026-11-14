// SPDX-License-Identifier: AGPL-3.0-only

// Package resource is the model every managed thing shares: a kind, the
// scope it lives in, a name, and a spec. A kind's level says what its
// scope is: a server for host-level kinds, an environment for services, a
// project for environments, and the organization for projects. Each kind
// that runs on servers has a driver that observes, plans, applies and
// verifies it there. The revision package builds history, plans, undo and
// drift on top, the same way for every kind. See
// docs/adr/0009-resources-revisions-and-plans.md and
// docs/adr/0014-object-model-and-store.md.
package resource

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/lazyre"

	"google.golang.org/protobuf/proto"
)

// Kind names a type of resource, such as "container" or "firewall.rule".
type Kind string

// Ref names one resource.
type Ref struct {
	Kind Kind
	// Scope is the ID of what the resource lives in, which its kind's level
	// says: a server (srv_…), an environment (env_…), a project (prj_…) or
	// the organization (org_…).
	Scope string
	Name  string // unique among resources of the kind in the scope
}

func (r Ref) String() string { return fmt.Sprintf("%s %s in %s", r.Kind, r.Name, r.Scope) }

// Level says what a kind's resources live in, and so where applying them
// runs.
type Level int

// Levels.
const (
	// LevelServer kinds are host-level: each lives on one server and
	// applies there, such as a firewall rule or a container run by hand.
	LevelServer Level = iota + 1
	// LevelEnvironment kinds live in an environment and are placed on
	// servers, where each revision is deployed, such as a service.
	LevelEnvironment
	// LevelProject kinds live in a project, and apply on the control plane
	// alone, such as an environment.
	LevelProject
	// LevelOrganization kinds live in the organization, and apply on the
	// control plane alone, such as a project.
	LevelOrganization
)

var levelNames = [...]string{"", "server", "environment", "project", "organization"}

func (l Level) String() string {
	if l > 0 && int(l) < len(levelNames) {
		return levelNames[l]
	}
	return fmt.Sprintf("level(%d)", int(l))
}

// RunsOnServers reports whether the level's resources are applied on
// servers, rather than on the control plane alone.
func (l Level) RunsOnServers() bool { return l == LevelServer || l == LevelEnvironment }

// Action is what applying does to one resource.
type Action int

// Actions, from least to most disruptive.
const (
	ActionNone    Action = iota // already as desired
	ActionCreate                // made for the first time
	ActionUpdate                // changed in place
	ActionReplace               // removed and made again, such as a recreated container
	ActionDelete                // removed
)

var actionNames = [...]string{"none", "create", "update", "replace", "delete"}

func (a Action) String() string {
	if int(a) < len(actionNames) {
		return actionNames[a]
	}
	return fmt.Sprintf("action(%d)", int(a))
}

// Effect is what a step does to whatever the resource serves, beyond the
// change itself. The panel words it ("Recreate", "Restart").
type Effect int

// Effects.
const (
	EffectNone     Effect = iota // nothing stops
	EffectRestart                // the process restarts in place
	EffectRecreate               // it is removed and made again
	EffectRoll                   // a new one takes over once it is healthy, with nothing stopped
)

// Warning flags a step that may hurt in a way the action alone doesn't
// say. The panel words it.
type Warning int

// Warnings.
const (
	// WarningMayLockOut means the step may cut off remote access to the
	// server, such as a rule that blocks SSH from everywhere.
	WarningMayLockOut Warning = iota + 1
)

// AccessGuard is a kind that decides who reaches a server, as firewall.rule
// does, and so applies under the firewall's timer: a change to one waits for
// the person who applied it to confirm they can still reach the server, and
// puts back what was there if nobody does in time. Every change to one claims
// the server's access too, so only one waits on a server at a time, and it
// takes a person and a passkey (ADR 0052).
type AccessGuard interface {
	GuardsAccess()
}

// GuardsAccess says whether a kind applies under the firewall's timer.
func GuardsAccess(t Type) bool {
	_, ok := t.(AccessGuard)
	return ok
}

// AccessStays is who still reaches a server's SSH once a change that narrows it
// is kept: the addresses pinned, what rules allow SSH's ports from, any
// WireGuard tunnel whose port no rule allows, and the panel's own ports where
// the change turns deny everything else on on the control server.
type AccessStays struct {
	SSHPorts   []uint32 `json:",omitempty"`
	Pinned     []string `json:",omitempty"`
	Allowed    []string `json:",omitempty"`
	Tunnels    []string `json:",omitempty"`
	PanelPorts []uint32 `json:",omitempty"`
}

// DecidesWhoGetsIn is a kind that decides who gets onto a server without
// being able to lock anyone out of a new session on its own, as a fail2ban
// jail does: a change to one takes a person and a passkey, as an AccessGuard's
// does, and applies with no timer (ADR 0052).
type DecidesWhoGetsIn interface {
	DecidesWhoGetsIn()
}

// TakesAPerson says whether a change to a kind takes a signed-in person and a
// passkey, as what decides who gets onto a server does.
func TakesAPerson(t Type) bool {
	_, decides := t.(DecidesWhoGetsIn)
	return decides || GuardsAccess(t)
}

// JailPlan is whom a fail2ban jail, as planned, never bans; the addresses
// signed in over SSH as it was planned that it doesn't ignore; and whether
// what it ignores covers every address.
type JailPlan struct {
	NeverBans          []string `json:",omitempty"`
	SignedInNotIgnored []string `json:",omitempty"`
	IgnoresEverything  bool     `json:",omitempty"`
}

// Decision is a driver's judgement of one step.
type Decision struct {
	// PlannedLeads says where the server paths the resource mounts led when this
	// was planned, for those that lead elsewhere. What makes a container checks
	// again, and refuses what leads elsewhere by then.
	PlannedLeads map[string]string `json:",omitempty"`
	Action       Action
	Effect       Effect
	// Downtime is how long what the resource serves is unavailable; zero
	// means none. Drivers must not understate it.
	Downtime time.Duration
	// Destroys is true when the step removes data the spec can't bring
	// back.
	Destroys bool
	Warnings []Warning
	// UntilHealthy is how long until what the resource serves is healthy
	// again, at most, counting Downtime, when its health check may take
	// longer than Downtime; zero when Downtime says it all. Drivers must
	// not understate it either.
	UntilHealthy time.Duration `json:",omitempty"`
	// Kept says where data was that the step takes out of use, which the
	// server keeps for 7 days, such as the paths in a container where its
	// image kept data. Undoing the step brings it back.
	Kept []string `json:",omitempty"`
	// Restores is true when the step brings back data the server kept, as
	// undoing a deletion does. Applying it fails if the data is gone by
	// then, rather than making the resource anew, empty.
	Restores bool `json:",omitempty"`
	// HidesLogs is true when the step deletes a service whose server keeps logs
	// of it: from then on they can't be read, and they go in 7 days, unless
	// undoing the step brings them back.
	HidesLogs bool `json:",omitempty"`
	// HostAccess is what the step lets the resource reach on its server
	// that it couldn't before. The control plane sets it from the specs,
	// whatever an agent says: a step with any needs a person to apply it. All
	// an agent adds is Leads, which can make it read louder and nothing else.
	HostAccess []Access `json:",omitempty"`
	// HealthUnknown is true when the server hasn't the image yet, so its
	// own health check isn't known, and UntilHealthy may be missing or
	// too short.
	HealthUnknown bool `json:",omitempty"`
	// Adopts is the full ID of a container this step takes on, which goes on running
	// as it is, as what the resource runs. Its step makes nothing.
	Adopts string `json:",omitempty"`
	// Releases says this deletion lets go of a container that was taken on and hasn't
	// been replaced since: it goes on running as it is.
	Releases bool `json:",omitempty"`
	// Elsewhere is what else may run what this step replaces, which came from a
	// Compose project and is about to stop being what that project finds.
	Elsewhere *Elsewhere `json:",omitempty"`
	// SharedNames are names what this step takes on keeps, that others hold too on a
	// network it's on.
	SharedNames []SharedName `json:",omitempty"`
	// Leads says where the server paths the resource mounts lead through symlinks,
	// by path, for those that lead elsewhere, and which driver a network Dockwell
	// didn't make uses, by name, where that isn't Docker's bridge. An agent reads it;
	// the control plane puts it on HostAccess (Access.Leading), and it goes no further.
	Leads map[string]string `json:",omitempty"`
	// LeadsMoved says which of Leads aren't where the resource's server path led
	// when what runs now was made. What a person approved isn't what it reaches
	// any more, so the control plane counts it as gained.
	LeadsMoved []string `json:",omitempty"`
	// Alongside is what containers Dockwell didn't make mount on the step's server,
	// as its agent surveyed it for the plan. It isn't kept with the plan.
	Alongside []Unmanaged `json:"-"`
	// Via is the links each server path there passes through on the way to where it
	// leads, by their own paths, as its agent surveyed them. It isn't kept either.
	Via map[string][]string `json:"-"`
	// Pull is true when the step asks the server for its image again first. With
	// no Changes, it replaces what runs only if that finds a newer image. With
	// nothing but a restart policy to change, it changes that in place otherwise.
	Pull bool `json:",omitempty"`
	// Pulled is true when a run stopped partway had already pulled on this server,
	// as the record says. Resuming needs no registry, so it compares what the
	// server has with what runs, and goes on from there.
	Pulled bool `json:",omitempty"`
	// Finishing is true when a run stopped partway took this step as far as it can
	// be seen to have gone: what it makes reads as asked for, and nothing else of it
	// is left. Nothing says its check passed, so that is all this step does.
	Finishing bool `json:",omitempty"`
	// NeverBegan is true of a Finishing step the record says no run started, so what
	// it finds is how the server already was. Where the record can't say, it's false.
	// It stays on the control plane.
	NeverBegan bool `json:"-"`
	// ImageGone is true when the digest its spec pins isn't on the server, and its
	// registry won't give it, so applying it could only fail. A registry that doesn't
	// answer says nothing of it.
	ImageGone bool `json:",omitempty"`
	// Ingress is what the step does to its server's ingress, beside the
	// route itself: starting it takes the server's public ports. A plan's
	// fingerprint covers it.
	Ingress *IngressEffect `json:",omitempty"`
	// Domain is what DNS said of a route's domain when this was planned. It
	// never stops a plan, and a fingerprint leaves it out, as people often
	// set DNS after.
	Domain *DomainCheck `json:",omitempty"`
	// Proof is how a route left to prove its domain automatically proves it, as
	// the control plane decided from Domain: "dns" or "http" (ADR 0088). Its
	// fingerprint covers it, so what a person approved is what's ordered.
	Proof string `json:",omitempty"`
	// Restore says what a step that brings a volume's data back from a backup
	// does. Its fingerprint covers it, so what stops can't change unreviewed.
	Restore *RestorePlan `json:",omitempty"`
	// BackupFirst says what a step that backs up a volume, before the rest of its
	// change, does.
	BackupFirst *BackupFirst `json:",omitempty"`
	// Move says what a step that moves a database to a newer release, by a dump into
	// a new volume, does beside replacing its container (ADR 0080). Its fingerprint
	// covers it, so what stops can't change unreviewed.
	Move *MovePlan `json:",omitempty"`
	// Install says what a step that installs a server's updates does, as its
	// agent simulated the exact command (ADR 0064). Its fingerprint covers it, so
	// a person never applies a list they didn't see.
	Install *Install `json:",omitempty"`
	// Reboot says a step reboots its server, for a server's host (ADR 0064).
	Reboot *Reboot `json:",omitempty"`
	// Unit says what a step does to one of its server's system services, for a
	// server's host (ADR 0082). Its fingerprint covers it.
	Unit *Unit `json:",omitempty"`
	// RegistryLogin names the registry login the step's pull signs in with, as the
	// control plane picked it (ADR 0086). Its fingerprint covers it, so a login
	// added between a plan and its apply makes the plan stale.
	RegistryLogin string `json:",omitempty"`
	// RegistryLoginVersion says which revision of which saved login that is, for
	// the fingerprint alone, so a login saved again under its name between a plan
	// and its apply makes the plan stale. It isn't kept.
	RegistryLoginVersion string `json:"-"`
	// BuildLogins name the registry logins the step's build may pull its base images
	// with, one for each registry, as the control plane picked them (ADR 0086). Its
	// fingerprint covers them.
	BuildLogins []string `json:",omitempty"`
	// BuildLoginVersions say which revision of which saved login each of those is,
	// which its fingerprint covers, and its apply holds to.
	BuildLoginVersions []string `json:",omitempty"`
	// Returning names stopped containers Docker starts again when it next
	// restarts, and that may then take the ports a server's proxy needs. It never
	// stops a plan, and a fingerprint leaves it out.
	Returning []string `json:",omitempty"`
	// Rollout is how the step replaces a running service, and why. Nil for a
	// step that replaces nothing, or one planned without its server, which
	// can't tell, so its Downtime is the most it could be.
	Rollout *RolloutPlan `json:",omitempty"`
	// Restart names the restart this step carries out: its change. A restart
	// changes no spec, so the driver marks what it makes with this, and a
	// step that finds it done has nothing to do.
	Restart string `json:",omitempty"`
	// Unencrypted is true when the step has a route serve plain HTTP that didn't,
	// as TLS ends before its server, so its traffic reaches the server
	// unencrypted. It never stops a plan and needs no person, so it isn't a
	// Warning, which does. The fingerprint covers the spec that says it.
	Unencrypted bool `json:",omitempty"`
	// Answers says who a route with tls off, or to a port on the server, answers,
	// from its spec. It never stops a plan.
	Answers *RouteAnswers `json:",omitempty"`
	// ServerPort is the port on the server a route sends to, and what listens there
	// if its server could say. It never stops a plan, and a fingerprint leaves it
	// out: the spec says the port.
	ServerPort *ServerPortCheck `json:",omitempty"`
	// Closes is what a step that turns deny everything else on closes: each port
	// that listens or that Docker publishes, that no rule lets in from anywhere,
	// such as "8080/tcp (shop-web-1)" (ADR 0052). It never stops a plan.
	Closes []string `json:",omitempty"`
	// Stays is who still reaches the server's SSH once a step that narrows it is
	// kept (ADR 0052). It never stops a plan.
	Stays *AccessStays `json:",omitempty"`
	// Jail is whom a step that sets a fail2ban jail never bans, and who is
	// signed in now that it could (ADR 0052). It never stops a plan.
	Jail *JailPlan `json:",omitempty"`
	// Goes says what someone visiting a route finds once the step takes it away, as
	// a route stops nothing the plan counts as downtime and yet stops people reaching
	// what it served. It never stops a plan, and a fingerprint leaves it out, as it
	// depends on the server's other routes.
	Goes *RouteGoes `json:",omitempty"`
	// Moves says a route leaves the service it sent to for another, as what a domain
	// reaches changing is easy to miss among a plan's fields. It never stops a plan,
	// and the fingerprint covers the spec that says it.
	Moves *RouteMove `json:",omitempty"`
	// Build is what the step builds before it replaces what runs, when the server
	// hasn't the image the spec names and the spec says how to build it. A build
	// that fails leaves what runs as it was (ADR 0046). Its fingerprint covers it.
	Build *BuildPlan `json:",omitempty"`
}

// BuildPlan is what a step builds: a commit of a repository, with its Dockerfile
// or with Railpack, into an image of its own.
type BuildPlan struct {
	URL, Ref, Commit    string
	Context, Dockerfile string `json:",omitempty"`
	Image               string
	// Builder is what the build's settings choose: BuilderAuto, BuilderDockerfile
	// or BuilderRailpack.
	Builder string `json:",omitempty"`
	// Detection is what reading the commit found, when the plan read it. A
	// fingerprint leaves it out: the commit and the settings decide it.
	Detection *BuildDetection `json:",omitempty"`
}

// What builds an image.
const (
	// BuilderAuto is the Dockerfile when the commit has one, and Railpack when
	// it hasn't.
	BuilderAuto       = ""
	BuilderDockerfile = "dockerfile"
	BuilderRailpack   = "railpack"
)

// BuildDetection is what reading a commit found before anything builds: what
// builds it, and with Railpack, what it installs and runs.
type BuildDetection struct {
	// Builder is BuilderDockerfile or BuilderRailpack.
	Builder string
	// Dockerfile is the Dockerfile's path in the repository, when it has one,
	// and BaseImage the image its last stage starts from, such as node:22-alpine.
	Dockerfile string `json:",omitempty"`
	BaseImage  string `json:",omitempty"`
	// Ports are the TCP ports the Dockerfile's last stage says the app listens
	// on (EXPOSE), lowest first.
	Ports []int32 `json:",omitempty"`
	// Railpack is the version of Railpack that read it.
	Railpack  string            `json:",omitempty"`
	Providers []string          `json:",omitempty"`
	Packages  []DetectedPackage `json:",omitempty"`
	Steps     []DetectedStep    `json:",omitempty"`
	Start     string            `json:",omitempty"`
	Warnings  []string          `json:",omitempty"`
	// Error is why Railpack can't build it, in its words, and Problem what kind of
	// problem that is (RailpackProblem), with the tool, for a version.
	Error       string `json:",omitempty"`
	Problem     string `json:",omitempty"`
	ProblemTool string `json:",omitempty"`
	// Text is the Dockerfile as the reading found it, its first MiB, for the agent's
	// own checks of what a build with registry logins pulls (ADR 0086). It goes
	// nowhere else.
	Text string `json:"-"`
}

// What stops Railpack building a commit, by what it said (RailpackProblem).
const (
	// ProblemNoProvider is a source Railpack can't tell the language of.
	ProblemNoProvider = "no_provider"
	// ProblemVersion is a version of a language or tool it can't find.
	ProblemVersion = "version"
	// ProblemNoStart is nothing that starts the app, with --error-missing-start.
	ProblemNoStart = "no_start"
	// ProblemNetwork is a server that couldn't reach where versions are listed:
	// the commit may build once it can.
	ProblemNetwork = "network"
	// ProblemOther is anything else Railpack refuses.
	ProblemOther = "other"
)

var (
	versionNotFound = lazyre.Regexp("failed to resolve version (\\S+) of (\\S+)|package `([^`]+)` not available in Mise")
	// What a lookup that couldn't reach the internet says, from mise or Go.
	offlineWords = []string{
		"temporary failure fetching", "failed to ensure mise is installed", "error sending request", "dns error",
		"no such host", "temporary failure in name resolution", "timed out", "timeout", "connection refused",
		"connection reset", "network is unreachable", "tls handshake", "rate limit",
	}
)

// RailpackProblem reads what Railpack said it can't do, and says what kind of
// problem it is, with the tool, for a version.
func RailpackProblem(said string) (problem, tool string) {
	lower := strings.ToLower(said)
	switch {
	case strings.Contains(lower, "could not determine how to build"):
		return ProblemNoProvider, ""
	case strings.Contains(lower, "no start command detected"):
		return ProblemNoStart, ""
	}
	for _, w := range offlineWords {
		if strings.Contains(lower, w) {
			return ProblemNetwork, ""
		}
	}
	if m := versionNotFound().FindStringSubmatch(said); m != nil {
		return ProblemVersion, strings.Trim(m[2]+m[3], "`")
	}
	if strings.Contains(lower, "failed to get latest version for package") {
		if i := strings.Index(said, "package `"); i >= 0 {
			if name, _, ok := strings.Cut(said[i+len("package `"):], "`"); ok {
				return ProblemVersion, name
			}
		}
		return ProblemVersion, ""
	}
	return ProblemOther, ""
}

// RailpackRefusal says why Railpack can't build what, such as
// "github.com/acme/web at 8be41d2", and what to do, by what it said.
func RailpackRefusal(d *BuildDetection, what string) string {
	why := strings.TrimSpace(d.Error)
	if i := strings.IndexByte(why, '\n'); i >= 0 {
		why = strings.TrimSpace(why[:i])
	}
	quoted := strconv.Quote(why)
	problem, tool := d.Problem, d.ProblemTool
	if problem == "" {
		problem, tool = RailpackProblem(d.Error)
	}
	switch problem {
	case ProblemNoProvider:
		return fmt.Sprintf("Railpack can't tell how to build %s: %s. Add a Dockerfile, or set both a build command and a start command", what, quoted)
	case ProblemVersion:
		if tool == "" {
			return fmt.Sprintf("Railpack can't find a version %s asks for: %s. Change the version the repository asks for", what, quoted)
		}
		return fmt.Sprintf("Railpack can't find the version of %s that %s asks for: %s. Change the version the repository asks for, or set %s as a build argument",
			tool, what, quoted, VersionVariable(tool))
	case ProblemNoStart:
		return fmt.Sprintf("Railpack found nothing that starts %s. Set the build's start command, or add a start script to the repository", what)
	case ProblemNetwork:
		return fmt.Sprintf("Railpack couldn't read %s, as it couldn't reach where versions are listed: %s. Check the server reaches github.com, then try again", what, quoted)
	}
	return fmt.Sprintf("Railpack can't build %s: %s. Fix what it says in the repository, or add a Dockerfile", what, quoted)
}

// variableName is a name a shell takes for a variable.
var variableName = lazyre.Regexp(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RailpackNames checks that a build's arguments and secrets can reach Railpack,
// which gives each to the build as an environment variable: a variable's name, not
// the build's own PATH or HOME, and for a secret, nothing Railpack reads as its own
// settings, which would change what it installs and starts unseen by the plan.
func RailpackNames(args map[string]string, secrets []string) error {
	for _, k := range slices.Sorted(maps.Keys(args)) {
		if err := railpackName("build argument", k); err != nil {
			return err
		}
	}
	for _, id := range secrets {
		if err := railpackName("secret", id); err != nil {
			return err
		}
		if err := RailpackSecret(id); err != nil {
			return err
		}
	}
	return nil
}

func railpackName(what, k string) error {
	switch {
	case !variableName().MatchString(k):
		return fmt.Errorf("the build's %s %s can't reach Railpack, which gives it to the build as an environment variable. Name it with letters, digits and _, starting with a letter or _", what, k)
	case k == "PATH" || k == "HOME":
		return fmt.Errorf("%s is the build's own, so a build with Railpack can't set it. Name the %s something else", k, what)
	}
	return nil
}

// RailpackSecret refuses a secret Railpack would read as its own settings, such as
// RAILPACK_START_CMD: what it installs and starts would change unseen by the plan,
// and the writer, and be kept with the build unmasked. As a build argument it's
// seen, and counted.
func RailpackSecret(id string) error {
	for _, p := range []string{"RAILPACK_", "NIXPACKS_"} {
		if strings.HasPrefix(strings.ToUpper(id), p) {
			return fmt.Errorf("%s is one of Railpack's own settings, so a build that may use Railpack can't take it as a secret: what it changes would be out of the plan's sight. Set it as a build argument", id)
		}
	}
	return nil
}

// VersionVariable is the build argument that chooses a tool's version for
// Railpack, such as RAILPACK_NODE_VERSION.
func VersionVariable(tool string) string {
	return "RAILPACK_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(tool)) + "_VERSION"
}

// DetectedPackage is a language or tool a Railpack build installs.
type DetectedPackage struct {
	Name, Requested, Resolved, Source string
}

// DetectedStep is a step of a Railpack build, and its commands.
type DetectedStep struct {
	Name     string
	Commands []string
}

// BuildBudget is the most a build may take, on top of what any step gets.
const BuildBudget = 30 * time.Minute

// RouteMove is a route leaving one service for another.
type RouteMove struct {
	// Where is its domain and path, as people read them.
	Where string
	From  string
	// To is the service it goes to. Empty when it goes to ToPort on the server.
	To     string `json:",omitempty"`
	ToPort uint32 `json:",omitempty"`
}

// VisitorsFind is what someone visiting a route finds once it's gone.
type VisitorsFind int

// What visitors find.
const (
	// VisitorsLoseIt is all a plan can say with no word from the server.
	VisitorsLoseIt VisitorsFind = iota
	// VisitorsRefused is a refused connection, as nothing holds the server's port.
	VisitorsRefused
	// VisitorsTLSError is a browser's connection error, as the proxy still runs and
	// has nothing for that name over HTTPS.
	VisitorsTLSError
	// VisitorsNotFound is a 404 from the proxy, which is what's in front gets for a
	// route that took no certificate.
	VisitorsNotFound
	// VisitorsRest is what the rest of the domain serves, or a 404 if nothing covers
	// that path.
	VisitorsRest
	// VisitorsNothingBehind is whatever what's in front shows when it can't connect,
	// as the proxy has stopped, and it took no certificate.
	VisitorsNothingBehind
)

// RouteGoes is a route a step takes away.
type RouteGoes struct {
	// Where is its domain and path, as people read them.
	Where string
	// Domain is the domain alone.
	Domain string
	Find   VisitorsFind
}

// RouteReach is who a route answers.
type RouteReach int

// Reaches.
const (
	// ReachThisServer is this server's own loopback alone.
	ReachThisServer RouteReach = iota + 1
	// ReachProxies is the proxies a route names, and whoever comes through them.
	ReachProxies
	// ReachAnyone is anyone who can reach the server's port.
	ReachAnyone
)

// RouteAnswers is who a route answers, as its plan says.
type RouteAnswers struct {
	Reach RouteReach
	// Proxies are what it answers, for ReachProxies.
	Proxies []string `json:",omitempty"`
	// Plain is true when it answers over plain HTTP.
	Plain bool `json:",omitempty"`
}

// ServerPortCheck is a port on the server that a route sends to.
type ServerPortCheck struct {
	Port uint32
	// Listening names what listens there. Empty when nothing does, or the server
	// couldn't say.
	Listening string `json:",omitempty"`
	// Nothing is true when the server said nothing listens there yet.
	Nothing bool `json:",omitempty"`
}

// RolloutPlan is how a step replaces a running service. A plan's fingerprint
// covers it.
type RolloutPlan struct {
	// Rolling is true when a new container takes over once it is healthy, so
	// nothing stops. False, the old one stops first.
	Rolling bool
	// What stops it rolling, or what two containers share if it rolls anyway:
	// server ports both would publish, volumes and server paths both would
	// mount and one write to, and where the image keeps data that moves over.
	Ports, Volumes, Paths, ImageData []string `json:",omitempty"`
	// Chosen is true when the spec's order asked for this.
	Chosen bool `json:",omitempty"`
	// Drain is the most what is in flight gets to finish.
	Drain time.Duration `json:",omitempty"`
	// StopGrace is the most an old container gets to stop, once drained.
	StopGrace time.Duration `json:",omitempty"`
	// HealthyWithin is the most the new container gets to be healthy.
	HealthyWithin time.Duration `json:",omitempty"`
	// Others are the other containers of the service the step removes, as a
	// rollout that stopped partway or docker compose up --scale left them.
	Others []string `json:",omitempty"`
	// CantTell are mounts whose real place the agent can't read, so it can't say
	// two containers wouldn't write to one place.
	CantTell []string `json:",omitempty"`
	// EditedRoutes are routes edited by hand that send to the service, which
	// Dockwell leaves as they are, so the new container keeps the old one's
	// address.
	EditedRoutes []string `json:",omitempty"`
	// EarlyNetworks are the service's networks, where other services can reach a
	// new container by name before it has passed its check, and EarlyNames the
	// other names it answers to there.
	EarlyNetworks, EarlyNames []string `json:",omitempty"`
	// HostNetwork is true when the old container or the new one is on the server's
	// own network, so the two can't run at once.
	HostNetwork bool `json:",omitempty"`
	// Dropped is what the old container mounts and the new one won't.
	Dropped []DroppedMount `json:",omitempty"`
	// Soak is how long a new container must stay healthy before anything moves
	// to it.
	Soak time.Duration `json:",omitempty"`
	// NoCheck is true when nothing checks the new container, so healthy is only
	// that it runs.
	NoCheck bool `json:",omitempty"`
	// RoutePorts is the port each route the change touches sends to on the service,
	// by the route's name. A route's own step comes after its service's, so these are
	// what the new container has to answer on, and what traffic moves to with it.
	RoutePorts map[string]uint32 `json:",omitempty"`
}

// DroppedMount is something the old container mounts and the new one won't.
type DroppedMount struct {
	Source, Target string
	// EmptyVolume says the new image declares a volume there, so an empty one
	// takes its place.
	EmptyVolume bool `json:",omitempty"`
}

// AlreadyRuns says what the step wants already serves, so nothing stops for it, and
// nothing starts: as when rolling back a new container that failed its check.
func (r *RolloutPlan) AlreadyRuns() bool {
	return r != nil && r.Rolling && r.HealthyWithin == 0
}

// TwoWriters reports a step that lets two containers write to one place, as
// a spec chose, for as long as it takes. Two writers can corrupt a database, so
// it is for a person to approve, as a plan that destroys data is.
func (r *RolloutPlan) TwoWriters() bool {
	return r != nil && r.Rolling && r.Chosen
}

// InstallBudget is how long installing a server's updates may take on top of
// what any step gets: the download's half hour, then an install that can build
// kernel modules and initramfs images on a small server (ADR 0064).
const InstallBudget = 3 * time.Hour

// Budget is the most a step may take beyond its pull and its own work: how
// long it may wait on a health check, on what is in flight, and on old
// containers to stop. An agent's call for the step gets this much more time.
func (d Decision) Budget() time.Duration {
	if d.Install != nil {
		return InstallBudget
	}
	if d.Restore != nil {
		return restoreBudget(d.Restore.Size)
	}
	if d.BackupFirst != nil {
		return restoreBudget(d.BackupFirst.Estimate)
	}
	b := max(d.Downtime, d.UntilHealthy)
	if r := d.Rollout; r != nil {
		b += r.HealthyWithin + r.Soak + r.Drain + r.StopGrace
	}
	if d.Build != nil {
		b += BuildBudget
	}
	// A move dumps and loads a database before its replace, at a restore's pace.
	if d.Move != nil {
		b += restoreBudget(d.Move.Estimate)
	}
	return b
}

// How a backup was taken.
const (
	// BackupDump is the database's own dump.
	BackupDump = "dump"
	// BackupCopy is a copy of a volume's files with nothing running on them.
	BackupCopy = "copy"
	// BackupCopyRunning is a copy of a volume's files with its app running.
	BackupCopyRunning = "copy_running"
)

// RestoreSource is what a restore brings back: a backup, or what a change
// replaced, which its server keeps for 7 days. Change names the change that
// restores, which it does once (ADR 0033).
type RestoreSource struct {
	Backup string `json:",omitempty"`
	// Undoes is a change that restored, for undoing it.
	Undoes string `json:",omitempty"`
	Change string `json:",omitempty"`
	// Server is the server that holds it, by ID, the one place it can happen.
	Server string `json:",omitempty"`
}

// RestorePlan is what a step that restores a volume does, for people to read.
type RestorePlan struct {
	// Backup is the backup it brings back, and Undoes the change it undoes
	// when it puts back what that replaced.
	Backup string `json:",omitempty"`
	Undoes string `json:",omitempty"`
	// Method is how what it brings back was taken: BackupDump and so on.
	Method string
	// Database names the database of a dump, such as PostgreSQL.
	Database string `json:",omitempty"`
	// OneDatabase is the one database it brings back alone, for an app's on a server
	// it shares (ADR 0084).
	OneDatabase string `json:",omitempty"`
	// Taken is when it was taken, and Size how large it is, in bytes.
	Taken time.Time
	Size  int64
	// Stops names what stops while it happens, sorted: services by their names,
	// and anything else by its container's.
	Stops []string `json:",omitempty"`
	// WrittenBy is the image that wrote a copy of a database's files, and RunsOn the
	// one that would read them, set when those are different releases.
	WrittenBy string `json:",omitempty"`
	RunsOn    string `json:",omitempty"`
	// Reached is what the backup's volume reached when it was taken that's as good
	// as root. Its agent says, and that can only make a plan louder.
	Reached []string `json:",omitempty"`
}

// Install is what installing a server's updates does: every package, from the
// version installed to the one it gets, those the others pull in too, and what
// follows (ADR 0064).
type Install struct {
	Packages []InstallPackage
	// Reboot says a package takes effect only after a reboot.
	Reboot bool `json:",omitempty"`
	// DockerRestarts says Docker's engine restarts, and KeepsRunning that it keeps
	// containers running meanwhile.
	DockerRestarts bool `json:",omitempty"`
	KeepsRunning   bool `json:",omitempty"`
	// Asked are the packages asked for by name, for an install of a selection:
	// every other package is one they need. None for the security fixes or all.
	Asked []string `json:",omitempty"`
}

// InstallPackage is one package an install installs.
type InstallPackage struct {
	Name         string
	Architecture string `json:",omitempty"`
	// From is the version installed: empty for a package the others pull in.
	From     string `json:",omitempty"`
	To       string
	Origin   string `json:",omitempty"`
	Security bool   `json:",omitempty"`
	// Reboot and Docker say it takes effect after a reboot, or restarts Docker.
	Reboot bool `json:",omitempty"`
	Docker bool `json:",omitempty"`
}

// InstallAsked is an install a change asks of a server's host.
type InstallAsked struct {
	// Change is the change's ID, which names the install's unit.
	Change string
	// All installs every update waiting, not only the security fixes.
	All bool `json:",omitempty"`
	// Names, at planning, installs only these packages and what they need, in
	// place of All: names the server's own read lists, and no others.
	Names []string `json:",omitempty"`
	// Packages are what the plan fixed: none at planning; on a resume, what it
	// checks against.
	Packages []InstallPackage `json:",omitempty"`
}

// Reboot is what rebooting a server does, for its host (ADR 0064): everything on
// it goes offline until it's back. The agent plans an empty one; the control plane
// says the rest.
type Reboot struct {
	// ControlServer says it's the server the control plane runs on, so the panel
	// and the API stop with it, and come back on their own.
	ControlServer bool `json:",omitempty"`
	// LastTook is how long the server's last reboot took, from its step's start
	// until its agent connected from the new boot, once Dockwell has seen one.
	LastTook time.Duration `json:",omitempty"`
}

// RebootAsked is a reboot a change asks of a server's host.
type RebootAsked struct {
	// Change is the change's ID, which the agent's mark of the reboot names, so it
	// reboots at most once for it.
	Change string
}

// Rebooter is a kind that reboots its server as a step of a change: the server's
// host (ADR 0064).
type Rebooter interface {
	// JudgeReboot plans the reboot, and reports false once there's none left to
	// take: the agent runs on another boot than the one the change's reboot left.
	JudgeReboot(ctx context.Context, ref Ref, ask RebootAsked) (Decision, bool, error)
	// ApplyReboot asks the server to reboot, for the change DeployOf says, at most
	// once: applied again on the same boot, or once on another, it does nothing.
	ApplyReboot(ctx context.Context, step Step) error
}

// Installer is a kind that installs its server's updates as a step of a change:
// the server's host (ADR 0064).
type Installer interface {
	// JudgeInstall plans the install, or on a resume what's left of it, and reports
	// false when nothing is: every package at its planned version.
	JudgeInstall(ctx context.Context, ref Ref, ask InstallAsked) (Decision, bool, error)
	// ApplyInstall carries out the step: its Install, for the change DeployOf says.
	ApplyInstall(ctx context.Context, step Step) error
}

// BackupFirst is a backup a change takes before anything else it does.
type BackupFirst struct {
	// ID is the backup's, which the control plane gave it.
	ID string
	// Method is how it's taken: BackupDump and so on.
	Method string
	// Database names the database of a dump.
	Database string `json:",omitempty"`
	// One is the one database it dumps alone, for an app's on a server it shares
	// (ADR 0084), and Also more on the same server, each under an ID of its own.
	One  string         `json:",omitempty"`
	Also []DatabaseDump `json:",omitempty"`
	// Stops names what stops while it's copied, sorted.
	Stops []string `json:",omitempty"`
	// Estimate is about how much it will take on the disk, in bytes.
	Estimate int64 `json:",omitempty"`
	// Unmeasured says nothing could tell how large it will be, so the time the step
	// gets is a guess: there was no backup before, and measuring took too long.
	Unmeasured bool `json:",omitempty"`
}

// MovePlan is what a step that moves a database to a newer release does, beside
// replacing its container (ADR 0080): it stops what else of its app runs on the
// server, dumps the database, loads that into the new release on a new volume,
// checks it starts there and answers, then replaces the container and starts what
// stopped. The volume its data was on stays as it was, for undo.
type MovePlan struct {
	// Backup is the dump's ID, a backup of From.
	Backup string
	// Database names the database, such as PostgreSQL, and FromSeries and ToSeries
	// the release series it moves between, such as "16" and "17".
	Database             string
	FromSeries, ToSeries string `json:",omitempty"`
	From, To             string // the volumes, by name
	// Stops names what else stops while it moves, sorted: services by their names,
	// and anything else by its container's.
	Stops []string `json:",omitempty"`
	// Estimate is about how large the dump will be, in bytes, and Unmeasured says
	// nothing could tell.
	Estimate   int64 `json:",omitempty"`
	Unmeasured bool  `json:",omitempty"`
	// Finishing says the move is done on the server, as a run cut short after the
	// replace left it, or a rollback of its change finds it: what's left is to
	// start what it stopped and to report its dump, which this step does.
	Finishing bool `json:",omitempty"`
	// Change is the change that moves it, which names what it stopped.
	Change string `json:",omitempty"`
	// StaysStopped names containers Dockwell doesn't manage on the old volume, which
	// stop for the move and stay stopped after it, as they'd go on with the old data.
	StaysStopped []string `json:",omitempty"`
}

// BackupTaken is a backup a step took, whole on its server.
type BackupTaken struct {
	ID, Method, Database, SHA256 string
	Size, Changed                int64
	// One is the one database it dumped alone, as the agent says, where it dumped one
	// (ADR 0084).
	One string
}

// Unmanaged is a container Dockwell didn't make, by its name, and the server paths it
// mounts, which a plan counts as held where something it plans can write around one.
type Unmanaged struct {
	Name  string
	Paths []string
	// Root says it's as good as root by what it mounts already, so moving one of its
	// paths gives it nothing new.
	Root bool
}

// IngressChange is what happens to the proxy that holds a server's public
// ports.
type IngressChange int

// Ingress changes.
const (
	// IngressStarts is the server's first route: the proxy starts, and
	// takes the ports.
	IngressStarts IngressChange = iota + 1
	// IngressStops is its last: the proxy stops and frees them, and its
	// certificates stay on the server.
	IngressStops
	// IngressRestarts is a newer version of the proxy, so what it serves is
	// down for the step's downtime.
	IngressRestarts
	// IngressTakesPorts is the first route that needs a certificate on a server
	// whose routes needed none: the proxy keeps running, and takes more ports.
	IngressTakesPorts
	// IngressFreesPorts is the last route that needs a certificate going: the
	// proxy keeps running, and frees the ports only that needed.
	IngressFreesPorts
)

// IngressEffect is what a step does to a server's ingress.
type IngressEffect struct {
	Change IngressChange
	// Proxy names it, such as "Caddy".
	Proxy string
	// Ports are the ports it takes or frees, such as "80/tcp".
	Ports []string `json:",omitempty"`
	// PanelDomain is the panel's domain, when the panel moves behind the
	// proxy as it starts, or takes the ports back as it stops.
	PanelDomain string `json:",omitempty"`
}

// DomainState is whether a domain's DNS points at a server.
type DomainState int

// Domain states.
const (
	// DomainUnknown is DNS that didn't answer in time.
	DomainUnknown DomainState = iota
	// DomainHere is a record that points at this server.
	DomainHere
	// DomainNoRecord is a domain with no A or AAAA record yet.
	DomainNoRecord
	// DomainElsewhere is records that point at other addresses.
	DomainElsewhere
	// DomainCantTell is a server with only private addresses, as behind
	// NAT, which can't tell whether a public address is its own.
	DomainCantTell
)

// DomainCheck is what DNS says of a domain, as a server sees it.
type DomainCheck struct {
	Domain string
	State  DomainState
	// Found are the addresses DNS gives for it, and Here the server's own.
	Found []string `json:",omitempty"`
	Here  []string `json:",omitempty"`
	// Public are the server's public addresses the control plane knows beyond its
	// interfaces, at most one of each family. They're in Here too, and they're the
	// ones to point a record at.
	Public []string `json:",omitempty"`
	// Unproven are those of Public nothing has shown to be a way in (ADR 0034).
	Unproven []string `json:",omitempty"`
	// Carrier says the server is behind carrier-grade NAT with no way in known, so
	// no record reaches it.
	Carrier bool `json:",omitempty"`
	// ReadAt is when DNS was read. A proof by a domain is only as good as this.
	ReadAt time.Time `json:"-"`
	// Proxied says a record points at Cloudflare's proxy, which can block a certificate
	// over HTTP-01 or TLS-ALPN.
	Proxied bool `json:",omitempty"`
	// Firewalls says which of them to open, on a cloud whose provider is known. It's
	// for what the agent says as a certificate fails, so it stays on the agent.
	Firewalls string `json:"-"`
}

// Own is the addresses on the server's own interfaces, leaving out the public ones
// it was told.
func (c *DomainCheck) Own() []string {
	if c == nil {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(c.Here), func(s string) bool { return slices.Contains(c.Public, s) })
}

// BehindNAT reports a server with no public address on an interface.
func (c *DomainCheck) BehindNAT() bool {
	return BehindNAT(c.Own())
}

// Access is something on a server a resource reaches beyond what Docker
// keeps for it, such as a server path a container mounts (HostAccesser).
// Its fields say all a person approves, and NewAccess compares them all. It
// never holds a credential.
type Access struct {
	Kind AccessKind
	// Path is the server path or socket, the volume's device, or a
	// network's parent interface. Empty for the server's network.
	Path string `json:",omitempty"`
	// ReadOnly is true for what is reached read-only.
	ReadOnly bool `json:",omitempty"`
	// Type is a volume's mount type, such as nfs.
	Type string `json:",omitempty"`
	// Address is a mount's remote server, such as 10.0.0.2.
	Address string `json:",omitempty"`
	// Relabel is how Docker relabels a server path for SELinux: z or Z.
	Relabel string `json:",omitempty"`
	// Driver is the plugin a volume runs on, or a network's driver.
	Driver string `json:",omitempty"`
	// Detail is a network's other options, as name=value, sorted, or for a
	// network the server has, the other names the resource answers to there.
	// For a server path, an external volume or a mount, read-write, it's who
	// writes there, where that's root or may be: one of the WrittenAs words.
	Detail string `json:",omitempty"`
	// Writer is the image repository whose code writes to a server path, an
	// external volume or a mount, read-write. A person approves one for what
	// runs there, and any image can hold a program that runs as root, whatever
	// the user, so code from another repository counts as new. Another tag of
	// the same repository is how CI deploys, and doesn't.
	Writer string `json:",omitempty"`
	// User is the name of a user by name who writes there, which only the image
	// says the number of, so another name counts as new.
	User string `json:",omitempty"`
	// SameAsRoot says this is the same as root on the server because of what it
	// is, whatever else it says: the capability SYS_ADMIN, or a path such as /etc,
	// or a plugin that mounts there (SystemPath). AsGoodAsRoot says it for every
	// kind.
	SameAsRoot bool `json:",omitempty"`
	// LeadsTo is where Path leads through symlinks, as the server's agent reads
	// it, when that is somewhere else. For a network Dockwell didn't make, it is
	// the driver the agent reads, when that isn't Docker's bridge.
	LeadsTo string `json:",omitempty"`
	// Holds is another mount's source inside Path, a read-write server path. What
	// writes Path can turn it into a link to anywhere on the server, which what mounts
	// it follows the next time it starts, so this is as good as root. HoldsFor names
	// what mounts it, empty where that's the same resource.
	Holds    string `json:",omitempty"`
	HoldsFor string `json:",omitempty"`
	// HeldIn is a read-write server path that holds Path, and HeldBy what can write
	// it, which can make Path lead anywhere: the other side of Holds.
	HeldIn string `json:",omitempty"`
	HeldBy string `json:",omitempty"`
}

// Leading is a with where its path leads on the server, as an agent says. An
// agent can only make a plan louder this way: what the path reads as written
// still counts, and the gate for a person is the control plane's own. So an agent
// that lies, or is too old to say, leaves the plan reading what the spec says.
func (a Access) Leading(to string) Access {
	if to != "" && a.Kind == AccessExternalNetwork {
		a.LeadsTo = to
		return a
	}
	if to == "" || to == a.Path || a.Kind != AccessPath && a.Kind != AccessDocker {
		return a
	}
	a.LeadsTo = to
	switch {
	case a.Kind == AccessDocker:
	case ControlsDocker(to):
		a.Kind, a.ReadOnly = AccessDocker, false // a socket mounted read-only still takes connections
	case SystemPath(to, a.ReadOnly):
		a.SameAsRoot = true
	}
	return a
}

// AsGoodAsRoot reports what is as good as root on the server. All host access
// needs a signed-in person; only this asks them for more than a click, as a
// warning that comes with every folder is a warning nobody reads (ADR 0028).
func (a Access) AsGoodAsRoot() bool {
	return a.SameAsRoot || a.Kind == AccessDocker || a.Kind == AccessNetwork || a.Kind == AccessBackupOf || a.Kind == AccessServerShell ||
		a.Kind == AccessNetworkDriver && a.Driver == "host"
}

// Who writes to a read-write server path, in its Access's Detail, where that's root
// or may be. A path whose writer is root reaches more than the same path without:
// root writes what anyone owns, and makes files that run as root (ADR 0032).
const (
	// WrittenAsRoot is root, however it's written.
	WrittenAsRoot = "root"
	// WrittenAsNamed is a user by a name other than root, which the image may
	// say is root.
	WrittenAsNamed = "named"
	// WrittenAsImage is whatever user the image runs as, which is root unless it
	// says otherwise.
	WrittenAsImage = "image"
)

// AccessKind is what a resource reaches on its server.
type AccessKind int

// Kinds of access.
const (
	// AccessPath is a server path, mounted into a container or by a
	// volume's bind option.
	AccessPath AccessKind = iota + 1
	// AccessDocker is Docker's or containerd's socket, or a path that
	// holds one: control of Docker, which is the same as root.
	AccessDocker
	// AccessNetwork is the server's own network.
	AccessNetwork
	// AccessMount is a mount Docker makes from a volume's options, such as
	// an NFS share, or what a volume plugin reaches.
	AccessMount
	// AccessNetworkDriver is a network that reaches past the server's own
	// bridge, such as a macvlan on its LAN, past its firewall.
	AccessNetworkDriver
	// AccessDomain is a domain another environment held within the undo
	// window, in Path, with that environment's name in Detail: claiming it
	// takes that environment's public traffic and certificate if the name
	// still points here (ADR 0024).
	AccessDomain
	// AccessPanelDomain is the panel's domain, in Path, moving behind the
	// server's proxy, named in Detail. Access to the panel depends on it, and a
	// token mustn't be able to cut off what it can't approve (ADR 0016).
	AccessPanelDomain
	// AccessExternalNetwork is a network on the server that Dockwell didn't
	// make, by its name in Docker: Path. Whatever else is on it can reach the
	// resource, and it them. Detail is the other names it answers to there.
	AccessExternalNetwork
	// AccessCapability is a Linux capability, by name: Path.
	AccessCapability
	// AccessGroup is a group its command joins, by name or number: Path.
	AccessGroup
	// AccessPrivileged is Docker's privileged mode: every device and capability
	// on the server.
	AccessPrivileged
	// AccessServerPort is a port on the server itself that a route sends to, in
	// Path, as 127.0.0.1:8080, at the domain and path in Detail, over plain HTTP when
	// Type is http. It reaches past containers, to whatever listens there, so each
	// of those is approved, not only the port (ADR 0030).
	AccessServerPort
	// AccessTrustedProxy is a proxy a route believes on where a request came from,
	// in Path: an address, a range or a named preset. What it says of a client
	// reaches services as true, so one that isn't really a proxy makes client
	// addresses forgeable (ADR 0030).
	AccessTrustedProxy
	// AccessPlainForAnyone is a route with tls off that answers anyone who can
	// reach the server's port 80, on the domain in Path, going around whatever is in
	// front of the server (ADR 0030).
	AccessPlainForAnyone
	// AccessHTTPSOff is the domain in Path going off HTTPS, as it had a certificate
	// within the undo window, so https:// stops answering (ADR 0030).
	AccessHTTPSOff
	// AccessExternalVolume is a volume on the server that Dockwell didn't make, by
	// its name in Docker: Path. Whatever else mounts it shares what it holds.
	AccessExternalVolume
	// AccessTakesOn is a container Dockwell didn't make, by its name: Path. Its
	// environment comes in with it, and a token mustn't be able to bring in what it
	// couldn't read (ADR 0031).
	AccessTakesOn
	// AccessBackupOf is a backup of a volume that reached Path, as good as root,
	// coming back into one that doesn't. What it holds can be read from there.
	AccessBackupOf
	// AccessServerShell is a shell command the agent runs on the server as root, on
	// a schedule (ADR 0053): the same as root.
	AccessServerShell
	// AccessCertificateKey is a certificate's key, proven by DNS by the control
	// plane, going to the server for the first time, for the domain in Path (ADR
	// 0088). An admin confirms it with a passkey.
	AccessCertificateKey
	// AccessOpened is a route that lets in someone what served its domain and path
	// kept out within the undo window: Path is the domain and path, and Detail says
	// how, as the Opens facts an Opener gives, joined with spaces. A token could
	// otherwise let itself in (ADR 0087).
	AccessOpened
)

// NewAccess lists what to reaches that from doesn't: access it didn't
// have, or has another way, as read-write where it was read-only, or
// relabelled where it wasn't, or at another address, or written by root where
// it wasn't, or by another who may be root. Access to Docker's socket is the
// same read-only or not, as a socket mounted read-only still takes
// connections.
func NewAccess(from, to []Access) []Access {
	// had is each way from has what a key says, once each, so a list of the same
	// access over and over costs no more than it once.
	had := make(map[Access]map[Access]bool, len(from))
	for i := range from {
		k := from[i].key()
		if had[k] == nil {
			had[k] = map[Access]bool{}
		}
		had[k][from[i]] = true
	}
	var out []Access
	seen := make(map[Access]bool)
next:
	for i := range to {
		a := to[i]
		if seen[a] {
			continue
		}
		for b := range had[a.key()] {
			if b.covers(a) {
				continue next
			}
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// Written reports a kind of access that something writes to, when it's
// read-write: what's there outlasts what runs, and others use it.
func Written(kind AccessKind) bool {
	return kind == AccessPath || kind == AccessMount || kind == AccessExternalVolume
}

// key is a without what one access can have more of than another: being
// read-write, and for what's written, who writes there.
func (a Access) key() Access {
	a.ReadOnly, a.Writer, a.User = false, "", ""
	if Written(a.Kind) {
		a.Detail = ""
	}
	return a
}

// covers reports a, which has other's key, reaching all other does: read-write
// where other is, and for what's written, by code from the same repository, as
// root or as who writes other.
func (a Access) covers(other Access) bool {
	if a.ReadOnly && !other.ReadOnly && other.Kind != AccessDocker {
		return false
	}
	if !Written(other.Kind) || other.ReadOnly {
		return true
	}
	if a.Writer != other.Writer {
		return false
	}
	return other.Detail == "" || a.Detail == WrittenAsRoot || a.Detail == other.Detail && a.User == other.User
}

// writtenBy says who writes what's read-write, for AccessWords, or nothing.
func writtenBy(a Access) string {
	if a.Writer == "" || a.ReadOnly || a.SameAsRoot {
		return ""
	}
	by := ", written by " + plain(a.Writer)
	switch a.Detail {
	case WrittenAsRoot:
		return by + " as root"
	case WrittenAsNamed:
		return by + " as a user by name, which only its image can say is root or not"
	case WrittenAsImage:
		return by + " as its own user, who may be root"
	}
	return by
}

// AccessWords says what a resource reaches, for people, such as "the
// server path /srv/app (read-only)". It names what a person approves,
// and never a credential.
func AccessWords(a Access) string {
	how := "read-write"
	if a.ReadOnly {
		how = "read-only"
	}
	switch a.Kind {
	case AccessPath:
		switch a.Relabel {
		case "z":
			how += ", relabelled for SELinux as shared"
		case "Z":
			how += ", relabelled for SELinux as private"
		}
		root := writtenBy(a)
		if a.SameAsRoot {
			root = ", which is as good as root"
		}
		if a.Holds != "" {
			mounts := "which it mounts as well"
			if a.HoldsFor != "" {
				mounts = "which " + plain(a.HoldsFor) + " mounts"
			}
			return "the server path " + plain(a.Path) + ", which holds " + plain(a.Holds) + ", " + mounts +
				", so what writes there can make that lead anywhere, as good as root. Any change to it but another tag asks for a passkey from now on"
		}
		if a.HeldBy != "" {
			return "the server path " + plain(a.Path) + ", inside " + plain(a.HeldIn) + ", which " + plain(a.HeldBy) +
				" can write, so it can make that lead anywhere, as good as root. Any change to " + plain(a.HeldBy) + " but another tag asks for a passkey from now on"
		}
		if a.LeadsTo != "" {
			return "the server path " + plain(a.Path) + ", which leads to " + plain(a.LeadsTo) + " (" + how + ")" + root
		}
		return "the server path " + plain(a.Path) + " (" + how + ")" + root
	case AccessDocker:
		if a.LeadsTo != "" {
			return "the server path " + plain(a.Path) + ", which leads to Docker's socket at " + plain(a.LeadsTo) + ", which gives control of Docker on the server, the same as root"
		}
		return "Docker's socket at " + plain(a.Path) + ", which gives control of Docker on the server, the same as root"
	case AccessNetwork:
		return "the server's own network"
	case AccessMount:
		if a.Driver != "" && a.Path != "" {
			return "the volume driver " + plain(a.Driver) + " at " + plain(a.Path) + ", which reaches what the plugin does (" + how + ")" + writtenBy(a)
		}
		if a.Driver != "" {
			return "the volume driver " + plain(a.Driver) + ", which reaches what the plugin does (" + how + ")" + writtenBy(a)
		}
		// So a device can't spell out the rest of this line, such as
		// "//6.6.6.6/x at 10.0.0.2", with a space or anything else.
		at, address := plain(a.Path), plain(a.Address)
		switch {
		case at != a.Path && address != "":
			at += " at " + address
		case strings.HasPrefix(at, ":"):
			// NFS's :/export follows its server, as people write it.
			at = address + at
		case address != "":
			// Run together, an address and a name could read as another
			// address.
			at += " at " + address
		}
		if at == "" {
			return "a " + plain(a.Type) + " mount (" + how + ")" + writtenBy(a)
		}
		return "the " + plain(a.Type) + " mount " + at + " (" + how + ")" + writtenBy(a)
	case AccessNetworkDriver:
		out := "a " + plain(a.Driver) + " network"
		if a.Path != "" {
			out += " on " + plain(a.Path)
		}
		if a.Driver != "bridge" {
			out += ", which reaches past the server's firewall"
		}
		if a.Detail != "" {
			out += ", with " + plain(a.Detail)
		}
		return out
	case AccessDomain:
		return "the domain " + plain(a.Path) + ", which the environment " + plain(a.Detail) + " held within the last 7 days"
	case AccessPanelDomain:
		return "the panel's domain, " + plain(a.Path) + ", which moves behind " + plain(a.Detail)
	case AccessExternalVolume:
		out := "the volume " + plain(a.Path) + ", which is on the server already and which Dockwell didn't make: whatever else mounts it shares what it holds"
		if a.ReadOnly {
			out += ", and this reads it only"
		}
		return out + writtenBy(a)
	case AccessTakesOn:
		return "the container " + plain(a.Path) + ", which Dockwell didn't make, and its environment, which comes in with it"
	case AccessExternalNetwork:
		out := "the network " + plain(a.Path) + ", which is on the server already, where it answers to its name"
		if a.LeadsTo != "" {
			out = "the " + plain(a.LeadsTo) + " network " + plain(a.Path) + ", which reaches past the server's firewall and is on the server already, where it answers to its name"
		}
		if a.Detail != "" {
			out += " and to " + plain(a.Detail)
		}
		return out + ": whatever else is on that network, other projects' services too, can reach it, and it can reach them"
	case AccessCapability:
		if a.SameAsRoot {
			return "the Linux capability " + plain(a.Path) + ", which is the same as root"
		}
		return "the Linux capability " + plain(a.Path)
	case AccessGroup:
		if a.SameAsRoot {
			return "the group " + plain(a.Path) + ", which is the same as root"
		}
		return "the group " + plain(a.Path) + ", which is the same as root if it is the group that owns Docker's socket on the server"
	case AccessPrivileged:
		return "privileged mode, which gives it every device and capability on the server, the same as root"
	case AccessServerPort:
		out := "the port " + plain(a.Path) + " on the server itself, past its containers, at " + plain(a.Detail)
		if a.Type == "http" {
			out += " over plain HTTP"
		}
		return out
	case AccessTrustedProxy:
		out := plain(a.Path) + " as a proxy it believes on where a request came from"
		if namesARange(a.Path) {
			out += ": whatever else is in that range can reach the route, and say who its client is"
		}
		return out
	case AccessPlainForAnyone:
		return "plain HTTP on " + plain(a.Path) + " for anyone who can reach this server, around whatever is in front of it"
	case AccessHTTPSOff:
		return "HTTPS on " + plain(a.Path) + ", which stops: https:// addresses get an error, and browsers that saw HSTS from it refuse plain HTTP until that runs out"
	case AccessBackupOf:
		return "a backup of a volume that reached " + plain(a.Path) + ", which can hold what's there, as good as root"
	case AccessServerShell:
		return "a shell command on the server, which runs as root"
	case AccessCertificateKey:
		if a.Detail != "" {
			return "the key of the shared certificate for " + a.Detail + ", proven by DNS, going to this server for the first time, for " + plain(a.Path)
		}
		return "a certificate's key for " + plain(a.Path) + ", proven by DNS, going to this server for the first time"
	case AccessOpened:
		return plain(a.Path) + " to someone it kept out: " + openedWords(a.Detail)
	}
	return fmt.Sprintf("access(%d)", int(a.Kind))
}

// namesARange reports a proxy that covers more than one address, so what else is
// on that network can be it. A lone address is written as a range of one, and a
// preset has its name. words.ts says the same for the panel.
func namesARange(proxy string) bool {
	p, err := netip.ParsePrefix(proxy)
	return err == nil && !p.IsSingleIP()
}

// The facts an Opener gives, each a way a route lets in someone it kept out, which
// AccessOpened's Detail lists with spaces between them (ADR 0087). A fact that ends
// in a colon is followed by the range or the user's name it's about. words.ts reads
// them for the panel.
const (
	OpensListOff  = "list-off"  // its list of addresses taken off
	OpensAllow    = "allow:"    // a range it lets in that its list didn't cover
	OpensUsersOff = "users-off" // its passwords taken off
	OpensUser     = "user:"     // a user it adds
	OpensPassword = "password:" // a user whose password changes
)

// openedWords says the Opens facts in Detail, in words.
func openedWords(detail string) string {
	var out []string
	for _, fact := range strings.Fields(detail) {
		switch {
		case fact == OpensListOff:
			out = append(out, "takes off its list of addresses")
		case fact == OpensUsersOff:
			out = append(out, "takes off its passwords")
		case strings.HasPrefix(fact, OpensAllow):
			out = append(out, "lets in "+plain(strings.TrimPrefix(fact, OpensAllow)))
		case strings.HasPrefix(fact, OpensUser):
			out = append(out, "adds the user "+plain(strings.TrimPrefix(fact, OpensUser)))
		case strings.HasPrefix(fact, OpensPassword):
			out = append(out, "changes the password of "+plain(strings.TrimPrefix(fact, OpensPassword)))
		default:
			out = append(out, plain(fact))
		}
	}
	return strings.Join(out, "; ")
}

// plain quotes a name in a plan that holds anything but what a name, a path, an
// address or a list of options holds, so it can't spell out the rest of the line,
// whatever let it through. words.ts does the same for the panel.
func plain(s string) string {
	inName := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.,/=:@+*-", r)
	}
	if strings.IndexFunc(s, func(r rune) bool { return !inName(r) }) >= 0 {
		return strconv.Quote(s)
	}
	return s
}

// Keeper is a kind whose resources keep some of what they were made with, whatever
// an edit says of it, so a plan never compares that with the edit: a volume keeps
// its password, which Docker can't change anyway, so no plan says whether a guess at
// it is right (ADR 0022). Keep puts what current keeps into desired, and says what
// it kept for a plan's note, or nothing where the edit had nothing of it. It refuses
// an edit it can't keep from, where what it keeps would go somewhere it hasn't been.
// What it says, and whether it refuses, depend on what the edit gave, never on
// whether that is the same as what current keeps. It's for what a caller wrote, so
// adopting drift and putting back a revision don't ask it (ADR 0022).
type Keeper interface {
	Keep(current, desired proto.Message) (Kept, error)
}

// Opener is a Claimer whose resources can keep out some of those who reach what they
// claim, as a private route does (ADR 0087). A Claim's Key covers itself and the Keys
// under it, after a "/", as a domain's route covers its paths, and what serves a Key
// is what has the longest Key that covers it. Opens lists, for a person to approve,
// how desired lets in anyone was kept out, as Opens facts such as
// "allow:198.51.100.0/24" or "user:ci", with no spaces in them; or nothing, where
// desired keeps out at least whom was kept out. was served the same Key, now or
// within the undo window.
type Opener interface {
	Opens(was, desired proto.Message) []string
	// KeepsOut reports a spec that keeps anyone out. A Group with one runs from one
	// place, as what serves a Key is worked out from specs wherever they run.
	KeepsOut(spec proto.Message) bool
}

// SealedLater is a kind kept in the clear until it had a field that may hold a
// secret, as routes were until they held users' password hashes (ADR 0087). A spec
// stored before then holds none, so the store reads one that's in the clear and
// holds no secret as it is, where it refuses that of a kind sealed from the start.
// Every spec written since is sealed, as any of a sealed kind is.
type SealedLater interface {
	SealedLater()
}

// ChangeChecker is a kind that checks an edit against the rest of its change, as a
// website's project changes what it serves only with the route that serves it.
// current is nil for a resource the change makes, and others are what the change's
// other edits are of. Like Keep, it's for what a caller wrote, so adopting drift and
// putting back a revision don't ask it.
type ChangeChecker interface {
	CheckInChange(current, desired proto.Message, others []Ref) error
}

// WriterNamer is a kind whose code isn't its image's alone: it names what writes
// what a spec mounts read-write, for a person to approve, in place of the image's
// repository. Empty says the image's repository is.
type WriterNamer interface {
	WriterOf(spec proto.Message) string
}

// NewDefaulter is a kind whose new resources take a default from their ref
// where their edit leaves it out, such as an environment's protection from its
// name. An edit of one that's there keeps what's there instead (Keeper).
type NewDefaulter interface {
	DefaultNew(ref Ref, spec proto.Message)
}

// EnvKeeper is a kind whose spec holds values no response carries, which an edit
// by a client that can't read them back names to keep as they are, rather than
// setting them (Edit.keep_env): a service's own environment variables. KeepEnv
// puts the values of those named into desired from current, and says what it
// kept for a plan's note, as a Keeper does. It refuses a name current hasn't got,
// or one desired sets too.
type EnvKeeper interface {
	KeepEnv(current, desired proto.Message, names []string) (Kept, error)
}

// NewChecker is a kind that asks more of a new resource than Validate can of an edit,
// where what an edit leaves out comes from what's there (Keeper).
type NewChecker interface {
	CheckNew(spec proto.Message) error
}

// Kept is what a Keeper kept: Reason names it, as a plan note's reason does, and
// Words say it after the resource's name, such as "keeps the password it was made
// with". Stays says why that can't go to a server that hasn't got it, and what to do
// instead, as a plan sends its spec to every server it would run on (ADR 0022).
// Empty means nothing.
type Kept struct {
	Reason, Words, Stays string
}

// restoreBudget is how long a restore may take, past what any step gets: stopping
// and starting aside, it copies what's there, then unpacks or loads what it brings
// back, each at no worse than a megabyte a second.
func restoreBudget(size int64) time.Duration {
	return time.Hour + 3*time.Duration(size/(1<<20))*time.Second
}

// Restorer is a driver that can bring a resource's data back from a backup. A
// restore changes no spec, so it makes no revision, as a restart doesn't, and it
// runs once for a change (ADR 0033).
type Restorer interface {
	// JudgeRestore judges a step that restores source, and reports false when the
	// change has done that already.
	JudgeRestore(ctx context.Context, ref Ref, source RestoreSource) (Decision, bool, error)
	// ApplyRestore carries out the step.
	ApplyRestore(ctx context.Context, step Step, source RestoreSource) error
}

// BackerUp is a driver that can back up a resource's data as a step of a change, as
// a catalog upgrade does before it replaces what uses it (ADR 0033).
type BackerUp interface {
	// JudgeBackup judges a step that takes backup id, and reports false when that's
	// been taken already, as a step that finished and went unheard has.
	JudgeBackup(ctx context.Context, ref Ref, asked BackupAsked) (Decision, bool, error)
	// ApplyBackup carries out the step, and reports what it took (Report).
	ApplyBackup(ctx context.Context, step Step) error
}

// Mover is a driver that can move the database a resource runs to a newer release,
// by a dump into a new volume, as the step that replaces it does (ADR 0080).
type Mover interface {
	// JudgeMove adds to a step that replaces the resource what moving its database
	// does, or says why it can't: what else stops, and for how long.
	JudgeMove(ctx context.Context, ref Ref, step Step, desired proto.Message, move MoveAsked) (Decision, error)
}

// HostAccesser is a kind whose specs can reach their server beyond what
// Docker keeps for them, as a container that mounts a server path does. A
// change that adds to or changes what one reaches needs a person to apply
// it, as one that destroys data does (ADR 0021).
type HostAccesser interface {
	HostAccess(spec proto.Message) []Access
}

// Step is what applying a change does to one resource.
type Step struct {
	Ref Ref
	Decision
	// Changes are the fields that differ between what the server has and
	// what is desired.
	Changes []Change
}

// Type is what the control plane knows of a kind: its spec's message, its
// level, and how to check a spec.
type Type interface {
	// Kind is the kind this type describes.
	Kind() Kind
	// Level says what the kind's resources live in.
	Level() Level
	// New returns an empty spec of the kind's message type.
	New() proto.Message
	// Validate checks a spec and fills in its defaults in place, so specs
	// that mean the same thing compare equal.
	Validate(spec proto.Message) error
}

// IDPrefixer is a Type whose resources' IDs say what they are, such as
// "env" for environments. Others get "res".
type IDPrefixer interface {
	IDPrefix() string
}

// Adoption is a container read as what a resource would run.
type Adoption struct {
	Spec proto.Message
	// Refused is what it runs with that no spec holds, in words for people, which a
	// replace would drop.
	Refused []string
	// Invalid says why Spec isn't one a service can hold, when it runs with what's fine
	// to Docker and no spec can say.
	Invalid string
	// Shows says where Spec would show a value of its environment, by the variable's
	// name and never the value.
	Shows []string
	// LeanedOn is what depends on it in a way a replace would cut off, each with how,
	// such as "app shares its network".
	LeanedOn []string
	// Manager is the tool that runs its Compose project, where its labels show one.
	Manager string
	// Folder is where its Compose project runs from.
	Folder string
}

// SharedName is a name more than one container answers to on a network: Name, which a
// container taken on goes on holding beside Others, on Network.
type SharedName struct {
	Name, Network string
	Others        []string
}

// Elsewhere is what else may still run a container that was taken on from a Compose
// project.
type Elsewhere struct {
	// Manager is the tool that runs the project, where its labels show one.
	Manager string `json:",omitempty"`
	// Container is the name it came with, and Project its Compose project.
	Container, Project string
	// Folder is where that project runs from.
	Folder string `json:",omitempty"`
}

// Adopter is a driver that can take on what runs on a server already, as what a
// resource runs, with nothing about it changing. See
// docs/adr/0031-containers-dockwell-didnt-make.md.
type Adopter interface {
	// ReadAdoption reads the container, by its full ID, as what the resource would
	// run.
	ReadAdoption(ctx context.Context, ref Ref, container string) (Adoption, error)
	// Adopts says what taking the container on as desired takes, and refuses
	// what has changed since it was read.
	Adopts(ctx context.Context, ref Ref, container string, desired proto.Message) (Decision, error)
}

// NameChecker is a Type that takes only some names, such as names that fit
// in a URL. Kinds without one take any name that isn't blank.
type NameChecker interface {
	// CheckName says why a name won't do, with an example of one that
	// would.
	CheckName(name string) error
}

// labelRE is the form of a DNS label, and maxLabel how long one gets. The length
// is checked apart from the form: a counted repetition, such as {0,61}, compiles
// to that many copies of what it repeats, which every agent pays for as it starts
// (ADR 0060).
var labelRE = lazyre.Regexp(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

const maxLabel = 63

// IsLabel reports whether name is one CheckLabel takes.
func IsLabel(name string) bool {
	return len(name) <= maxLabel && labelRE().MatchString(name)
}

// CheckLabel requires a name that is a DNS label, so it fits in URLs, in
// hostnames such as a service's URL, and on the command line: 1 to 63
// lowercase letters, digits and dashes, starting with a letter and ending
// with a letter or digit. Projects, environments and services take such
// names. what names the kind, and example is a name that would do.
func CheckLabel(what, name, example string) error {
	if !IsLabel(name) {
		return fmt.Errorf("%q isn't a valid %s name: use up to 63 lowercase letters, digits and dashes, starting with a letter and ending with a letter or digit, such as %s",
			name, what, example)
	}
	return nil
}

// NewNameChecker is a NameChecker with names only a resource made anew can't
// take, as a newer rule keeps them for something else: one stored with such a
// name goes on being edited, undone and adopted.
type NewNameChecker interface {
	CheckNewName(name string) error
}

// Valuer is a Type whose resources are values that others in their scope
// use, such as an environment's variables and secrets that its services
// use. A valuer runs on no server itself, whatever its level: it has
// revisions and plans, and its value goes to the servers of whatever uses
// it, when that is applied.
type Valuer interface {
	// Value returns a spec's value.
	Value(spec proto.Message) string
}

// ValueMaker is a kind whose spec is a value of its own, which another resource's
// value can be moved into (ValueMover).
type ValueMaker interface {
	// MakeValue is a spec holding value.
	MakeValue(value string) proto.Message
}

// ValueMover is a kind whose spec holds values of its own that it can hand to a
// resource of their own, as a service hands its own environment variable to a
// secret of its environment (ADR 0081). The value goes from one spec to another in
// the control plane, so nobody reads it on the way.
type ValueMover interface {
	// MoveValues takes each value names names out of desired, using one of the kind
	// it says of each name instead, and says each value current had, by name.
	MoveValues(current, desired proto.Message, names []string) (Kind, map[string]string, error)
}

// Use is one resource that another uses: in the same scope, unless Scope names
// another, as a link's value from another environment does (ADR 0084).
type Use struct {
	// As is the name the user sees it as, such as DATABASE_URL.
	As   string
	Kind Kind
	// Name is its name in the scope.
	Name string
	// Revision is the revision it is pinned to, 0 when it isn't.
	Revision int
	// Scope is the ID of the scope it lives in, when that isn't the user's own.
	Scope string
}

// Unplaced is a Type that lives in an environment and runs on no server, as a
// valuer doesn't: the control plane acts on it alone, as it runs a backup policy's
// schedule. It has revisions and plans, and no steps.
type Unplaced interface {
	Unplaced()
}

// Capped is a Type an install keeps at most so many of, such as checks of a URL,
// which each cost the control plane a request an interval (ADR 0066). A plan
// that would take the install past Cap is refused, with CapWords' reason.
type Capped interface {
	Cap() int
	// CapWords says why a plan that would make the install have more than Cap
	// is refused, and what to do instead.
	CapWords(have int) string
}

// Rotates is a Type whose sensitive fields plan as a change whenever an edit sets
// them, as a secret's value does, and are never compared with what's there, so a
// plan can't confirm a guess at one (ADR 0022).
type Rotates interface {
	RotatesSensitive()
}

// RunsOnNoServer reports a Type whose resources run on no server, whatever its level.
func RunsOnNoServer(t Type) bool {
	if _, ok := t.(Valuer); ok {
		return true
	}
	_, ok := t.(Unplaced)
	return ok
}

// Binder is a Type whose specs use other resources of their scope by name,
// such as a service that uses its environment's variables and secrets.
// The revision store checks that each is there, pins each use to the
// revision it resolves to when a change is planned, so a binder's spec
// changes whenever what it uses does, and adds every binder a change
// affects to that change.
type Binder interface {
	// Uses lists what a spec uses, by the name it sees each as.
	Uses(spec proto.Message) []Use
	// Pin sets the revision of each use in spec, by the name it is seen
	// as. A use missing from revisions is unpinned.
	Pin(spec proto.Message, revisions map[string]int)
}

// Rebinder is a Binder that can use a resource of another kind in place of
// one it uses, by the same name, as when a variable becomes a secret. The
// revision store rebinds every resource that uses a value which a change
// replaces with one of another kind, so the switch is one plan.
type Rebinder interface {
	// Rebind makes spec use the resource of kind to named name wherever
	// it uses the one of kind from, under the same names.
	Rebind(spec proto.Message, name string, from, to Kind)
}

// Driver manages one kind of resource on a server. Drivers run where the
// resource is: on the agent. Kinds whose level doesn't run on servers have
// a Type and no driver.
type Driver interface {
	Type
	// Observe returns the resource as it is on the server, as a spec, or
	// nil if it doesn't exist. It holds intent only: nothing the server
	// adds, such as IDs or start times, or every check would see drift.
	Observe(ctx context.Context, ref Ref) (proto.Message, error)
	// Plan judges moving the resource from actual to desired, which differ
	// in changes. A nil actual means create, a nil desired delete.
	Plan(actual, desired proto.Message, changes []Change) (Decision, error)
	// Apply carries out a step; desired is nil for a deletion.
	Apply(ctx context.Context, step Step, desired proto.Message) error
	// Verify confirms the resource is healthy, once the caller has checked
	// it matches desired. It isn't called after a deletion.
	Verify(ctx context.Context, ref Ref, desired proto.Message) error
}

// Registry holds the type, and the driver where there is one, of each
// kind.
type Registry struct {
	types map[Kind]Type
}

// NewRegistry returns a registry of types. Drivers are types too.
func NewRegistry(types ...Type) *Registry {
	r := &Registry{types: make(map[Kind]Type, len(types))}
	for _, t := range types {
		r.types[t.Kind()] = t
	}
	return r
}

// HostAccess lists what spec reaches on its server, if its kind can reach
// anything.
func (r *Registry) HostAccess(kind Kind, spec proto.Message) []Access {
	if spec == nil {
		return nil
	}
	if h, ok := r.types[kind].(HostAccesser); ok {
		return h.HostAccess(spec)
	}
	return nil
}

// ControlOnly is a kind whose spec says something only the control plane keeps,
// such as how an app from a folder was made (ADR 0058). Its servers never see
// that, so they neither plan on it nor read it back, and drift is what differs
// without it.
type ControlOnly interface {
	// OnServer is a copy of spec without what only the control plane keeps, or
	// spec itself when it has none of that.
	OnServer(spec proto.Message) proto.Message
	// Readopted is what a server has, read back as observed, with what only the
	// control plane keeps of current put back, where that still holds of it: as
	// drift that's kept becomes the latest revision.
	Readopted(current, observed proto.Message) proto.Message
}

// OnServer is spec as its server sees it (ControlOnly).
func (r *Registry) OnServer(kind Kind, spec proto.Message) proto.Message {
	if spec == nil {
		return nil
	}
	if c, ok := r.types[kind].(ControlOnly); ok {
		return c.OnServer(spec)
	}
	return spec
}

// Readopted is what a server has, with what only the control plane keeps of
// current put back where that still holds (ControlOnly).
func (r *Registry) Readopted(kind Kind, current, observed proto.Message) proto.Message {
	if current == nil || observed == nil {
		return observed
	}
	if c, ok := r.types[kind].(ControlOnly); ok {
		return c.Readopted(current, observed)
	}
	return observed
}

// Shower is a kind whose spec says some of its sensitive values hold no secret, as
// a service's shown_env does of its app's own values (ADR 0081). A plan shows such a
// value where both sides of a change show it, and masks it where either doesn't.
type Shower interface {
	// Shown is each value spec shows, by the path Diff names it by, such as
	// "app.env[DB_HOST]".
	Shown(spec proto.Message) map[string]string
}

// Shown is each value spec shows, by its path (Shower), or nil.
func (r *Registry) Shown(kind Kind, spec proto.Message) map[string]string {
	if spec == nil {
		return nil
	}
	if s, ok := r.types[kind].(Shower); ok {
		return s.Shown(spec)
	}
	return nil
}

// ShowChanges puts back into changes the values a plan may show: a sensitive
// field's change whose every side that has a value shows it, by from and to,
// what Shown said of the specs on each side. A value one side hides stays
// masked, so a change that shows a hidden value, or hides a shown one, never
// says it.
func ShowChanges(changes []Change, from, to map[string]string) {
	if len(from) == 0 && len(to) == 0 {
		return
	}
	for i := range changes {
		c := &changes[i]
		if !c.Sensitive {
			continue
		}
		was, wasShown := from[c.Path]
		now, nowShown := to[c.Path]
		switch c.Op {
		case OpAdded:
			if nowShown {
				c.To, c.Sensitive = now, false
			}
		case OpRemoved:
			if wasShown {
				c.From, c.Sensitive = was, false
			}
		case OpModified:
			if wasShown && nowShown {
				c.From, c.To, c.Sensitive = was, now, false
			}
		}
	}
}

// notHere is why a driver can't run on a server that doesn't have what its
// kind needs, and may never: nftables, fail2ban. That's the server as it is,
// not a fault, so it isn't said as a warning.
type notHere struct{ error }

func (e notHere) Unwrap() error { return e.error }

// NotHere marks why a driver can't run as the server not having what its kind
// needs, such as a tool nobody installed, rather than something gone wrong.
// The kind is offered once the server has it.
func NotHere(err error) error {
	if err == nil {
		return nil
	}
	return notHere{err}
}

// IsNotHere reports a reason NotHere marked.
func IsNotHere(err error) bool {
	var n notHere
	return errors.As(err, &n)
}

// Readier is a driver that can't always run, such as one that needs
// Docker. An agent offers its kind only while Ready says it can.
type Readier interface {
	// Ready says why the driver can't run now, for people, with how to
	// fix it, such as "Docker isn't running. Start it with …".
	Ready(ctx context.Context) error
}

// Normalizer is a Type that can order a spec it wouldn't take, as Validate
// does one it would. Observe uses it, so a resource that a newer rule
// refuses reads as what it is, and not as drift as well.
type Normalizer interface {
	Normalize(spec proto.Message)
}

// Referrer is a kind whose specs name other resources in their scope, as
// a service names the volumes it mounts and the networks it joins. The
// engine refuses a change that would leave one naming what isn't there,
// or isn't on its servers, and makes what is named first.
type Referrer interface {
	References(spec proto.Message) []Reference
	// ReferenceKinds are the kinds its specs can name. So a change to no
	// resource of those kinds leaves what is stored naming what it did, and
	// nothing need open a stored spec to find out, as a sealed one takes
	// its key to.
	ReferenceKinds() []Kind
}

// Tied is a kind that stands for what reads the values it sets, as a link stands for
// an app's need, met (ADR 0084). A change that deletes what it names, or a service
// that reads those values, takes it in when nothing left reads them, with each of them
// nothing left uses; and what it names can't go while something left reads them.
type Tied interface {
	Referrer
	// Ties are what it stands for, by kind and name, in its own scope: the values it
	// sets, and what else a resource of that name makes for them, as a link to a
	// server it shares makes its network.
	Ties(name string, spec proto.Message) []Ref
}

// Ties are the values a spec of kind sets, where its kind is Tied.
func (r *Registry) Ties(kind Kind, name string, spec proto.Message) []Ref {
	if t, ok := r.types[kind].(Tied); ok && spec != nil {
		return t.Ties(name, spec)
	}
	return nil
}

// TiedKinds are the kinds that are Tied.
func (r *Registry) TiedKinds() []Kind {
	var out []Kind
	for kind, t := range r.types {
		if _, ok := t.(Tied); ok {
			out = append(out, kind)
		}
	}
	slices.Sort(out)
	return out
}

// Reference is a resource a spec names, in the spec's own scope.
type Reference struct {
	Kind Kind
	Name string
	// Verb says how the spec uses it, for people: "mount", or "join".
	Verb string
	// ReadOnly says the spec uses it read-only, as a read-only mount of a
	// volume does: what the volume reaches, the spec reaches read-only.
	ReadOnly bool
	// Apart says the spec gains nothing of what the resource reaches, as a
	// route sends traffic to a service and has no part in what it mounts.
	Apart bool
	// Inside says the spec's resource lives inside the one it names, as a
	// database user lives in its database server. When one change deletes
	// both on a server, the one it names goes first, so deleting this one
	// takes nothing with it: its data stays where the other kept it.
	Inside bool
}

// Claim is something only one resource may hold across an install, whatever
// its scope, such as a domain and a path.
type Claim struct {
	// Key is what one resource alone may hold: app.example.com/api.
	Key string
	// Group is what one scope alone may hold, so that no other scope can
	// claim a part of it: app.example.com.
	Group string
	// Manner is how a claim serves its Group, which every claim on a Group must
	// agree on: a domain with a certificate, or without. Empty is one manner too.
	Manner string
	// MannerWords say what Manner is, for people: "takes no certificate".
	MannerWords string
	// Words say what Key is, for people: "app.example.com/api".
	Words string
	// GroupWords say what Group is, for people: "the domain app.example.com".
	GroupWords string
}

// Claimer is a kind whose specs hold something across the install. The
// engine refuses a change that would have two resources hold one Key, or two
// scopes hold one Group, and asks for a person when a change claims a Group
// another scope held within the undo window (ADR 0024).
type Claimer interface {
	Claims(spec proto.Message) []Claim
}

// TakesOver says, for a Claimer, what a person approves when a scope claims
// a Group another scope held, whose name is held.
type TakesOver interface {
	TakesOver(group, held string) Access
}

// WeakensClaims is a Claimer whose specs can serve a Group a weaker way than
// another spec of the kind does, which a person approves when it comes to a Group
// served the stronger way within the undo window. A token could otherwise take a
// live domain off HTTPS, in place or by deleting a route in one change and making
// another in the next (ADR 0030).
type WeakensClaims interface {
	// Weak reports a spec that serves what it claims the weaker way.
	Weak(spec proto.Message) bool
	// Weakens is what a person approves for it.
	Weakens(group string) Access
}

// SingleServer is a kind whose resources run on one server at a time, whose
// answer says why, for people.
type SingleServer interface {
	OneServer() string
}

// Answerer is a kind whose specs answer to names on networks, as a service does.
// The engine refuses a change that leaves two of them answering to one name on a
// network in their scope, as each could get the other's traffic. It isn't a Claimer,
// which holds something across an install: this is about one network on one server,
// and reading every stored spec in the install for it would open every sealed one.
type Answerer interface {
	Answers(ref Ref, spec proto.Message) []Answer
}

// Answer is a name a resource answers to on a network.
type Answer struct {
	// Network is the network's name: in its scope, or in Docker when External.
	Network string
	// External says the network is one the server has.
	External bool
	Name     string
}

// Answers lists the names spec answers to, if its kind is an Answerer.
func (r *Registry) Answers(ref Ref, spec proto.Message) []Answer {
	if spec == nil {
		return nil
	}
	if c, ok := r.types[ref.Kind].(Answerer); ok {
		return c.Answers(ref, spec)
	}
	return nil
}

// DataKeeper is a kind whose specs name data that outlives what runs, so a plan can
// say what a rollback leaves as it is.
type DataKeeper interface {
	// KeptData names the volumes and server paths a spec writes to.
	KeptData(spec proto.Message) []string
}

// TextNamer is a kind whose spec can write, as text, what reads as naming a value
// it has: ${NAME} in a field that doesn't name values, which runs as those words.
type TextNamer interface {
	// NamedAsText says, by field, what a spec writes as ${NAME} in a field that holds
	// text, for a name it has a value for.
	NamedAsText(spec proto.Message) map[string][]string
}

// NamedAsText says what spec writes as ${NAME} as text, if its kind is a TextNamer.
func (r *Registry) NamedAsText(kind Kind, spec proto.Message) map[string][]string {
	if spec == nil {
		return nil
	}
	if n, ok := r.types[kind].(TextNamer); ok {
		return n.NamedAsText(spec)
	}
	return nil
}

// KeptData names what spec writes to, if its kind is a DataKeeper.
func (r *Registry) KeptData(ref Ref, spec proto.Message) []string {
	if spec == nil {
		return nil
	}
	if k, ok := r.types[ref.Kind].(DataKeeper); ok {
		return k.KeptData(spec)
	}
	return nil
}

// References lists what spec names, if its kind is a Referrer.
func (r *Registry) References(kind Kind, spec proto.Message) []Reference {
	if spec == nil {
		return nil
	}
	if ref, ok := r.types[kind].(Referrer); ok {
		return ref.References(spec)
	}
	return nil
}

// Named reports whether any kind's specs can name a resource of kind.
func (r *Registry) Named(kind Kind) bool {
	for _, t := range r.types {
		if ref, ok := t.(Referrer); ok && slices.Contains(ref.ReferenceKinds(), kind) {
			return true
		}
	}
	return false
}

// Claims lists what spec holds across the install, if its kind holds
// anything.
func (r *Registry) Claims(kind Kind, spec proto.Message) []Claim {
	if spec == nil {
		return nil
	}
	if c, ok := r.types[kind].(Claimer); ok {
		return c.Claims(spec)
	}
	return nil
}

// Claimers lists the kinds whose specs hold something across the install,
// sorted.
func (r *Registry) Claimers() []Kind {
	var out []Kind
	for k, t := range r.types {
		if _, ok := t.(Claimer); ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// HasReferrers reports whether any kind names others.
func (r *Registry) HasReferrers() bool {
	for _, t := range r.types {
		if _, ok := t.(Referrer); ok {
			return true
		}
	}
	return false
}

// SharedReadier is a Readier whose answer others share, such as each
// Docker driver's, which is whether one Docker Engine answers.
type SharedReadier interface {
	Readier
	// ReadyKey is what the answer is about, such as the Docker client:
	// drivers whose keys are equal share one answer.
	ReadyKey() any
}

// Ready lists the kinds whose drivers can run now, sorted, and why each
// other kind with a driver can't. Drivers that share an answer are asked
// once.
func (r *Registry) Ready(ctx context.Context) (ready []Kind, unavailable map[Kind]string) {
	ready, why := r.ReadyWhy(ctx)
	for k, err := range why {
		if unavailable == nil {
			unavailable = map[Kind]string{}
		}
		unavailable[k] = err.Error()
	}
	return ready, unavailable
}

// ReadyWhy is Ready with each reason as the driver gave it, so a caller can
// tell a server that doesn't have what a kind needs (IsNotHere) from a fault.
func (r *Registry) ReadyWhy(ctx context.Context) (ready []Kind, unavailable map[Kind]error) {
	answers := map[any]error{}
	ask := func(d Readier) error {
		s, ok := d.(SharedReadier)
		if !ok {
			return d.Ready(ctx)
		}
		key := s.ReadyKey()
		if err, asked := answers[key]; asked {
			return err
		}
		err := d.Ready(ctx)
		answers[key] = err
		return err
	}
	for _, k := range r.Kinds() {
		d, _ := r.types[k].(Readier)
		if d == nil {
			ready = append(ready, k)
			continue
		}
		if err := ask(d); err != nil {
			if unavailable == nil {
				unavailable = map[Kind]error{}
			}
			unavailable[k] = err
			continue
		}
		ready = append(ready, k)
	}
	return ready, unavailable
}

// With returns a registry with more types, which replace any of the same
// kind.
func (r *Registry) With(types ...Type) *Registry {
	all := make([]Type, 0, len(r.types)+len(types))
	for _, t := range r.types {
		all = append(all, t)
	}
	return NewRegistry(append(all, types...)...)
}

// Type returns the type of kind.
func (r *Registry) Type(kind Kind) (Type, error) {
	t, ok := r.types[kind]
	if !ok {
		return nil, fmt.Errorf("dockwell doesn't manage resources of kind %q", kind)
	}
	return t, nil
}

// ErrNoDriver means a server's agent has no driver for a kind, so it can't
// plan or apply resources of that kind.
var ErrNoDriver = errors.New("no driver for this kind")

// ErrAgentTooOld is an agent from before a container's command could name a value,
// which would read such a container's command as Docker has it, value and all
// (ADR 0022). Nothing is read, planned or applied on it that names a value.
var ErrAgentTooOld = errors.New("the agent is older than what this server runs")

// Kinds lists the kinds the registry has drivers for, sorted.
func (r *Registry) Kinds() []Kind {
	var out []Kind
	for k, t := range r.types {
		if _, ok := t.(Driver); ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// All lists every kind the registry has a type for, sorted.
func (r *Registry) All() []Kind {
	out := slices.Collect(maps.Keys(r.types))
	slices.Sort(out)
	return out
}

// Driver returns the driver for kind.
func (r *Registry) Driver(kind Kind) (Driver, error) {
	t, err := r.Type(kind)
	if err != nil {
		return nil, err
	}
	d, ok := t.(Driver)
	if !ok {
		return nil, fmt.Errorf("%s resources apply on the control plane, not on a server", kind)
	}
	return d, nil
}

// KindOf returns the kind whose spec is spec's message type.
func (r *Registry) KindOf(spec proto.Message) (Kind, bool) {
	name := spec.ProtoReflect().Descriptor().FullName()
	for k, t := range r.types {
		if t.New().ProtoReflect().Descriptor().FullName() == name {
			return k, true
		}
	}
	return "", false
}

// Validate checks spec against its kind: the right message type, valid
// values, and defaults filled in.
func (r *Registry) Validate(kind Kind, spec proto.Message) error {
	d, err := r.Type(kind)
	if err != nil {
		return err
	}
	want := d.New().ProtoReflect().Descriptor().FullName()
	if got := spec.ProtoReflect().Descriptor().FullName(); got != want {
		return fmt.Errorf("a %s spec must be a %s, not a %s", kind, want, got)
	}
	return d.Validate(spec)
}

// Decode reads a stored spec of kind.
func (r *Registry) Decode(kind Kind, b []byte) (proto.Message, error) {
	d, err := r.Type(kind)
	if err != nil {
		return nil, err
	}
	spec := d.New()
	if err := proto.Unmarshal(b, spec); err != nil {
		return nil, fmt.Errorf("can't read a stored %s spec: %w", kind, err)
	}
	return spec, nil
}

// Encode writes a spec deterministically, so equal specs encode equal. An
// empty spec encodes to no bytes, but never to nil, which the store keeps
// for a deletion.
func Encode(spec proto.Message) ([]byte, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(spec)
	if err == nil && b == nil {
		b = []byte{}
	}
	return b, err
}
