// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
)

// What an app asks of its container itself, as Compose names it: its hostname, an
// init, a terminal, its input kept open, its stop signal, its processes' limits, a
// read-only filesystem, how many processes it runs and the DNS servers it asks
// (ADR 0085). Each changes the container alone and reaches nothing on the server.

// maxDNS is how many DNS servers a container may ask. A resolver reads 3; a few
// more is a mistake, never a need.
const maxDNS = 8

// ulimitNames are the limits Docker takes, as ulimit names them.
var ulimitNames = []string{
	"core", "cpu", "data", "fsize", "locks", "memlock", "msgqueue", "nice", "nofile", "nproc",
	"rss", "rtprio", "rttime", "sigpending", "stack",
}

// AsksOfItself reports a container that asks something of itself, which an agent
// sets only once it says it does (agent Hello.own_settings).
func AsksOfItself(c *resourcev1.Container) bool {
	return c.GetHostname() != "" || c.GetInit() || c.GetTty() || c.GetStdinOpen() || c.GetStopSignal() != "" ||
		len(c.GetUlimits()) > 0 || c.GetReadOnly() || c.GetPidsLimit() != 0 || len(c.GetDns()) > 0 || c.GetNoNewPrivileges()
}

// validateOwn checks what a container asks of itself, and writes each in the form
// Docker reports it, so what runs reads back as the spec says.
func validateOwn(c *resourcev1.Container) error {
	if h := c.GetHostname(); h != "" {
		if len(h) > maxHostname || !isHostname(h) {
			return fmt.Errorf("%q isn't a hostname a container can have: use letters, digits and dashes, in labels joined by dots, such as mail.example.com", h)
		}
		if c.GetHostNetwork() {
			return fmt.Errorf("a container on the server's own network has the server's hostname, so it takes no hostname of its own, such as %s. Leave one of them out", h)
		}
	}
	if s := c.GetStopSignal(); s != "" {
		signal, ok := stopSignal(s)
		if !ok {
			return fmt.Errorf("%q isn't a signal Docker sends: give one by its name, such as SIGQUIT, or its number, such as 3", s)
		}
		c.StopSignal = signal
	}
	if p := c.GetPidsLimit(); p < 0 {
		return fmt.Errorf("the container's pids_limit is %d, and it takes the most processes it may run, or 0 for no limit", p)
	}
	if n := len(c.GetDns()); n > maxDNS {
		return fmt.Errorf("the container asks %d DNS servers, and Dockwell takes up to %d. A resolver reads the first 3: give fewer", n, maxDNS)
	}
	for i, s := range c.GetDns() {
		ip, err := netip.ParseAddr(s)
		if err != nil || ip.Zone() != "" {
			return fmt.Errorf("the DNS server %q isn't an IP address. Give one, such as 1.1.1.1", s)
		}
		c.Dns[i] = ip.String()
	}
	seen := map[string]bool{}
	for _, u := range c.GetUlimits() {
		u.Name = strings.ToLower(strings.TrimSpace(u.GetName()))
		switch {
		case !slices.Contains(ulimitNames, u.GetName()):
			return fmt.Errorf("%q isn't a limit Docker sets: use one of %s", u.GetName(), strings.Join(ulimitNames, ", "))
		case seen[u.GetName()]:
			return fmt.Errorf("the limit %s is set twice; set it once", u.GetName())
		case u.GetSoft() < -1 || u.GetHard() < -1:
			return fmt.Errorf("the limit %s is %d soft and %d hard, and takes a number, or -1 for no limit", u.GetName(), u.GetSoft(), u.GetHard())
		case u.GetHard() != -1 && (u.GetSoft() == -1 || u.GetSoft() > u.GetHard()):
			return fmt.Errorf("the limit %s is %d soft, past its hard limit of %d. A process can only raise its soft limit as far as its hard one: make the soft limit no more than the hard", u.GetName(), u.GetSoft(), u.GetHard())
		}
		seen[u.GetName()] = true
	}
	sortUlimits(c)
	return nil
}

// stopSignal is a signal as Docker takes it, its name in capitals with SIG before
// it, such as SIGQUIT, or its number, and false for anything else.
func stopSignal(s string) (string, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if n, err := strconv.Atoi(s); err == nil {
		return s, n >= 1 && n <= 64
	}
	name := strings.TrimPrefix(s, "SIG")
	if name == "" || len(name) > 16 {
		return "", false
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '+' || r == '-'):
		default:
			return "", false
		}
	}
	return "SIG" + name, true
}

func sortUlimits(c *resourcev1.Container) {
	slices.SortFunc(c.Ulimits, func(a, b *resourcev1.Ulimit) int { return cmp.Compare(a.GetName(), b.GetName()) })
}

// dockerUlimits are a spec's limits as Docker takes them.
func dockerUlimits(c *resourcev1.Container) []docker.Ulimit {
	var out []docker.Ulimit
	for _, u := range c.GetUlimits() {
		out = append(out, docker.Ulimit{Name: u.GetName(), Soft: u.GetSoft(), Hard: u.GetHard()})
	}
	return out
}

// ulimitsOf reads a container's limits back as a spec keeps them.
func ulimitsOf(list []docker.Ulimit) []*resourcev1.Ulimit {
	var out []*resourcev1.Ulimit
	for _, u := range list {
		out = append(out, &resourcev1.Ulimit{Name: u.Name, Soft: u.Soft, Hard: u.Hard})
	}
	slices.SortFunc(out, func(a, b *resourcev1.Ulimit) int { return cmp.Compare(a.GetName(), b.GetName()) })
	return out
}

// noNewPrivileges is the security option a spec's no_new_privileges asks Docker for.
const noNewPrivileges = "no-new-privileges"

// NoNewPrivileges reads Docker's security option that keeps a container's processes
// from gaining privileges, as a Compose file or Docker writes it, with :, = or
// nothing before its value, and false for any other option.
func NoNewPrivileges(opt string) (on, ok bool) {
	opt = strings.TrimSpace(opt)
	if opt == noNewPrivileges {
		return true, true
	}
	value, ok := strings.CutPrefix(opt, noNewPrivileges)
	if !ok || value == "" || value[0] != ':' && value[0] != '=' {
		return false, false
	}
	on, err := strconv.ParseBool(value[1:])
	return on, err == nil
}

// dockerInit is whether a spec runs an init, as Docker takes it: unset for none,
// so the daemon's own default stands as it did.
func dockerInit(c *resourcev1.Container) *bool {
	if !c.GetInit() {
		return nil
	}
	on := true
	return &on
}
