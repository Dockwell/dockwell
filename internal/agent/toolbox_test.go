// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/resource"
)

func toolboxFixture(t *testing.T) *hostToolbox {
	t.Helper()
	root := t.TempDir()
	tbox := &hostToolbox{tools: &hosttool.Runner{Root: root}, dir: t.TempDir()}
	for _, folder := range []string{"/etc/systemd/system", "/etc/sysctl.d", "/proc/sys/vm", "/usr/bin", "/run/systemd/system", "/var/lib", "/var/lib/apt/lists", "/var/lib/dpkg", "/var/cache/apt/archives"} {
		if err := os.MkdirAll(tbox.path(folder), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return tbox
}

func fixtureFile(t *testing.T, box *hostToolbox, name, value string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(box.path(name), []byte(value), mode); err != nil {
		t.Fatal(err)
	}
}

func toolboxAsk(op *v1.HostToolboxOperation) *v1.HostToolboxPlan {
	return &v1.HostToolboxPlan{Operation: op, ChangeId: "chg_01ABCDEFGHIJKLMNOPQRSTUVWX"}
}

func TestToolboxIdentityUsesHostTools(t *testing.T) {
	box := toolboxFixture(t)
	fixtureFile(t, box, "/usr/bin/systemctl", "#!/bin/sh\nexit 0\n", 0o755)
	fixtureFile(t, box, "/etc/hostname", "current\n", 0o644)
	fixtureFile(t, box, "/etc/hosts", "127.0.0.1 localhost\n9.8.7.6 admin.local # a hand edit\n", 0o644)
	fixtureFile(t, box, "/usr/bin/hostnamectl", "#!/bin/sh\ncase \"$1\" in --static) echo current;; set-hostname) [ \"$2\" = next ]; esac\n", 0o755)
	fixtureFile(t, box, "/usr/bin/timedatectl", "#!/bin/sh\ncase \"$1 $2\" in 'show --property=Timezone') echo UTC;; 'list-timezones --no-pager') printf 'UTC\\nAsia/Shanghai\\n';; 'set-timezone Asia/Shanghai') exit 0;; 'set-ntp false') exit 0;; 'show --property=CanNTP') printf 'CanNTP=yes\\nNTP=yes\\nNTPSynchronized=yes\\n';; *) exit 1;; esac\n", 0o755)
	ask := toolboxAsk(&v1.HostToolboxOperation{Operation: &v1.HostToolboxOperation_Hostname{Hostname: "next"}})
	edit := toolboxEdit{plan: ask}
	if err := box.planHostname(t.Context(), &edit, "next"); err != nil {
		t.Fatal(err)
	}
	if err := box.writeFiles(t.Context(), edit); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(box.path("/etc/hosts"))
	if err != nil || string(raw) != "127.0.0.1 localhost\n9.8.7.6 admin.local # a hand edit\n127.0.1.1 next # Dockwell hostname\n" {
		t.Fatalf("hosts = %q, %v", raw, err)
	}
	zone := toolboxAsk(&v1.HostToolboxOperation{Operation: &v1.HostToolboxOperation_TimeZone{TimeZone: "Asia/Shanghai"}})
	reviewed, err := box.plan(t.Context(), zone)
	if err != nil {
		t.Fatal(err)
	}
	if err := box.apply(t.Context(), reviewed.plan); err != nil {
		t.Fatal(err)
	}
	if err := box.apply(t.Context(), reviewed.plan); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, err := box.plan(t.Context(), toolboxAsk(&v1.HostToolboxOperation{Operation: &v1.HostToolboxOperation_TimeZone{TimeZone: "../passwd"}})); err == nil {
		t.Fatal("accepted a path outside zoneinfo")
	}
	available, enabled, synced := box.ntp(t.Context())
	if !available || !enabled || !synced {
		t.Fatalf("NTP %t %t %t", available, enabled, synced)
	}
}

func TestToolboxKeepChecksConnectionAndTimeoutReverts(t *testing.T) {
	box := toolboxFixture(t)
	fixtureFile(t, box, "/etc/hosts", "127.0.0.1 localhost\n", 0o644)
	for _, tool := range []string{"sh", "flock", "rm", "mv", "cp", "mktemp"} {
		if err := os.Symlink("/usr/bin/"+tool, box.path("/usr/bin/"+tool)); err != nil {
			t.Fatal(err)
		}
	}
	ask := toolboxAsk(&v1.HostToolboxOperation{Operation: &v1.HostToolboxOperation_Hosts{Hosts: &v1.ToolboxHosts{Hostname: "app.local", Address: "127.0.1.2"}}})
	edit, err := box.plan(t.Context(), ask)
	if err != nil {
		t.Fatal(err)
	}
	dir := box.changeDir(ask.GetChangeId())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := box.makeRevert(dir, edit); err != nil {
		t.Fatal(err)
	}
	if err := box.writeFiles(t.Context(), edit); err != nil {
		t.Fatal(err)
	}
	if err := toolboxSetState(dir, "ready"); err != nil {
		t.Fatal(err)
	}
	deadline := filepath.Join(dir, "deadline")
	if err := os.WriteFile(deadline, []byte(time.Now().Add(time.Minute).Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatal(err)
	}
	box.probe = func(_ context.Context) error { return errors.New("DNS is down") }
	if got, _ := box.keep(t.Context(), ask.GetChangeId()); got != resource.AccessNotProved {
		t.Fatalf("keep without connectivity = %v", got)
	}
	if err := os.WriteFile(deadline, []byte(time.Now().Add(-time.Second).Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := box.awaitKeep(t.Context(), dir); err == nil {
		t.Fatal("timeout reported success")
	}
	raw, err := os.ReadFile(box.path("/etc/hosts"))
	if err != nil || string(raw) != "127.0.0.1 localhost\n" {
		t.Fatalf("hosts after timeout = %q, %v", raw, err)
	}
}

func TestToolboxCleanupSkipsOpenLinkedAndNewFiles(t *testing.T) {
	box := toolboxFixture(t)
	if err := os.Mkdir(box.path("/var/log"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(box.path("/var/log"), 0o775); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old", "open", "hardlink", "new"} {
		fixtureFile(t, box, "/var/log/"+name+".1", name, 0o600)
		if name != "new" {
			then := time.Now().Add(-4 * 24 * time.Hour)
			if err := os.Chtimes(box.path("/var/log/"+name+".1"), then, then); err != nil {
				t.Fatal(err)
			}
		}
	}
	file, err := os.Open(box.path("/var/log/open.1"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Link(box.path("/var/log/hardlink.1"), box.path("/var/log/alias.1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(box.path("/var/log/old.1"), box.path("/var/log/symlink.1")); err != nil {
		t.Fatal(err)
	}
	ask := toolboxAsk(&v1.HostToolboxOperation{Operation: &v1.HostToolboxOperation_Cleanup{Cleanup: &v1.ToolboxCleanup{Kind: v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_ROTATED_LOGS, Days: 3}}})
	edit, err := box.plan(t.Context(), ask)
	if err != nil {
		t.Fatal(err)
	}
	if len(edit.plan.GetFiles()) != 1 || edit.plan.GetFiles()[0].GetPath() != "/var/log/old.1" {
		t.Fatalf("cleanup plan = %v", edit.plan.GetFiles())
	}
	openedAfterReview, err := os.Open(box.path("/var/log/old.1"))
	if err != nil {
		t.Fatal(err)
	}
	err = box.apply(t.Context(), edit.plan)
	_ = openedAfterReview.Close()
	if err == nil {
		t.Fatal("cleanup accepted a log opened after review")
	}
	if err := box.apply(t.Context(), edit.plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(box.path("/var/log/old.1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reviewed file remains: %v", err)
	}
	for _, name := range []string{"open", "hardlink", "alias", "new", "symlink"} {
		if _, err := os.Lstat(box.path("/var/log/" + name + ".1")); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestToolboxCleanupIncludesLogsOwnedBySyslog(t *testing.T) {
	var caps [2]unix.CapUserData
	if err := unix.Capget(&unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}, &caps[0]); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 || caps[0].Effective&(1<<unix.CAP_LEASE) == 0 {
		t.Skip("needs privilege to set syslog ownership and lease the file")
	}
	box := toolboxFixture(t)
	if err := os.Mkdir(box.path("/var/log"), 0o755); err != nil {
		t.Fatal(err)
	}
	const path = "/var/log/syslog.1"
	fixtureFile(t, box, path, "rotated syslog", 0o640)
	if err := os.Chown(box.path(path), 101, 4); err != nil {
		t.Fatal(err)
	}
	ask := toolboxAsk(&v1.HostToolboxOperation{Operation: &v1.HostToolboxOperation_Cleanup{Cleanup: &v1.ToolboxCleanup{
		Kind: v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_ROTATED_LOGS, Days: 3,
	}}})
	then := time.Now().Add(-4 * 24 * time.Hour)
	if err := os.Chtimes(box.path(path), then, then); err != nil {
		t.Fatal(err)
	}
	plan, err := box.plan(t.Context(), ask)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.plan.GetFiles()) != 1 || plan.plan.GetFiles()[0].GetPath() != path {
		t.Fatalf("cleanup plan omitted %s: %v", path, plan.plan.GetFiles())
	}
	if err := box.apply(t.Context(), plan.plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(box.path(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reviewed log wasn't removed: %v", err)
	}
}

func TestToolboxCleanupRestoresAnOpenLogRotatedIntoPlace(t *testing.T) {
	box := toolboxFixture(t)
	if err := os.Mkdir(box.path("/var/log"), 0o775); err != nil {
		t.Fatal(err)
	}
	const active = "/var/log/syslog"
	const rotated = "/var/log/syslog.1"
	const older = "/var/log/syslog.2"
	fixtureFile(t, box, active, "active", 0o640)
	fixtureFile(t, box, rotated, "rotated", 0o640)
	open, err := os.Open(box.path(active))
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	candidates, err := box.scanCleanup(t.Context(), v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_ROTATED_LOGS, time.Time{}, 0)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("scan: %v, %v", candidates, err)
	}
	candidate := candidates[0]
	lease, err := box.leaseCleanup(candidate)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.file.Close()
	defer lease.parent.Close()
	if !lease.exclusive() || !candidate.matches(lease.parent) {
		t.Fatal("the reviewed entry changed before rotation")
	}
	// Rotate the active log into the reviewed name after the last public
	// directory check. Only the older inode still has an exclusive lease.
	if err := os.Rename(box.path(rotated), box.path(older)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(box.path(active), box.path(rotated)); err != nil {
		t.Fatal(err)
	}
	if err := box.unlinkRotatedLog(candidate, lease); err == nil {
		t.Fatal("cleaned a different inode")
	}
	stillOpen, err := open.Stat()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := os.Stat(box.path(rotated))
	if err != nil || !os.SameFile(stillOpen, restored) {
		t.Fatalf("the open log wasn't restored: %v", err)
	}
	previous, err := os.Stat(box.path(older))
	if err != nil || !os.SameFile(candidate.info, previous) {
		t.Fatalf("the older log changed: %v", err)
	}
}

func TestToolboxUnitLiteralsAndPrivateEnv(t *testing.T) {
	spec := &v1.ToolboxService{Name: "worker", User: "root", WorkingDirectory: "/srv/100% safe", Argv: []string{"/bin/echo", "$HOME", "a b", "100%"}, Restart: "on-failure", Environment: []*v1.ToolboxBinding{{Name: "TOKEN"}}}
	file, err := toolboxServiceFile(spec, "dockwell-toolbox-worker.service")
	if err != nil || !strings.Contains(file, "ExecStart=:\"/bin/echo\" \"$HOME\" \"a b\" \"100%%\"") || !strings.Contains(file, "WorkingDirectory=/srv/100%% safe") {
		t.Fatalf("unit = %q, %v", file, err)
	}
	spec.WorkingDirectory = `/srv/literal\backslash`
	file, err = toolboxServiceFile(spec, "dockwell-toolbox-worker.service")
	if err != nil || !strings.Contains(file, `WorkingDirectory=/srv/literal\backslash`) {
		t.Fatalf("literal working directory = %q, %v", file, err)
	}
	spec.WorkingDirectory += `\`
	if _, err := toolboxServiceFile(spec, "dockwell-toolbox-worker.service"); err == nil {
		t.Fatal("accepted a working directory ending in backslash")
	}
	ctx := resource.WithValues(t.Context(), map[string]string{"TOKEN": "value\"\nother=word"})
	env, err := toolboxEnvironment(ctx, spec.GetEnvironment())
	if err != nil || string(env) != toolboxUnitMarker+"TOKEN=\"value\\\"\nother=word\"\n" {
		t.Fatalf("environment = %q, %v", env, err)
	}
}

func TestToolboxSwapKeepsFstabWhenVerificationFails(t *testing.T) {
	box := toolboxFixture(t)
	const original = "# keep this file\n"
	fixtureFile(t, box, fstabPath, original, 0o644)
	for _, name := range []string{"mkswap", "swapon", "swapoff"} {
		fixtureFile(t, box, "/usr/bin/"+name, "#!/bin/sh\nexit 0\n", 0o755)
	}
	fixtureFile(t, box, "/usr/bin/findmnt", "#!/bin/sh\nexit 1\n", 0o755)
	w, err := box.file(fstabPath, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	w.next = []byte(original + toolboxSwapLine() + "\n")
	if err := box.changeSwap(t.Context(), w, 1, false, false); err == nil {
		t.Fatal("accepted a file which failed findmnt verification")
	}
	if _, err := os.Stat(box.path(toolboxSwapPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("left an untracked swap file: %v", err)
	}
	backup, err := os.ReadFile(box.path(fstabPath + ".dockwell-toolbox.bak"))
	if err != nil || string(backup) != original {
		t.Fatalf("fstab backup = %q, %v", backup, err)
	}
	fixtureFile(t, box, "/usr/bin/findmnt", "#!/bin/sh\n[ \"$1\" = --verify ] && [ \"$2\" = --tab-file ] && test -s \"$3\"\n", 0o755)
	if err := box.changeSwap(t.Context(), w, 1, false, false); err != nil {
		t.Fatal(err)
	}
	w, err = box.file(fstabPath, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if string(w.old) != original+toolboxSwapLine()+"\n" {
		t.Fatalf("unexpected fstab: %q", w.old)
	}
	if fi, err := os.Stat(box.path(toolboxSwapPath)); err != nil || fi.Size() != swapMib || fi.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected swap file: %v, %v", fi, err)
	}
	w.next = []byte(original)
	if err := box.changeSwap(t.Context(), w, 0, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(box.path(toolboxSwapPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("swap file still there: %v", err)
	}
}

func TestToolboxDNSUsesTheResolverOwner(t *testing.T) {
	box := toolboxFixture(t)
	fixtureFile(t, box, "/etc/resolv.conf", "# Generated by NetworkManager\nnameserver 192.0.2.53\n", 0o644)
	if owner := box.dnsOwner(t.Context()); owner.name != "NetworkManager" || owner.target != "" {
		t.Fatalf("NetworkManager file: %+v", owner)
	}
	fixtureFile(t, box, "/etc/resolv.conf", "# custom resolver\nsearch example.net\nnameserver 192.0.2.53\n", 0o644)
	edit := toolboxEdit{plan: &v1.HostToolboxPlan{}}
	if err := box.planDNS(t.Context(), &edit, []string{"2001:db8::53"}); err != nil {
		t.Fatal(err)
	}
	if w := edit.files[0]; w.path != "/etc/resolv.conf" || string(w.next) != "nameserver 2001:db8::53\n# custom resolver\nsearch example.net\n" {
		t.Fatalf("DNS file: %s\n%s", w.path, w.next)
	}
	if err := os.MkdirAll(box.path("/run/systemd/resolve"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, box, "/run/systemd/resolve/stub-resolv.conf", "nameserver 127.0.0.53\n", 0o644)
	if err := os.Remove(box.path("/etc/resolv.conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../run/systemd/resolve/stub-resolv.conf", box.path("/etc/resolv.conf")); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, box, "/usr/bin/resolvectl", "#!/bin/sh\nprintf 'Global: 192.0.2.1\nLink 2 (eth0): 192.0.2.2\n'\n", 0o755)
	edit = toolboxEdit{plan: &v1.HostToolboxPlan{}}
	if err := box.planDNS(t.Context(), &edit, []string{"192.0.2.53"}); err != nil {
		t.Fatal(err)
	}
	if w := edit.files[0]; w.path != toolboxResolvedDropin || !strings.Contains(string(w.next), "DNS=192.0.2.53\n") {
		t.Fatalf("resolved file: %s\n%s", w.path, w.next)
	}
	if got := fmt.Sprint(edit.plan.GetDifferences()); !strings.Contains(got, "Link DNS servers stay in use") ||
		!strings.Contains(got, "192.0.2.2") || !strings.Contains(got, "192.0.2.1") {
		t.Fatalf("resolved servers missing from plan: %s", got)
	}
}

func TestToolboxKeepUsesFreshHostsAndTLS(t *testing.T) {
	box := toolboxFixture(t)
	server := httptest.NewUnstartedServer(nil)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	const name = "localhost" // ProxyFromEnvironment always bypasses localhost.
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conf := &tls.Config{ServerName: server.Certificate().DNSNames[0], RootCAs: roots, MinVersion: tls.VersionTLS13}
	fixtureFile(t, box, "/etc/hosts", "127.0.0.1 "+name+"\n", 0o644)
	cached, err := box.freshControlAddresses(t.Context(), name)
	if err != nil || len(cached) != 1 {
		t.Fatalf("cached lookup: %v, %v", cached, err)
	}
	dir := box.changeDir(toolboxAsk(nil).GetChangeId())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := toolboxSetState(dir, "ready"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deadline"), []byte(time.Now().Add(time.Minute).Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatal(err)
	}
	box.probe = func(ctx context.Context) error {
		return box.probeControlURL(ctx, "https://"+net.JoinHostPort(name, port), conf)
	}
	if err := box.probe(t.Context()); err != nil {
		t.Fatalf("pre-change TLS: %v", err)
	}
	fixtureFile(t, box, "/etc/hosts", "127.0.0.2 "+name+"\n", 0o644)
	dialer := tls.Dialer{Config: conf}
	connection, err := dialer.DialContext(t.Context(), "tcp", net.JoinHostPort(cached[0].String(), port))
	if err != nil {
		t.Fatalf("the stale answer should still open a TLS connection: %v", err)
	}
	_ = connection.Close()
	if got, _ := box.keep(t.Context(), filepath.Base(dir)); got != resource.AccessNotProved || toolboxState(dir) != "ready" {
		t.Fatalf("keep with broken name resolution = %v, state %s", got, toolboxState(dir))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	offered := false
	wait := resource.WithOutcome(ctx, func(o resource.Outcome) { offered = offered || o.Waits != nil })
	_ = box.awaitKeep(wait, dir)
	if offered {
		t.Fatal("offered Keep while the fresh connection failed")
	}
	fixtureFile(t, box, "/etc/hosts", "127.0.0.1 other\n", 0o644)
	if err := os.MkdirAll(box.path("/run/systemd/resolve"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, box, "/usr/bin/resolvectl", fmt.Sprintf("#!/bin/sh\n[ \"$1\" = flush-caches ] && touch '%s'\n", box.path("/flushed")), 0o755)
	fixtureFile(t, box, "/usr/bin/getent", fmt.Sprintf("#!/bin/sh\ntest -f '%s' && printf '127.0.0.1 STREAM %s\\n'\n", box.path("/flushed"), name), 0o755)
	if got, _ := box.keep(t.Context(), filepath.Base(dir)); got != resource.AccessKept {
		t.Fatalf("keep after connection restored = %v", got)
	}
}

func TestToolboxKeepFollowsProxyEnvironment(t *testing.T) {
	mode := os.Getenv("DOCKWELL_TOOLBOX_TEST_PROXY")
	if mode == "" {
		// ProxyFromEnvironment caches its configuration for the process.
		for _, name := range []string{"proxy", "bypass"} {
			t.Run(name, func(t *testing.T) {
				cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestToolboxKeepFollowsProxyEnvironment$")
				cmd.Env = append(os.Environ(), "DOCKWELL_TOOLBOX_TEST_PROXY="+name)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
			})
		}
		return
	}
	box := toolboxFixture(t)
	control := httptest.NewUnstartedServer(http.NotFoundHandler())
	control.EnableHTTP2 = true
	control.StartTLS()
	defer control.Close()
	controlAddress := control.Listener.Addr().String()
	_, controlPort, err := net.SplitHostPort(controlAddress)
	if err != nil {
		t.Fatal(err)
	}
	name := control.Certificate().DNSNames[0]
	target := net.JoinHostPort(name, controlPort)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if mode != "proxy" || req.Method != http.MethodConnect || req.Host != target || req.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			t.Errorf("unexpected CONNECT to %q or missing proxy credentials", req.Host)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		upstream, err := net.Dial("tcp", controlAddress)
		if err != nil {
			t.Error(err)
			return
		}
		defer upstream.Close()
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		go func() { _, _ = io.Copy(upstream, rw) }()
		_, _ = io.Copy(conn, upstream)
	}))
	defer proxy.Close()
	_, proxyPort, err := net.SplitHostPort(proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	const proxyName = "toolbox-proxy.example.invalid"
	t.Setenv("HTTPS_PROXY", "http://user:pass@"+net.JoinHostPort(proxyName, proxyPort))
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	hop := proxyName
	if mode == "bypass" {
		t.Setenv("NO_PROXY", target)
		hop = name
	}
	fixtureFile(t, box, "/etc/hosts", "127.0.0.1 "+hop+"\n", 0o644)
	box.probe = func(ctx context.Context) error {
		roots := x509.NewCertPool()
		roots.AddCert(control.Certificate())
		return box.probeControlURL(ctx, "https://"+target, &tls.Config{ServerName: name, RootCAs: roots, MinVersion: tls.VersionTLS13})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := box.probe(ctx); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, box, "/etc/hosts", "127.0.0.2 "+hop+"\n", 0o644)
	if err := box.probe(ctx); err == nil {
		t.Fatal("Keep passed after the connection's first hop stopped answering")
	}
}

func TestToolboxRecoverDNSAfterRestart(t *testing.T) {
	for _, netplan := range []bool{false, true} {
		t.Run(fmt.Sprintf("netplan=%t", netplan), func(t *testing.T) {
			box := toolboxFixture(t)
			for _, tool := range []string{"sh", "flock", "rm", "mv", "cp", "mktemp"} {
				if err := os.Symlink("/usr/bin/"+tool, box.path("/usr/bin/"+tool)); err != nil {
					t.Fatal(err)
				}
			}
			fixtureFile(t, box, "/etc/resolv.conf", "nameserver 192.0.2.53\n", 0o644)
			ask := toolboxAsk(&v1.HostToolboxOperation{Operation: &v1.HostToolboxOperation_Dns{Dns: &v1.ToolboxDNS{Servers: []string{"192.0.2.54"}}}})
			edit := toolboxEdit{plan: ask}
			path := "/etc/resolv.conf"
			if netplan {
				path = toolboxNetplanDropin
				for _, dir := range []string{"/etc/netplan", "/run/systemd/resolve", "/proc/net"} {
					if err := os.MkdirAll(box.path(dir), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Remove(box.path("/etc/resolv.conf")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../run/systemd/resolve/stub-resolv.conf", box.path("/etc/resolv.conf")); err != nil {
					t.Fatal(err)
				}
				fixtureFile(t, box, "/run/systemd/resolve/stub-resolv.conf", "nameserver 127.0.0.53\n", 0o644)
				fixtureFile(t, box, "/proc/net/route", "eth0 00000000 01000000 0003 0 0 10 00000000\n", 0o644)
				fixtureFile(t, box, "/etc/netplan/50-cloud-init.yaml", "network: {}\n", 0o600)
				fixtureFile(t, box, "/usr/bin/netplan", fmt.Sprintf("#!/bin/sh\ncase \"$1:$2\" in get:ethernets.eth0) echo 'dhcp4: true';; apply:) test ! -e '%s';; esac\n", box.path(path)), 0o755)
			}
			if err := box.planDNS(t.Context(), &edit, ask.GetOperation().GetDns().GetServers()); err != nil {
				t.Fatal(err)
			}
			dir := box.changeDir(ask.GetChangeId())
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := box.makeRevert(dir, edit); err != nil {
				t.Fatal(err)
			}
			if err := toolboxSetState(dir, "pending"); err != nil {
				t.Fatal(err)
			}
			if err := box.write(edit.files[0]); err != nil {
				t.Fatal(err)
			}
			restarted := &hostToolbox{tools: &hosttool.Runner{Root: box.root()}, dir: box.dir}
			if netplan {
				fixtureFile(t, box, "/usr/bin/netplan", "#!/bin/sh\nexit 1\n", 0o755)
				err := restarted.recoverToolbox(t.Context())
				if err == nil || toolboxState(dir) != "pending" {
					t.Fatalf("transient restore = %v, %s", err, toolboxState(dir))
				}
				msg := err.Error()
				restarted.recovery.Store(&msg)
				if restarted.status(t.Context()).GetRecoveryError() == "" {
					t.Fatal("failed recovery was not visible")
				}
				fixtureFile(t, box, "/usr/bin/netplan", fmt.Sprintf("#!/bin/sh\n[ \"$1\" = apply ] && test ! -e '%s'\n", box.path(path)), 0o755)
				restarted.retryRecovery(t.Context(), slog.Default(), time.Millisecond)
			} else {
				if err := restarted.recoverToolbox(t.Context()); err != nil {
					t.Fatal(err)
				}
				if raw, _ := restarted.read(path); string(raw) != "nameserver 192.0.2.53\n" {
					t.Fatalf("restored resolver = %q", raw)
				}
			}
			if toolboxState(dir) != "rolled_back" || restarted.recoveryError() != "" {
				t.Fatalf("recovery state = %s, %s", toolboxState(dir), restarted.recoveryError())
			}
		})
	}
}

func TestToolboxServiceRechecksPathsAndDropins(t *testing.T) {
	box := toolboxFixture(t)
	for _, name := range []string{"/srv/app", "/var/lib/docker/volumes/app/_data"} {
		if err := os.MkdirAll(box.path(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fixtureFile(t, box, "/srv/app/run", "service", 0o755)
	fixtureFile(t, box, "/var/lib/docker/volumes/app/_data/run", "service", 0o755)
	if err := os.Chmod(box.path("/var/lib/docker/volumes/app/_data/run"), 0o775); err != nil {
		t.Fatal(err)
	}
	unit := toolboxUnitDir + "/dockwell-toolbox-worker.service"
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> '%s'
case "$1:$2" in
--version:) echo 'systemd 259' ;;
show:--property=FragmentPath) if [ -f '%s' ]; then echo '%s'; fi ;;
show:--property=UnitPath) echo '/etc/systemd/system /run/systemd/system /usr/lib/systemd/system' ;;
show:--property=DropInPaths) if [ -f '%s' ]; then cat '%s'; fi ;;
is-active:--) echo active ;;
is-enabled:--) echo enabled ;;
daemon-reload:) if [ -f '%s' ]; then mv '%s' '%s'; fi ;;
start:--) test ! -e '%s' ;;
esac
`, box.path("/commands"), box.path(unit), unit, box.path("/dropin"), box.path("/dropin"), box.path("/late"), box.path("/late"), box.path("/dropin"), box.path("/fail"))
	fixtureFile(t, box, "/usr/bin/systemctl", script, 0o755)
	svc := &v1.ToolboxService{Name: "worker", User: "0", WorkingDirectory: "/srv/app", Argv: []string{"/var/lib/docker/volumes/app/_data/run"}, Restart: "no", StartOnBoot: true}
	ask := toolboxAsk(&v1.HostToolboxOperation{Operation: &v1.HostToolboxOperation_Service{Service: svc}})
	if _, err := box.plan(t.Context(), ask); err == nil || !strings.Contains(err.Error(), svc.Argv[0]) {
		t.Fatalf("volume file refusal = %v", err)
	}
	svc.Argv[0] = "/srv/app/run"
	edit, err := box.plan(t.Context(), ask)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(box.path("/srv/app"), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := box.apply(t.Context(), edit.plan); err == nil || !strings.Contains(err.Error(), "/srv/app") {
		t.Fatalf("group-writable folder accepted at apply: %v", err)
	}
	if err := os.Chmod(box.path("/srv/app"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, subdir := range []string{"dockwell-toolbox-worker.service.d", "dockwell-toolbox-.service.d", "dockwell-.service.d"} {
		dropin := "/etc/systemd/system/" + subdir
		if err := os.Mkdir(box.path(dropin), 0o755); err != nil {
			t.Fatal(err)
		}
		fixtureFile(t, box, dropin+"/override.conf", "[Service]\nRestartSec=4\n", 0o644)
		if _, err := box.plan(t.Context(), ask); err == nil || !strings.Contains(err.Error(), dropin) {
			t.Fatalf("create drop-in refusal = %v", err)
		}
		if err := os.RemoveAll(box.path(dropin)); err != nil {
			t.Fatal(err)
		}
	}
	fixtureFile(t, box, "/late", "/etc/systemd/system/service.d/late.conf\n", 0o644)
	if err := box.apply(t.Context(), edit.plan); err == nil || !strings.Contains(err.Error(), "drop-ins changed") {
		t.Fatalf("drop-in introduced during reload: %v", err)
	}
	if err := os.Remove(box.path("/dropin")); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, box, "/fail", "", 0o644)
	if err := box.apply(t.Context(), edit.plan); err == nil || !strings.Contains(err.Error(), "didn't start") {
		t.Fatalf("start failure = %v", err)
	}
	if _, err := os.Lstat(box.path(unit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed start left its unit: %v", err)
	}
	if err := os.Remove(box.path("/fail")); err != nil {
		t.Fatal(err)
	}
	dropin := "/usr/lib/systemd/system/service.d/10-distribution.conf"
	if err := os.MkdirAll(box.path(filepath.Dir(dropin)), 0o755); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, box, dropin, "[Service]\nRestartSec=3\n", 0o644)
	adminDir := "/etc/systemd/system/service.d"
	if err := os.Mkdir(box.path(adminDir), 0o755); err != nil {
		t.Fatal(err)
	}
	mask := adminDir + "/05-masked.conf"
	if err := os.Symlink("/dev/null", box.path(mask)); err != nil {
		t.Fatal(err)
	}
	shadowed := filepath.Join(filepath.Dir(dropin), filepath.Base(mask))
	fixtureFile(t, box, shadowed, "[Service]\nRestartSec=10\n", 0o644)
	runtime := "/run/systemd/real-service-overrides/20-runtime.conf"
	if err := os.Mkdir(box.path(filepath.Dir(runtime)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../real-service-overrides", box.path("/run/systemd/system/service.d")); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, box, runtime, "[Service]\nTimeoutStopSec=5\n", 0o644)
	ask.ChangeId += "dropin"
	edit, err = box.plan(t.Context(), ask)
	differences := fmt.Sprint(edit.plan.GetDifferences())
	if err != nil || !strings.Contains(differences, dropin) || !strings.Contains(differences, mask) ||
		!strings.Contains(differences, runtime) || strings.Contains(differences, shadowed) {
		t.Fatalf("plan didn't include the type-wide drop-in: %v, %v", edit.plan, err)
	}
	fixtureFile(t, box, dropin, "[Service]\nRestartSec=5\n", 0o644)
	if err := box.apply(t.Context(), edit.plan); err == nil || !strings.Contains(err.Error(), "host changed") {
		t.Fatalf("accepted a changed type-wide drop-in: %v", err)
	}
	fixtureFile(t, box, dropin, "[Service]\nRestartSec=3\n", 0o644)
	fixtureFile(t, box, "/late", mask+" "+dropin+" "+runtime+"\n", 0o644)
	if err := box.apply(t.Context(), edit.plan); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, box, "/dropin", "/etc/systemd/system/service.d/override.conf\n", 0o644)
	remove := toolboxAsk(&v1.HostToolboxOperation{Operation: &v1.HostToolboxOperation_Service{Service: &v1.ToolboxService{Name: "worker", Remove: true}}})
	remove.ChangeId += "remove"
	plan, err := box.plan(t.Context(), remove)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(plan.plan.GetDifferences()), "override.conf") {
		t.Fatalf("removal hides drop-ins: %s", plan.plan)
	}
	if err := box.apply(t.Context(), plan.plan); err != nil {
		t.Fatal(err)
	}
	raw, _ := box.read("/commands")
	for _, cmd := range []string{"enable -- dockwell-toolbox-worker.service", "start -- dockwell-toolbox-worker.service", "stop -- dockwell-toolbox-worker.service", "disable -- dockwell-toolbox-worker.service"} {
		if !strings.Contains(string(raw), cmd+"\n") {
			t.Errorf("missing systemctl %s", cmd)
		}
	}
	if _, err := os.Lstat(box.path(unit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove left its unit: %v", err)
	}
}

func TestToolboxCleanupRefusesSwappedLinks(t *testing.T) {
	for _, link := range []string{"hard", "symlink", "directory"} {
		t.Run(link, func(t *testing.T) {
			box := toolboxFixture(t)
			if err := os.MkdirAll(box.path("/var/log/app"), 0o755); err != nil {
				t.Fatal(err)
			}
			const path = "/var/log/app/worker.1"
			fixtureFile(t, box, path, "reviewed", 0o644)
			fixtureFile(t, box, "/outside", "keep", 0o644)
			candidates, err := box.scanCleanup(t.Context(), v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_ROTATED_LOGS, time.Time{}, 0)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("scan = %v, %v", candidates, err)
			}
			switch link {
			case "hard":
				err = os.Link(box.path(path), box.path("/linked"))
			case "symlink":
				if err = os.Remove(box.path(path)); err == nil {
					err = os.Symlink(box.path("/outside"), box.path(path))
				}
			default:
				if err = os.Rename(box.path("/var/log/app"), box.path("/moved")); err == nil {
					err = os.Symlink(box.path("/moved"), box.path("/var/log/app"))
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := box.cleanFiles(t.Context(), candidates); err == nil {
				t.Fatal("removed a candidate after a link swap")
			}
			if raw, _ := box.read("/outside"); string(raw) != "keep" {
				t.Fatalf("outside file changed: %q", raw)
			}
		})
	}
}

func TestToolboxTemporaryAgeUsesAllTimestamps(t *testing.T) {
	box := toolboxFixture(t)
	fixtureFile(t, box, "/file", "temporary", 0o600)
	old := time.Now().Add(-7 * 24 * time.Hour)
	if err := os.Chtimes(box.path("/file"), old, old); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(box.path("/file"))
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-3 * 24 * time.Hour)
	kind := v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_TEMPORARY
	if toolboxOldEnough(kind, fi, cutoff) {
		t.Fatal("a fresh ctime was treated as old")
	}
	st := fi.Sys().(*syscall.Stat_t)
	st.Ctim = st.Mtim
	if !toolboxOldEnough(kind, fi, cutoff) {
		t.Fatal("all timestamps are old")
	}
	st.Atim = syscall.NsecToTimespec(time.Now().UnixNano())
	if toolboxOldEnough(kind, fi, cutoff) {
		t.Fatal("a fresh atime was treated as old")
	}
}

func TestToolboxAPTLock(t *testing.T) {
	if path := os.Getenv("TOOLBOX_APT_LOCK_HELPER"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		flock := unix.Flock_t{Type: unix.F_WRLCK, Whence: unix.SEEK_SET}
		if err := unix.FcntlFlock(file.Fd(), unix.F_SETLK, &flock); err != nil {
			t.Fatal(err)
		}
		fmt.Println("locked")
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
		return
	}
	box := toolboxFixture(t)
	fixtureFile(t, box, "/var/cache/apt/archives/worker.deb", "archive", 0o644)
	if err := os.Mkdir(box.path("/var/cache/apt/archives/partial"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, box, "/var/cache/apt/archives/partial/worker.deb", "partial archive", 0o644)
	ask := toolboxAsk(&v1.HostToolboxOperation{Operation: &v1.HostToolboxOperation_Cleanup{Cleanup: &v1.ToolboxCleanup{Kind: v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_APT}}})
	edit, err := box.plan(t.Context(), ask)
	if err != nil || len(edit.plan.GetFiles()) != 1 {
		t.Fatalf("scan = %v, %v", edit.plan, err)
	}
	for _, path := range []string{"/var/lib/dpkg/lock-frontend", "/var/lib/apt/lists/lock", "/var/cache/apt/archives/lock"} {
		t.Run(path, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestToolboxAPTLock$")
			cmd.Env = append(os.Environ(), "TOOLBOX_APT_LOCK_HELPER="+box.path(path))
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = stdin.Close()
				_ = cmd.Wait()
			}()
			if ready, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || ready != "locked\n" {
				t.Fatalf("lock holder: %q, %v", ready, err)
			}
			if _, err := box.plan(t.Context(), ask); err == nil || !strings.Contains(err.Error(), "apt is running") {
				t.Fatalf("plan with lock held = %v", err)
			}
			if err := box.apply(t.Context(), edit.plan); err == nil || !strings.Contains(err.Error(), "apt is running") {
				t.Fatalf("apply with lock held = %v", err)
			}
		})
	}
	if err := box.apply(t.Context(), edit.plan); err != nil {
		t.Fatal(err)
	}
	if raw, _ := box.read("/var/cache/apt/archives/partial/worker.deb"); string(raw) != "partial archive" {
		t.Fatalf("partial download = %q", raw)
	}
}

func TestToolboxCloudInitAndDHCPOverrides(t *testing.T) {
	box := toolboxFixture(t)
	for _, dir := range []string{"/etc/cloud/cloud.cfg.d", "/etc/dhcp", "/var/lib/dhcp"} {
		if err := os.MkdirAll(box.path(dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fixtureFile(t, box, "/etc/cloud/cloud.cfg", "preserve_hostname: false\nmanage_etc_hosts: true\n", 0o644)
	fixtureFile(t, box, "/etc/hostname", "old.example\n", 0o644)
	fixtureFile(t, box, "/etc/hosts", "127.0.0.1 localhost\n127.0.1.1 old.example old alias # distro\n", 0o644)
	hostname := toolboxEdit{plan: &v1.HostToolboxPlan{}}
	if err := box.planHostname(t.Context(), &hostname, "new.example"); err != nil {
		t.Fatal(err)
	}
	if err := box.writeFiles(t.Context(), hostname); err != nil {
		t.Fatal(err)
	}
	if raw, _ := box.read(toolboxCloudConfig); string(raw) != toolboxUnitMarker+"preserve_hostname: true\nmanage_etc_hosts: false\n" {
		t.Fatalf("cloud-init override = %q", raw)
	}
	if raw, _ := box.read("/etc/hosts"); string(raw) != "127.0.0.1 localhost\n127.0.1.1 new.example alias # distro\n" {
		t.Fatalf("hosts after rename = %q", raw)
	}
	fixtureFile(t, box, "/etc/resolv.conf", "nameserver 192.0.2.53\n", 0o644)
	fixtureFile(t, box, "/var/lib/dhcp/dhclient.eth0.leases", "lease {}\n", 0o644)
	fixtureFile(t, box, "/etc/dhcp/dhclient.conf", "request routers,\n  domain-name-servers;\n", 0o644)
	dns := toolboxEdit{plan: &v1.HostToolboxPlan{}}
	if err := box.planDNS(t.Context(), &dns, []string{"192.0.2.54"}); err != nil {
		t.Fatal(err)
	}
	if len(dns.files) != 2 || dns.files[0].path != "/etc/dhcp/dhclient.conf" ||
		!strings.Contains(string(dns.files[0].next), "supersede domain-name-servers 192.0.2.54;") ||
		dns.files[1].path != "/etc/resolv.conf" {
		t.Fatalf("DHCP edits = %+v", dns.files)
	}
}

func TestToolboxSwappinessChecksSysctlConf(t *testing.T) {
	box := toolboxFixture(t)
	fixtureFile(t, box, "/proc/sys/vm/swappiness", "60\n", 0o644)
	fixtureFile(t, box, "/etc/sysctl.conf", "vm/swappiness = 20 # local override\n", 0o644)
	edit := toolboxEdit{plan: &v1.HostToolboxPlan{}}
	if err := box.planSwappiness(&edit, 35); err == nil || !strings.Contains(err.Error(), "/etc/sysctl.conf") {
		t.Fatalf("conflicting sysctl.conf = %v", err)
	}
	if err := box.planSwappiness(&edit, 20); err != nil || edit.files[0].path != swappinessPath {
		t.Fatalf("matching sysctl.conf = %v", err)
	}
}

func TestToolboxCanRemoveSwapLeftByInterruptedCreate(t *testing.T) {
	box := toolboxFixture(t)
	fixtureFile(t, box, fstabPath, "# fstab\n", 0o644)
	orphan, err := os.Create(box.path(toolboxSwapPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := orphan.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	if err := orphan.Truncate(128 * swapMib); err != nil {
		t.Fatal(err)
	}
	if err := orphan.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(box.path(toolboxSwapPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := toolboxAtomic(filepath.Join(box.dir, swapIntentName), []byte(toolboxSwapIdentity(fi)), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, box, "/proc/swaps", "Filename Type Size Used Priority\n"+toolboxSwapPath+" file 131068 0 -2\n", 0o644)
	workspace, err := os.MkdirTemp(box.path("/var/lib"), ".dockwell-swap-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "new"), []byte("left by the interrupted swap"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mkswap", "swapon", "swapoff", "findmnt", "systemctl"} {
		fixtureFile(t, box, "/usr/bin/"+name, "#!/bin/sh\nexit 0\n", 0o755)
	}
	if _, managed, _, _, _ := box.swaps(); managed != 128 {
		t.Fatalf("interrupted swap shown as %d MiB", managed)
	}
	edit := toolboxEdit{plan: &v1.HostToolboxPlan{}}
	if err := box.planSwap(&edit, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned swap workspace remains: %v", err)
	}
	if err := edit.apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(box.path(toolboxSwapPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned swap remains: %v", err)
	}
}

func TestToolboxRootServiceEnvironmentRequiresDirectValues(t *testing.T) {
	box := toolboxFixture(t)
	fixtureFile(t, box, "/usr/bin/worker", "stub", 0o755)
	spec := &v1.ToolboxService{User: "0", WorkingDirectory: "/", Argv: []string{"/usr/bin/worker"}}
	for _, name := range []string{
		"HOME", "PATH", "SHELL", "GIT_SSH_COMMAND", "GIT_CONFIG_COUNT", "SSL_CERT_FILE", "SSL_CERT_DIR",
		"CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy", "GCONV_PATH", "PYTHONSTARTUP", "RUBYOPT", "JAVA_TOOL_OPTIONS",
		"_JAVA_OPTIONS", "BASH_ENV", "ENV", "LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "DYLD_FRAMEWORK_PATH",
		"NODE_OPTIONS", "PYTHONPATH", "PERL5LIB", "TMPDIR", "XDG_CONFIG_HOME", "MALLOC_CHECK_",
	} {
		t.Run(name, func(t *testing.T) {
			spec.Environment = []*v1.ToolboxBinding{{Name: name}}
			if err := box.checkServicePaths(spec); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("root binding %s = %v", name, err)
			}
			spec.Environment[0].SealedLiteral = []byte("sealed")
			if err := box.checkServicePaths(spec); err != nil {
				t.Fatal(err)
			}
		})
	}
	spec.Environment = []*v1.ToolboxBinding{{Name: "APP_VERSION"}}
	if err := box.checkServicePaths(spec); err != nil {
		t.Fatal(err)
	}
}

func TestToolboxServiceReviewsArgumentAndInterpreterPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		path string
		mode os.FileMode
	}{
		{"python script", []string{"/usr/bin/python3", "/srv/app/worker.py"}, "/srv/app/worker.py", 0o664},
		{"option value", []string{"/usr/bin/python3", "--entry=/srv/app/worker.py"}, "/srv/app/worker.py", 0o664},
		{"shebang", []string{"/srv/app/run"}, "/usr/bin/python3", 0o775},
		{"absolute env interpreter", []string{"/srv/app/env-run"}, "/usr/bin/python3", 0o775},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := toolboxFixture(t)
			if err := os.MkdirAll(box.path("/srv/app"), 0o755); err != nil {
				t.Fatal(err)
			}
			fixtureFile(t, box, "/usr/bin/python3", "interpreter", 0o755)
			fixtureFile(t, box, "/usr/bin/env", "env", 0o755)
			fixtureFile(t, box, "/srv/app/worker.py", "worker", 0o644)
			fixtureFile(t, box, "/srv/app/run", "#!\t /usr/bin/python3 -I\n", 0o755)
			fixtureFile(t, box, "/srv/app/env-run", "#!/usr/bin/env /usr/bin/python3\n", 0o755)
			unit := toolboxUnitDir + "/dockwell-toolbox-worker.service"
			fixtureFile(t, box, "/usr/bin/systemctl", fmt.Sprintf(`#!/bin/sh
case "$1:$2" in
--version:) echo 'systemd 259' ;;
show:--property=UnitPath) echo '/etc/systemd/system' ;;
show:--property=FragmentPath) if [ -f '%s' ]; then echo '%s'; fi ;;
daemon-reload:) chmod '%o' '%s' ;;
start:--) touch '%s' ;;
esac
`, box.path(unit), unit, tc.mode, box.path(tc.path), box.path("/started")), 0o755)
			svc := &v1.ToolboxService{Name: "worker", User: "0", WorkingDirectory: "/", Argv: tc.argv, Restart: "no"}
			ask := toolboxAsk(&v1.HostToolboxOperation{Operation: &v1.HostToolboxOperation_Service{Service: svc}})
			if err := os.Chmod(box.path(tc.path), tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := box.plan(t.Context(), ask); err == nil || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("plan accepted the writable path: %v", err)
			}
			if err := os.Chmod(box.path(tc.path), tc.mode&^0o022); err != nil {
				t.Fatal(err)
			}
			plan, err := box.plan(t.Context(), ask)
			if err != nil {
				t.Fatal(err)
			}
			if err := box.apply(t.Context(), plan.plan); err == nil || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("apply accepted a path made writable during reload: %v", err)
			}
			if _, err := os.Stat(box.path("/started")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("started with a writable path: %v", err)
			}
		})
	}
}

func TestToolboxEnvironmentVerifiesDirectValue(t *testing.T) {
	const key = "a test key"
	binding := &v1.ToolboxBinding{Name: "TOKEN", Secret: true, SealedLiteral: []byte("sealed"), ValueDigest: resource.ToolboxLiteralDigest(key, "")}
	values := map[string]string{"TOKEN": "", resource.ToolboxLiteralKey("TOKEN"): key}
	ctx := resource.WithValues(t.Context(), values)
	bindings := []*v1.ToolboxBinding{binding}
	env, err := toolboxEnvironment(ctx, bindings)
	if err != nil || string(env) != toolboxUnitMarker+"TOKEN=\"\"\n" {
		t.Fatalf("reviewed empty value = %q, %v", env, err)
	}
	values["TOKEN"] = "substitute"
	if env, err := toolboxEnvironment(ctx, bindings); err == nil || env != nil {
		t.Fatal("accepted a substituted direct value")
	}
	values["TOKEN"] = ""
	delete(values, resource.ToolboxLiteralKey("TOKEN"))
	if env, err := toolboxEnvironment(ctx, bindings); err == nil || env != nil {
		t.Fatal("accepted a direct value without its verification key")
	}
}
