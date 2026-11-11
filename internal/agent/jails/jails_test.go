// SPDX-License-Identifier: AGPL-3.0-only

package jails

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/resource"
)

// stand is a server's files with a fail2ban-client that notes each run, and
// refuses -t while refuse is there, and reload while noreload is.
type stand struct {
	root string
	t    *testing.T
}

func newStand(t *testing.T) stand {
	t.Helper()
	s := stand{root: t.TempDir(), t: t}
	if err := os.MkdirAll(filepath.Join(s.root, "etc", "fail2ban", "jail.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(s.root, "usr", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
d=$(dirname "$0")/../..
echo "$*" >> "$d/runs"
case "$1" in
-t) if [ -e "$d/refuse" ]; then echo "ERROR  No section: 'Definition'" >&2; exit 255; fi; echo "OK: configuration test is successful" ;;
ping) if [ -e "$d/stopped" ]; then echo "Failed to access socket path" >&2; exit 255; fi; echo "Server replied: pong" ;;
reload) if [ -e "$d/noreload" ]; then echo "ERROR  reload failed" >&2; exit 255; fi; echo OK ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "fail2ban-client"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s stand) manager() *Manager {
	return &Manager{
		Dir: filepath.Join(s.root, "agent"), Root: s.root, Tools: &hosttool.Runner{Root: s.root},
		Control: func(context.Context) ([]string, error) { return []string{"203.0.113.10"}, nil },
	}
}

func (s stand) file() string {
	b, err := os.ReadFile(filepath.Join(s.root, File))
	if os.IsNotExist(err) {
		return "(none)"
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return string(b)
}

func (s stand) runs() string {
	b, _ := os.ReadFile(filepath.Join(s.root, "runs"))
	return strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", "; ")
}

func (s stand) mark(name string, on bool) {
	p := filepath.Join(s.root, name)
	if on {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			s.t.Fatal(err)
		}
		return
	}
	_ = os.Remove(p)
}

var sshd = &resourcev1.Jail{Filter: "sshd", Backend: "systemd", Ports: []uint32{22, 2222}, MaxRetry: 3, FindTime: "10m", BanTime: "1h", Ignore: []string{"198.51.100.0/24"}}

// A jail is written whole into Dockwell's own file, checked with -t, then
// loaded, ignoring the server's loopback and the control server; a restart
// reads it back; removing the last jail removes the file.
func TestAJailIsWrittenCheckedAndLoaded(t *testing.T) {
	s := newStand(t)
	m := s.manager()
	if err := m.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(t.Context(), "sshd", sshd); err != nil {
		t.Fatal(err)
	}
	want := `
[sshd]
enabled = true
filter = sshd
backend = systemd
port = 22,2222
maxretry = 3
findtime = 600
bantime = 3600
ignoreip = 127.0.0.1/8 ::1 203.0.113.10 198.51.100.0/24
`
	if got := s.file(); !strings.HasSuffix(got, want) || !strings.HasPrefix(got, "# Dockwell writes this file whole") {
		t.Errorf("the file\n%s\nwant it to end\n%s", got, want)
	}
	if got := s.runs(); got != "-t; ping; reload" {
		t.Errorf("fail2ban-client ran %q", got)
	}
	again := s.manager()
	if j, err := again.Jail(t.Context(), "sshd"); err != nil || j.GetMaxRetry() != 3 || j.GetBanTime() != "1h" {
		t.Errorf("after a restart the jail reads %v: %v", j, err)
	}
	forGood := &resourcev1.Jail{Filter: "nginx-http-auth", LogPaths: []string{"/var/log/nginx/error.log", "/var/log/nginx/other.log"}, BanForGood: true}
	if err := again.Put(t.Context(), "nginx-http-auth", forGood); err != nil {
		t.Fatal(err)
	}
	if got := s.file(); !strings.Contains(got, "[nginx-http-auth]\nenabled = true\nfilter = nginx-http-auth\nlogpath = /var/log/nginx/error.log\n          /var/log/nginx/other.log\nbantime = -1\n") ||
		strings.Index(got, "[nginx-http-auth]") > strings.Index(got, "[sshd]") {
		t.Errorf("a second jail, in name order:\n%s", got)
	}
	for _, name := range []string{"sshd", "nginx-http-auth"} {
		if err := again.Delete(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.file(); got != "(none)" {
		t.Errorf("with no jails left the file reads %q", got)
	}
}

// A file -t refuses, or a reload that fails, leaves fail2ban as it was: the
// old file goes back, and the jail isn't recorded.
func TestAJailFail2banRefusesLeavesItAsItWas(t *testing.T) {
	s := newStand(t)
	m := s.manager()
	if err := m.Put(t.Context(), "sshd", sshd); err != nil {
		t.Fatal(err)
	}
	before := s.file()
	s.mark("refuse", true)
	changed := &resourcev1.Jail{Filter: "sshd", Backend: "systemd", MaxRetry: 9}
	err := m.Put(t.Context(), "sshd", changed)
	if err == nil || !strings.Contains(err.Error(), "fail2ban-client -t refused the jails, so fail2ban stays as it was") || !strings.Contains(err.Error(), "No section") {
		t.Errorf("a file -t refuses: %v", err)
	}
	if s.file() != before {
		t.Errorf("after -t refused, the file reads\n%s", s.file())
	}
	s.mark("refuse", false)
	s.mark("noreload", true)
	if err := m.Put(t.Context(), "sshd", changed); err == nil || !strings.Contains(err.Error(), "didn't reload") {
		t.Errorf("a reload that fails: %v", err)
	}
	if s.file() != before {
		t.Errorf("after the reload failed, the file reads\n%s", s.file())
	}
	if j, _ := m.Jail(t.Context(), "sshd"); j.GetMaxRetry() != 3 {
		t.Errorf("the jail reads %v, want it as it was", j)
	}
}

// A fail2ban that isn't running takes the file as it starts: the step says so,
// and nothing reloads.
func TestAJailForAStoppedFail2banLoadsAsItStarts(t *testing.T) {
	s := newStand(t)
	s.mark("stopped", true)
	var said string
	ctx := resource.WithOutcome(t.Context(), func(o resource.Outcome) { said = o.Note })
	if err := s.manager().Put(ctx, "sshd", sshd); err != nil {
		t.Fatal(err)
	}
	if said != "fail2ban isn't running, so the jail loads as it starts." || strings.Contains(s.runs(), "reload") {
		t.Errorf("said %q, ran %q", said, s.runs())
	}
}

// A server without fail2ban, or without its jail.d folder, says so.
func TestAServerWithoutFail2banSaysSo(t *testing.T) {
	none := &Manager{Root: t.TempDir(), Tools: &hosttool.Runner{Root: t.TempDir()}}
	if err := none.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "fail2ban isn't installed") {
		t.Errorf("no fail2ban: %v", err)
	}
	s := newStand(t)
	if err := os.RemoveAll(filepath.Join(s.root, "etc", "fail2ban", "jail.d")); err != nil {
		t.Fatal(err)
	}
	if err := s.manager().Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "jail.d") {
		t.Errorf("no jail.d: %v", err)
	}
}

// The control server's addresses come from its URL: an address as it is, a
// name as it resolves, loopback left out.
func TestControlAddresses(t *testing.T) {
	names := func(_ context.Context, host string) ([]string, error) {
		switch host {
		case "control.example.com":
			return []string{"2001:db8::10", "203.0.113.10", "203.0.113.10"}, nil
		case "localhost":
			return []string{"127.0.0.1", "::1"}, nil
		}
		return nil, errors.New("no such host")
	}
	for url, want := range map[string]string{
		"https://203.0.113.10:7601":          "[203.0.113.10]",
		"https://[2001:db8::10]:7601":        "[2001:db8::10]",
		"https://control.example.com:7601":   "[2001:db8::10 203.0.113.10]",
		"https://localhost:7601":             "[]",
		"https://127.0.0.1:7601":             "[]",
		"https://[::ffff:203.0.113.10]:7601": "[203.0.113.10]",
	} {
		got, err := ControlAddresses(url, names)(t.Context())
		if err != nil || fmt.Sprint(got) != want {
			t.Errorf("%s: %v, %v, want %s", url, got, err, want)
		}
	}
	if got, err := ControlAddresses("https://gone.example.com:7601", names)(t.Context()); err == nil {
		t.Errorf("a name that doesn't resolve: %v, and no error", got)
	}
	// Only addresses with a zone isn't "none": the control server is somewhere a
	// jail can't name, so it's an error, and nothing with a % reaches the file.
	zoned := func(context.Context, string) ([]string, error) { return []string{"fe80::1%eth0", "::1"}, nil }
	for _, url := range []string{"https://[fe80::1%25eth0]:7601", "https://[fe80::1%25%25(x)s]:7601", "https://link.example.com:7601"} {
		if got, err := ControlAddresses(url, zoned)(t.Context()); err == nil || !strings.Contains(err.Error(), "with a zone") || len(got) != 0 {
			t.Errorf("%s: %v, %v, want it refused for its zone", url, got, err)
		}
	}
	mixed := func(context.Context, string) ([]string, error) { return []string{"fe80::1%eth0", "203.0.113.10"}, nil }
	if got, err := ControlAddresses("https://control.example.com:7601", mixed)(t.Context()); err != nil || fmt.Sprint(got) != "[203.0.113.10]" {
		t.Errorf("one with a zone and one without: %v, %v", got, err)
	}
}

// The control server's addresses are kept from one write to the next: when its
// name stops resolving, a jail ignores those it last had, and when there never
// were any, the change is refused, as the jail would be written without what
// its plan says it never bans.
func TestTheControlServersAddressesAreKeptAndNeverMissingSilently(t *testing.T) {
	s := newStand(t)
	m := s.manager()
	var fail error
	m.Control = func(context.Context) ([]string, error) {
		if fail != nil {
			return nil, fail
		}
		return []string{"203.0.113.10"}, nil
	}
	fail = errors.New("can't look up the control server, control.example.com: no such host")
	err := m.Put(t.Context(), "sshd", sshd)
	if err == nil || !strings.Contains(err.Error(), "no address of it from before") || s.file() != "(none)" || s.runs() != "" {
		t.Fatalf("with no address now or before: %v, file %q, ran %q", err, s.file(), s.runs())
	}
	fail = nil
	if err := m.Put(t.Context(), "sshd", sshd); err != nil {
		t.Fatal(err)
	}
	fail = errors.New("no such host")
	again := s.manager()
	again.Control = m.Control
	if err := again.Put(t.Context(), "nginx-http-auth", &resourcev1.Jail{Filter: "nginx-http-auth", LogPaths: []string{"/var/log/nginx/error.log"}}); err != nil {
		t.Fatalf("with the addresses it last had: %v", err)
	}
	if got := s.file(); strings.Count(got, "ignoreip = 127.0.0.1/8 ::1 203.0.113.10") != 2 {
		t.Errorf("the file:\n%s", got)
	}
	// Removing jails goes on with the addresses it last had too.
	for _, name := range []string{"sshd", "nginx-http-auth"} {
		if err := again.Delete(t.Context(), name); err != nil {
			t.Errorf("removing %s while the name doesn't resolve: %v", name, err)
		}
	}
	if got := s.file(); got != "(none)" {
		t.Errorf("after the last jail went, the file holds %q", got)
	}
}

// A link where the file goes, or where a guessable temporary name would be, is
// never followed: the file is written beside under a name of its own, and what
// isn't a regular file isn't Dockwell's to replace.
func TestALinkIsNeverWrittenThrough(t *testing.T) {
	s := newStand(t)
	victim := filepath.Join(s.root, "victim")
	if err := os.WriteFile(victim, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.root, File)
	if err := os.Symlink(victim, path+".new"); err != nil {
		t.Fatal(err)
	}
	m := s.manager()
	if err := m.Put(t.Context(), "sshd", sshd); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "mine\n" {
		t.Errorf("a link at the old temporary name was written through: %q", b)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		t.Errorf("the file is %v, %v", info, err)
	}

	// A link at the file's own name, to a file or to nothing.
	for _, target := range []string{victim, filepath.Join(s.root, "nowhere")} {
		other := newStand(t)
		link := filepath.Join(other.root, File)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		err := other.manager().Put(t.Context(), "sshd", sshd)
		if err == nil || !strings.Contains(err.Error(), "isn't a regular file") {
			t.Errorf("a link at the file's name, to %s: %v", filepath.Base(target), err)
		}
		if info, lerr := os.Lstat(link); lerr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the link was replaced: %v, %v", info, lerr)
		}
		if got := other.runs(); got != "" {
			t.Errorf("fail2ban-client ran %q", got)
		}
	}
	if b, _ := os.ReadFile(victim); string(b) != "mine\n" {
		t.Errorf("a link at the file's name was written through: %q", b)
	}
}

// A jail's plan says whom it never bans, which of the addresses signed in over
// SSH now it could, and when what it ignores covers every address.
func TestAJailsPlanSaysWhomItCouldBan(t *testing.T) {
	s := newStand(t)
	m := s.manager()
	m.Sessions = func(context.Context) []string { return []string{"198.51.100.7", "203.0.113.99", "2001:db8::7"} }
	got := m.JailPlan(t.Context(), &resourcev1.Jail{Filter: "sshd", Backend: "systemd", Ignore: []string{"198.51.100.0/24"}})
	if fmt.Sprint(got.NeverBans) != "[127.0.0.1/8 ::1 203.0.113.10 198.51.100.0/24]" || fmt.Sprint(got.SignedInNotIgnored) != "[203.0.113.99 2001:db8::7]" || got.IgnoresEverything {
		t.Errorf("the plan: %+v", got)
	}
	all := m.JailPlan(t.Context(), &resourcev1.Jail{Filter: "sshd", Backend: "systemd", Ignore: []string{"0.0.0.0/0"}})
	if !all.IgnoresEverything || fmt.Sprint(all.SignedInNotIgnored) != "[2001:db8::7]" {
		t.Errorf("ignoring every IPv4 address: %+v", all)
	}
	if err := m.Put(t.Context(), "sshd", sshd); err != nil {
		t.Fatal(err)
	}
	if stored := m.Stored(); stored["sshd"].GetMaxRetry() != 3 || len(stored) != 1 {
		t.Errorf("stored %v", stored)
	}
}

// The agent checks a jail again before it writes it, whatever sent it: a log
// path that carries a new line would set an action, which fail2ban runs as
// root, and a jail named DEFAULT would set every jail. Nothing is written, and
// fail2ban isn't asked anything.
func TestNothingThatSaysMoreThanAJailIsWritten(t *testing.T) {
	for name, c := range map[string]struct {
		jail string
		spec *resourcev1.Jail
	}{
		"an action in a log path": {"sshd", &resourcev1.Jail{Filter: "sshd", LogPaths: []string{"/var/log/auth.log\naction = dummy[actionstart=\"/usr/bin/touch /tmp/x\"]"}}},
		"a section in a log path": {"sshd", &resourcev1.Jail{Filter: "sshd", LogPaths: []string{"/var/log/auth.log\r[DEFAULT]\rbantime = -1"}}},
		"every jail's section":    {"DEFAULT", &resourcev1.Jail{Filter: "sshd", Backend: "systemd", MaxRetry: 1, BanForGood: true}},
		"a filter with a line":    {"sshd", &resourcev1.Jail{Filter: "sshd\nenabled = false", Backend: "systemd"}},
	} {
		s := newStand(t)
		m := s.manager()
		if err := m.Put(t.Context(), c.jail, c.spec); err == nil {
			t.Errorf("%s: written", name)
		}
		if got := s.file(); got != "(none)" {
			t.Errorf("%s: the file holds %q", name, got)
		}
		if got := s.runs(); got != "" {
			t.Errorf("%s: fail2ban-client ran %q", name, got)
		}
	}
}

// A dockwell.conf Dockwell didn't write isn't Dockwell's to replace or remove.
func TestAFileDockwellDidntWriteStays(t *testing.T) {
	s := newStand(t)
	theirs := "[sshd]\nenabled = true\nmaxretry = 9\n"
	if err := os.WriteFile(filepath.Join(s.root, File), []byte(theirs), 0o600); err != nil {
		t.Fatal(err)
	}
	m := s.manager()
	err := m.Put(t.Context(), "sshd", &resourcev1.Jail{Filter: "sshd", Backend: "systemd"})
	if err == nil || !strings.Contains(err.Error(), "Dockwell didn't write it") {
		t.Errorf("writing over someone's file = %v", err)
	}
	if err := m.Delete(t.Context(), "sshd"); err == nil {
		t.Error("removing someone's file passed")
	}
	if got := s.file(); got != theirs {
		t.Errorf("the file holds %q", got)
	}
	if got := s.runs(); got != "" {
		t.Errorf("fail2ban-client ran %q", got)
	}
}

// A jail is removed while the control server's name doesn't resolve and no
// address of it was kept, when the file says them: what the remaining jails'
// lines hold beyond loopback and their own lists, taken only if writing the
// jails Dockwell holds with it gives the file back to the byte. A file someone
// changed proves nothing, and the refusal stands.
func TestTheControlServersAddressesAreReadBackFromTheFile(t *testing.T) {
	s := newStand(t)
	m := s.manager()
	nginx := &resourcev1.Jail{Filter: "nginx-http-auth", LogPaths: []string{"/var/log/nginx/error.log"}, Ignore: []string{"office.example.com"}}
	for name, j := range map[string]*resourcev1.Jail{"sshd": sshd, "nginx-http-auth": nginx} {
		if err := m.Put(t.Context(), name, j); err != nil {
			t.Fatal(err)
		}
	}
	kept := filepath.Join(s.root, "agent", "control.json")
	lost := func() *Manager {
		if err := os.Remove(kept); err != nil {
			t.Fatal(err)
		}
		again := s.manager()
		again.Control = func(context.Context) ([]string, error) { return nil, errors.New("no such host") }
		return again
	}
	before := s.file()
	again := lost()
	if err := again.Delete(t.Context(), "nginx-http-auth"); err != nil {
		t.Fatalf("removing a jail with the addresses in the file alone: %v", err)
	}
	if got := s.file(); !strings.Contains(got, "ignoreip = 127.0.0.1/8 ::1 203.0.113.10 198.51.100.0/24") || strings.Contains(got, "nginx-http-auth") {
		t.Errorf("the file:\n%s\nwas:\n%s", got, before)
	}
	if b, err := os.ReadFile(kept); err != nil || string(b) != `["203.0.113.10"]` {
		t.Errorf("kept %q, %v", b, err)
	}

	// Someone changed the file: it doesn't read back as written, so nothing is taken from it.
	path := filepath.Join(s.root, File)
	if err := os.WriteFile(path, []byte(strings.Replace(s.file(), "203.0.113.10", "203.0.113.66", 1)+"# by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := s.file()
	other := lost()
	err := other.Put(t.Context(), "recidive", &resourcev1.Jail{Filter: "recidive", LogPaths: []string{"/var/log/fail2ban.log"}})
	if err == nil || !strings.Contains(err.Error(), "no address of it from before") || s.file() != changed {
		t.Errorf("with a file changed by hand: %v, file\n%s", err, s.file())
	}
}

// What a jail ignores covers every address as one range, or as halves that add
// up to it; a list with a gap doesn't. A host name is kept, and fail2ban looks
// it up.
func TestAJailsPlanSaysWhenItsHalvesIgnoreEveryone(t *testing.T) {
	s := newStand(t)
	m := s.manager()
	for name, c := range map[string]struct {
		ignore []string
		want   bool
	}{
		"one range":               {[]string{"0.0.0.0/0"}, true},
		"two halves":              {[]string{"0.0.0.0/1", "128.0.0.0/1"}, true},
		"a half and two quarters": {[]string{"0.0.0.0/1", "128.0.0.0/2", "192.0.0.0/2"}, true},
		"every IPv6 address":      {[]string{"::/1", "8000::/1"}, true},
		"a half and a quarter":    {[]string{"0.0.0.0/1", "128.0.0.0/2"}, false},
		"an office and a name":    {[]string{"198.51.100.0/24", "office.example.com"}, false},
	} {
		got := m.JailPlan(t.Context(), &resourcev1.Jail{Filter: "sshd", Backend: "systemd", Ignore: c.ignore})
		if got.IgnoresEverything != c.want {
			t.Errorf("%s: ignores everything %t, want %t", name, got.IgnoresEverything, c.want)
		}
	}
	plan := m.JailPlan(t.Context(), &resourcev1.Jail{Filter: "sshd", Backend: "systemd", Ignore: []string{"office.example.com"}})
	if !slices.Contains(plan.NeverBans, "office.example.com") {
		t.Errorf("never bans %v, without the name", plan.NeverBans)
	}
	if err := m.Put(t.Context(), "sshd", &resourcev1.Jail{Filter: "sshd", Backend: "systemd", Ignore: []string{"Office.Example.com", "198.51.100.7"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.file(); !strings.Contains(got, "ignoreip = 127.0.0.1/8 ::1 203.0.113.10 198.51.100.7/32 office.example.com\n") {
		t.Errorf("the file:\n%s", got)
	}
}
