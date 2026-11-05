// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
)

// A rollout moves a service from the containers it has to one that matches its
// spec, whatever it finds, so a rollout that a crash stopped at any step, or a
// rollback of one, is the same code with less to do. See
// docs/adr/0027-health-checks-rollouts-and-rollback.md.
//
// The steps it can stop between, for internal/fault's Points, in order. A
// rolling step goes through each once, and each is safe to find done.
const (
	PointReserved    = "reserved"    // the new instance has an address
	PointCreated     = "created"     // it exists, and hasn't started
	PointStarted     = "started"     // it runs, and isn't known to be healthy
	PointHealthy     = "healthy"     // it passed its check, and has no traffic
	PointUpstream    = "upstream"    // it has the traffic, and the old one has what is in flight
	PointDrained     = "drained"     // what the old one had is done
	PointMarked      = "marked"      // the old one reads as a rollout's to stop, and may not have been told
	PointStopped     = "stopped"     // the old one stopped
	PointRemoved     = "removed"     // the old one is gone, and still has an address
	PointForgotten   = "forgotten"   // its address is free: nothing is left to do
	PointSwapReady   = "swapready"   // a stop-first step made the new one, under its own name
	PointSwapMarked  = "swapmarked"  // and marked the old one, which may not have been told
	PointSwapStopped = "swapstopped" // and stopped it, and hasn't removed it
	PointSwapGone    = "swapgone"    // and removed the old one
	PointSwapNamed   = "swapnamed"   // and gave the new one its name, and hasn't started it
)

// Rolling are the points of a rolling step, in order, and StopFirst those of a
// step that stops the old one first.
var (
	Rolling   = []string{PointReserved, PointCreated, PointStarted, PointHealthy, PointUpstream, PointDrained, PointMarked, PointStopped, PointRemoved, PointForgotten}
	StopFirst = []string{PointSwapReady, PointSwapMarked, PointSwapStopped, PointSwapGone, PointSwapNamed, PointStarted}
)

// What a container carries, as Docker can't change a label on one that exists.
const (
	// LabelRestart names the restart that made it: its change.
	LabelRestart = docker.OwnPrefix + "restart"
	// LabelRollout holds its spec's rollout settings, as JSON.
	LabelRollout = docker.OwnPrefix + "rollout"
)

// Timing of a rollout.
const (
	// DefaultDrain is how long what is in flight gets to finish, as the PRD
	// says of connection draining.
	DefaultDrain = 30 * time.Second
	// ListenWait is how long a new instance with no health check gets to take
	// connections on the ports its routes send to.
	ListenWait = 2 * time.Minute
	// DefaultSettle is how long a container with no check must keep running to
	// count as started.
	DefaultSettle = 2 * time.Second
	// DefaultSoak is how long a new container must stay healthy before anything
	// moves to it or the old one goes, as one pass says little of an app that
	// comes up and falls over on its first real work (ADR 0027).
	DefaultSoak = 5 * time.Second
	// soakPasses is how many of the agent's own checks must pass in a row within
	// it.
	soakPasses = 3
	// settled is how long a container that a step didn't start has to have run for
	// a pass to be enough: it isn't new.
	settled = time.Minute
)

// instance is one of a service's containers.
type instance struct {
	*docker.Container
	number int
	// temp means it has the name a replace gives what it swaps in, as Compose
	// does, and hasn't got the name of its number yet.
	temp bool
	// stopping means a rollout marked it to stop. Docker goes on stopping a
	// container whoever asked has gone, so it may yet stop if it hasn't, and a
	// stopped one isn't one a person stopped by hand.
	stopping bool
}

func (i *instance) name() string { return strings.TrimPrefix(i.Name, "/") }

// serving reports a container that runs and isn't on its way out.
func (i *instance) serving() bool { return notRunning(i.Container) == "" && !i.stopping }

// swapName is the name a replace gives what it swaps in, as Compose does: the
// container it replaces, 12 digits of its ID, then its name.
var swapName = regexp.MustCompile(`^/?[0-9a-f]{12}_`)

// PlainName is the name a container goes by, without what a rollout has in front
// of it for now: the mark on one it is stopping, or what one that is to take the
// name waits under. That name is what a person knows it by, and what a log says.
func PlainName(name string) string { return rolloutPrefix.ReplaceAllString(name, "") }

var rolloutPrefix = regexp.MustCompile(`^/?(?:` + stoppingPrefix + `[0-9a-f]{12}_)?(?:[0-9a-f]{12}_)?`)

// stoppingPrefix starts the name of a container a rollout is stopping. Docker
// keeps a name through anything, and a label can't change, so the mark is there
// however the step ends, a SIGKILL included.
const stoppingPrefix = "stopping_"

// instances lists the containers of t's service, newest first, by the labels
// Dockwell and Compose give them. A one-off, as docker compose run makes, isn't
// one. Numbers only go up, so the highest is the newest, and where a replace
// is swapping one in, it is newer than the one it replaces, of the same number.
func (d *Docker) instances(ctx context.Context, t Target) ([]*instance, error) {
	list, err := d.Client.ListContainers(ctx, map[string][]string{
		"label": {docker.LabelProject + "=" + t.Project, docker.LabelService + "=" + t.Service},
	})
	if err != nil {
		return nil, err
	}
	out := make([]*instance, 0, len(list))
	for k := range list {
		s := &list[k]
		if s.Labels[docker.LabelOneoff] == "True" {
			continue
		}
		c, err := d.Client.InspectContainer(ctx, s.ID)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(c.Name, "/")
		out = append(out, &instance{Container: c, number: s.Number(), temp: swapName.MatchString(c.Name), stopping: strings.HasPrefix(name, stoppingPrefix)})
	}
	slices.SortStableFunc(out, func(a, b *instance) int {
		if a.number != b.number {
			return b.number - a.number
		}
		switch {
		case a.temp && !b.temp:
			return -1
		case b.temp && !a.temp:
			return 1
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out, nil
}

// instanceName is what a container of t of a number is named: Compose's name
// for a service's, and its own for a container on its own, which has one.
func instanceName(t Target, number int) string {
	if !t.Scoped {
		return t.Name
	}
	return docker.InstanceName(t.Project, t.Service, number)
}

func (t Target) instance(number int) ingress.Instance {
	return ingress.Instance{Project: t.Project, Service: t.Service, Number: number}
}

// matches reports a container that reads back as spec, as planning would read
// it.
func (d *Docker) matches(ctx context.Context, t Target, c *docker.Container, spec *resourcev1.Container) (bool, error) {
	got, err := d.projected(ctx, t, c)
	if err != nil {
		return false, err
	}
	Normalize(got)
	leaveOutValues(got, spec)
	return resource.Equal(got, spec), nil
}

// leaveOutValues takes out of what a container reads back the values of what a
// service uses, where spec has none. Only a step's call brings them, so a plan goes
// by the label, which names each and its revision, and a step has the values to
// compare too (ADR 0022).
func leaveOutValues(got, spec *resourcev1.Container) {
	uses, err := binding.ParseLabel(spec.GetLabels()[binding.LabelKey])
	if err != nil {
		return
	}
	for _, u := range uses {
		if _, has := spec.GetEnv()[u.As]; !has {
			delete(got.GetEnv(), u.As)
		}
	}
}

// pick splits a service's instances into the one the spec is met by, if any,
// the newest that matches, and the others, which go. A restart wants one that
// restart made. A container on its own is never kept: it has one name, and a
// replace makes it anew.
func (d *Docker) pick(ctx context.Context, t Target, spec *resourcev1.Container, restart string, all []*instance) (keep *instance, others []*instance, err error) {
	for _, i := range all {
		if keep != nil || !t.Scoped || restart != "" && i.Config.Labels[LabelRestart] != restart {
			others = append(others, i)
			continue
		}
		same, err := d.matches(ctx, t, i.Container, spec)
		if err != nil {
			return nil, nil, err
		}
		if same {
			keep = i
		} else {
			others = append(others, i)
		}
	}
	return keep, others, nil
}

// portsOverlap lists the server ports spec would publish that have a publishes
// too.
func portsOverlap(spec, have *resourcev1.Container) []string {
	var out []string
	for _, p := range spec.GetPorts() {
		for _, o := range have.GetPorts() {
			if p.GetHostPort() == o.GetHostPort() && protocolName(p.GetProtocol()) == protocolName(o.GetProtocol()) && overlapValid(p.GetHostIp(), o.GetHostIp()) {
				out = append(out, fmt.Sprintf("%d/%s", p.GetHostPort(), protocolName(p.GetProtocol())))
				break
			}
		}
	}
	return out
}

// overlapValid is overlap for addresses read back from Docker, which may not
// parse: one that doesn't overlaps anything, as what can't be read can't be
// called safe.
func overlapValid(a, b string) bool {
	if a == "" || b == "" || a == b {
		return true
	}
	x, err1 := netip.ParseAddr(a)
	y, err2 := netip.ParseAddr(b)
	if err1 != nil || err2 != nil {
		return true
	}
	return x.Is4() == y.Is4() && (x.IsUnspecified() || y.IsUnspecified())
}

// decide says how a step gets from what the server has to spec, from what the
// two would share. What it can't read as safe to share stops it rolling. Two
// writers on a database's files, even for seconds, can corrupt it, and Dockwell
// can't tell a database from a directory of uploads, so a read-write mount both
// have is exclusive unless the spec says otherwise (ADR 0027). It compares where
// each mount really lands, so one directory under two names is one directory, and
// says so when it can't tell.
func (d *Docker) decide(ctx context.Context, t Target, spec *resourcev1.Container, keep *instance, others []*instance) (*resource.RolloutPlan, error) {
	p := &resource.RolloutPlan{Drain: drainOf(spec), Soak: d.soak()}
	ports, volumes, paths, data, cant := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	targets := map[string]bool{}
	for _, m := range spec.GetMounts() {
		targets[m.GetTarget()] = true
	}
	landings := map[string]landing{}
	for _, o := range others {
		have := Project(o.Container, nil, t)
		for _, port := range portsOverlap(spec, have) {
			ports[port] = true
		}
		for _, m := range spec.GetMounts() {
			for _, h := range have.GetMounts() {
				if m.GetReadOnly() && h.GetReadOnly() {
					continue
				}
				if v := m.GetVolume(); v != "" && v == h.GetVolume() {
					volumes[v] = true
					continue
				}
				a, b := d.landing(ctx, t, m, landings), d.landing(ctx, t, h, landings)
				switch shareOf(a, b) {
				case sharesOne:
					paths[bothWrite(a, b)] = true
				case cantTellIfShared:
					cant[a.name], cant[b.name] = true, true
				case sharesNothing:
				}
			}
		}
		// What the image keeps moves to the new one, unless a mount now covers it.
		for path := range imageVolumes(o.Container) {
			if !targets[path] {
				data[path] = true
			}
		}
		p.StopGrace = max(p.StopGrace, stopGrace(o.Container))
		p.Others = append(p.Others, o.name())
	}
	p.Ports, p.Volumes, p.Paths, p.ImageData = slices.Sorted(maps.Keys(ports)), slices.Sorted(maps.Keys(volumes)), slices.Sorted(maps.Keys(paths)), slices.Sorted(maps.Keys(data))
	p.CantTell = slices.Sorted(maps.Keys(cant))
	// Either way round: one that moves off the server's own network still needs
	// what the old one holds there, if it publishes the same port.
	p.HostNetwork = spec.GetHostNetwork() || slices.ContainsFunc(others, func(o *instance) bool { return o.HostConfig.NetworkMode == "host" })
	if d.Ingress != nil && t.Scoped {
		routing, err := d.Ingress.Routing(ctx, ingress.Service{Project: t.Project, Name: t.Service})
		if err != nil {
			return nil, err
		}
		p.EditedRoutes = routing.Edited
	}
	shared := len(p.Volumes)+len(p.Paths)+len(p.ImageData)+len(p.CantTell) > 0
	order := spec.GetRollout().GetOrder()
	switch {
	case !t.Scoped:
	case keep != nil && keep.serving():
		// What the spec wants already runs, as when rolling back a new one that
		// failed its check: nothing needs to stop for it.
		p.Rolling = true
	case shared && resource.DeployOf(ctx).Rollback:
		// A rollback runs before anyone reads its plan, so nobody approves two writers,
		// whatever the order it goes back to says. And what it replaces has just failed.
	case len(p.Ports) > 0 || len(p.EditedRoutes) > 0 || p.HostNetwork:
		// A route edited by hand isn't Dockwell's to move, so what it dials has to
		// stay what the new container has: its number, and so its address. And a
		// container on the server's own network listens on the server's ports, so
		// with another there it can't bind them, or a check of them reads the other.
	case order == resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST:
		p.Rolling, p.Chosen = true, shared
	case order == resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST:
		p.Chosen = true
	default:
		p.Rolling = !shared
	}
	if p.Rolling && keep == nil {
		// Docker has no way to add an alias to a container that has joined a
		// network, and joining again would cut what the new one has open there.
		// On a network the server has, that means other projects' containers too.
		p.EarlyNetworks = slices.Concat(spec.GetNetworks(), spec.GetExternalNetworks())
		if len(p.EarlyNetworks) > 0 {
			p.EarlyNames = spec.GetAliases()
		}
	}
	return p, nil
}

// landing is where a mount really lands on the server, as far as the agent can
// read it.
type landing struct {
	// name is how a person knows it: a volume's name, or a server path.
	name string
	// path is the directory it lands in, with links followed, for a server path
	// and a volume that binds one.
	path string
	// volume is its name, for a volume Docker keeps itself, which nothing else
	// reaches.
	volume string
}

// Whether two mounts land on one place.
const (
	sharesNothing = iota
	sharesOne
	cantTellIfShared
)

// shareOf says whether two mounts land on one place. A landing with neither a
// path nor a volume of its own is on another driver or can't be read, and could be
// anything, other than what Docker keeps for itself.
func shareOf(a, b landing) int {
	switch {
	case a.volume != "" && b.volume != "":
		if a.volume == b.volume {
			return sharesOne
		}
		return sharesNothing
	case a.volume != "" || b.volume != "":
		return sharesNothing
	case a.path != "" && b.path != "":
		if within(a.path, b.path) || within(b.path, a.path) {
			return sharesOne
		}
		return sharesNothing
	}
	return cantTellIfShared
}

// bothWrite says where two mounts that share a place both reach: the one under the
// other.
func bothWrite(a, b landing) string {
	if len(b.path) > len(a.path) {
		return b.path
	}
	return cmp.Or(a.path, a.name)
}

// landing reads where a mount lands, keeping what it read in seen.
func (d *Docker) landing(ctx context.Context, t Target, m *resourcev1.Mount, seen map[string]landing) landing {
	key := m.GetVolume() + "|" + m.GetHostPath()
	if l, ok := seen[key]; ok {
		return l
	}
	l := d.readLanding(ctx, t, m)
	seen[key] = l
	return l
}

func (d *Docker) readLanding(ctx context.Context, t Target, m *resourcev1.Mount) landing {
	if p := m.GetHostPath(); p != "" {
		resolved, err := d.resolvePath(p)
		if err != nil {
			return landing{name: p}
		}
		return landing{name: p, path: resolved}
	}
	name := m.GetVolume()
	v, err := d.Client.InspectVolume(ctx, t.DockerName(name))
	switch {
	case docker.IsNotFound(err):
		// The same change makes it: a step that finds otherwise when it runs says
		// to plan again.
		return landing{name: name, volume: name}
	case err != nil:
		return landing{name: name}
	}
	if dev, ok := BindDevice(v.Driver, v.Options); ok {
		resolved, err := d.resolvePath(dev)
		if err != nil {
			return landing{name: name}
		}
		return landing{name: name, path: resolved}
	}
	if (v.Driver == "local" || v.Driver == "") && len(v.Options) == 0 {
		return landing{name: name, volume: name}
	}
	return landing{name: name}
}

// resolvePath is where a server path leads, with links followed as far as there
// is anything there: a directory Docker is yet to make can't be a link.
func (d *Docker) resolvePath(p string) (string, error) {
	if d.ResolvePath != nil {
		return d.ResolvePath(p)
	}
	rest := ""
	for at := filepath.Clean(p); ; {
		resolved, err := filepath.EvalSymlinks(at)
		switch {
		case err == nil:
			return filepath.Join(resolved, rest), nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", err
		}
		parent := filepath.Dir(at)
		if parent == at {
			return filepath.Clean(p), nil
		}
		rest = filepath.Join(filepath.Base(at), rest)
		at = parent
	}
}

// soak is how long a new container must stay healthy.
func (d *Docker) soak() time.Duration {
	switch {
	case d.Soak < 0:
		return 0
	case d.Soak > 0:
		return d.Soak
	}
	return DefaultSoak
}

// drainOf is how long what is in flight to an old container gets to finish.
func drainOf(spec *resourcev1.Container) time.Duration {
	if v, err := time.ParseDuration(spec.GetRollout().GetDrain()); err == nil && v > 0 {
		return v
	}
	return DefaultDrain
}

// stopGrace is how long a container gets to stop, as it was made with.
func stopGrace(c *docker.Container) time.Duration {
	if s := c.Config.StopTimeout; s != nil {
		return time.Duration(*s) * time.Second
	}
	return StopTimeout
}

// judgeRollout says how a step replaces what runs: rolling has no downtime, and
// takes as long as the new one takes to be healthy, as what serves goes on
// serving. Stopping first has the downtime Plan said, from what is really there.
func (d *Docker) judgeRollout(ctx context.Context, t Target, desired *resourcev1.Container, keep *instance, others []*instance, dec *resource.Decision) error {
	dec.Action = resource.ActionReplace
	p, err := d.decide(ctx, t, desired, keep, others)
	if err != nil {
		return err
	}
	dec.Rollout = p
	img, err := d.Client.InspectImage(ctx, docker.WithTag(desired.GetImage()))
	switch {
	case err == nil:
	case docker.IsNotFound(err):
		img = nil
	default:
		return err
	}
	if keep == nil {
		p.Dropped = dropped(t, desired, newest(others), img)
	}
	if !p.Rolling {
		dec.Effect = resource.EffectRecreate
		dec.Downtime = p.StopGrace + LogCatchUp + recreateStart
		return nil
	}
	dec.Effect, dec.Downtime, dec.UntilHealthy = resource.EffectRoll, 0, 0
	if keep != nil && keep.serving() {
		p.Soak = 0
		return nil
	}
	var own *docker.Healthcheck
	if img != nil {
		own = img.Config.Healthcheck
	} else {
		dec.HealthUnknown = true
	}
	p.HealthyWithin = HealthBound(desired.GetHealthcheck(), own)
	switch {
	case dec.HealthUnknown:
		// Its own check isn't known until it is pulled, and it gets what any does.
		p.HealthyWithin = max(p.HealthyWithin, or(d.HealthWait, DefaultHealthWait))
	case p.HealthyWithin > 0:
	default:
		p.NoCheck = true
		p.HealthyWithin = or(d.Settle, DefaultSettle)
		if d.Ingress != nil && t.Scoped {
			routing, err := d.Ingress.Routing(ctx, ingress.Service{Project: t.Project, Name: t.Service})
			if err != nil {
				return err
			}
			if routing.Routed {
				// What it waits for then is that it listens, which is a check of a kind.
				p.HealthyWithin, p.NoCheck = or(d.ListenWait, ListenWait), false
			}
		}
	}
	return nil
}

// dropped is what old mounts that spec doesn't, at a place spec doesn't cover. It
// stays on the server, and the service no longer sees it, which a change to its
// mounts doesn't say on its own. Where the new image declares a volume, an empty
// one takes its place, which nothing but the image says.
func dropped(t Target, spec *resourcev1.Container, old *docker.Container, img *docker.Image) []resource.DroppedMount {
	if old == nil {
		return nil
	}
	var out []resource.DroppedMount
	for _, h := range Project(old, nil, t).GetMounts() {
		if h.GetReadOnly() || slices.ContainsFunc(spec.GetMounts(), func(m *resourcev1.Mount) bool { return m.GetTarget() == h.GetTarget() }) {
			continue
		}
		m := resource.DroppedMount{Source: cmp.Or(h.GetVolume(), h.GetHostPath()), Target: h.GetTarget()}
		if img != nil {
			_, m.EmptyVolume = img.Config.Volumes[h.GetTarget()]
		}
		out = append(out, m)
	}
	return out
}

// neverStarted is the state of a container that was made and hasn't started yet.
const neverStarted = "created"

// Left says what is left of a service that reads back as its spec: another
// container, what a replace hasn't finished naming, or a restart it hasn't had.
func (d *Docker) Left(ctx context.Context, t Target, desired *resourcev1.Container, restart string) (resource.Decision, bool, error) {
	all, err := d.instances(ctx, t)
	if err != nil || len(all) == 0 && restart == "" {
		return resource.Decision{}, false, err
	}
	keep, others, err := d.pick(ctx, t, desired, restart, all)
	if err != nil {
		return resource.Decision{}, false, err
	}
	// One that was made and never started is what a replace that died just
	// before its last step leaves, and nothing else starts it: Docker's restart
	// policy passes over what never ran. One someone stopped by hand has run, and
	// that isn't drift (ADR 0021): it stays stopped.
	// One a rollout marked isn't one a person stopped: the step that marked it
	// didn't get to the end.
	if keep != nil && len(others) == 0 && !keep.temp && !keep.stopping && keep.State.Status != neverStarted {
		return resource.Decision{}, false, nil
	}
	dec := resource.Decision{}
	if err := d.judgeRollout(ctx, t, desired, keep, others, &dec); err != nil {
		return resource.Decision{}, false, err
	}
	return dec, true, nil
}

// roll moves a service to spec, from whatever it finds.
func (d *Docker) roll(ctx context.Context, t Target, spec *resourcev1.Container, dec resource.Decision) error {
	release := func() {}
	if d.Ingress != nil && t.Scoped {
		release = d.Ingress.Hold(ingress.Service{Project: t.Project, Name: t.Service})
	}
	defer release()
	all, err := d.instances(ctx, t)
	if err != nil {
		return err
	}
	keep, others, err := d.pick(ctx, t, spec, dec.Restart, all)
	if err != nil {
		return err
	}
	if keep == nil && len(others) == 0 {
		return d.stopFirst(ctx, t, spec, dec, nil, nil)
	}
	// One that's stuck restarting or paused reads as running, and doesn't serve, and
	// nothing short of a new one puts that right.
	if keep != nil && stuck(keep) {
		others, keep = withNewestFirst(others, keep), nil
	}
	// A tag that moved names a new image, and a container of its old one still reads
	// as the spec, which only says the tag. A container that's already on the new one
	// is where a run stopped partway got to, and this goes on from there.
	if keep != nil && dec.Pull {
		img, err := d.Client.InspectImage(ctx, docker.WithTag(spec.GetImage()))
		switch {
		case err == nil && img.ID != keep.Image:
			others, keep = withNewestFirst(others, keep), nil
		case err != nil && !docker.IsNotFound(err):
			return err
		}
	}
	plan, err := d.decide(ctx, t, spec, keep, others)
	if err != nil {
		return err
	}
	// What was planned as nothing stopping can't turn into two writers, or
	// into downtime nobody reviewed, because the server changed since, or because what
	// it shares is a volume its own change has just made, which the plan couldn't read.
	// Where none of it runs, as when the tag of what's stopped moved, it can't be either.
	runs := slices.ContainsFunc(others, func(o *instance) bool { return o.State.Running })
	if dec.Rollout != nil && dec.Rollout.Rolling && runs && (!plan.Rolling || plan.TwoWriters() && !dec.Rollout.Chosen) {
		shared := slices.Concat(plan.Ports, plan.Volumes, plan.Paths, plan.ImageData, plan.CantTell, plan.EditedRoutes)
		if plan.HostNetwork {
			shared = append(shared, "the server's own network")
		}
		return fmt.Errorf("the plan was to start a new %s before stopping the old one, but it now shares %s with what runs, so two containers can't run at once. Nothing was stopped. Plan the change again",
			t.Service, human.List(shared))
	}
	// What isn't running and is already what's asked for only has to start, as under
	// docker compose up, so what it wrote stays. Its plan can't say that, as a pull may
	// yet find something newer, so the step says what it did.
	startsOnly := keep != nil && len(others) == 0 && dec.Restart == "" && notRunning(keep.Container) != "" &&
		!keep.temp && !keep.stopping && keep.State.Status != neverStarted
	if plan.Rolling {
		err = d.startFirst(ctx, t, spec, dec, keep, others, plan)
	} else {
		err = d.stopFirst(ctx, t, spec, dec, keep, others)
	}
	if err == nil && startsOnly {
		resource.Report(ctx, resource.Outcome{
			Did:  resource.Did(resource.ActionUpdate),
			Note: t.Service + " wasn't running, and nothing about it changed, so the same container started",
		})
	}
	return err
}

// nextNumber is the number of a new instance: one past the highest of the
// service's, as Compose numbers a new one, and past any name someone else's
// container has. A number is never used twice while one of the service's is
// there, so what names an old one, such as an address on the table or a kept
// log, can't come to mean a new one.
func (d *Docker) nextNumber(ctx context.Context, t Target, all []*instance) (int, error) {
	n := 1
	for _, i := range all {
		n = max(n, i.number+1)
	}
	for {
		c, err := d.find(ctx, instanceName(t, n))
		if err != nil {
			return 0, err
		}
		if c == nil {
			return n, nil
		}
		n++
	}
}

// startFirst makes the new container, under a number of its own, and moves what
// is left over to it once it is healthy. It stops nothing until then, and if the
// new one fails, what ran goes on as it was: the rollback removes the new one.
func (d *Docker) startFirst(ctx context.Context, t Target, spec *resourcev1.Container, dec resource.Decision, keep *instance, others []*instance, plan *resource.RolloutPlan) error {
	// One that a stop-first step left under its swap name can't take the name
	// of its number while the one it was to replace has it: make another.
	if keep != nil && keep.temp {
		if slices.ContainsFunc(others, func(o *instance) bool { return o.number == keep.number }) {
			others, keep = append(others, keep), nil
		} else {
			if err := d.Client.RenameContainer(ctx, keep.ID, instanceName(t, keep.number)); err != nil {
				return fmt.Errorf("can't give the new %s its name: %w", t.Service, err)
			}
			keep.temp, keep.Name = false, "/"+instanceName(t, keep.number)
		}
	}
	// What a rollout that stopped partway was stopping is what this one wants: it
	// has to finish stopping before it can start, or it could stop under what
	// comes to depend on it.
	if keep != nil && keep.stopping {
		if err := d.bringBack(ctx, t, keep); err != nil {
			return err
		}
	}
	made := false
	if keep == nil {
		number, err := d.nextNumber(ctx, t, others)
		if err != nil {
			return err
		}
		t.Number = number
		d.Points.Reach(PointReserved)
		id, discard, err := d.make(ctx, t, spec, dec, instanceName(t, number), newest(others))
		if err != nil {
			return err
		}
		d.Points.Reach(PointCreated)
		c, err := d.Client.InspectContainer(ctx, id)
		if err != nil {
			return discard(err)
		}
		keep, made = &instance{Container: c, number: number}, true
	}
	if !keep.State.Running {
		resource.Progress(ctx, "Starting "+t.Service)
		if err := d.Client.StartContainer(ctx, keep.ID); err != nil {
			err = fmt.Errorf("docker can't start %s: %w", keep.name(), hideIn(err, valuesInContainer(keep.Container)))
			if made && dec.Restart != "" {
				d.discard(ctx, keep.ID, keep.name())
			}
			return err
		}
	}
	d.Points.Reach(PointStarted)
	if len(others) > 0 {
		resource.Progress(ctx, "Waiting for the new "+t.Service+" to be healthy, with the old one still serving")
	}
	soak := d.soak()
	if !made && keep.State.Running && !keep.State.StartedAt.IsZero() && time.Since(keep.State.StartedAt) >= settled {
		soak = 0 // it has been running: it isn't new
	}
	if err := d.waitHealthy(ctx, t, keep, soak); err != nil {
		// A restart has no revision to roll back to, and so no rollback to
		// remove what it made: it goes now, and what ran goes on.
		if dec.Restart != "" {
			d.discardFailed(ctx, keep)
		}
		return err
	}
	d.soaked.Store(keep.ID, true)
	d.Points.Reach(PointHealthy)
	if d.Ingress != nil {
		// As one reload, as each costs what the old socket's queue holds, and
		// clients reconnecting after one are what the next catches (ADR 0027).
		from := make([]ingress.Instance, 0, len(others))
		for _, o := range others {
			if !o.temp {
				from = append(from, t.instance(o.number))
			}
		}
		if err := d.Ingress.Move(ctx, t.instance(keep.number), from); err != nil {
			return fmt.Errorf("can't send traffic to the new %s, so what ran stays as it is: %w", t.Service, err)
		}
	}
	d.Points.Reach(PointUpstream)
	for _, o := range others {
		if err := d.retire(ctx, t, spec, o, plan.Drain); err != nil {
			return err
		}
	}
	return nil
}

// retire takes an old instance out of service: no new requests, what is in
// flight gets to finish, then it stops and goes.
func (d *Docker) retire(ctx context.Context, t Target, spec *resourcev1.Container, o *instance, drain time.Duration) error {
	if d.Ingress != nil && o.State.Running {
		resource.Progress(ctx, fmt.Sprintf("Letting what is in flight to the old %s finish, for up to %s", t.Service, human.Duration(drain)))
		wait, cancel := context.WithTimeout(ctx, drain)
		err := d.Ingress.Drain(wait, t.instance(o.number))
		cancel()
		if err != nil {
			return fmt.Errorf("can't take the old %s out of its routes: %w", t.Service, err)
		}
	}
	d.Points.Reach(PointDrained)
	keep := d.beginKeepingLogs(ctx, o.Container)
	resource.Progress(ctx, "Stopping the old "+t.Service)
	if o.State.Running || o.stopping {
		if err := d.markStopping(ctx, t, o); err != nil {
			return err
		}
		d.Points.Reach(PointMarked)
		if err := d.Client.StopContainer(ctx, o.ID, stopGrace(o.Container)); err != nil && !docker.IsNotFound(err) {
			return fmt.Errorf("can't stop the old %s: %w", t.Service, err)
		}
	}
	d.Points.Reach(PointStopped)
	d.finishKeepingLogs(ctx, keep, o.Container)
	if err := d.Client.RemoveContainer(ctx, o.ID, true); err != nil && !docker.IsNotFound(err) {
		return fmt.Errorf("can't remove the old %s: %w", t.Service, err)
	}
	d.keep(t, covered(o.Container, spec))
	d.Points.Reach(PointRemoved)
	if d.Ingress != nil && !o.temp {
		if err := d.Ingress.RemoveUpstream(ctx, t.instance(o.number)); err != nil {
			// It has stopped and gone, so nothing depends on this now: the
			// agent's reconcile forgets it.
			d.logger().WarnContext(ctx, "can't forget an address of a container that has gone; the agent tries again",
				slog.String("container", o.name()), slog.String("error", err.Error()))
		}
	}
	d.Points.Reach(PointForgotten)
	return nil
}

// markStopping says on a container, before it is told to stop, that a rollout is
// stopping it. It is what tells it from one a person stopped, whatever comes of
// the step, and what says to wait for Docker, which goes on stopping it however
// the agent fares.
func (d *Docker) markStopping(ctx context.Context, t Target, o *instance) error {
	if o.stopping {
		return nil
	}
	name := stoppingPrefix + o.ID[:12] + "_" + o.name()
	if err := d.Client.RenameContainer(ctx, o.ID, name); err != nil && !docker.IsNotFound(err) {
		return fmt.Errorf("can't mark the old %s as stopping, so it wasn't stopped: %w", t.Service, err)
	}
	o.stopping, o.Name = true, "/"+name
	return nil
}

// bringBack starts a container a rollout marked: it waits for Docker to finish
// stopping it, as Docker goes on whoever asked, starts it, and only then gives it
// back the name of its number, so what dies on the way still reads as a rollout's to
// finish, and not as stopped by hand. Stopping one that has stopped is nothing.
func (d *Docker) bringBack(ctx context.Context, t Target, keep *instance) error {
	if err := d.Client.StopContainer(ctx, keep.ID, stopGrace(keep.Container)); err != nil && !docker.IsNotFound(err) {
		return fmt.Errorf("can't finish stopping the old %s, so it can't start again: %w", t.Service, err)
	}
	if err := d.Client.StartContainer(ctx, keep.ID); err != nil {
		return fmt.Errorf("docker can't start the old %s: %w", t.Service, hideIn(err, valuesInContainer(keep.Container)))
	}
	final := instanceName(t, keep.number)
	if err := d.Client.RenameContainer(ctx, keep.ID, final); err != nil {
		return fmt.Errorf("can't give the old %s its name back: %w", t.Service, err)
	}
	keep.stopping, keep.Name, keep.State.Running = false, "/"+final, true
	return nil
}

// takeBack puts what ran back, after a step began to stop it and can't go on. The
// caller may have stopped waiting, as when the agent's stream dropped, so it has
// its own time, and an agent carries on serving with nothing to tell it what to do.
// What it can't do, the next step to this service finds marked, and finishes.
func (d *Docker) takeBack(ctx context.Context, t Target, o *instance, made func(error) error, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishWait+stopGrace(o.Container))
	defer cancel()
	// What this step made hasn't run, and has nothing to say a mark doesn't.
	_ = made(nil)
	if err := d.bringBack(ctx, t, o); err != nil {
		return fmt.Errorf("%w. The old %s had begun to stop, and can't be started again, so nothing of it may be running: %w. The next change to it finishes this, or start it with docker start %s", cause, t.Service, err, o.ID[:12])
	}
	return fmt.Errorf("%w. The old %s is running again", cause, t.Service)
}

// stuck reports a container Docker counts as running that doesn't serve.
func stuck(i *instance) bool { return i.State.Restarting || i.State.Paused }

// withNewestFirst puts one back among the others, in the order instances reads them.
func withNewestFirst(others []*instance, one *instance) []*instance {
	out := append(slices.Clone(others), one)
	slices.SortStableFunc(out, func(a, b *instance) int { return b.number - a.number })
	return out
}

func newest(others []*instance) *docker.Container {
	if len(others) == 0 {
		return nil
	}
	return others[0].Container
}

// stopFirst stops what is there before the new one starts, so there are never
// two, which has the downtime the plan states. The new one is made first, under
// a name of its own, so a pull or a refused create costs no downtime. Once the
// old one is gone, the rename and the start finish even if the call is
// cancelled, for up to 30 s, as a service left with no container is worse than
// a step that ends late.
func (d *Docker) stopFirst(ctx context.Context, t Target, spec *resourcev1.Container, dec resource.Decision, keep *instance, others []*instance) error {
	number := 1
	switch {
	case keep != nil:
		number = keep.number
	case len(others) > 0:
		number = max(others[0].number, 1)
	}
	t.Number = number
	final := instanceName(t, number)
	if keep == nil && len(others) == 0 {
		// There is nothing there to swap with. Someone else's container at the
		// name is refused here, as Create would fail on it, and it isn't ours
		// to stop.
		if c, err := d.find(ctx, final); err != nil {
			return err
		} else if c != nil {
			return owned(Target{Name: final, Project: t.Project, Service: t.Service}, c)
		}
	}
	name := final
	id := ""
	discard := func(err error) error { return err }
	if keep != nil {
		id, name = keep.ID, keep.name()
	} else {
		if len(others) > 0 {
			name = others[0].ID[:12] + "_" + final
			left, err := d.find(ctx, name)
			if err == nil && left != nil {
				err = owned(Target{Name: name, Project: t.Project, Service: t.Service}, left)
			}
			if err != nil {
				return err
			}
			if left != nil {
				_ = d.Client.RemoveContainer(ctx, left.ID, true) // a try that failed before
				others = slices.DeleteFunc(others, func(o *instance) bool { return o.ID == left.ID })
			}
		}
		var err error
		if id, discard, err = d.make(ctx, t, spec, dec, name, newest(others)); err != nil {
			return err
		}
	}
	d.Points.Reach(PointSwapReady)
	// What ran can go back only while nothing that ran has gone, and the new one
	// hasn't started.
	canGoBack := keep == nil || !keep.State.Running
	for _, o := range others {
		if o.ID == id {
			continue
		}
		ran := o.State.Running || o.stopping
		// While it still serves, so the downtime doesn't wait for most of its log.
		keep := d.beginKeepingLogs(ctx, o.Container)
		if ran {
			resource.Progress(ctx, "Stopping the old "+t.Service)
			if err := d.markStopping(ctx, t, o); err != nil {
				return discard(err)
			}
			d.Points.Reach(PointSwapMarked)
			if err := d.Client.StopContainer(ctx, o.ID, stopGrace(o.Container)); err != nil && !docker.IsNotFound(err) {
				err = fmt.Errorf("can't stop the old %s: %w", t.Service, err)
				if canGoBack {
					return d.takeBack(ctx, t, o, discard, err)
				}
				return err
			}
			d.Points.Reach(PointSwapStopped)
		}
		d.finishKeepingLogs(ctx, keep, o.Container)
		if err := d.Client.RemoveContainer(ctx, o.ID, true); err != nil && !docker.IsNotFound(err) {
			err = fmt.Errorf("can't remove the old %s: %w", t.Service, err)
			if ran && canGoBack {
				return d.takeBack(ctx, t, o, discard, err)
			}
			return err
		}
		canGoBack = canGoBack && !ran
		d.keep(t, covered(o.Container, spec))
	}
	d.Points.Reach(PointSwapGone)
	// The old one is gone: finish the swap even if the caller stops waiting. If
	// the new one can't take the name, the next run finishes it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishWait)
	defer cancel()
	// What this step keeps is what a rollout that stopped partway marked: Docker
	// may still be stopping it.
	if keep != nil && keep.stopping {
		if err := d.bringBack(ctx, t, keep); err != nil {
			return err
		}
		name = final
	}
	if name != final {
		if err := d.Client.RenameContainer(ctx, id, final); err != nil {
			return fmt.Errorf("removed the old %s, but can't give the new one its name: %w. The next change to it finishes the swap, as the rollback that follows does", t.Service, err)
		}
	}
	d.Points.Reach(PointSwapNamed)
	if keep == nil || !keep.State.Running {
		resource.Progress(ctx, "Starting "+t.Service)
		if err := d.Client.StartContainer(ctx, id); err != nil {
			return fmt.Errorf("docker can't start %s: %w", t.Name, hideIn(err, valuesInSpec(spec)))
		}
	}
	d.Points.Reach(PointStarted)
	if d.Ingress != nil && t.Scoped {
		if err := d.Ingress.AddUpstream(ctx, t.instance(number)); err != nil {
			return fmt.Errorf("can't send traffic to the new %s: %w", t.Service, err)
		}
		for _, o := range others {
			if o.number != number && !o.temp {
				if err := d.Ingress.RemoveUpstream(ctx, t.instance(o.number)); err != nil {
					d.logger().WarnContext(ctx, "can't forget an address of a container that has gone; the agent tries again",
						slog.String("container", o.name()), slog.String("error", err.Error()))
				}
			}
		}
	}
	return nil
}

// make creates a container of spec under name, on its networks, not started.
// Volumes Docker made for paths its image declares come from old, if any, so
// their data stays, and those kept for it come back. discard removes what it
// made, and gives back what it took.
func (d *Docker) make(ctx context.Context, t Target, spec *resourcev1.Container, dec resource.Decision, name string, old *docker.Container) (string, func(error) error, error) {
	// The names it takes are checked again with the lock held, until it is on its
	// networks: a check a moment ago doesn't see another change making a container.
	if networks, _ := joinedByName(t, spec); len(networks) > 0 {
		d.joining.Lock()
		defer d.joining.Unlock()
		if err := d.checkNetworks(ctx, t, spec); err != nil {
			return "", nil, err
		}
	}
	kept, err := d.keptVolumes(ctx, t)
	if err != nil {
		return "", nil, err
	}
	req, later, restored := d.createRequest(ctx, t, spec, old, kept)
	if err := rendered(req, spec); err != nil {
		return "", nil, err
	}
	if dec.Restores && len(restored) == 0 {
		return "", nil, fmt.Errorf("the data %s's image kept, which this change was to mount again, is gone: the server keeps it for 7 days after a change takes it out of use, then removes it. "+
			"Plan the change again: it makes %s with its image's volumes empty", t.Name, t.Name)
	}
	if dec.Restart != "" {
		req.Labels[LabelRestart] = dec.Restart
	}
	undo := func() {}
	if len(restored) > 0 {
		if undo, err = d.Deleted.TakeImageVolumes(t.Name, slices.Collect(maps.Keys(restored))); err != nil {
			return "", nil, err
		}
	}
	id, err := d.Client.CreateContainer(ctx, name, req)
	if err != nil {
		undo()
		return "", nil, fmt.Errorf("docker can't create %s: %w", t.Name, hideIn(err, valuesInSpec(spec)))
	}
	discard := func(err error) error {
		d.discard(ctx, id, name)
		undo()
		return err
	}
	for _, n := range later {
		if err := d.Client.ConnectNetwork(ctx, n, id, endpointAliases(t, spec)); err != nil {
			return "", nil, discard(fmt.Errorf("can't put %s on the network %s: %w", t.Name, n, err))
		}
	}
	if err := d.joinIngress(ctx, t, spec, id); err != nil {
		return "", nil, discard(err)
	}
	return id, discard, nil
}

// numberLabel is the container-number label of what is made at t.
func numberLabel(t Target) string { return strconv.Itoa(max(t.Number, 1)) }
