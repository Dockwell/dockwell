// SPDX-License-Identifier: AGPL-3.0-only

package system_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/system"
	"github.com/dockwell/dockwell/internal/agent/system/systemtest"
	"github.com/dockwell/dockwell/internal/resource"
)

func reader(s *systemtest.Stand) *system.Reader {
	return &system.Reader{Tools: &hosttool.Runner{Root: s.Root}, Root: s.Root}
}

func say(lines *[]string) func(string) { return func(l string) { *lines = append(*lines, l) } }

// A restart plans what restarts with it, takes the action with systemctl's own
// words as its progress, and checks the unit runs after.
func TestARestartPlansWhatGoesWithItAndChecksItRuns(t *testing.T) {
	s := systemtest.New(t,
		systemtest.Unit{Props: systemtest.Running("php8.3-fpm.service", "RequiredBy", "nginx.service worker.service", "ConsistsOf", "")},
		systemtest.Unit{Props: systemtest.Running("nginx.service")},
		systemtest.Unit{Props: systemtest.Stopped("worker.service")},
	)
	r := reader(s)
	u, left, err := r.PlanUnit(t.Context(), "php8.3-fpm.service", resource.UnitRestart)
	if err != nil || !left {
		t.Fatalf("PlanUnit = %v, %t, %v", u, left, err)
	}
	if !slices.Equal(u.With, []string{"nginx.service"}) || !u.WasRunning || u.WasAtBoot != "enabled" || u.Guard != resource.GuardNone || u.DockerRestarts {
		t.Errorf("the plan reads %+v; want nginx.service, the running one that needs it, alone", u)
	}
	var lines []string
	if err := r.ApplyUnit(t.Context(), *u, say(&lines)); err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 || lines[0] != "systemctl restart php8.3-fpm.service" {
		t.Errorf("the step said %q", lines)
	}
	if !slices.ContainsFunc(s.Calls(), func(c string) bool { return c == "systemctl restart --no-pager -- php8.3-fpm.service" }) {
		t.Errorf("systemctl ran %q", s.Calls())
	}
}

// What stands as asked plans nothing, so a resume of one that finished does nothing.
func TestAnActionThatChangesNothingPlansNothing(t *testing.T) {
	s := systemtest.New(t,
		systemtest.Unit{Props: systemtest.Running("nginx.service")},
		systemtest.Unit{Props: systemtest.Stopped("redis-server.service")},
		systemtest.Unit{Props: systemtest.Running("cron.service", "UnitFileState", "masked")},
	)
	r := reader(s)
	for _, c := range []struct {
		name   string
		action resource.UnitAction
	}{
		{"nginx.service", resource.UnitStart},
		{"nginx.service", resource.UnitEnable},
		{"redis-server.service", resource.UnitStop},
		{"redis-server.service", resource.UnitDisable},
		{"cron.service", resource.UnitMask},
		{"nginx.service", resource.UnitUnmask},
	} {
		if _, left, err := r.PlanUnit(t.Context(), c.name, c.action); err != nil || left {
			t.Errorf("%s %s planned %t, %v; want nothing to do", c.action, c.name, left, err)
		}
	}
}

// What Dockwell needs is refused by every name it goes by, an alias too, and what
// the init refuses is refused with why.
func TestGuardsAndTheInitRefuseWhatTheyWont(t *testing.T) {
	s := systemtest.New(t,
		systemtest.Unit{Props: systemtest.Running("ssh.service", "Names", "ssh.service sshd.service")},
		systemtest.Unit{Props: systemtest.Running("dockwell.service")},
		systemtest.Unit{Props: systemtest.Running("systemd-journald.service", "CanStop", "no", "RefuseManualStop", "yes")},
		systemtest.Unit{Props: systemtest.Stopped("apt-daily.service", "UnitFileState", "static")},
		systemtest.Unit{Props: systemtest.Stopped("old.service", "UnitFileState", "masked", "LoadState", "masked")},
		systemtest.Unit{Props: systemtest.Running("nginx.service")},
		// What carries the network or SSH outside the fixed names (review H1).
		systemtest.Unit{Props: systemtest.Running("ifup@eth0.service")},
		systemtest.Unit{Props: systemtest.Running("sshd.socket", "Triggers", "sshd@.service")},
		systemtest.Unit{Props: systemtest.Running("ssh-alt.socket", "Triggers", "ssh.service")},
		systemtest.Unit{Props: systemtest.Running("dockwell-agent.service")},
		// A stop that drags what Dockwell needs along, and what sign-ins go through (review L4).
		systemtest.Unit{Props: systemtest.Running("storage.service", "RequiredBy", "docker.service")},
		systemtest.Unit{Props: systemtest.Running("docker.service")},
		systemtest.Unit{Props: systemtest.Running("dbus.service")},
	)
	s.Own(t, "dockwell-agent.service")
	r := reader(s)
	for _, c := range []struct {
		name   string
		action resource.UnitAction
		want   string
	}{
		{"sshd.service", resource.UnitStop, "how you reach this server over SSH"},
		{"ssh.service", resource.UnitDisable, "how you reach"},
		{"dockwell.service", resource.UnitRestart, "Dockwell runs as dockwell.service"},
		{"systemd-journald.service", resource.UnitStop, "won't stop"},
		{"apt-daily.service", resource.UnitEnable, "nothing to enable"},
		{"old.service", resource.UnitStart, "Unmask it first"},
		{"nginx.service", resource.UnitReload, "Restart it instead"},
		{"missing.service", resource.UnitStart, "there's no unit missing.service"},
		{"../etc.service", resource.UnitStart, "isn't a unit's name"},
		{"ifup@eth0.service", resource.UnitStop, "reaches the network through ifup@eth0.service"},
		{"sshd.socket", resource.UnitStop, "how you reach"},
		{"ssh-alt.socket", resource.UnitMask, "how you reach"},
		{"dockwell-agent.service", resource.UnitRestart, "Dockwell runs as dockwell-agent.service"},
		{"storage.service", resource.UnitStop, "stopping storage.service stops docker.service with it"},
		{"dbus.service", resource.UnitStop, "services and sign-ins go through dbus.service"},
	} {
		_, _, err := r.PlanUnit(t.Context(), c.name, c.action)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s = %v, want %q", c.action, c.name, err, c.want)
		}
	}
	if slices.ContainsFunc(s.Calls(), func(c string) bool { return !strings.HasPrefix(c, "systemctl show") }) {
		t.Errorf("a refusal ran %q", s.Calls())
	}
}

// Restarting SSH asks sshd whether its config loads first, and refuses with its
// line when it doesn't; a restart of SSH takes a passkey.
func TestRestartingSSHChecksItsConfigFirst(t *testing.T) {
	s := systemtest.New(t, systemtest.Unit{Props: systemtest.Running("ssh.service", "Names", "ssh.service sshd.service", "CanReload", "yes")})
	r := reader(s)
	u, _, err := r.PlanUnit(t.Context(), "sshd.service", resource.UnitRestart)
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "ssh.service" || u.Guard != resource.GuardSSH || !u.Passkey() || u.Confirm() {
		t.Errorf("the plan reads %+v", u)
	}
	s.SSHDFails(t, "/etc/ssh/sshd_config line 12: Bad configuration option: PermitRootLogn")
	err = r.ApplyUnit(t.Context(), *u, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "line 12: Bad configuration option") || !strings.Contains(err.Error(), "without SSH") {
		t.Fatalf("restarting SSH with a broken config = %v", err)
	}
	if slices.ContainsFunc(s.Calls(), func(c string) bool { return strings.HasPrefix(c, "systemctl restart") }) {
		t.Errorf("SSH restarted with a config that doesn't load: %q", s.Calls())
	}
}

// A start that leaves the unit failed fails with the last lines its own process
// wrote, not systemd's.
func TestAFailedStartSaysWhatTheUnitSaid(t *testing.T) {
	s := systemtest.New(t, systemtest.Unit{
		Props: systemtest.Stopped("nginx.service"),
		Fails: true,
		Journal: []string{
			systemtest.Entry("c4", "init.scope", "1", "Failed to start nginx.service - A high performance web server."),
			systemtest.Entry("c3", "init.scope", "1", "nginx.service: Failed with result 'exit-code'."),
			systemtest.Entry("c2", "nginx.service", "4242", "nginx: configuration file /etc/nginx/nginx.conf test failed"),
			systemtest.Entry("c1", "nginx.service", "4242", "nginx: [emerg] unknown directive \\\"foo\\\" in /etc/nginx/nginx.conf:12"),
		},
	})
	r := reader(s)
	u, _, err := r.PlanUnit(t.Context(), "nginx.service", resource.UnitStart)
	if err != nil {
		t.Fatal(err)
	}
	err = r.ApplyUnit(t.Context(), *u, func(string) {})
	// A change's steps are read below an admin, so the error points at the log
	// rather than quote it (review L1); the list says why, to an admin.
	if err == nil || strings.Contains(err.Error(), "unknown directive") || !strings.Contains(err.Error(), "say why") {
		t.Fatalf("a failed start = %v", err)
	}
	if got := r.Why(t.Context(), "nginx.service"); !slices.ContainsFunc(got, func(l string) bool { return strings.Contains(l, `unknown directive "foo"`) }) {
		t.Errorf("why reads %q", got)
	}
}

// Disabling and enabling at boot is checked after, and a network restart doesn't
// wait, so the agent's answer goes before the network moves.
func TestAtBootAndTheNetwork(t *testing.T) {
	s := systemtest.New(t,
		systemtest.Unit{Props: systemtest.Running("nginx.service")},
		systemtest.Unit{Props: systemtest.Running("systemd-networkd.service")},
	)
	r := reader(s)
	u, _, err := r.PlanUnit(t.Context(), "nginx.service", resource.UnitDisable)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyUnit(t.Context(), *u, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if got := s.Prop(t, "nginx.service", "UnitFileState"); got != "disabled" {
		t.Errorf("nginx.service reads %s at boot", got)
	}
	if back, ok := u.Undone(); !ok || back != resource.UnitEnable {
		t.Errorf("undoing the disable is %s, %t", back, ok)
	}
	n, _, err := r.PlanUnit(t.Context(), "systemd-networkd.service", resource.UnitRestart)
	if err != nil {
		t.Fatal(err)
	}
	if !n.Passkey() || !n.Confirm() {
		t.Errorf("a network restart takes passkey %t, typed code %t", n.Passkey(), n.Confirm())
	}
	if err := r.ApplyUnit(t.Context(), *n, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.Calls(), "systemctl restart --no-block --no-pager -- systemd-networkd.service") {
		t.Errorf("the network restarted as %q", s.Calls())
	}
}

// Stopping a unit says what starts it again, and restarting Docker says it does.
func TestAStopSaysWhatStartsItAgain(t *testing.T) {
	s := systemtest.New(t,
		systemtest.Unit{Props: systemtest.Running("cups.service", "TriggeredBy", "cups.socket cups.path")},
		systemtest.Unit{Props: systemtest.Running("cups.socket")},
		systemtest.Unit{Props: systemtest.Stopped("cups.path")},
		systemtest.Unit{Props: systemtest.Running("docker.service", "TriggeredBy", "docker.socket")},
	)
	r := reader(s)
	u, _, err := r.PlanUnit(t.Context(), "cups.service", resource.UnitStop)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(u.Triggers, []string{"cups.socket"}) {
		t.Errorf("what starts it again reads %q", u.Triggers)
	}
	d, _, err := r.PlanUnit(t.Context(), "docker.service", resource.UnitRestart)
	if err != nil {
		t.Fatal(err)
	}
	if !d.DockerRestarts || !d.Confirm() || !d.Passkey() {
		t.Errorf("restarting Docker reads %+v", d)
	}
}

// Under OpenRC, rc-service and rc-update act, and what OpenRC has no word for is
// refused.
func TestOpenRCActsWithItsOwnTools(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	for p, content := range map[string]string{
		"run/openrc/softlevel":        "default\n",
		"etc/init.d/nginx":            "#!/sbin/openrc-run\n",
		"etc/runlevels/default/nginx": "",
		"sbin/rc-status":              "#!/bin/sh\nprintf 'Runlevel: default\\n nginx    [  started  ]\\n'\n",
		"sbin/rc-service":             "#!/bin/sh\necho \"rc-service $*\" >> '" + calls + "'\n",
		"sbin/rc-update":              "#!/bin/sh\necho \"rc-update $*\" >> '" + calls + "'\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r := &system.Reader{Tools: &hosttool.Runner{Root: root}, Root: root}
	u, left, err := r.PlanUnit(t.Context(), "nginx", resource.UnitRestart)
	if err != nil || !left || !u.OpenRC || !u.WasRunning {
		t.Fatalf("PlanUnit = %+v, %t, %v", u, left, err)
	}
	if err := r.ApplyUnit(t.Context(), *u, func(string) {}); err != nil {
		t.Fatal(err)
	}
	d, left, err := r.PlanUnit(t.Context(), "nginx", resource.UnitDisable)
	if err != nil || !left {
		t.Fatalf("PlanUnit disable = %v, %t, %v", d, left, err)
	}
	if err := r.ApplyUnit(t.Context(), *d, func(string) {}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(calls)
	if got := strings.TrimSpace(string(b)); got != "rc-service nginx restart\nrc-update del nginx default" {
		t.Errorf("OpenRC ran %q", got)
	}
	if _, _, err := r.PlanUnit(t.Context(), "nginx", resource.UnitMask); err == nil || !strings.Contains(err.Error(), "OpenRC has no mask") {
		t.Errorf("masking under OpenRC = %v", err)
	}
	if _, _, err := r.PlanUnit(t.Context(), "missing", resource.UnitStart); err == nil {
		t.Error("a service with no script planned")
	}
}

// The list says each unit's guard and what systemd lets a person do to it.
func TestTheListSaysGuardsAndWhatCanBeDone(t *testing.T) {
	s := systemtest.New(t,
		systemtest.Unit{Props: systemtest.Running("ssh.service", "Names", "ssh.service sshd.service", "CanReload", "yes")},
		systemtest.Unit{Props: systemtest.Running("systemd-journald.service", "RefuseManualStop", "yes")},
		systemtest.Unit{Props: systemtest.Running("nftables.service")},
	)
	list, err := reader(s).Services(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*dockwellv1.SystemServiceInfo{}
	for _, u := range list.GetServices() {
		by[u.GetName()] = u
	}
	if u := by["ssh.service"]; u.GetGuard() != dockwellv1.SystemServiceGuard_SYSTEM_SERVICE_GUARD_SSH || !u.GetCanReload() {
		t.Errorf("ssh.service reads %v", u)
	}
	if u := by["systemd-journald.service"]; !u.GetStopRefused() || u.GetStartRefused() {
		t.Errorf("systemd-journald.service reads %v", u)
	}
	if u := by["nftables.service"]; u.GetGuard() != dockwellv1.SystemServiceGuard_SYSTEM_SERVICE_GUARD_FIREWALL {
		t.Errorf("nftables.service reads %v", u)
	}
}

// sshd that can't be found, or dropbear, which has nothing to check, refuses a
// restart rather than risk a server without SSH (review L2).
func TestAnSSHRestartThatCantBeCheckedIsRefused(t *testing.T) {
	s := systemtest.New(t,
		systemtest.Unit{Props: systemtest.Running("ssh.service")},
		systemtest.Unit{Props: systemtest.Running("dropbear.service")},
	)
	s.NoSSHD(t)
	r := reader(s)
	for _, name := range []string{"ssh.service", "dropbear.service"} {
		u, _, err := r.PlanUnit(t.Context(), name, resource.UnitRestart)
		if err != nil {
			t.Fatal(err)
		}
		err = r.ApplyUnit(t.Context(), *u, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "won't restart "+name) {
			t.Errorf("restarting %s unchecked = %v", name, err)
		}
	}
	if slices.ContainsFunc(s.Calls(), func(c string) bool { return strings.HasPrefix(c, "systemctl restart") }) {
		t.Errorf("SSH restarted unchecked: %q", s.Calls())
	}
}

// What a step says is checked again on the server as it stands: a step that
// claims no guard for SSH, or no passkey for what an alias makes Docker, is
// refused, as is a runlevel that isn't one (review L3).
func TestApplyingChecksTheUnitAgain(t *testing.T) {
	s := systemtest.New(t,
		systemtest.Unit{Props: systemtest.Running("ssh.service", "Names", "ssh.service sshd.service")},
		systemtest.Unit{Props: systemtest.Running("moby.service", "Names", "moby.service docker.service")},
	)
	r := reader(s)
	if err := r.ApplyUnit(t.Context(), resource.Unit{Name: "ssh.service", Action: resource.UnitStop}, func(string) {}); err == nil || !strings.Contains(err.Error(), "how you reach") {
		t.Errorf("a stop of SSH said to have no guard = %v", err)
	}
	if err := r.ApplyUnit(t.Context(), resource.Unit{Name: "moby.service", Action: resource.UnitRestart}, func(string) {}); err == nil || !strings.Contains(err.Error(), "Plan it again") {
		t.Errorf("a restart of Docker by another name with no passkey = %v", err)
	}
	if slices.ContainsFunc(s.Calls(), func(c string) bool { return !strings.HasPrefix(c, "systemctl show") }) {
		t.Errorf("a refused step ran %q", s.Calls())
	}
	root := t.TempDir()
	for p, content := range map[string]string{
		"run/openrc/softlevel":     "default\n",
		"etc/init.d/nginx":         "#!/sbin/openrc-run\n",
		"etc/runlevels/default/.k": "",
		"sbin/rc-update":           "#!/bin/sh\nexit 0\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	o := &system.Reader{Tools: &hosttool.Runner{Root: root}, Root: root}
	err := o.ApplyUnit(t.Context(), resource.Unit{Name: "nginx", Action: resource.UnitDisable, OpenRC: true, WasAtBoot: "../../../tmp"}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "isn't one of this server's runlevels") {
		t.Errorf("a runlevel that isn't one = %v", err)
	}
	if err := o.ApplyUnit(t.Context(), resource.Unit{Name: "nginx", Action: resource.UnitDisable, OpenRC: true, WasAtBoot: "default"}, func(string) {}); err != nil {
		t.Errorf("taking nginx out of default = %v", err)
	}
}

// Starting a firewall loads its own rules: the plan asks for the passkey and the
// typed code and holds the server, and after it Dockwell's table goes back and the
// step says Docker's rules come back as Docker restarts (review M1).
func TestStartingAFirewallPutsDockwellsTableBack(t *testing.T) {
	s := systemtest.New(t, systemtest.Unit{Props: systemtest.Stopped("nftables.service")})
	r := reader(s)
	reloads := 0
	r.AfterFirewall = func(context.Context) error { reloads++; return nil }
	u, _, err := r.PlanUnit(t.Context(), "nftables.service", resource.UnitStart)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Passkey() || !u.Confirm() || !u.ReplacesRules() {
		t.Errorf("starting nftables takes passkey %t, typed %t, replaces rules %t", u.Passkey(), u.Confirm(), u.ReplacesRules())
	}
	var lines []string
	if err := r.ApplyUnit(t.Context(), *u, say(&lines)); err != nil {
		t.Fatal(err)
	}
	if reloads != 1 || !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "restart docker.service") }) {
		t.Errorf("after the start: %d reloads, the step said %q", reloads, lines)
	}
}

// A network restart checks the settings load first, where ifupdown can say
// (review L5).
func TestANetworkRestartChecksItsSettingsFirst(t *testing.T) {
	s := systemtest.New(t, systemtest.Unit{Props: systemtest.Running("ifup@eth0.service")})
	s.IfUp(t, "/etc/network/interfaces:12: misplaced option")
	r := reader(s)
	u, _, err := r.PlanUnit(t.Context(), "ifup@eth0.service", resource.UnitRestart)
	if err != nil {
		t.Fatal(err)
	}
	err = r.ApplyUnit(t.Context(), *u, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "misplaced option") {
		t.Fatalf("restarting with settings that don't load = %v", err)
	}
	if !slices.Contains(s.Calls(), "ifup --no-act eth0") || slices.ContainsFunc(s.Calls(), func(c string) bool { return strings.HasPrefix(c, "systemctl restart") }) {
		t.Errorf("ran %q", s.Calls())
	}
	s.IfUp(t, "")
	if err := r.ApplyUnit(t.Context(), *u, func(string) {}); err != nil {
		t.Fatalf("restarting with settings that load = %v", err)
	}
}
