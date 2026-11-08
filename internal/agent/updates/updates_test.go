// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// byName finds a package in what a read listed.
func byName(ps []*dockwellv1.PackageUpdate, name string) *dockwellv1.PackageUpdate {
	for _, p := range ps {
		if p.GetName() == name {
			return p
		}
	}
	return nil
}

func TestAptSimulationOnUbuntu(t *testing.T) {
	waiting, held := parseAptSimulation(fixture(t, "apt-simulate-ubuntu.txt"))
	if len(waiting) != 8 {
		t.Fatalf("listed %d, want 8: %v", len(waiting), waiting)
	}
	ssl := byName(waiting, "libssl3t64")
	if ssl.GetInstalled() != "3.0.13-0ubuntu3.4" || ssl.GetAvailable() != "3.0.13-0ubuntu3.5" || ssl.GetArchitecture() != "amd64" {
		t.Errorf("libssl3t64 = %v", ssl)
	}
	if !ssl.GetSecurity() || ssl.GetOrigin() != "noble-security" {
		t.Errorf("libssl3t64 is from %q, security %v; want noble-security, true", ssl.GetOrigin(), ssl.GetSecurity())
	}
	if k := byName(waiting, "linux-image-6.8.0-49-generic"); k.GetInstalled() != "" || k.GetAvailable() != "6.8.0-49.49" {
		t.Errorf("a new kernel = %v, want nothing installed", k)
	}
	if s := byName(waiting, "systemd"); s.GetSecurity() || s.GetOrigin() != "noble-updates" {
		t.Errorf("systemd = %v, want noble-updates and no security fix", s)
	}
	if d := byName(waiting, "docker-ce"); d.GetOrigin() != "noble" || d.GetAvailable() != "5:27.4.0-1~ubuntu.24.04~noble" {
		t.Errorf("docker-ce = %v", d)
	}
	want := []string{"python3-distupgrade", "ubuntu-advantage-tools", "ubuntu-release-upgrader-core"}
	if fmt.Sprint(held) != fmt.Sprint(want) {
		t.Errorf("held back %v, want %v", held, want)
	}
}

func TestAptSimulationOnDebian(t *testing.T) {
	waiting, held := parseAptSimulation(fixture(t, "apt-simulate-debian.txt"))
	if len(waiting) != 2 || len(held) != 0 {
		t.Fatalf("listed %v, held %v", waiting, held)
	}
	if ssl := byName(waiting, "libssl3"); !ssl.GetSecurity() || ssl.GetOrigin() != "stable-security" {
		t.Errorf("libssl3 = %v, want a security fix from stable-security", ssl)
	}
	if tz := byName(waiting, "tzdata"); tz.GetSecurity() || tz.GetArchitecture() != "all" {
		t.Errorf("tzdata = %v", tz)
	}
}

func TestAptCacheShow(t *testing.T) {
	got := parseAptCacheShow(fixture(t, "apt-cache-show.txt"))
	if got["linux-modules-6.8.0-49-generic:amd64"] != "Linux kernel extra modules for version 6.8.0" ||
		got["linux-image-6.8.0-49-generic:amd64"] != "Signed kernel image generic" {
		t.Errorf("got %v", got)
	}
}

func TestDnfCheckUpdate(t *testing.T) {
	waiting := parseDnfCheckUpdate(fixture(t, "dnf-check-update.txt"))
	var names []string
	for _, p := range waiting {
		names = append(names, p.GetName()+"."+p.GetArchitecture()+"="+p.GetAvailable()+"@"+p.GetOrigin())
	}
	want := "[kernel.x86_64=5.14.0-503.15.1.el9_5@baseos openssl.x86_64=1:3.2.2-6.el9_5@baseos openssl-libs.x86_64=1:3.2.2-6.el9_5@baseos " +
		"python3-dnf-plugin-versionlock-and-a-long-name.noarch=4.3.0-16.el9@baseos vim-minimal.x86_64=2:8.2.2637-21.el9@baseos]"
	if fmt.Sprint(names) != want {
		t.Errorf("got  %v\nwant %s", names, want)
	}
}

func TestDnfUpdateinfo(t *testing.T) {
	got := parseDnfUpdateinfo(fixture(t, "dnf-updateinfo.txt"))
	if a := got["kernel.x86_64"]; a.id != "ALSA-2024:10232" || a.severity != "Important" {
		t.Errorf("kernel's advisory = %+v", a)
	}
	if a := got["openssl-libs.x86_64"]; a.id != "ALSA-2024:9999" || a.severity != "Critical" {
		t.Errorf("openssl-libs's advisory = %+v, want the most severe", a)
	}
	if _, ok := got["vim-minimal.x86_64"]; ok {
		t.Error("vim-minimal has no advisory")
	}
}

func TestSplitNEVRA(t *testing.T) {
	for in, want := range map[string]string{
		"openssl-libs-1:3.2.2-6.el9_5.x86_64":     "openssl-libs x86_64",
		"kernel-5.14.0-503.15.1.el9_5.x86_64":     "kernel x86_64",
		"python3-dnf-4.14.0-17.el9.alma.1.noarch": "python3-dnf noarch",
	} {
		name, arch, ok := splitNEVRA(in)
		if !ok || name+" "+arch != want {
			t.Errorf("splitNEVRA(%q) = %q %q %v, want %q", in, name, arch, ok, want)
		}
	}
	if _, _, ok := splitNEVRA("nonsense"); ok {
		t.Error("read nonsense as a package")
	}
}

func TestApkVersion(t *testing.T) {
	waiting := parseApkVersion(fixture(t, "apk-version.txt"))
	if len(waiting) != 3 {
		t.Fatalf("listed %v", waiting)
	}
	if p := byName(waiting, "py3-foo-bar"); p.GetInstalled() != "1.2.3-r0" || p.GetAvailable() != "1.2.4-r0" {
		t.Errorf("py3-foo-bar = %v", p)
	}
	info := parseApkInfo(fixture(t, "apk-info.txt"))
	if info["busybox"] != "Size optimized toolbox of many common UNIX utilities" || info["py3-foo-bar"] != "A library that does foo and bar" {
		t.Errorf("descriptions = %v", info)
	}
}

func TestEffects(t *testing.T) {
	apt, dnf, apk := dockwellv1.PackageManager_PACKAGE_MANAGER_APT, dockwellv1.PackageManager_PACKAGE_MANAGER_DNF, dockwellv1.PackageManager_PACKAGE_MANAGER_APK
	reboot, docker, none := dockwellv1.PackageEffect_PACKAGE_EFFECT_REBOOT, dockwellv1.PackageEffect_PACKAGE_EFFECT_DOCKER_RESTART, dockwellv1.PackageEffect_PACKAGE_EFFECT_UNSPECIFIED
	for _, c := range []struct {
		m    dockwellv1.PackageManager
		name string
		want dockwellv1.PackageEffect
	}{
		{apt, "linux-image-6.8.0-49-generic", reboot},
		{apt, "linux-image-virtual", reboot},
		{apt, "libc6", reboot},
		{apt, "systemd", none}, // Debian and Ubuntu re-execute it
		{dnf, "systemd", reboot},
		{apt, "linux-libc-dev", none},
		{apt, "openssl", none},
		{apt, "docker-ce", docker},
		{apt, "docker-ce-cli", none},
		{dnf, "kernel", reboot},
		{dnf, "kernel-headers", none},
		{dnf, "glibc", reboot},
		{apk, "linux-lts", reboot},
		{apk, "linux-pam", none},
		{apk, "docker-engine", docker},
	} {
		if got := effectOf(c.m, c.name); got != c.want {
			t.Errorf("effectOf(%v, %q) = %v, want %v", c.m, c.name, got, c.want)
		}
	}
}

func TestKernels(t *testing.T) {
	for _, c := range []struct {
		running string
		dirs    []string
		want    bool
	}{
		{"6.8.0-48-generic", []string{"6.8.0-48-generic", "6.8.0-49-generic"}, true},
		{"6.8.0-49-generic", []string{"6.8.0-48-generic", "6.8.0-49-generic"}, false},
		{"6.8.0-48-generic", []string{"6.8.0-48-generic", "6.11.0-9-lowlatency"}, false},
		{"6.1.0-26-amd64", []string{"6.1.0-26-amd64", "6.1.0-27-amd64"}, true},
		{"6.6.58-0-lts", []string{"6.6.62-0-lts"}, true},
		{"5.14.0-427.13.1.el9_4.x86_64", []string{"5.14.0-427.13.1.el9_4.x86_64", "5.14.0-503.15.1.el9_5.x86_64"}, true},
		{"5.14.0-503.15.1.el9_5.x86_64", []string{"5.14.0-427.13.1.el9_4.x86_64", "5.14.0-503.15.1.el9_5.x86_64"}, false},
		// A removed kernel leaves its folder with no modules in it.
		{"6.8.0-48-generic", []string{"6.8.0-48-generic", "!6.8.0-50-generic"}, false},
	} {
		root := t.TempDir()
		write(t, root, "proc/sys/kernel/osrelease", c.running+"\n", 0o644)
		for _, d := range c.dirs {
			if name, empty := strings.CutPrefix(d, "!"); empty {
				if err := os.MkdirAll(filepath.Join(root, "lib", "modules", name), 0o755); err != nil {
					t.Fatal(err)
				}
				continue
			}
			write(t, root, filepath.Join("lib", "modules", d, "modules.dep"), "", 0o644)
		}
		r := New(Options{Root: root})
		if got := r.newerKernel(); got != c.want {
			t.Errorf("running %s with %v: newer kernel = %v, want %v", c.running, c.dirs, got, c.want)
		}
	}
	if New(Options{Root: t.TempDir()}).newerKernel() {
		t.Error("a server with no kernels to read says it needs a reboot")
	}
}

func write(t *testing.T, root, name, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// stand is a server's files with stand-ins for its tools, which print the
// fixtures and note how they were called.
type stand struct {
	t    *testing.T
	root string
}

func newStand(t *testing.T, release string) *stand {
	t.Helper()
	s := &stand{t: t, root: t.TempDir()}
	write(t, s.root, "etc/os-release", release, 0o644)
	return s
}

// tool puts a stand-in at path: a shell script whose body runs with the call
// noted, and with $FIX the testdata folder and $ROOT the stand.
func (s *stand) tool(path, body string) {
	s.t.Helper()
	fix, err := filepath.Abs("testdata")
	if err != nil {
		s.t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nFIX=%q\nROOT=%q\necho \"$(basename \"$0\") $*\" >> \"$ROOT/calls\"\n%s\n", fix, s.root, body)
	write(s.t, s.root, path, script, 0o755)
}

// calls is how the stand-ins were called, one line each.
func (s *stand) calls() []string {
	b, _ := os.ReadFile(filepath.Join(s.root, "calls"))
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), " ", "_"))
}

func (s *stand) called(prefix string) int {
	n := 0
	for _, c := range s.calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

const ubuntu = `PRETTY_NAME="Ubuntu 24.04.1 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
ID=ubuntu
ID_LIKE=debian
`

// ubuntuStand is an Ubuntu server whose apt has the fixture's updates, with its
// lists refreshed at listed.
func ubuntuStand(t *testing.T, listed time.Time) *stand {
	s := newStand(t, ubuntu)
	s.tool(aptGet, `case "$*" in
*" update") touch "$ROOT/var/lib/apt/lists/archive.ubuntu.com_dists_noble_InRelease"; exit "$(cat "$ROOT/update-exit" 2>/dev/null || echo 0)" ;;
*" upgrade") cat "$FIX/apt-simulate-ubuntu.txt" ;;
*) echo "unexpected: $*" >&2; exit 2 ;;
esac`)
	s.tool(dpkgQuery, `cat "$FIX/dpkg-query.txt"; exit 1`)
	s.tool(aptCache, `cat "$FIX/apt-cache-show.txt"`)
	lists := "var/lib/apt/lists/archive.ubuntu.com_dists_noble_InRelease"
	write(t, s.root, lists, "", 0o644)
	if err := os.Chtimes(filepath.Join(s.root, lists), listed, listed); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReadOnUbuntu(t *testing.T) {
	s := ubuntuStand(t, time.Now().Add(-2*time.Hour))
	write(t, s.root, "run/reboot-required", "*** System restart required ***\n", 0o644)
	write(t, s.root, "run/reboot-required.pkgs", "linux-base\nlinux-image-6.8.0-48-generic\nlinux-base\n", 0o644)
	write(t, s.root, "usr/bin/unattended-upgrade", "", 0o755)
	write(t, s.root, "etc/apt/apt.conf.d/20auto-upgrades", "APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n", 0o644)
	live := 0
	r := New(Options{Root: s.root, LiveRestore: func(context.Context) bool { live++; return true }})
	got, err := r.Read(t.Context(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetManager() != dockwellv1.PackageManager_PACKAGE_MANAGER_APT || !got.GetMarksSecurity() || got.GetUnavailable() != "" {
		t.Fatalf("read %v", got)
	}
	if s.called("apt-get_-q_-o_DPkg") != 0 {
		t.Errorf("refreshed lists two hours old: %v", s.calls())
	}
	ps := got.GetPackages()
	if len(ps) != 8 {
		t.Fatalf("listed %d", len(ps))
	}
	// Security fixes come first, by name.
	if ps[0].GetName() != "libssh-4" || !ps[0].GetSecurity() || ps[len(ps)-1].GetName() != "systemd" {
		t.Errorf("in the order %v", ps)
	}
	if p := byName(ps, "libssl3t64"); p.GetSummary() != "Secure Sockets Layer toolkit - shared libraries" {
		t.Errorf("libssl3t64's description = %q", p.GetSummary())
	}
	if p := byName(ps, "linux-image-6.8.0-49-generic"); p.GetSummary() != "Signed kernel image generic" || p.GetEffect() != dockwellv1.PackageEffect_PACKAGE_EFFECT_REBOOT {
		t.Errorf("the new kernel = %v", p)
	}
	if p := byName(ps, "docker-ce"); p.GetEffect() != dockwellv1.PackageEffect_PACKAGE_EFFECT_DOCKER_RESTART || !got.GetDockerLiveRestore() || live != 1 {
		t.Errorf("docker-ce = %v, live restore %v asked %d times", p, got.GetDockerLiveRestore(), live)
	}
	if !got.GetRebootRequired() || fmt.Sprint(got.GetRebootPackages()) != "[linux-base linux-image-6.8.0-48-generic]" {
		t.Errorf("reboot %v for %v", got.GetRebootRequired(), got.GetRebootPackages())
	}
	if a := got.GetAutomatic(); a.GetTool() != "unattended-upgrades" || !a.GetOn() {
		t.Errorf("automatic = %v", a)
	}
	if got.GetListsTime().AsTime().Before(time.Now().Add(-3*time.Hour)) || got.GetReadTime() == nil {
		t.Errorf("lists at %v, read at %v", got.GetListsTime(), got.GetReadTime())
	}
}

func TestListsRefreshWhenADayOld(t *testing.T) {
	s := ubuntuStand(t, time.Now().Add(-25*time.Hour))
	var said []string
	ctx := resource.WithProgress(t.Context(), func(m string) { said = append(said, m) })
	r := New(Options{Root: s.root})
	got, err := r.Read(ctx, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if s.called("apt-get_-q_-o_DPkg::Lock::Timeout=120_update") != 1 || fmt.Sprint(said) != "["+Refreshing+"]" {
		t.Errorf("calls %v, said %v", s.calls(), said)
	}
	if time.Since(got.GetListsTime().AsTime()) > time.Minute || got.GetRefreshError() != "" {
		t.Errorf("lists at %v, refresh error %q", got.GetListsTime().AsTime(), got.GetRefreshError())
	}
	// Without refresh, as when nobody asked for it, they're read as they are.
	s2 := ubuntuStand(t, time.Now().Add(-25*time.Hour))
	if _, err := New(Options{Root: s2.root}).Read(t.Context(), false, false); err != nil || s2.called("apt-get_-q_-o_DPkg") != 0 {
		t.Errorf("read without refresh: %v, calls %v", err, s2.calls())
	}
}

func TestCheckNowAtMostOnceAMinute(t *testing.T) {
	s := ubuntuStand(t, time.Now())
	now := time.Now()
	r := New(Options{Root: s.root, Now: func() time.Time { return now }})
	for range 3 {
		if _, err := r.Read(t.Context(), true, true); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.called("apt-get_-q_-o_DPkg::Lock::Timeout=120_update"); n != 1 {
		t.Errorf("refreshed %d times in a minute, want once", n)
	}
	now = now.Add(ForceEvery + time.Second)
	if _, err := r.Read(t.Context(), true, true); err != nil {
		t.Fatal(err)
	}
	if n := s.called("apt-get_-q_-o_DPkg::Lock::Timeout=120_update"); n != 2 {
		t.Errorf("refreshed %d times, want twice a minute apart", n)
	}
}

func TestARefreshThatFailsReadsTheListsAsTheyAre(t *testing.T) {
	listed := time.Now().Add(-30 * time.Hour).Truncate(time.Second)
	s := ubuntuStand(t, listed)
	s.tool(aptGet, `case "$*" in
*" update") echo "E: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/noble/InRelease  Temporary failure resolving 'archive.ubuntu.com'" >&2; exit 100 ;;
*" upgrade") cat "$FIX/apt-simulate-ubuntu.txt" ;;
esac`)
	got, err := New(Options{Root: s.root}).Read(t.Context(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.GetRefreshError(), "Temporary failure resolving") || !strings.Contains(got.GetRefreshError(), "status 100") {
		t.Errorf("refresh error = %q", got.GetRefreshError())
	}
	if len(got.GetPackages()) != 8 || !got.GetListsTime().AsTime().Equal(listed) {
		t.Errorf("read %d from lists at %v, want 8 from %v", len(got.GetPackages()), got.GetListsTime().AsTime(), listed)
	}
}

func TestReadOnAlmaLinux(t *testing.T) {
	s := newStand(t, "NAME=\"AlmaLinux\"\nID=\"almalinux\"\nID_LIKE=\"rhel centos fedora\"\nPRETTY_NAME=\"AlmaLinux 9.5 (Teal Serval)\"\n")
	s.tool(dnfPath, `case "$*" in
*makecache) mkdir -p "$ROOT/var/cache/dnf/baseos-1/repodata"; touch "$ROOT/var/cache/dnf/baseos-1/repodata/repomd.xml" ;;
*check-update) cat "$FIX/dnf-check-update.txt"; exit 100 ;;
*"updateinfo list --security") cat "$FIX/dnf-updateinfo.txt" ;;
*"needs-restarting -r") printf 'Core libraries or services have been updated since boot-up:\n  * kernel\n  * glibc\n\nReboot is required to fully utilize these updates.\nMore information: https://access.redhat.com/solutions/27943\n'; exit 1 ;;
*) exit 2 ;;
esac`)
	s.tool(rpmPath, `cat "$FIX/rpm-query.txt"; exit 1`)
	write(t, s.root, "usr/bin/dnf-automatic", "", 0o755)
	write(t, s.root, "etc/systemd/system/timers.target.wants/dnf-automatic.timer", "", 0o644)
	write(t, s.root, "etc/dnf/automatic.conf", "[commands]\napply_updates = no\n", 0o644)
	got, err := New(Options{Root: s.root}).Read(t.Context(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetManager() != dockwellv1.PackageManager_PACKAGE_MANAGER_DNF || !got.GetMarksSecurity() || s.called("dnf_-q_-y_--refresh_makecache") != 1 {
		t.Fatalf("read %v, calls %v", got, s.calls())
	}
	ps := got.GetPackages()
	k := byName(ps, "kernel")
	if k.GetInstalled() != "5.14.0-503.11.1.el9_5" || !k.GetSecurity() || k.GetAdvisory() != "ALSA-2024:10232" || k.GetEffect() != dockwellv1.PackageEffect_PACKAGE_EFFECT_REBOOT {
		t.Errorf("kernel = %v, want it from the newest installed, a security fix that reboots", k)
	}
	if p := byName(ps, "openssl"); p.GetInstalled() != "1:3.2.2-5.el9_5" || p.GetSeverity() != "Moderate" || p.GetSummary() == "" {
		t.Errorf("openssl = %v", p)
	}
	if p := byName(ps, "python3-dnf-plugin-versionlock-and-a-long-name"); p.GetInstalled() != "" || p.GetArchitecture() != "noarch" || p.GetSecurity() {
		t.Errorf("a new package = %v", p)
	}
	if !got.GetRebootRequired() || fmt.Sprint(got.GetRebootPackages()) != "[kernel glibc]" {
		t.Errorf("reboot %v for %v", got.GetRebootRequired(), got.GetRebootPackages())
	}
	if a := got.GetAutomatic(); a.GetTool() != "dnf-automatic" || a.GetOn() {
		t.Errorf("automatic = %v, want dnf-automatic that only downloads", a)
	}
}

func TestReadOnAlpine(t *testing.T) {
	s := newStand(t, "NAME=\"Alpine Linux\"\nID=alpine\nVERSION_ID=3.20.3\nPRETTY_NAME=\"Alpine Linux v3.20\"\n")
	s.tool(apkPath, `case "$*" in
"-q update") mkdir -p "$ROOT/var/cache/apk"; touch "$ROOT/var/cache/apk/APKINDEX.1a2b3c4d.tar.gz" ;;
"version -l <") cat "$FIX/apk-version.txt" ;;
"info -d -- "*) cat "$FIX/apk-info.txt" ;;
*) exit 2 ;;
esac`)
	write(t, s.root, "proc/sys/kernel/osrelease", "6.6.58-0-virt\n", 0o644)
	write(t, s.root, "lib/modules/6.6.62-0-virt/modules.dep", "", 0o644)
	got, err := New(Options{Root: s.root}).Read(t.Context(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetMarksSecurity() || len(got.GetPackages()) != 3 || s.called("apk_-q_update") != 1 {
		t.Fatalf("read %v, calls %v", got, s.calls())
	}
	if p := byName(got.GetPackages(), "openssl"); p.GetSummary() != "Toolkit for Transport Layer Security (TLS)" || p.GetSecurity() {
		t.Errorf("openssl = %v", p)
	}
	if !got.GetRebootRequired() {
		t.Error("a newer kernel installed isn't a reboot needed")
	}
	if a := got.GetAutomatic(); a.GetTool() != "" {
		t.Errorf("automatic = %v", a)
	}
}

func TestWhatCantBeRead(t *testing.T) {
	arch := newStand(t, "NAME=\"Arch Linux\"\nID=arch\nPRETTY_NAME=\"Arch Linux\"\n")
	got, err := New(Options{Root: arch.root}).Read(t.Context(), true, false)
	if err != nil || got.GetUnavailable() != "Dockwell reads updates with apt, dnf 4 or apk, and Arch Linux uses none of them" {
		t.Errorf("on Arch: %v, %v", got, err)
	}
	bare := newStand(t, ubuntu)
	got, err = New(Options{Root: bare.root}).Read(t.Context(), true, false)
	if err != nil || got.GetUnavailable() != "Dockwell reads updates on Ubuntu 24.04.1 LTS with apt-get, and /usr/bin/apt-get isn't there" {
		t.Errorf("with no apt-get: %v, %v", got, err)
	}
	got, err = New(Options{Root: ubuntuStand(t, time.Now()).root, Unavailable: "not in tests"}).Read(t.Context(), true, true)
	if err != nil || got.GetUnavailable() != "not in tests" {
		t.Errorf("turned off: %v, %v", got, err)
	}
	broken := ubuntuStand(t, time.Now())
	broken.tool(aptGet, `echo "E: The package lists or status file could not be parsed or opened." >&2; exit 100`)
	if _, err := New(Options{Root: broken.root}).Read(t.Context(), true, false); err == nil ||
		err.Error() != "Reading what apt would upgrade ended with status 100: E: The package lists or status file could not be parsed or opened." {
		t.Errorf("a read that fails: %v", err)
	}
}

func TestOneRunAtATime(t *testing.T) {
	s := ubuntuStand(t, time.Now())
	s.tool(aptGet, `case "$*" in
*" upgrade") echo started >> "$ROOT/runs"; sleep 0.3; cat "$FIX/apt-simulate-ubuntu.txt"; echo ended >> "$ROOT/runs" ;;
esac`)
	r := New(Options{Root: s.root})
	done := make(chan error, 2)
	for range 2 {
		go func() { _, err := r.Read(t.Context(), false, false); done <- err }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(s.root, "runs"))
	if got := strings.Fields(string(b)); fmt.Sprint(got) != "[started ended started ended]" {
		t.Errorf("runs went %v, want one after the other", got)
	}
}

func TestARunOutOfTimeStopsWithWhatItStarted(t *testing.T) {
	s := newStand(t, ubuntu)
	// It starts a child that outlives it, as a maintainer's script can, and
	// ignores SIGTERM, so only SIGKILL ends it.
	s.tool(aptGet, `trap '' TERM; sleep 30 & echo $! > "$ROOT/child"; echo "W: Waiting for cache lock" >&2; wait`)
	r := New(Options{Root: s.root})
	r.grace = 200 * time.Millisecond
	start := time.Now()
	_, err := r.run(t.Context(), "Refreshing apt's package lists", aptGet, 300*time.Millisecond, "-q", "update")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "Refreshing apt's package lists didn't finish within") ||
		!strings.Contains(err.Error(), "after saying: W: Waiting for cache lock") {
		t.Errorf("err = %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v to stop", took)
	}
	b, err := os.ReadFile(filepath.Join(s.root, "child"))
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscan(string(b), &pid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("its child %d still runs", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestARunThatPrintsTooMuchFails(t *testing.T) {
	s := newStand(t, ubuntu)
	s.tool(aptGet, `head -c 5000000 /dev/zero`)
	r := New(Options{Root: s.root})
	if _, err := r.run(t.Context(), "Reading what apt would upgrade", aptGet, 10*time.Second, "-q", "--simulate", "upgrade"); !errors.Is(err, errTooMuch) {
		t.Errorf("err = %v", err)
	}
}

func TestARunGetsNothingOfTheAgentsEnvironment(t *testing.T) {
	t.Setenv("DOCKWELL_SECRET_TEST", "leaked")
	s := newStand(t, ubuntu)
	s.tool(aptGet, `env | sort`)
	res, err := New(Options{Root: s.root}).run(t.Context(), "Printing the environment", aptGet, 10*time.Second, "x")
	if err != nil {
		t.Fatal(err)
	}
	got := string(res.out)
	if strings.Contains(got, "leaked") || !strings.Contains(got, "LC_ALL=C") || !strings.Contains(got, "DEBIAN_FRONTEND=noninteractive") {
		t.Errorf("environment:\n%s", got)
	}
}

// A refresh that failed holds off the next one a read would make for an hour, as a
// mirror that's down is down for a while, and says why meanwhile. Check now
// doesn't wait.
func TestAFailedRefreshHoldsOff(t *testing.T) {
	listed := time.Now().Add(-30 * time.Hour)
	s := ubuntuStand(t, listed)
	// It fails, and leaves the lists as they were, as apt does.
	s.tool(aptGet, `case "$*" in
*" update") echo "E: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/noble/InRelease" >&2; exit 100 ;;
*" upgrade") cat "$FIX/apt-simulate-ubuntu.txt" ;;
esac`)
	now := time.Now()
	r := New(Options{Root: s.root, Now: func() time.Time { return now }})
	refreshes := func() int { return s.called("apt-get_-q_-o_DPkg::Lock::Timeout=120_update") }
	got, err := r.Read(t.Context(), true, false)
	if err != nil || refreshes() != 1 || !strings.Contains(got.GetRefreshError(), "Refreshing apt's package lists ended with status 100") {
		t.Fatalf("first read: %v, %d refreshes, refresh error %q", err, refreshes(), got.GetRefreshError())
	}
	now = now.Add(10 * time.Minute)
	got, err = r.Read(t.Context(), true, false)
	if err != nil || refreshes() != 1 || got.GetRefreshError() == "" {
		t.Errorf("10 min later: %v, %d refreshes, want still 1 and the error said: %q", err, refreshes(), got.GetRefreshError())
	}
	if _, err := r.Read(t.Context(), true, true); err != nil || refreshes() != 2 {
		t.Errorf("Check now within the hour: %v, %d refreshes, want 2", err, refreshes())
	}
	now = now.Add(RetryAfter + time.Minute)
	if _, err := r.Read(t.Context(), true, false); err != nil || refreshes() != 3 {
		t.Errorf("past the hour: %v, %d refreshes, want 3", err, refreshes())
	}
}

// When the agent last refreshed, and last tried to, outlives a restart, as apt
// gives a list the repository's own time, which can be well before it was fetched.
// Debian's daily stamp counts as a refresh too.
func TestRefreshesOutliveARestart(t *testing.T) {
	listed := time.Now().Add(-30 * time.Hour)
	s := ubuntuStand(t, listed)
	// It refreshes, and the list keeps the repository's time.
	s.tool(aptGet, `case "$*" in
*" update") exit 0 ;;
*" upgrade") cat "$FIX/apt-simulate-ubuntu.txt" ;;
esac`)
	dir := t.TempDir()
	refreshes := func() int { return s.called("apt-get_-q_-o_DPkg::Lock::Timeout=120_update") }
	got, err := New(Options{Root: s.root, StateDir: dir}).Read(t.Context(), true, false)
	if err != nil || refreshes() != 1 || time.Since(got.GetListsTime().AsTime()) > time.Minute {
		t.Fatalf("first read: %v, %d refreshes, lists at %v", err, refreshes(), got.GetListsTime().AsTime())
	}
	if _, err := New(Options{Root: s.root, StateDir: dir}).Read(t.Context(), true, false); err != nil || refreshes() != 1 {
		t.Errorf("after a restart: %v, %d refreshes, want still 1", err, refreshes())
	}

	debian := ubuntuStand(t, listed)
	write(t, debian.root, "var/lib/apt/periodic/update-stamp", "", 0o644)
	if _, err := New(Options{Root: debian.root}).Read(t.Context(), true, false); err != nil || debian.called("apt-get_-q_-o_DPkg") != 0 {
		t.Errorf("with Debian's stamp fresh: %v, calls %v", err, debian.calls())
	}
}

// Fedora's dnf is dnf 5, whose output isn't dnf 4's, so neither it nor any server
// whose dnf leads to dnf 5 is read, rather than read wrong.
func TestNoUpdatesReadWithDnf5(t *testing.T) {
	fedora := newStand(t, "NAME=\"Fedora Linux\"\nID=fedora\nPRETTY_NAME=\"Fedora Linux 41 (Server Edition)\"\n")
	fedora.tool(dnfPath, `exit 0`)
	got, err := New(Options{Root: fedora.root}).Read(t.Context(), true, false)
	if err != nil || got.GetUnavailable() != "Dockwell reads updates with apt, dnf 4 or apk, and Fedora Linux 41 (Server Edition) uses none of them" {
		t.Errorf("on Fedora: %v, %v", got, err)
	}
	alma := newStand(t, "NAME=\"AlmaLinux\"\nID=\"almalinux\"\nPRETTY_NAME=\"AlmaLinux 10.0\"\n")
	alma.tool("/usr/bin/dnf5", `exit 0`)
	if err := os.Symlink("dnf5", filepath.Join(alma.root, "usr", "bin", "dnf")); err != nil {
		t.Fatal(err)
	}
	got, err = New(Options{Root: alma.root}).Read(t.Context(), true, false)
	if err != nil || got.GetUnavailable() != "Dockwell reads updates with dnf 4, and AlmaLinux 10.0 has dnf 5, which it doesn't read yet" {
		t.Errorf("with dnf 5: %v, %v", got, err)
	}
	if alma.called("") != 0 {
		t.Errorf("ran %v", alma.calls())
	}
}

// check-update is read a line at a time: a long name's line joins the next, and a
// line of anything else is passed over without moving what follows.
func TestDnfCheckUpdateLineByLine(t *testing.T) {
	out := []byte(`
less.x86_64                          590-6.el9                          baseos
Security: kernel-core-5.14.0-503.15.1.el9_5.x86_64 is an installed security update
a-very-long-package-name-that-wraps-in-dnf.noarch
                                     1.2-3.el9                          appstream
stray
tzdata.noarch                        2026f-1.el9_8                      baseos
`)
	var names []string
	for _, p := range parseDnfCheckUpdate(out) {
		names = append(names, p.GetName()+"="+p.GetAvailable()+"@"+p.GetOrigin())
	}
	if want := "[less=590-6.el9@baseos a-very-long-package-name-that-wraps-in-dnf=1.2-3.el9@appstream tzdata=2026f-1.el9_8@baseos]"; fmt.Sprint(names) != want {
		t.Errorf("got  %v\nwant %s", names, want)
	}
}

// A tool that exits leaving a child with its output open doesn't hold the read,
// and the child goes with the run.
func TestARunEndsWhatItsToolLeft(t *testing.T) {
	s := newStand(t, ubuntu)
	s.tool(aptGet, `sleep 30 & echo $! > "$ROOT/child"; exit 0`)
	r := New(Options{Root: s.root})
	r.grace = 200 * time.Millisecond
	start := time.Now()
	if _, err := r.run(t.Context(), "Reading what apt would upgrade", aptGet, 10*time.Second, "x"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v", took)
	}
	b, err := os.ReadFile(filepath.Join(s.root, "child"))
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscan(string(b), &pid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the child %d it left still runs", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
