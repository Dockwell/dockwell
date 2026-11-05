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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

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

// Decision is a driver's judgement of one step.
type Decision struct {
	Action Action
	Effect Effect
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
	// Leads says where the server paths the resource mounts lead through symlinks,
	// by path, for those that lead elsewhere. An agent reads it; the control plane
	// puts it on HostAccess (Access.Leading), and it goes no further.
	Leads map[string]string `json:",omitempty"`
	// Pull is true when the step asks the server for its image again first. With
	// no Changes, it replaces what runs only if that finds a newer image. With
	// nothing but a restart policy to change, it changes that in place otherwise.
	Pull bool `json:",omitempty"`
	// Pulled is true when a run stopped partway had already pulled on this server,
	// as the record says. Resuming needs no registry, so it compares what the
	// server has with what runs, and goes on from there.
	Pulled bool `json:",omitempty"`
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
	// Goes says what someone visiting a route finds once the step takes it away, as
	// a route stops nothing the plan counts as downtime and yet stops people reaching
	// what it served. It never stops a plan, and a fingerprint leaves it out, as it
	// depends on the server's other routes.
	Goes *RouteGoes `json:",omitempty"`
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
}

// DroppedMount is something the old container mounts and the new one won't.
type DroppedMount struct {
	Source, Target string
	// EmptyVolume says the new image declares a volume there, so an empty one
	// takes its place.
	EmptyVolume bool `json:",omitempty"`
}

// TwoWriters reports a step that lets two containers write to one place, as
// a spec chose, for as long as it takes. Two writers can corrupt a database, so
// it is for a person to approve, as a plan that destroys data is.
func (r *RolloutPlan) TwoWriters() bool {
	return r != nil && r.Rolling && r.Chosen
}

// Budget is the most a step may take beyond its pull and its own work: how
// long it may wait on a health check, on what is in flight, and on old
// containers to stop. An agent's call for the step gets this much more time.
func (d Decision) Budget() time.Duration {
	b := max(d.Downtime, d.UntilHealthy)
	if r := d.Rollout; r != nil {
		b += r.HealthyWithin + r.Soak + r.Drain + r.StopGrace
	}
	return b
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
	Detail string `json:",omitempty"`
	// SameAsRoot says this is the same as root on the server because of what it
	// is, whatever else it says: the capability SYS_ADMIN, or a path such as /etc,
	// or a plugin that mounts there (SystemPath). AsGoodAsRoot says it for every
	// kind.
	SameAsRoot bool `json:",omitempty"`
	// LeadsTo is where Path leads through symlinks, as the server's agent reads
	// it, when that is somewhere else.
	LeadsTo string `json:",omitempty"`
}

// Leading is a with where its path leads on the server, as an agent says. An
// agent can only make a plan louder this way: what the path reads as written
// still counts, and the gate for a person is the control plane's own. So an agent
// that lies, or is too old to say, leaves the plan reading what the spec says.
func (a Access) Leading(to string) Access {
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
	return a.SameAsRoot || a.Kind == AccessDocker || a.Kind == AccessNetwork ||
		a.Kind == AccessNetworkDriver && a.Driver == "host"
}

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
)

// NewAccess lists what to reaches that from doesn't: access it didn't
// have, or has another way, as read-write where it was read-only, or
// relabelled where it wasn't, or at another address. Access to Docker's
// socket is the same read-only or not, as a socket mounted read-only still
// takes connections.
func NewAccess(from, to []Access) []Access {
	// writable says, for each access with ReadOnly cleared, whether from
	// has it read-write.
	writable := make(map[Access]bool, len(from))
	for i := range from {
		b := from[i]
		ro := b.ReadOnly
		b.ReadOnly = false
		writable[b] = writable[b] || !ro
	}
	var out []Access
	seen := make(map[Access]bool)
	for i := range to {
		a := &to[i]
		key := *a
		key.ReadOnly = false
		rw, has := writable[key]
		if has && (a.ReadOnly || rw || a.Kind == AccessDocker) || seen[*a] {
			continue
		}
		seen[*a] = true
		out = append(out, *a)
	}
	return out
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
		root := ""
		if a.SameAsRoot {
			root = ", which is as good as root"
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
			return "the volume driver " + plain(a.Driver) + " at " + plain(a.Path) + ", which reaches what the plugin does (" + how + ")"
		}
		if a.Driver != "" {
			return "the volume driver " + plain(a.Driver) + ", which reaches what the plugin does (" + how + ")"
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
			return "a " + plain(a.Type) + " mount (" + how + ")"
		}
		return "the " + plain(a.Type) + " mount " + at + " (" + how + ")"
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
	case AccessExternalNetwork:
		out := "the network " + plain(a.Path) + ", which is on the server already, where it answers to its name"
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
		return plain(a.Path) + " as a proxy it believes on where a request came from"
	case AccessPlainForAnyone:
		return "plain HTTP on " + plain(a.Path) + " for anyone who can reach this server, around whatever is in front of it"
	case AccessHTTPSOff:
		return "HTTPS on " + plain(a.Path) + ", which stops: https:// addresses get an error, and browsers that saw HSTS from it refuse plain HTTP until that runs out"
	}
	return fmt.Sprintf("access(%d)", int(a.Kind))
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

// NameChecker is a Type that takes only some names, such as names that fit
// in a URL. Kinds without one take any name that isn't blank.
type NameChecker interface {
	// CheckName says why a name won't do, with an example of one that
	// would.
	CheckName(name string) error
}

var labelRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// CheckLabel requires a name that is a DNS label, so it fits in URLs, in
// hostnames such as a service's URL, and on the command line: 1 to 63
// lowercase letters, digits and dashes, starting with a letter and ending
// with a letter or digit. Projects, environments and services take such
// names. what names the kind, and example is a name that would do.
func CheckLabel(what, name, example string) error {
	if !labelRE.MatchString(name) {
		return fmt.Errorf("%q isn't a valid %s name: use up to 63 lowercase letters, digits and dashes, starting with a letter and ending with a letter or digit, such as %s",
			name, what, example)
	}
	return nil
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

// Use is one resource that another uses, in the same scope.
type Use struct {
	// As is the name the user sees it as, such as DATABASE_URL.
	As   string
	Kind Kind
	// Name is its name in the scope.
	Name string
	// Revision is the revision it is pinned to, 0 when it isn't.
	Revision int
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
				unavailable = map[Kind]string{}
			}
			unavailable[k] = err.Error()
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
