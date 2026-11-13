// SPDX-License-Identifier: AGPL-3.0-only

package resource

import "testing"

// Each guard refuses, asks a passkey for and asks the typed code for what ADR 0082's
// table says, and nothing it refuses asks for either.
func TestUnitGuardsFollowTheTable(t *testing.T) {
	type want struct{ refuse, passkey, confirm string }
	// Each string lists the actions by their first letters: Start, sTop, Restart,
	// reLoad, Enable, Disable, Mask, Unmask.
	table := map[UnitGuard]want{
		GuardNone:     {"", "", ""},
		GuardDockwell: {"STRLEDMU", "", ""},
		GuardDocker:   {"TDM", "RL", "R"},
		GuardSSH:      {"TDM", "RL", ""},
		GuardNetwork:  {"TDM", "RL", "R"},
		GuardFirewall: {"", "TRLDM", "TDM"},
	}
	letter := map[UnitAction]byte{UnitStart: 'S', UnitStop: 'T', UnitRestart: 'R', UnitReload: 'L', UnitEnable: 'E', UnitDisable: 'D', UnitMask: 'M', UnitUnmask: 'U'}
	has := func(s string, b byte) bool {
		for i := range len(s) {
			if s[i] == b {
				return true
			}
		}
		return false
	}
	for g, w := range table {
		for _, a := range UnitActions {
			l := letter[a]
			if g.Refuses(a) != has(w.refuse, l) || g.Passkey(a) != has(w.passkey, l) || g.Confirm(a) != has(w.confirm, l) {
				t.Errorf("guard %d, %s: refuses %t, passkey %t, typed %t", g, a, g.Refuses(a), g.Passkey(a), g.Confirm(a))
			}
			if g.Refuses(a) && g.Refusal("x.service", "web-1", a) == "" {
				t.Errorf("guard %d refuses %s without saying why", g, a)
			}
		}
	}
	if GuardOf("sshd.service") != GuardSSH || GuardOf("nginx.service", "docker.service") != GuardDocker || GuardOf("nginx.service") != GuardNone {
		t.Error("GuardOf reads a unit's names wrong")
	}
}

// Undoing an action puts back how the unit stood; a restart has nothing to put back.
func TestUndoingAUnitActionPutsBackHowItStood(t *testing.T) {
	for _, c := range []struct {
		u    Unit
		back UnitAction
		ok   bool
	}{
		{Unit{Action: UnitStop, WasRunning: true}, UnitStart, true},
		{Unit{Action: UnitStart}, UnitStop, true},
		{Unit{Action: UnitDisable, WasAtBoot: "enabled"}, UnitEnable, true},
		{Unit{Action: UnitEnable, WasAtBoot: "disabled"}, UnitDisable, true},
		{Unit{Action: UnitMask, WasAtBoot: "enabled"}, UnitUnmask, true},
		{Unit{Action: UnitUnmask, WasAtBoot: "masked"}, UnitMask, true},
		{Unit{Action: UnitDisable, WasAtBoot: "default", OpenRC: true}, UnitEnable, true},
		{Unit{Action: UnitRestart, WasRunning: true}, "", false},
		{Unit{Action: UnitReload, WasRunning: true}, "", false},
	} {
		back, ok := c.u.Undone()
		if back != c.back || ok != c.ok {
			t.Errorf("undoing %+v = %s, %t; want %s, %t", c.u, back, ok, c.back, c.ok)
		}
	}
}

// A plan that names a guarded unit asks what its name asks, whatever guard it says.
func TestAUnitsNameSetsItsGuardAtLeast(t *testing.T) {
	u := Unit{Name: "docker.service", Action: UnitRestart}
	if !u.Passkey() || !u.Confirm() {
		t.Errorf("restarting docker.service planned with no guard takes passkey %t, typed %t", u.Passkey(), u.Confirm())
	}
}
