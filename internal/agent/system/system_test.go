// SPDX-License-Identifier: AGPL-3.0-only

package system

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
)

// stand is a server's files with a stand-in for its init's tool, which prints the
// fixtures and notes each call. init is "systemd", "openrc" or "" for another.
func stand(t *testing.T, init string) (root string, calls func() []string) {
	t.Helper()
	return standOf(t, init, init)
}

// standOf is stand with the fixtures in testdata/set.
func standOf(t *testing.T, init, set string) (root string, calls func() []string) {
	t.Helper()
	root = t.TempDir()
	fixtures, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, content string, mode os.FileMode) {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	head := "#!/bin/sh\necho \"$*\" >> '" + filepath.Join(root, "calls") + "'\n"
	switch init {
	case "systemd":
		write("run/systemd/system/.keep", "", 0o644)
		write("usr/bin/systemctl", head+"case \"$1\" in\nlist-units) cat '"+fixtures+"/"+set+"/list-units.txt' ;;\nlist-unit-files) cat '"+fixtures+"/"+set+"/list-unit-files.txt' ;;\nshow) cat '"+fixtures+"/"+set+"/show.txt' ;;\nesac\n", 0o755)
	case "openrc":
		write("run/openrc/softlevel", "default\n", 0o644)
		write("sbin/rc-status", head+"cat '"+fixtures+"/openrc/rc-status.txt'\n", 0o755)
	}
	return root, func() []string {
		b, _ := os.ReadFile(filepath.Join(root, "calls"))
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

func read(t *testing.T, root string) *dockwellv1.SystemServiceList {
	t.Helper()
	r := &Reader{Tools: &hosttool.Runner{Root: root}, Root: root}
	list, err := r.Services(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func names(list *dockwellv1.SystemServiceList) []string {
	var out []string
	for _, s := range list.GetServices() {
		out = append(out, s.GetName())
	}
	return out
}

func find(t *testing.T, list *dockwellv1.SystemServiceList, name string) *dockwellv1.SystemServiceInfo {
	t.Helper()
	for _, s := range list.GetServices() {
		if s.GetName() == name {
			return s
		}
	}
	t.Fatalf("%s isn't listed: %v", name, names(list))
	return nil
}

// A systemd server's services, failed first, then running, then the rest: each
// loaded unit and each installed one, and the sockets Dockwell needs, but not a
// unit systemd doesn't know nor a template.
func TestASystemdServersServicesAreListedFailedFirst(t *testing.T) {
	root, _ := stand(t, "systemd")
	list := read(t, root)
	if list.GetInit() != dockwellv1.SystemInit_SYSTEM_INIT_SYSTEMD || list.GetTime() == nil {
		t.Errorf("the list reads init %v at %v", list.GetInit(), list.GetTime())
	}
	want := []string{
		"nginx.service",
		"containerd.service", "cron.service", "docker.service", "docker.socket", "dockwell.service", "ssh.socket", "systemd-journald.service",
		"apparmor.service", "apt-daily.service", "redis-server.service", "ssh.service", "ufw.service",
	}
	if got := names(list); !slices.Equal(got, want) {
		t.Errorf("listed %v\nwant   %v", got, want)
	}

	nginx := find(t, list, "nginx.service")
	if nginx.GetState() != dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_FAILED || nginx.GetResult() != "exit-code" || nginx.GetExitStatus() != 1 ||
		nginx.GetAtBoot() != "enabled" || nginx.GetDescription() != "A high performance web server and a reverse proxy server" ||
		!nginx.GetSince().AsTime().Equal(time.Date(2026, 11, 10, 3, 12, 44, 0, time.UTC)) || nginx.MemoryBytes != nil || nginx.GetMainPid() != 0 {
		t.Errorf("a failed unit reads %v", nginx)
	}
	containerd := find(t, list, "containerd.service")
	if containerd.GetMainPid() != 812 || containerd.GetMemoryBytes() != 41943040 || containerd.GetResult() != "" {
		t.Errorf("a running unit reads %v", containerd)
	}
	// What a unit doesn't account reads unknown, as [not set] or as systemd 239's
	// largest number.
	for _, n := range []string{"cron.service", "docker.service"} {
		if s := find(t, list, n); s.MemoryBytes != nil {
			t.Errorf("%s's memory reads %d, want unknown", n, s.GetMemoryBytes())
		}
	}
	if s := find(t, list, "ufw.service"); s.GetState() != dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_EXITED {
		t.Errorf("a one-shot that ran reads %v", s.GetState())
	}
	if s := find(t, list, "redis-server.service"); s.GetState() != dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_INACTIVE || s.GetAtBoot() != "disabled" || s.GetSince() != nil {
		t.Errorf("an installed unit that never ran reads %v", s)
	}
	// What Dockwell needs is marked, the SSH socket beside its service.
	for _, s := range list.GetServices() {
		needed := slices.Contains([]string{"containerd.service", "docker.service", "docker.socket", "dockwell.service", "ssh.service", "ssh.socket"}, s.GetName())
		if s.GetNeeded() != needed {
			t.Errorf("%s reads needed %t", s.GetName(), s.GetNeeded())
		}
	}
}

// On Debian and Ubuntu, list-unit-files lists aliases as units of their own, such
// as sshd.service for ssh.service and procps.service for systemd-sysctl.service.
// Each unit is listed once: an alias isn't asked about, and one show reads as the
// unit it names isn't listed again. The unit files are this machine's (systemd
// 257), with Debian 12's sshd.service; show's blocks are as systemd prints them.
func TestAnAliasIsntListedAsASecondUnit(t *testing.T) {
	root, calls := standOf(t, "systemd", "debian-aliases")
	list := read(t, root)
	want := []string{
		"dbus.service", "ssh.service", "systemd-logind.service",
		"kmod-static-nodes.service", "systemd-modules-load.service", "systemd-sysctl.service",
	}
	if got := names(list); !slices.Equal(got, want) {
		t.Errorf("listed %v\nwant   %v", got, want)
	}
	for _, c := range calls() {
		if !strings.HasPrefix(c, "show ") {
			continue
		}
		for _, alias := range []string{"sshd.service", "procps.service", "kmod.service", "dbus-org.freedesktop.login1.service"} {
			if slices.Contains(strings.Fields(c), alias) {
				t.Errorf("show was asked about the alias %s: %s", alias, c)
			}
		}
	}
}

// A name systemctl prints that reads as a flag goes after "--", where it's a name;
// one with "=" isn't a unit's, and isn't asked about. A systemctl that fails says
// so, and lists nothing.
func TestOddNamesAndAFailingSystemctl(t *testing.T) {
	root, calls := standOf(t, "systemd", "odd")
	if got := names(read(t, root)); !slices.Equal(got, []string{"cron.service"}) {
		t.Errorf("listed %v, want cron.service alone", got)
	}
	for _, c := range calls() {
		if !strings.HasPrefix(c, "show ") {
			continue
		}
		args := strings.Fields(c)
		dashes, flag := slices.Index(args, "--"), slices.Index(args, "--now.service")
		if dashes < 0 || flag < dashes {
			t.Errorf("a name that reads as a flag came before \"--\": %s", c)
		}
		if slices.Contains(args, "a=b.service") {
			t.Errorf("a name with \"=\" was asked about: %s", c)
		}
	}

	if err := os.WriteFile(filepath.Join(root, "usr", "bin", "systemctl"), []byte("#!/bin/sh\necho 'Failed to connect to bus: No such file or directory' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Reader{Tools: &hosttool.Runner{Root: root}, Root: root}
	if list, err := r.Services(t.Context()); err == nil || !strings.Contains(err.Error(), "can't read the server's system services") || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Errorf("a failing systemctl reads %v, %v", list, err)
	}
}

// systemctl show reads only what the list shows, never Environment or Exec*,
// which can hold a password, and takes unit names only after "--".
func TestAListReadsOnlyWhatItShows(t *testing.T) {
	root, calls := stand(t, "systemd")
	read(t, root)
	var show string
	for _, c := range calls() {
		if strings.HasPrefix(c, "show ") {
			show = c
		}
	}
	if show == "" {
		t.Fatalf("no show among %q", calls())
	}
	props, units, ok := strings.Cut(strings.TrimPrefix(show, "show "), " -- ")
	if !ok {
		t.Fatalf("show passes no units after --: %q", show)
	}
	if !strings.Contains(props, "--property=Id,Description,LoadState,ActiveState,SubState,UnitFileState,MainPID,MemoryCurrent,StateChangeTimestamp,ExecMainStatus,Result") {
		t.Errorf("show reads %q", props)
	}
	for _, p := range strings.Split(strings.TrimPrefix(strings.Fields(props)[0], "--property="), ",") {
		if strings.HasPrefix(p, "Environment") || strings.HasPrefix(p, "Exec") && p != "ExecMainStatus" {
			t.Errorf("show reads %s", p)
		}
	}
	if strings.Contains(units, "getty@.service") {
		t.Errorf("show asks of a template: %q", units)
	}
}

// An OpenRC server's services, by runlevel: those in one start at boot in it,
// the dynamic ones don't.
func TestAnOpenRCServersServicesAreListed(t *testing.T) {
	root, _ := stand(t, "openrc")
	list := read(t, root)
	if list.GetInit() != dockwellv1.SystemInit_SYSTEM_INIT_OPENRC {
		t.Fatalf("the list reads init %v", list.GetInit())
	}
	if got := names(list)[0]; got != "nginx" {
		t.Errorf("listed %v, want the crashed one first", names(list))
	}
	if s := find(t, list, "nginx"); s.GetState() != dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_FAILED || s.GetDetail() != "crashed" || s.GetAtBoot() != "default" {
		t.Errorf("a crashed service reads %v", s)
	}
	if s := find(t, list, "docker"); s.GetState() != dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_RUNNING || !s.GetNeeded() || s.GetAtBoot() != "default" {
		t.Errorf("Docker reads %v", s)
	}
	if s := find(t, list, "containerd"); s.GetAtBoot() != "" || !s.GetNeeded() {
		t.Errorf("a service started as needed reads %v", s)
	}
	if s := find(t, list, "redis"); s.GetState() != dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_INACTIVE || s.GetAtBoot() != "" {
		t.Errorf("a stopped manual service reads %v", s)
	}
}

// A server whose init is neither says so, and runs nothing; so does one with
// systemd installed that isn't its init.
func TestAnotherInitIsSaidAndNothingRuns(t *testing.T) {
	root, calls := stand(t, "")
	if err := os.MkdirAll(filepath.Join(root, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr", "bin", "systemctl"), []byte("#!/bin/sh\necho \"$*\" >> '"+filepath.Join(root, "calls")+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	list := read(t, root)
	if list.GetInit() != dockwellv1.SystemInit_SYSTEM_INIT_OTHER || len(list.GetServices()) != 0 {
		t.Errorf("another init reads %v", list)
	}
	if c := calls(); len(c) != 1 || c[0] != "" {
		t.Errorf("another init ran %q", c)
	}
}

// A list runs on the reader's own runner: a firewall change holding the agent's
// shared one, waiting on its confirm timer, holds up no list.
func TestAListDoesntWaitForTheSharedRunner(t *testing.T) {
	root, _ := stand(t, "systemd")
	shared := &hosttool.Runner{Root: root}
	held, err := shared.Turn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Done()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r := &Reader{Tools: &hosttool.Runner{Root: root}, Root: root}
	if _, err := r.Services(ctx); err != nil {
		t.Fatalf("a list waited on the shared runner: %v", err)
	}
}
