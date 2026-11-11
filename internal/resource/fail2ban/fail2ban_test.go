// SPDX-License-Identifier: AGPL-3.0-only

package fail2ban_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/fail2ban"
)

// A jail takes what fail2ban takes, in a canonical order, and refuses the rest
// in words that say what would do.
func TestValidateTakesWhatFail2banTakes(t *testing.T) {
	var d fail2ban.Driver
	j := &resourcev1.Jail{Filter: "sshd", Backend: "systemd", Ports: []uint32{2222, 22, 22}, FindTime: "10m", BanTime: "1h", Ignore: []string{"198.51.100.7", "203.0.113.9/24"}}
	if err := d.Validate(j); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(j.GetPorts()) != "[22 2222]" || fmt.Sprint(j.GetIgnore()) != "[198.51.100.7/32 203.0.113.0/24]" {
		t.Errorf("ports %v, ignore %v", j.GetPorts(), j.GetIgnore())
	}
	for name, bad := range map[string]*resourcev1.Jail{
		"no filter":             {Backend: "systemd"},
		"a port of 0":           {Filter: "sshd", Backend: "systemd", Ports: []uint32{0}},
		"a time fail2ban won't": {Filter: "sshd", Backend: "systemd", FindTime: "soon"},
		"for good and a time":   {Filter: "sshd", Backend: "systemd", BanForGood: true, BanTime: "1h"},
		"another backend":       {Filter: "sshd", Backend: "gamin"},
		"nowhere to read":       {Filter: "sshd"},
		"the journal and logs":  {Filter: "sshd", Backend: "systemd", LogPaths: []string{"/var/log/auth.log"}},
		"a relative log":        {Filter: "sshd", LogPaths: []string{"auth.log"}},
		"a name for an ignore":  {Filter: "sshd", Backend: "systemd", Ignore: []string{"office"}},
	} {
		if err := d.Validate(bad); err == nil {
			t.Errorf("%s passed", name)
		}
	}
	if err := d.CheckName("sshd"); err != nil {
		t.Error(err)
	}
	if err := d.CheckName("ssh d"); err == nil {
		t.Error("a name with a space passed")
	}
}

// What a jail names goes into fail2ban's own file, which reads a new line as
// the next setting, a section as every jail's, and an action as a command to
// run as root. So nothing that could say more than a log file's path gets in,
// and no jail takes the name of a section that sets them all.
func TestAJailCantSayMoreThanItsFields(t *testing.T) {
	var d fail2ban.Driver
	for name, log := range map[string]string{
		"a new line and an action":     "/var/log/auth.log\naction = dummy[actionstart=\"/usr/bin/touch /tmp/x\"]",
		"a carriage return":            "/var/log/auth.log\r[DEFAULT]\rbantime = -1",
		"a value for fail2ban to fill": "/var/log/%(sshd_log)s",
		"a second file after a space":  "/var/log/auth.log /etc/shadow",
		"a tab":                        "/var/log/auth.log\t/etc/shadow",
		"a comment":                    "/var/log/auth.log;x",
		"a file outside the logs":      "/etc/shadow",
		"a way up out of the logs":     "/var/log/../../etc/shadow",
		"the logs' folder alone":       "/var/log",
	} {
		if err := d.Validate(&resourcev1.Jail{Filter: "sshd", LogPaths: []string{log}}); err == nil {
			t.Errorf("a log path with %s passed", name)
		}
	}
	for _, log := range []string{"/var/log/auth.log", "/var/log/secure", "/var/log/nginx/*error.log", "/var/log/app_1/access.log.?"} {
		if err := d.Validate(&resourcev1.Jail{Filter: "sshd", LogPaths: []string{log}}); err != nil {
			t.Errorf("%s: %v", log, err)
		}
	}
	for _, name := range []string{"DEFAULT", "default", "Default", "INCLUDES", "includes"} {
		if err := d.CheckName(name); err == nil {
			t.Errorf("a jail named %s passed, which fail2ban reads as more than one jail", name)
		}
	}
	for _, filter := range []string{"sshd\naction = x", "sshd[mode=aggressive]", "%(known/x)s"} {
		if err := d.Validate(&resourcev1.Jail{Filter: filter, Backend: "systemd"}); err == nil {
			t.Errorf("the filter %q passed", filter)
		}
	}
}

// A jail decides who gets onto the server, so a change to one takes a person,
// and it can't lock anyone out of a new session on its own, so it waits for no
// confirmation and warns of nothing.
func TestAJailTakesAPersonAndNoTimer(t *testing.T) {
	d := &fail2ban.Driver{}
	if !resource.TakesAPerson(d) || resource.GuardsAccess(d) {
		t.Errorf("takes a person %t, under the timer %t", resource.TakesAPerson(d), resource.GuardsAccess(d))
	}
	dec, err := d.Plan(nil, &resourcev1.Jail{Filter: "sshd"}, nil)
	if err != nil || dec.Action != resource.ActionCreate || len(dec.Warnings) != 0 {
		t.Errorf("plan %+v, %v", dec, err)
	}
}

// A jail names a few of each thing, as fail2ban reads them all at every start;
// and what it ignores is an address or a range, never one with a zone, which
// fail2ban doesn't take and whose % it would read as a value to fill in.
func TestAJailsListsAreBoundedAndNeverCarryAZone(t *testing.T) {
	var d fail2ban.Driver
	many := func(n int, of func(i int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = of(i)
		}
		return out
	}
	ports := make([]uint32, fail2ban.MaxPorts+1)
	for i := range ports {
		ports[i] = uint32(1000 + i)
	}
	for name, bad := range map[string]*resourcev1.Jail{
		"too many ports":         {Filter: "sshd", Backend: "systemd", Ports: ports},
		"too many log files":     {Filter: "sshd", LogPaths: many(fail2ban.MaxLogPaths+1, func(i int) string { return fmt.Sprintf("/var/log/app%d.log", i) })},
		"too many to ignore":     {Filter: "sshd", Backend: "systemd", Ignore: many(fail2ban.MaxIgnore+1, func(i int) string { return fmt.Sprintf("198.51.%d.%d", i/250, i%250) })},
		"an address with a zone": {Filter: "sshd", Backend: "systemd", Ignore: []string{"fe80::1%eth0"}},
		"a zone that fills in":   {Filter: "sshd", Backend: "systemd", Ignore: []string{"fe80::1%%(x)s"}},
	} {
		if err := d.Validate(bad); err == nil {
			t.Errorf("%s passed", name)
		}
	}
	ok := &resourcev1.Jail{Filter: "sshd", Backend: "systemd", Ports: ports[:fail2ban.MaxPorts], Ignore: []string{"0.0.0.0/0", "2001:db8::/32"}}
	if err := d.Validate(ok); err != nil {
		t.Errorf("a jail at its bounds: %v", err)
	}
}

// A name and a log file's path take what the expressions they were took, to
// the byte: each length is checked apart from its form now (ADR 0060).
func TestNamesAndLogPathsTakeWhatTheirExpressionsTook(t *testing.T) {
	var d fail2ban.Driver
	agree := func(what, was string, now func(string) bool, cases []string) {
		t.Helper()
		re := regexp.MustCompile(was)
		took := 0
		for _, c := range cases {
			if re.MatchString(c) {
				took++
			}
			if got, want := now(c), re.MatchString(c); got != want {
				t.Errorf("%s %q (%d bytes): the check says %v, the expression said %v", what, c, len(c), got, want)
			}
		}
		if took == 0 || took == len(cases) {
			t.Errorf("%s: the expression took %d of %d cases, which tells nothing", what, took, len(cases))
		}
	}
	names := []string{"", "sshd", "nginx-http-auth", "a_b", "-sshd", "_x", "ssh d", "sshd\n", "sshd.local", "é", "0", "recidive"}
	for _, n := range []int{1, 63, 64, 65, 300} {
		names = append(names, strings.Repeat("a", n), strings.Repeat("a", n-1)+".", "-"+strings.Repeat("a", n-1))
	}
	// Reserved names are refused beyond the form, so they're left out of this.
	agree("a jail's name", `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`, func(s string) bool { return d.CheckName(s) == nil }, names)

	paths := []string{"", "/", "/var/log/auth.log", "/var/log/nginx/*error.log", "/var/log/a b", "/var/log/a\nb", "/var/log/%(x)s", "var/log/x", "/var/log/a;b", "/var/log/a@b:c+d"}
	for _, n := range []int{1, 255, 256, 257, 900} {
		paths = append(paths, "/"+strings.Repeat("a", n-1), "/"+strings.Repeat("a", n), "/"+strings.Repeat("a", n-1)+" ")
	}
	// The form alone: where the file is, and that its path is clean, are checked beside it.
	agree("a log file's path", `^/[A-Za-z0-9._+@:*?/-]{1,255}$`, func(s string) bool {
		err := fail2ban.CheckLogPath(s)
		return err == nil || !strings.Contains(err.Error(), "is a full path of letters")
	}, paths)
}

// What a jail ignores may be a host name, as an admin's own list may hold one,
// which fail2ban looks up itself: kept, in lower case, and nothing that isn't
// an address, a range or a name gets in.
func TestAJailMayIgnoreAHostName(t *testing.T) {
	var d fail2ban.Driver
	j := &resourcev1.Jail{Filter: "sshd", Backend: "systemd", Ignore: []string{"Office.Example.com", "198.51.100.7", "vpn-1.example.net"}}
	if err := d.Validate(j); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(j.GetIgnore()) != "[198.51.100.7/32 office.example.com vpn-1.example.net]" {
		t.Errorf("ignore %v", j.GetIgnore())
	}
	for _, bad := range []string{"office", "office example.com", "office.example.com\nbantime = -1", "a..b", "-x.example.com", "%(x)s", "fe80::1%eth0", "1.2.3", strings.Repeat("a", 250) + ".com"} {
		if err := d.Validate(&resourcev1.Jail{Filter: "sshd", Backend: "systemd", Ignore: []string{bad}}); err == nil {
			t.Errorf("%q passed as something to ignore", bad)
		}
	}
}
