// SPDX-License-Identifier: AGPL-3.0-only

package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
)

// Acting on a unit (ADR 0082): planned from what systemctl show says of it, and
// taken with the init's own tool, on the reader's runner, so a firewall change
// waiting on its confirm timer holds up no restart.

// ActDeadline bounds an action: systemd's own start timeout is 90 s by default,
// and systemctl waits for the job.
const ActDeadline = 2 * time.Minute

// planProperties are all that planning reads of a unit: how it stands, what
// systemd lets a person do, and what goes with it.
var planProperties = []string{
	"Id", "Names", "LoadState", "ActiveState", "SubState", "UnitFileState",
	"CanStart", "CanStop", "CanReload", "RefuseManualStart", "RefuseManualStop",
	"RequiredBy", "BoundBy", "ConsistsOf", "TriggeredBy",
}

var (
	rcService = hosttool.Tool{Name: "rc-service", Paths: []string{"/sbin/rc-service", "/usr/sbin/rc-service", "/bin/rc-service", "/usr/bin/rc-service"}}
	rcUpdate  = hosttool.Tool{Name: "rc-update", Paths: []string{"/sbin/rc-update", "/usr/sbin/rc-update", "/bin/rc-update", "/usr/bin/rc-update"}}
	sshd      = hosttool.Tool{Name: "sshd", Paths: []string{"/usr/sbin/sshd", "/sbin/sshd"}}
)

// rcName is an OpenRC service's name, as its script in /etc/init.d is called.
var rcName = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]*$`) })

var _ host.Units = (*Reader)(nil)

// unitState is how a unit stands, as systemctl show says it.
type unitState map[string]string

func (u unitState) active() bool {
	switch u["ActiveState"] {
	case "active", "reloading", "activating", "refreshing":
		return true
	}
	return false
}

func (u unitState) names() []string { return append([]string{u["Id"]}, strings.Fields(u["Names"])...) }

// show reads the named units' properties, a block a unit, by the names asked.
func (r *Reader) show(ctx context.Context, props []string, names ...string) ([]unitState, error) {
	args := append([]string{"show", "--property=" + strings.Join(props, ","), "--no-pager", "--"}, names...)
	out, err := r.run(ctx, systemctl, args...)
	if err != nil {
		return nil, err
	}
	var units []unitState
	for _, block := range strings.Split(string(out), "\n\n") {
		u := unitState{}
		for _, line := range strings.Split(block, "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				u[k] = v
			}
		}
		if u["Id"] != "" {
			units = append(units, u)
		}
	}
	return units, nil
}

// unit reads one unit by its name or an alias of it: the block that goes by it.
func (r *Reader) unit(ctx context.Context, props []string, name string) (unitState, error) {
	units, err := r.show(ctx, props, name)
	if err != nil {
		return nil, err
	}
	for _, u := range units {
		if slices.Contains(u.names(), name) {
			if u["LoadState"] == "not-found" {
				break
			}
			return u, nil
		}
	}
	return nil, fmt.Errorf("there's no unit %s on this server", name)
}

// PlanUnit plans an action on a unit: how it stands, what stops or restarts with
// it, and what starts it again. It reports false when the unit stands as asked.
func (r *Reader) PlanUnit(ctx context.Context, name string, action resource.UnitAction) (*resource.Unit, bool, error) {
	if r.Unavailable != "" {
		return nil, false, errors.New(r.Unavailable)
	}
	if !action.Valid() {
		return nil, false, fmt.Errorf("%q isn't an action Dockwell takes on a system service", action)
	}
	switch {
	case r.exists("run/systemd/system"):
		return r.planSystemd(ctx, name, action)
	case r.exists("run/openrc"):
		return r.planOpenRC(ctx, name, action)
	}
	return nil, false, errors.New("this server starts its services with an init Dockwell doesn't act on. It acts on systemd's and OpenRC's")
}

func (r *Reader) planSystemd(ctx context.Context, name string, action resource.UnitAction) (*resource.Unit, bool, error) {
	if !unitName().MatchString(name) || strings.Contains(name, "@.") {
		return nil, false, fmt.Errorf("%q isn't a unit's name: one ends in .service or .socket", name)
	}
	u, err := r.unit(ctx, planProperties, name)
	if err != nil {
		return nil, false, err
	}
	id := u["Id"]
	guard := resource.GuardOf(u.names()...)
	if why := guard.Refusal(id, "this server", action); why != "" {
		return nil, false, errors.New(why)
	}
	plan := &resource.Unit{Name: id, Action: action, WasRunning: u.active(), WasState: u["ActiveState"], WasAtBoot: u["UnitFileState"], Guard: guard}
	masked := u["LoadState"] == "masked" || strings.HasPrefix(u["UnitFileState"], "masked")
	switch action {
	case resource.UnitStart, resource.UnitRestart, resource.UnitReload:
		if masked {
			return nil, false, fmt.Errorf("%s is masked, so it can't start. Unmask it first", id)
		}
	case resource.UnitStop, resource.UnitEnable, resource.UnitDisable, resource.UnitMask, resource.UnitUnmask:
	}
	switch action {
	case resource.UnitStart:
		if u["CanStart"] == "no" || u["RefuseManualStart"] == "yes" {
			return nil, false, fmt.Errorf("systemd won't start %s by hand: it starts when another unit needs it", id)
		}
		if u.active() {
			return plan, false, nil
		}
	case resource.UnitRestart:
		if u["CanStart"] == "no" || u["RefuseManualStart"] == "yes" || u.active() && (u["CanStop"] == "no" || u["RefuseManualStop"] == "yes") {
			return nil, false, fmt.Errorf("systemd won't restart %s by hand: another unit starts and stops it", id)
		}
	case resource.UnitStop:
		if u["CanStop"] == "no" || u["RefuseManualStop"] == "yes" {
			return nil, false, fmt.Errorf("systemd won't stop %s by hand: it stops with the unit that needs it", id)
		}
		if !u.active() {
			return plan, false, nil
		}
	case resource.UnitReload:
		if u["CanReload"] != "yes" {
			return nil, false, fmt.Errorf("%s can't read its configuration again without stopping. Restart it instead", id)
		}
		if !u.active() {
			return nil, false, fmt.Errorf("%s isn't running, so there's nothing to reload. Start it instead", id)
		}
	case resource.UnitEnable, resource.UnitDisable:
		switch w := u["UnitFileState"]; {
		case w == "enabled" && action == resource.UnitEnable, (w == "disabled" || w == "") && action == resource.UnitDisable:
			return plan, false, nil
		case masked:
			return nil, false, fmt.Errorf("%s is masked. Unmask it first", id)
		case w == "static" || w == "indirect" || w == "generated" || w == "transient" || w == "alias":
			return nil, false, fmt.Errorf("%s starts when another unit needs it, so it has nothing to %s at boot", id, action)
		}
	case resource.UnitMask:
		if masked {
			return plan, false, nil
		}
	case resource.UnitUnmask:
		if !masked {
			return plan, false, nil
		}
	}
	// What goes with it, and what starts it again, of what's active.
	var with, triggers []string
	if action == resource.UnitStop || action == resource.UnitRestart {
		with = fieldsOf(u, "RequiredBy", "BoundBy", "ConsistsOf")
	}
	if action == resource.UnitStop {
		triggers = fieldsOf(u, "TriggeredBy")
	}
	if len(with)+len(triggers) > 0 {
		active, err := r.activeOf(ctx, append(slices.Clone(with), triggers...))
		if err != nil {
			return nil, false, err
		}
		plan.With = slices.DeleteFunc(with, func(n string) bool { return !active[n] })
		plan.Triggers = slices.DeleteFunc(triggers, func(n string) bool { return !active[n] })
	}
	plan.DockerRestarts = action == resource.UnitRestart && (id == "docker.service" || id == "docker.socket" || slices.Contains(plan.With, "docker.service"))
	return plan, true, nil
}

// fieldsOf is the unit names in the unit's properties, each once, in order.
func fieldsOf(u unitState, props ...string) []string {
	var out []string
	for _, p := range props {
		for _, n := range strings.Fields(u[p]) {
			if !slices.Contains(out, n) && unitName().MatchString(n) {
				out = append(out, n)
			}
		}
	}
	return out
}

// activeOf says which of the named units are active now.
func (r *Reader) activeOf(ctx context.Context, names []string) (map[string]bool, error) {
	units, err := r.show(ctx, []string{"Id", "ActiveState"}, names...)
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	for _, u := range units {
		active[u["Id"]] = u.active()
	}
	return active, nil
}

func (r *Reader) planOpenRC(ctx context.Context, name string, action resource.UnitAction) (*resource.Unit, bool, error) {
	if !rcName().MatchString(name) || !r.script(name) {
		return nil, false, fmt.Errorf("there's no service %s in /etc/init.d on this server", name)
	}
	guard := resource.GuardOf(name)
	if why := guard.Refusal(name, "this server", action); why != "" {
		return nil, false, errors.New(why)
	}
	switch action {
	case resource.UnitReload, resource.UnitMask, resource.UnitUnmask:
		return nil, false, fmt.Errorf("OpenRC has no %s for a service. Restart it, or disable it at boot", action)
	case resource.UnitStart, resource.UnitStop, resource.UnitRestart, resource.UnitEnable, resource.UnitDisable:
	}
	out, err := r.run(ctx, rcStatus, "--all", "--nocolor")
	if err != nil {
		return nil, false, err
	}
	var found *resource.Unit
	for _, s := range parseRCStatus(out) {
		if s.GetName() == name {
			found = &resource.Unit{Name: name, Action: action, WasRunning: s.GetDetail() == "started" || s.GetDetail() == "starting", WasState: s.GetDetail(), WasAtBoot: s.GetAtBoot(), Guard: guard, OpenRC: true}
		}
	}
	if found == nil {
		// rc-status lists only what's in a runlevel or started; one in neither is stopped.
		found = &resource.Unit{Name: name, Action: action, WasState: "stopped", Guard: guard, OpenRC: true}
	}
	switch action {
	case resource.UnitStart:
		return found, !found.WasRunning, nil
	case resource.UnitStop:
		return found, found.WasRunning, nil
	case resource.UnitEnable:
		return found, found.WasAtBoot == "", nil
	case resource.UnitDisable:
		return found, found.WasAtBoot != "", nil
	case resource.UnitRestart, resource.UnitReload, resource.UnitMask, resource.UnitUnmask:
	}
	found.DockerRestarts = action == resource.UnitRestart && name == "docker"
	return found, true, nil
}

// ApplyUnit takes the action, then reads the unit again and fails when it doesn't
// stand as asked, with the last lines its own process wrote.
func (r *Reader) ApplyUnit(ctx context.Context, u resource.Unit, say func(string)) error {
	if r.Unavailable != "" {
		return errors.New(r.Unavailable)
	}
	// What a plan says the agent checks again: a plan from elsewhere can't take
	// what the unit's guard refuses.
	if why := resource.GuardOf(u.Name).Refusal(u.Name, "this server", u.Action); why != "" {
		return errors.New(why)
	}
	if u.Guard == resource.GuardSSH && (u.Action == resource.UnitRestart || u.Action == resource.UnitReload) {
		if err := r.sshdChecks(ctx); err != nil {
			return err
		}
	}
	if u.OpenRC {
		return r.applyOpenRC(ctx, u, say)
	}
	if !unitName().MatchString(u.Name) {
		return fmt.Errorf("%q isn't a unit's name", u.Name)
	}
	args := []string{string(u.Action)}
	// The network moves under the agent's own connection: its answer goes first.
	noBlock := u.Guard == resource.GuardNetwork && (u.Action == resource.UnitRestart || u.Action == resource.UnitReload)
	if noBlock {
		args = append(args, "--no-block")
	}
	args = append(args, "--no-pager", "--", u.Name)
	say(fmt.Sprintf("systemctl %s %s", u.Action, u.Name))
	res, err := r.Tools.Run(ctx, systemctl, ActDeadline, nil, args...)
	if err != nil {
		return fmt.Errorf("can't %s %s: %w", u.Action, u.Name, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(res.Errs+"\n"+string(res.Out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			say(line)
		}
	}
	if res.Code != 0 {
		return r.failed(ctx, u, hosttool.Fail("systemctl", res))
	}
	if noBlock {
		return nil
	}
	now, err := r.unit(ctx, []string{"Id", "Names", "LoadState", "ActiveState", "SubState", "UnitFileState", "Result"}, u.Name)
	if err != nil {
		return err
	}
	return r.stands(ctx, u, now)
}

// stands checks a unit stands as its action asked, as systemd says now.
func (r *Reader) stands(ctx context.Context, u resource.Unit, now unitState) error {
	switch u.Action {
	case resource.UnitStart, resource.UnitRestart, resource.UnitReload:
		// A one-shot unit goes back to inactive once it's done well; only failed fails.
		if now["ActiveState"] == "failed" {
			return r.failed(ctx, u, fmt.Errorf("it failed (%s)", cmpOr(now["Result"], now["SubState"])))
		}
	case resource.UnitStop:
		if now.active() {
			return fmt.Errorf("%s is running again after it stopped: something starts it, such as its socket or a timer", u.Name)
		}
	case resource.UnitEnable:
		if w := now["UnitFileState"]; w != "enabled" && w != "enabled-runtime" {
			return fmt.Errorf("%s still reads %s at boot after it was enabled", u.Name, w)
		}
	case resource.UnitDisable:
		if w := now["UnitFileState"]; w == "enabled" || w == "enabled-runtime" {
			return fmt.Errorf("%s still starts at boot after it was disabled", u.Name)
		}
	case resource.UnitMask:
		if !strings.HasPrefix(now["UnitFileState"], "masked") && now["LoadState"] != "masked" {
			return fmt.Errorf("%s isn't masked after it was masked", u.Name)
		}
	case resource.UnitUnmask:
		if strings.HasPrefix(now["UnitFileState"], "masked") || now["LoadState"] == "masked" {
			return fmt.Errorf("%s is still masked after it was unmasked", u.Name)
		}
	}
	return nil
}

func cmpOr(a, b string) string {
	if a != "" && a != "success" {
		return a
	}
	return b
}

// failed is an action that failed, with the last lines the unit's own process wrote
// to the journal, which say why where systemd's own lines only say that it did.
func (r *Reader) failed(ctx context.Context, u resource.Unit, err error) error {
	why := r.Why(ctx, u.Name)
	if len(why) == 0 {
		return fmt.Errorf("can't %s %s: %w", u.Action, u.Name, err)
	}
	return fmt.Errorf("can't %s %s: %w. It said: %s", u.Action, u.Name, err, strings.Join(why, " / "))
}

// sshdChecks runs sshd -t, as ADR 0052 does before it changes SSH, so a restart
// never leaves the server without SSH for a config that doesn't load.
func (r *Reader) sshdChecks(ctx context.Context) error {
	res, err := r.Tools.Run(ctx, sshd, Deadline, nil, "-t")
	if errors.Is(err, hosttool.ErrMissing) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("can't check sshd's config: %w", err)
	}
	if res.Code != 0 {
		line := strings.TrimSpace(res.Errs)
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		return fmt.Errorf("sshd's config doesn't pass its own check, so restarting it would leave this server without SSH: %s. Fix the config, then try again", line)
	}
	return nil
}

func (r *Reader) applyOpenRC(ctx context.Context, u resource.Unit, say func(string)) error {
	if !rcName().MatchString(u.Name) {
		return fmt.Errorf("%q isn't a service's name", u.Name)
	}
	tool, args := rcService, []string{u.Name, string(u.Action)}
	switch u.Action {
	case resource.UnitEnable:
		tool, args = rcUpdate, []string{"add", u.Name, "default"}
	case resource.UnitDisable:
		tool, args = rcUpdate, []string{"del", u.Name, cmpOr(u.WasAtBoot, "default")}
	case resource.UnitReload, resource.UnitMask, resource.UnitUnmask:
		return fmt.Errorf("OpenRC has no %s for a service", u.Action)
	case resource.UnitStart, resource.UnitStop, resource.UnitRestart:
	}
	say(tool.Name + " " + strings.Join(args, " "))
	res, err := r.Tools.Run(ctx, tool, ActDeadline, nil, args...)
	if err != nil {
		return fmt.Errorf("can't %s %s: %w", u.Action, u.Name, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(res.Out)+"\n"+res.Errs), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			say(line)
		}
	}
	if res.Code != 0 {
		return r.failed(ctx, u, hosttool.Fail(tool.Name, res))
	}
	return nil
}

// script reports an OpenRC service's script in /etc/init.d, below Root.
func (r *Reader) script(name string) bool {
	root := r.Root
	if root == "" {
		root = "/"
	}
	fi, err := os.Stat(filepath.Join(root, "etc", "init.d", name))
	return err == nil && fi.Mode().IsRegular()
}
