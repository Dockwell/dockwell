// SPDX-License-Identifier: AGPL-3.0-only

// Package fail2ban is the driver for fail2ban's jails, the "fail2ban.jail"
// kind, which Dockwell writes in a file of its own on the server (ADR 0052).
// It talks to fail2ban through Backend, which the agent implements with
// fail2ban-client and tests with a stand-in.
package fail2ban

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/lazyre"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is a fail2ban jail.
const Kind resource.Kind = "fail2ban.jail"

// Backend is the part of fail2ban the driver needs: the jails Dockwell wrote,
// by name, each written, checked with fail2ban-client -t and loaded at once.
type Backend interface {
	// Jail returns the named jail as Dockwell wrote it, or nil.
	Jail(ctx context.Context, name string) (*resourcev1.Jail, error)
	// Put writes the named jail, checks it, and loads it, or leaves fail2ban
	// as it was.
	Put(ctx context.Context, name string, jail *resourcev1.Jail) error
	// Delete removes the named jail, and loads what's left.
	Delete(ctx context.Context, name string) error
}

// Planner is a backend that says whom a jail, as planned, never bans, and who
// is signed in now that it could, for the plan to say (ADR 0052).
type Planner interface {
	JailPlan(ctx context.Context, desired *resourcev1.Jail) *resource.JailPlan
}

// How many of each a jail may name. fail2ban reads every one at each start,
// and nobody reads a plan of thousands.
const (
	MaxPorts    = 32
	MaxLogPaths = 16
	MaxIgnore   = 64
)

// Driver manages fail2ban's jails.
type Driver struct {
	Backend Backend
}

var (
	_ resource.Driver           = (*Driver)(nil)
	_ resource.DecidesWhoGetsIn = (*Driver)(nil)
	_ resource.Readier          = (*Driver)(nil)
	_ resource.NameChecker      = (*Driver)(nil)
)

// DecidesWhoGetsIn marks a jail as deciding who gets onto the server, so a
// change to one takes a signed-in person and a passkey. It can't lock anyone
// out of a new session on its own, so it applies with no timer (ADR 0052).
func (*Driver) DecidesWhoGetsIn() {}

// Kind returns "fail2ban.jail".
func (*Driver) Kind() resource.Kind { return Kind }

// Level is the server: a jail is host-level.
func (*Driver) Level() resource.Level { return resource.LevelServer }

// New returns an empty Fail2banJail.
func (*Driver) New() proto.Message { return &resourcev1.Jail{} }

// Ready says whether the server can run fail2ban's jails, when the backend can say.
func (d *Driver) Ready(ctx context.Context) error {
	if r, ok := d.Backend.(resource.Readier); ok {
		return r.Ready(ctx)
	}
	return nil
}

// jailName is what fail2ban takes as a jail's name, and as a section of its
// files: letters, digits, - and _.
// Its length, and a log file's, is checked apart from its form: a counted
// repetition compiles to as many copies of what it repeats, which every agent
// pays for as it starts (ADR 0060).
var jailName = lazyre.Regexp(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

const (
	maxJailName = 64
	maxLogPath  = 256
	maxHostName = 253
)

// isJailName reports a name fail2ban takes as a jail's, or a filter's.
func isJailName(name string) bool { return len(name) <= maxJailName && jailName().MatchString(name) }

// hostName is a DNS name as a jail's ignore list may hold one beside addresses,
// which fail2ban looks up itself: letters, digits, - and dots, with a letter.
var hostName = lazyre.Regexp(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// isHostName reports a DNS name, and not an address: one with a dot, or
// localhost, as a word alone is more often a slip than a name.
func isHostName(s string) bool {
	if strings.EqualFold(s, "localhost") {
		return true
	}
	return len(s) <= maxHostName && hostName().MatchString(s) && strings.Contains(s, ".") && !strings.Contains(s, "..") &&
		strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
}

// reserved are the sections of fail2ban's files that aren't jails: DEFAULT sets
// every jail, the stock ones it would turn on included, and INCLUDES names
// other files to read. fail2ban reads "default" in any case as DEFAULT.
var reserved = []string{"default", "includes"}

// CheckName takes a name fail2ban takes as a jail's, and refuses the sections
// of its files that set more than one jail.
func (*Driver) CheckName(name string) error {
	if !isJailName(name) {
		return fmt.Errorf("a jail's name is letters, digits, - and _, such as sshd, not %q", name)
	}
	if slices.Contains(reserved, strings.ToLower(name)) {
		return fmt.Errorf("%q isn't a jail: fail2ban reads that section as settings for every jail, or as other files to read. Name the jail itself, such as sshd", name)
	}
	return nil
}

// LogRoot is where a jail's log files are. fail2ban reads them as root, and
// follows whatever is there, so Dockwell names only the server's own logs.
const LogRoot = "/var/log/"

// logPath is a log file as Dockwell writes it into fail2ban's file: a full
// path of letters, digits and . _ - + @ :, with * and ? for the files
// logrotate keeps beside it. Nothing else gets in, as fail2ban reads a space
// or a new line as the next file or the next setting, and a % as a value of
// its own to fill in.
var logPath = lazyre.Regexp(`^/[A-Za-z0-9._+@:*?/-]+$`)

// CheckLogPath takes a log file a jail may read.
func CheckLogPath(p string) error {
	if len(p) > maxLogPath || !logPath().MatchString(p) || !path.IsAbs(p) || path.Clean(p) != p {
		return fmt.Errorf("a jail's log file is a full path of letters, digits and . _ - + @ :, such as /var/log/auth.log, not %q", p)
	}
	if !strings.HasPrefix(p, LogRoot) {
		return fmt.Errorf("a jail reads log files under %s, such as /var/log/auth.log, not %q. Have the app log there, or read the journal with backend systemd", LogRoot, p)
	}
	return nil
}

// Validate requires a filter, and values fail2ban takes, and puts ports and
// addresses in a canonical order.
func (*Driver) Validate(spec proto.Message) error {
	j := spec.(*resourcev1.Jail)
	switch {
	case len(j.GetPorts()) > MaxPorts:
		return fmt.Errorf("a jail closes %d ports at most, not %d. Leave ports empty to close every port to what it bans", MaxPorts, len(j.GetPorts()))
	case len(j.GetLogPaths()) > MaxLogPaths:
		return fmt.Errorf("a jail reads %d log files at most, not %d. Name them with * where they share a folder", MaxLogPaths, len(j.GetLogPaths()))
	case len(j.GetIgnore()) > MaxIgnore:
		return fmt.Errorf("a jail ignores %d addresses or ranges at most, not %d. Name wider ranges", MaxIgnore, len(j.GetIgnore()))
	}
	if !isJailName(j.GetFilter()) {
		return fmt.Errorf("a jail names its filter, as fail2ban names it, such as sshd, not %q", j.GetFilter())
	}
	for _, p := range j.GetPorts() {
		if p == 0 || p > 65535 {
			return fmt.Errorf("a jail's ports run from 1 to 65535, not %d", p)
		}
	}
	slices.Sort(j.Ports)
	j.Ports = slices.Compact(j.Ports)
	for _, f := range []struct{ name, value string }{{"find_time", j.GetFindTime()}, {"ban_time", j.GetBanTime()}} {
		if f.value == "" {
			continue
		}
		if d, err := time.ParseDuration(f.value); err != nil || d < time.Second {
			return fmt.Errorf("a jail's %s is a length of time of a second or more, such as 10m, not %q", f.name, f.value)
		}
	}
	if j.GetBanForGood() && j.GetBanTime() != "" {
		return errors.New("a jail that bans for good has no ban_time; leave it empty")
	}
	switch j.GetBackend() {
	case "", "systemd":
	default:
		return fmt.Errorf("a jail reads the journal, backend systemd, or log files, with no backend, not %q", j.GetBackend())
	}
	if j.GetBackend() == "systemd" && len(j.GetLogPaths()) > 0 {
		return errors.New("a jail that reads the journal reads no log files; leave log_paths empty")
	}
	if j.GetBackend() == "" && len(j.GetLogPaths()) == 0 {
		return errors.New("a jail reads the journal, with backend systemd, or the log files log_paths names; give it one")
	}
	for _, p := range j.GetLogPaths() {
		if err := CheckLogPath(p); err != nil {
			return err
		}
	}
	ignore := make([]string, 0, len(j.GetIgnore()))
	for _, s := range j.GetIgnore() {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			switch {
			case aerr == nil && a.Zone() == "":
				p = netip.PrefixFrom(a, a.BitLen())
			case aerr != nil && isHostName(s):
				// A name, as an admin's own ignoreip may hold: fail2ban looks it up.
				ignore = append(ignore, strings.ToLower(s))
				continue
			default:
				return fmt.Errorf("%q isn't an address, a CIDR range such as 203.0.113.0/24, or a host name", s)
			}
		}
		ignore = append(ignore, p.Masked().String())
	}
	slices.Sort(ignore)
	j.Ignore = slices.Compact(ignore)
	return nil
}

// Observe asks fail2ban's backend for the jail as Dockwell wrote it.
func (d *Driver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	j, err := d.Backend.Jail(ctx, ref.Name)
	if err != nil || j == nil {
		return nil, err
	}
	return j, nil
}

// Plan never has downtime, and can't lock anyone out of a new session: a jail
// bans an address only once it keeps failing to sign in.
func (d *Driver) Plan(actual, desired proto.Message, _ []resource.Change) (resource.Decision, error) {
	var dec resource.Decision
	switch {
	case actual == nil:
		dec.Action = resource.ActionCreate
	case desired == nil:
		dec.Action = resource.ActionDelete
	default:
		dec.Action = resource.ActionUpdate
	}
	if j, _ := desired.(*resourcev1.Jail); j != nil {
		if p, ok := d.Backend.(Planner); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			dec.Jail = p.JailPlan(ctx, j)
		}
	}
	return dec, nil
}

// Apply writes or removes the jail.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	switch step.Action {
	case resource.ActionCreate, resource.ActionUpdate, resource.ActionReplace:
		return d.Backend.Put(ctx, step.Ref.Name, desired.(*resourcev1.Jail))
	case resource.ActionDelete:
		return d.Backend.Delete(ctx, step.Ref.Name)
	default:
		return nil
	}
}

// Verify has nothing to check beyond what Put did: fail2ban-client -t and the
// reload.
func (*Driver) Verify(context.Context, resource.Ref, proto.Message) error { return nil }
