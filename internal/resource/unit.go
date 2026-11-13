// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"context"
	"fmt"
)

// UnitAction is what a change does to one of a server's system services, its
// systemd units or OpenRC services, as a flagged edit of the server's host that
// makes no revision (ADR 0082).
type UnitAction string

// What a change can do to a system service.
const (
	UnitStart   UnitAction = "start"
	UnitStop    UnitAction = "stop"
	UnitRestart UnitAction = "restart"
	UnitReload  UnitAction = "reload"
	UnitEnable  UnitAction = "enable"
	UnitDisable UnitAction = "disable"
	UnitMask    UnitAction = "mask"
	UnitUnmask  UnitAction = "unmask"
)

// UnitActions are every action, in the order the panel offers them.
var UnitActions = []UnitAction{UnitStart, UnitStop, UnitRestart, UnitReload, UnitEnable, UnitDisable, UnitMask, UnitUnmask}

// Valid reports an action Dockwell knows.
func (a UnitAction) Valid() bool {
	for _, k := range UnitActions {
		if a == k {
			return true
		}
	}
	return false
}

// Verb is the action as a sentence starts with it, such as "Restart".
func (a UnitAction) Verb() string {
	switch a {
	case UnitStart:
		return "Start"
	case UnitStop:
		return "Stop"
	case UnitRestart:
		return "Restart"
	case UnitReload:
		return "Reload"
	case UnitEnable:
		return "Enable"
	case UnitDisable:
		return "Disable"
	case UnitMask:
		return "Mask"
	case UnitUnmask:
		return "Unmask"
	}
	return string(a)
}

// AtBoot reports an action that changes whether a unit starts at boot, rather
// than whether it runs now.
func (a UnitAction) AtBoot() bool {
	return a == UnitEnable || a == UnitDisable || a == UnitMask || a == UnitUnmask
}

// UnitGuard is how Dockwell guards one of a server's system services, as it needs
// it to reach the server or run what's there (ADR 0082). Its values are the API's
// SystemServiceGuard.
type UnitGuard int32

// The guards.
const (
	GuardNone UnitGuard = iota
	GuardDockwell
	GuardDocker
	GuardSSH
	GuardNetwork
	GuardFirewall
)

// GuardedUnits names the units each guard covers, by systemd's names and OpenRC's.
// A unit is guarded when its own name or any alias it goes by is here.
var GuardedUnits = map[string]UnitGuard{
	"dockwell.service": GuardDockwell, "dockwell-access.service": GuardDockwell, "dockwell": GuardDockwell,
	"docker.service": GuardDocker, "docker.socket": GuardDocker, "containerd.service": GuardDocker, "docker": GuardDocker, "containerd": GuardDocker,
	"ssh.service": GuardSSH, "sshd.service": GuardSSH, "ssh.socket": GuardSSH, "sshd": GuardSSH,
	"systemd-networkd.service": GuardNetwork, "NetworkManager.service": GuardNetwork, "networking.service": GuardNetwork,
	"systemd-resolved.service": GuardNetwork, "networking": GuardNetwork,
	"nftables.service": GuardFirewall, "ufw.service": GuardFirewall, "firewalld.service": GuardFirewall,
	"netfilter-persistent.service": GuardFirewall, "iptables.service": GuardFirewall, "ip6tables.service": GuardFirewall,
	"iptables": GuardFirewall, "ip6tables": GuardFirewall, "nftables": GuardFirewall,
}

// GuardOf is the guard of a unit that goes by these names, the strictest that any
// of them has.
func GuardOf(names ...string) UnitGuard {
	g := GuardNone
	for _, n := range names {
		if h, ok := GuardedUnits[n]; ok && (g == GuardNone || h < g) {
			g = h
		}
	}
	return g
}

// cuts reports an action that leaves a unit stopped, or kept from starting.
func cuts(a UnitAction) bool { return a == UnitStop || a == UnitDisable || a == UnitMask }

// Refuses reports an action the guard never takes.
func (g UnitGuard) Refuses(a UnitAction) bool {
	switch g {
	case GuardDockwell:
		return true
	case GuardDocker, GuardSSH, GuardNetwork:
		return cuts(a)
	case GuardNone, GuardFirewall:
	}
	return false
}

// Refusal says why the guard refuses an action on the unit, on a server, or "".
func (g UnitGuard) Refusal(name, server string, a UnitAction) string {
	if !g.Refuses(a) {
		return ""
	}
	switch g {
	case GuardDockwell:
		return fmt.Sprintf("Dockwell runs as %s on %s, so it can't %s it from here", name, server, a)
	case GuardDocker:
		return fmt.Sprintf("every app on %s runs in Docker, so Dockwell won't %s %s", server, a, name)
	case GuardSSH:
		return fmt.Sprintf("%s is how you reach %s over SSH, so Dockwell won't %s it", name, server, a)
	case GuardNetwork:
		return fmt.Sprintf("%s reaches the network through %s, so Dockwell won't %s it", server, name, a)
	case GuardNone, GuardFirewall:
	}
	return ""
}

// Passkey reports an action on a unit so guarded that's as good as root: a person
// applies it with a passkey.
func (g UnitGuard) Passkey(a UnitAction) bool {
	if g.Refuses(a) {
		return false
	}
	switch g {
	case GuardDocker, GuardSSH, GuardNetwork:
		return a == UnitRestart || a == UnitReload
	case GuardFirewall:
		return a != UnitStart && a != UnitEnable && a != UnitUnmask
	case GuardNone, GuardDockwell:
	}
	return false
}

// Confirm reports an action on a unit so guarded that takes the plan's typed code
// too: it stops everything on the server, may cut the agent off, or opens every
// port the firewall closes.
func (g UnitGuard) Confirm(a UnitAction) bool {
	if g.Refuses(a) {
		return false
	}
	switch g {
	case GuardDocker, GuardNetwork:
		return a == UnitRestart
	case GuardFirewall:
		return cuts(a)
	case GuardNone, GuardDockwell, GuardSSH:
	}
	return false
}

// UnitAsked is an action a change asks of one of a server's system services, for
// its host.
type UnitAsked struct {
	// Change is the change's ID.
	Change string
	// Name is the unit's name, as asked; the agent acts on the unit it resolves to.
	Name string
	// Action is what to do to it.
	Action UnitAction
}

// Unit is what an action on one of a server's system services does, as its agent
// planned it (ADR 0082).
type Unit struct {
	// Name is the unit's own name, as its init knows it.
	Name string
	// Action is what the step does to it.
	Action UnitAction
	// WasRunning, WasState and WasAtBoot are how it stood as it was planned: what
	// an undo puts back.
	WasRunning bool   `json:",omitempty"`
	WasState   string `json:",omitempty"`
	WasAtBoot  string `json:",omitempty"`
	// With are the running units that stop or restart with it.
	With []string `json:",omitempty"`
	// Triggers are the active units that start it again, for a stop.
	Triggers []string `json:",omitempty"`
	// Guard is how Dockwell guards it.
	Guard UnitGuard `json:",omitempty"`
	// DockerRestarts says it restarts Docker, with every container on the server.
	DockerRestarts bool `json:",omitempty"`
	// OpenRC says the server's init is OpenRC.
	OpenRC bool `json:",omitempty"`
}

// Undone is the action that puts back what a takes away, from how the unit stood
// before it, and false for one nothing undoes: a restart or a reload, or one that
// changed nothing an action can put back.
func (u Unit) Undone() (UnitAction, bool) {
	switch u.Action {
	case UnitStart:
		return UnitStop, !u.WasRunning
	case UnitStop:
		return UnitStart, u.WasRunning
	case UnitEnable:
		return UnitDisable, !enabledWord(u.WasAtBoot)
	case UnitDisable:
		return UnitEnable, enabledWord(u.WasAtBoot)
	case UnitMask:
		return UnitUnmask, !maskedWord(u.WasAtBoot)
	case UnitUnmask:
		return UnitMask, maskedWord(u.WasAtBoot)
	case UnitRestart, UnitReload:
	}
	return "", false
}

// enabledWord reports an at-boot word that starts a unit at boot: systemd's
// enabled, or an OpenRC runlevel.
func enabledWord(w string) bool {
	switch w {
	case "enabled", "enabled-runtime", "alias":
		return true
	case "", "disabled", "static", "indirect", "generated", "transient", "masked", "masked-runtime", "linked", "linked-runtime", "bad":
		return false
	}
	// An OpenRC runlevel, such as default or boot.
	return true
}

func maskedWord(w string) bool { return w == "masked" || w == "masked-runtime" }

// guard is the stricter of the guard the agent planned with and the one the unit's
// own name has, so a plan can't ask less of a person than the unit's name does.
func (u Unit) guard() UnitGuard {
	if g := GuardOf(u.Name); g != GuardNone && (u.Guard == GuardNone || g < u.Guard) {
		return g
	}
	return u.Guard
}

// Passkey reports a unit's action that's as good as root.
func (u Unit) Passkey() bool { return u.guard().Passkey(u.Action) }

// Confirm reports a unit's action that takes the plan's typed code.
func (u Unit) Confirm() bool { return u.guard().Confirm(u.Action) }

// UnitActor is a kind that acts on its server's system services as a step of a
// change: the server's host (ADR 0082).
type UnitActor interface {
	// JudgeUnit plans the action, and reports false when there's nothing to do:
	// the unit stands as asked already.
	JudgeUnit(ctx context.Context, ref Ref, ask UnitAsked) (Decision, bool, error)
	// ApplyUnit takes the step's action, and checks what came of it.
	ApplyUnit(ctx context.Context, step Step) error
}
