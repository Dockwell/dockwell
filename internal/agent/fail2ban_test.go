// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// fail2banClientScript answers as fail2ban-client 1.0.2 does, from testdata:
// two jails, sshd reading the journal and nginx-http-auth a file, the latter
// on a fail2ban that doesn't say when its bans began.
func fail2banClientScript(t *testing.T) string {
	t.Helper()
	data, err := filepath.Abs("testdata/fail2ban")
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`d=%q
case "$*" in
version) echo 1.0.2 ;;
ping) echo 'Server replied: pong' ;;
status) cat "$d/status.txt" ;;
"status sshd") cat "$d/status-sshd.txt" ;;
"status nginx-http-auth") cat "$d/status-nginx-http-auth.txt" ;;
"get sshd maxretry") echo 5 ;;
"get sshd findtime") echo 600 ;;
"get sshd bantime") echo 3600 ;;
"get sshd banip --with-time") cat "$d/banip-sshd.txt" ;;
"get nginx-http-auth maxretry") echo 3 ;;
"get nginx-http-auth findtime") echo 60 ;;
"get nginx-http-auth bantime") echo -1 ;;
*) echo "Invalid command ($*)" >&2; exit 255 ;;
esac
`, data)
}

func readFail2ban(t *testing.T, s standIns) *dockwellv1.ServerFail2Ban {
	t.Helper()
	r := newFail2banReader(Config{Tools: s.runner()})
	r.loc = time.UTC
	got, err := r.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// fail2ban's jails are read as it runs them: what each counts and bans for,
// where it reads failures, and whom it bans now, the latest first, with when
// each ban began and ends where fail2ban says, and the addresses alone where
// it doesn't.
func TestFail2banIsReadAsItRuns(t *testing.T) {
	s := newStandIns(t)
	s.tool(t, "/usr/bin/fail2ban-client", fail2banClientScript(t))
	got := readFail2ban(t, s)
	if got.GetState() != dockwellv1.Fail2BanState_FAIL2BAN_STATE_RUNNING || got.GetVersion() != "1.0.2" || len(got.GetJails()) != 2 {
		t.Fatalf("state %v, version %q, jails %d: %s", got.GetState(), got.GetVersion(), len(got.GetJails()), got.GetWhy())
	}
	nginx, sshd := got.GetJails()[0], got.GetJails()[1]
	if sshd.GetName() != "sshd" || sshd.GetMaxRetry() != 5 || sshd.GetFindTime().AsDuration() != 10*time.Minute ||
		sshd.GetBanTime().AsDuration() != time.Hour || sshd.GetBansForGood() {
		t.Errorf("sshd: %v", sshd)
	}
	if sshd.GetFailedNow() != 1 || sshd.GetFailedTotal() != 27 || sshd.GetBannedNow() != 2 || sshd.GetBannedTotal() != 5 ||
		fmt.Sprint(sshd.GetJournalMatches()) != "[_SYSTEMD_UNIT=sshd.service + _COMM=sshd]" || len(sshd.GetFiles()) != 0 {
		t.Errorf("sshd's counts and source: %v", sshd)
	}
	var bans []string
	for _, b := range sshd.GetBans() {
		bans = append(bans, fmt.Sprintf("%s %s-%s", b.GetAddress(), b.GetSince().AsTime().Format("15:04"), b.GetUntil().AsTime().Format("15:04")))
	}
	if strings.Join(bans, ", ") != "198.51.100.77 03:10-04:10, 192.0.2.211 02:40-03:40" {
		t.Errorf("sshd's bans: %v", bans)
	}
	if nginx.GetName() != "nginx-http-auth" || !nginx.GetBansForGood() || nginx.GetBanTime() != nil || fmt.Sprint(nginx.GetFiles()) != "[/var/log/nginx/error.log]" {
		t.Errorf("nginx-http-auth: %v", nginx)
	}
	if len(nginx.GetBans()) != 1 || nginx.GetBans()[0].GetAddress() != "203.0.113.50" || nginx.GetBans()[0].GetSince() != nil {
		t.Errorf("nginx-http-auth's bans, with no times said: %v", nginx.GetBans())
	}
}

// fail2ban that isn't installed, or whose server isn't running, says so, and
// why, as fail2ban-client said it.
func TestFail2banThatIsntRunningSaysSo(t *testing.T) {
	if got := readFail2ban(t, newStandIns(t)); got.GetState() != dockwellv1.Fail2BanState_FAIL2BAN_STATE_NOT_INSTALLED {
		t.Errorf("not installed: %v", got.GetState())
	}
	s := newStandIns(t)
	s.tool(t, "/usr/bin/fail2ban-client", `case "$1" in
version) echo 1.0.2 ;;
*) echo 'ERROR  Failed to access socket path: /var/run/fail2ban/fail2ban.sock. Is fail2ban running?' >&2; exit 255 ;;
esac
`)
	got := readFail2ban(t, s)
	if got.GetState() != dockwellv1.Fail2BanState_FAIL2BAN_STATE_STOPPED || !strings.Contains(got.GetWhy(), "Is fail2ban running?") || got.GetVersion() != "1.0.2" {
		t.Errorf("stopped: %v %q %q", got.GetState(), got.GetWhy(), got.GetVersion())
	}
	s.tool(t, "/usr/bin/fail2ban-client", `case "$1" in
ping) echo 'Server replied: pong' ;;
*) echo 'ERROR  the server said nothing' >&2; exit 255 ;;
esac
`)
	if got := readFail2ban(t, s); got.GetState() != dockwellv1.Fail2BanState_FAIL2BAN_STATE_UNREADABLE || !strings.Contains(got.GetWhy(), "the server said nothing") {
		t.Errorf("unreadable: %v %q", got.GetState(), got.GetWhy())
	}
}

// A jail's name is an argument to fail2ban-client only when it can't read as
// one of its options, and a jail that bans thousands sends the latest of them
// with fail2ban's own count of them all.
func TestAJailNamedLikeAnOptionIsNeverAnArgument(t *testing.T) {
	s := newStandIns(t)
	s.tool(t, "/usr/bin/fail2ban-client", `echo "$*" >> "$(dirname "$0")/asked"
case "$*" in
version) echo 1.0.2 ;;
ping) echo pong ;;
status) printf 'Status\n|- Number of jail:\t2\n`+"`"+`- Jail list:\t--str2sec, busy\n' ;;
"status busy") printf 'Status for the jail: busy\n`+"`"+`- Actions\n   |- Currently banned:\t540\n' ;;
"get busy banip --with-time") i=0; while [ $i -lt 540 ]; do echo "198.51.100.$((i % 250)) 	2026-11-08 03:10:00 + 600 = 2026-11-08 03:20:00"; i=$((i + 1)); done ;;
*) exit 255 ;;
esac
`)
	got := readFail2ban(t, s)
	if len(got.GetJails()) != 2 || got.GetJails()[0].GetUnreadable() == "" {
		t.Fatalf("jails %v", got.GetJails())
	}
	asked, err := os.ReadFile(filepath.Join(string(s), "usr", "bin", "asked"))
	if err != nil || strings.Contains(string(asked), "--str2sec") {
		t.Errorf("fail2ban-client was asked %q: %v", asked, err)
	}
	if busy := got.GetJails()[1]; len(busy.GetBans()) != maxBans || busy.GetBannedNow() != 540 {
		t.Errorf("busy sends %d bans and counts %d", len(busy.GetBans()), busy.GetBannedNow())
	}
}

// isJailArg takes what the expression it was took, to the byte: its length is
// checked apart from its form now (ADR 0060).
func TestIsJailArgTakesWhatItsExpressionTook(t *testing.T) {
	was := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	cases := []string{"", "sshd", "nginx-http-auth", "a.b_c-d", "-sshd", "--str2sec", ".sshd", "ssh d", "sshd\n", "sshd;x", "é", "0"}
	for _, n := range []int{1, 127, 128, 129, 400} {
		cases = append(cases, strings.Repeat("a", n), strings.Repeat("a", n-1)+" ", "-"+strings.Repeat("a", n-1))
	}
	took := 0
	for _, c := range cases {
		if was.MatchString(c) {
			took++
		}
		if got, want := isJailArg(c), was.MatchString(c); got != want {
			t.Errorf("%q (%d bytes): the check says %v, the expression said %v", c, len(c), got, want)
		}
	}
	if took == 0 || took == len(cases) {
		t.Errorf("the expression took %d of %d cases, which tells nothing", took, len(cases))
	}
}
