// SPDX-License-Identifier: AGPL-3.0-only

// Package host is a server's host, kind "server.host": what Dockwell does to the
// server itself. Its spec holds the server's labels, which only the control plane
// keeps (ADR 0091); installing the server's updates is a flagged edit of it, which
// makes no revision (ADR 0064), and so is an action on one of its system services
// (ADR 0082).
package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is a server's host.
const Kind resource.Kind = "server.host"

// Name is the name a server's one host goes by.
const Name = "host"

// Backend installs a server's updates: the agent's package manager and init
// system. Nil on the control plane, which plans through the agent.
type Backend interface {
	// Plan simulates installing the updates waiting: the packages named and what
	// they need, or with none named the security fixes, or all.
	Plan(ctx context.Context, all bool, names []string) (*resource.Install, error)
	// Left are the planned packages not yet at their planned version, and
	// whether an install for the change still runs.
	Left(ctx context.Context, change string, planned []resource.InstallPackage) (left []resource.InstallPackage, running bool, err error)
	// Install installs the planned packages for the change, saying each line.
	Install(ctx context.Context, change string, planned []resource.InstallPackage, say func(string)) error
	// Rebooted says where the change's reboot is, by the mark the agent keeps of it.
	Rebooted(ctx context.Context, change string) (RebootState, error)
	// Reboot marks the change's reboot with this boot, and asks the server to
	// reboot once the answer is on its way. Asked again on the same boot, or on
	// another, it does nothing.
	Reboot(ctx context.Context, change string) error
}

// RebootState is where a change's reboot of a server is.
type RebootState int

// Where a reboot is.
const (
	// RebootNone is a reboot never asked of this server, as a call that never got
	// there.
	RebootNone RebootState = iota
	// RebootAsked is one asked on this boot: the server hasn't gone down yet.
	RebootAsked
	// RebootDone is one asked on another boot: the server rebooted.
	RebootDone
	// RebootFailed is one whose command failed on this boot: the server is still
	// up, and applying the step again says what the command said.
	RebootFailed
)

// Units acts on the server's system services (ADR 0082): the agent's reader of
// them, with the init system's own tools. Nil on the control plane.
type Units interface {
	// PlanUnit reads how the unit stands and plans the action: what it would stop
	// or restart, and what starts it again. It reports false when the unit stands
	// as asked already, and refuses what the unit's guard or its init refuses.
	PlanUnit(ctx context.Context, name string, action resource.UnitAction) (*resource.Unit, bool, error)
	// ApplyUnit takes the action, saying each line, then reads the unit again and
	// fails when it doesn't stand as asked.
	ApplyUnit(ctx context.Context, unit resource.Unit, say func(string)) error
}

// Panels stops and starts the own containers of a panel an import came from (ADR
// 0089): the agent's reader of it, with Docker. Nil on the control plane.
type Panels interface {
	// PlanPanel reads how the panel's own containers and services stand, and plans
	// stopping or starting them. It reports false when they stand as asked already.
	PlanPanel(ctx context.Context, ask resource.PanelAsked) (*resource.Panel, bool, error)
	// ApplyPanel takes the step, saying each line, then reads them again and fails
	// where one doesn't stand as asked.
	ApplyPanel(ctx context.Context, p resource.Panel, say func(string)) error
}

// Upgrader upgrades the agent by the install script a change carries (ADR 0094).
// Nil on the control plane.
type Upgrader interface {
	// Own is the agent's own version, without a "v".
	Own() string
	// Upgrade checks the script against its SHA-256 and starts it for the change,
	// at most once: asked again while it runs it does nothing, and once it has
	// ended with the agent at its old version it fails with what the script said.
	Upgrade(ctx context.Context, ask resource.AgentUpgradeAsked) error
	// Forget lets go of what the change's upgrade left, once it's done.
	Forget(change string)
}

// Driver is a server's host.
type Driver struct {
	Backend Backend
	// Units acts on the server's system services; nil acts on none.
	Units Units
	// Panels stops and starts a panel's own containers; nil acts on none.
	Panels Panels
	// Upgrader upgrades the agent; nil upgrades none.
	Upgrader Upgrader
}

var (
	_ resource.Driver           = (*Driver)(nil)
	_ resource.Installer        = (*Driver)(nil)
	_ resource.Rebooter         = (*Driver)(nil)
	_ resource.UnitActor        = (*Driver)(nil)
	_ resource.PanelActor       = (*Driver)(nil)
	_ resource.AgentUpgrader    = (*Driver)(nil)
	_ resource.ControlOnly      = (*Driver)(nil)
	_ resource.ControlOnlySayer = (*Driver)(nil)
)

// Kind returns "server.host".
func (*Driver) Kind() resource.Kind { return Kind }

// Level is the server.
func (*Driver) Level() resource.Level { return resource.LevelServer }

// New returns an empty ServerHost.
func (*Driver) New() proto.Message { return &resourcev1.ServerHost{} }

// CheckName takes the one name a server's host goes by.
func (*Driver) CheckName(name string) error {
	if name != Name {
		return fmt.Errorf("a server has one host, named %q, not %q", Name, name)
	}
	return nil
}

// Validate checks the server's labels, as Kubernetes labels (ADR 0091).
func (*Driver) Validate(spec proto.Message) error {
	h, ok := spec.(*resourcev1.ServerHost)
	if !ok {
		return fmt.Errorf("a server's host is a ServerHost, not %T", spec)
	}
	return resource.CheckLabels(h.GetLabels())
}

// OnServer leaves out the labels: only the control plane keeps them, so a change
// of them alone takes no step on the server, and no drift reads them as missing
// there (ADR 0091).
func (*Driver) OnServer(spec proto.Message) proto.Message {
	h, ok := spec.(*resourcev1.ServerHost)
	if !ok || len(h.GetLabels()) == 0 {
		return spec
	}
	out := proto.CloneOf(h)
	out.Labels = nil
	return out
}

// SayControlOnly says a change of labels alone takes no step on the server, so it
// applies with the server online or not.
func (*Driver) SayControlOnly(servers []string) (reason, message string) {
	return "labels_only", fmt.Sprintf("Changes labels alone, which only Dockwell keeps: no step runs on %s, so this applies whether it's online or not", strings.Join(servers, " or "))
}

// Readopted puts current's labels back on what the server has, which never holds
// them.
func (*Driver) Readopted(current, observed proto.Message) proto.Message {
	c, ok := current.(*resourcev1.ServerHost)
	o, isHost := observed.(*resourcev1.ServerHost)
	if !ok || !isHost || len(c.GetLabels()) == 0 {
		return observed
	}
	out := proto.CloneOf(o)
	out.Labels = maps.Clone(c.GetLabels())
	return out
}

// Observe reads a host that's always there: the server itself.
func (*Driver) Observe(context.Context, resource.Ref) (proto.Message, error) {
	return &resourcev1.ServerHost{}, nil
}

// Plan has nothing to change: the spec is empty, and a host is never deleted
// from the server, only from what Dockwell keeps.
func (*Driver) Plan(_, _ proto.Message, _ []resource.Change) (resource.Decision, error) {
	return resource.Decision{Action: resource.ActionNone}, nil
}

// Apply has nothing to do.
func (*Driver) Apply(context.Context, resource.Step, proto.Message) error { return nil }

// Verify has nothing to check.
func (*Driver) Verify(context.Context, resource.Ref, proto.Message) error { return nil }

var errNoBackend = errors.New("this agent can't install updates")

// JudgeInstall plans an install: at planning, the exact command simulated; on a
// resume, what's left of the planned packages, nothing when every one is at its
// version and no install for the change runs.
func (d *Driver) JudgeInstall(ctx context.Context, _ resource.Ref, ask resource.InstallAsked) (resource.Decision, bool, error) {
	if d.Backend == nil {
		return resource.Decision{}, false, errNoBackend
	}
	dec := resource.Decision{Action: resource.ActionUpdate, Effect: resource.EffectNone}
	if len(ask.Packages) == 0 {
		in, err := d.Backend.Plan(ctx, ask.All, ask.Names)
		if err != nil {
			return resource.Decision{}, false, err
		}
		if len(in.Packages) == 0 {
			return resource.Decision{}, false, nil
		}
		dec.Install = in
		return dec, true, nil
	}
	left, running, err := d.Backend.Left(ctx, ask.Change, ask.Packages)
	if err != nil {
		return resource.Decision{}, false, err
	}
	if len(left) == 0 && !running {
		return resource.Decision{}, false, nil
	}
	// The step carries the whole plan: what's done reads done again as it applies.
	// What it needs after is each package's, as the plan said: an apply plans
	// again, and its fingerprint has to read as the plan's did.
	dec.Install = &resource.Install{Packages: ask.Packages}
	for _, p := range ask.Packages {
		dec.Install.Reboot = dec.Install.Reboot || p.Reboot
		dec.Install.DockerRestarts = dec.Install.DockerRestarts || p.Docker
	}
	return dec, true, nil
}

// ApplyInstall installs the step's packages, for the change applying it.
func (d *Driver) ApplyInstall(ctx context.Context, step resource.Step) error {
	if d.Backend == nil {
		return errNoBackend
	}
	change := resource.DeployOf(ctx).Change
	if change == "" {
		return errors.New("an install needs its change, which names its unit")
	}
	return d.Backend.Install(ctx, change, step.Install.Packages, func(line string) { resource.Progress(ctx, line) })
}

// JudgeReboot plans a reboot of the server, and nothing once the change's reboot
// took it to another boot. One asked on this boot still plans, and applying it
// again does nothing, so a resume waits for the server rather than reboot it twice;
// one whose command failed plans too, and applying it fails with the command's words.
func (d *Driver) JudgeReboot(ctx context.Context, _ resource.Ref, ask resource.RebootAsked) (resource.Decision, bool, error) {
	if d.Backend == nil {
		return resource.Decision{}, false, errCantReboot
	}
	state, err := d.Backend.Rebooted(ctx, ask.Change)
	if err != nil {
		return resource.Decision{}, false, err
	}
	if state == RebootDone {
		return resource.Decision{}, false, nil
	}
	return resource.Decision{Action: resource.ActionUpdate, Effect: resource.EffectRestart, Reboot: &resource.Reboot{}}, true, nil
}

// ApplyReboot asks the server to reboot, for the change applying the step.
func (d *Driver) ApplyReboot(ctx context.Context, _ resource.Step) error {
	if d.Backend == nil {
		return errCantReboot
	}
	change := resource.DeployOf(ctx).Change
	if change == "" {
		return errors.New("a reboot needs its change, which its mark names")
	}
	return d.Backend.Reboot(ctx, change)
}

var errCantReboot = errors.New("this agent can't reboot its server")

var errNoUnits = errors.New("this agent can't act on system services")

// JudgeUnit plans an action on one of the server's system services, and nothing
// when the unit stands as asked already, so a resume of one that finished has
// nothing left to do.
func (d *Driver) JudgeUnit(ctx context.Context, _ resource.Ref, ask resource.UnitAsked) (resource.Decision, bool, error) {
	if d.Units == nil {
		return resource.Decision{}, false, errNoUnits
	}
	u, left, err := d.Units.PlanUnit(ctx, ask.Name, ask.Action)
	if err != nil {
		return resource.Decision{}, false, err
	}
	if !left {
		return resource.Decision{}, false, nil
	}
	effect := resource.EffectNone
	if u.Action == resource.UnitStop || u.Action == resource.UnitRestart {
		effect = resource.EffectRestart
	}
	return resource.Decision{Action: resource.ActionUpdate, Effect: effect, Unit: u}, true, nil
}

// ApplyUnit takes the step's action on the unit it planned.
func (d *Driver) ApplyUnit(ctx context.Context, step resource.Step) error {
	if d.Units == nil {
		return errNoUnits
	}
	return d.Units.ApplyUnit(ctx, *step.Unit, func(line string) { resource.Progress(ctx, line) })
}

var errNoPanels = errors.New("this agent can't stop or start a panel, as it reads no Docker")

// JudgePanel plans stopping or starting a panel's own containers, and nothing when
// they stand as asked already, so a resume of one that finished has nothing left to
// do. What stops is the panel, and none of what it ran: those were taken on, or go
// on as they are.
func (d *Driver) JudgePanel(ctx context.Context, _ resource.Ref, ask resource.PanelAsked) (resource.Decision, bool, error) {
	if d.Panels == nil {
		return resource.Decision{}, false, errNoPanels
	}
	p, left, err := d.Panels.PlanPanel(ctx, ask)
	if err != nil || !left {
		return resource.Decision{}, false, err
	}
	return resource.Decision{Action: resource.ActionUpdate, Panel: p}, true, nil
}

// ApplyPanel stops or starts what the step planned.
func (d *Driver) ApplyPanel(ctx context.Context, step resource.Step) error {
	if d.Panels == nil {
		return errNoPanels
	}
	return d.Panels.ApplyPanel(ctx, *step.Panel, func(line string) { resource.Progress(ctx, line) })
}

var errCantUpgrade = errors.New("this agent can't upgrade itself")

// JudgeAgentUpgrade plans upgrading the agent to the version asked, and nothing
// once it runs that or a newer one: a resume finds the upgrade done once the agent
// is back at it. One that runs still plans, and applying it again does nothing.
func (d *Driver) JudgeAgentUpgrade(_ context.Context, _ resource.Ref, ask resource.AgentUpgradeAsked) (resource.Decision, bool, error) {
	if d.Upgrader == nil {
		return resource.Decision{}, false, errCantUpgrade
	}
	own := d.Upgrader.Own()
	if !Older(own, ask.Version) {
		d.Upgrader.Forget(ask.Change)
		return resource.Decision{}, false, nil
	}
	return resource.Decision{Action: resource.ActionUpdate, Effect: resource.EffectNone, AgentUpgrade: &resource.AgentUpgrade{From: own, To: ask.Version, ScriptSHA256: ask.ScriptSHA256}}, true, nil
}

// ApplyAgentUpgrade starts the install script the step's call carries, for the
// change applying it.
func (d *Driver) ApplyAgentUpgrade(ctx context.Context, step resource.Step) error {
	if d.Upgrader == nil {
		return errCantUpgrade
	}
	ask, ok := resource.AgentUpgradeOf(ctx)
	if !ok || len(ask.Script) == 0 {
		return errors.New("its call carried no install script. The control plane is older than the agent, so upgrade it first")
	}
	if ask.Change = cmp.Or(ask.Change, resource.DeployOf(ctx).Change); ask.Change == "" {
		return errors.New("an upgrade needs its change, which its mark names")
	}
	if step.AgentUpgrade != nil && step.AgentUpgrade.To != ask.Version {
		return fmt.Errorf("its step upgrades to %s, and its call to %s. Plan the upgrade again", step.AgentUpgrade.To, ask.Version)
	}
	return d.Upgrader.Upgrade(ctx, ask)
}

// Older reports whether version a comes before b, as releases do: by major, minor
// and patch, then a release after its pre-releases. A version that isn't a
// release's, as a development build's, comes before any other.
func Older(a, b string) bool {
	pa, oka := semver(a)
	pb, okb := semver(b)
	switch {
	case !okb:
		return false
	case !oka:
		return true
	}
	for i := range 3 {
		if pa.n[i] != pb.n[i] {
			return pa.n[i] < pb.n[i]
		}
	}
	switch {
	case pa.pre == pb.pre:
		return false
	case pa.pre == "":
		return false
	case pb.pre == "":
		return true
	}
	xa, xb := strings.Split(pa.pre, "."), strings.Split(pb.pre, ".")
	for i := range min(len(xa), len(xb)) {
		na, ea := strconv.Atoi(xa[i])
		nb, eb := strconv.Atoi(xb[i])
		switch {
		case ea == nil && eb == nil && na != nb:
			return na < nb
		case (ea == nil) != (eb == nil):
			return ea == nil
		case ea != nil && xa[i] != xb[i]:
			return xa[i] < xb[i]
		}
	}
	return len(xa) < len(xb)
}

type version struct {
	n   [3]int
	pre string
}

// semver reads a release's version, with or without its "v".
func semver(v string) (version, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	core, pre, _ := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	var out version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, false
		}
		out.n[i] = n
	}
	out.pre = pre
	return out, true
}
