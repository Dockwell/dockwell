// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
)

// standIns is a folder laid out as a server's files, with tools that print
// what a test gives them, as the agent runs them under DOCKWELL_TEST_TOOLS.
type standIns string

func newStandIns(t *testing.T) standIns { return standIns(t.TempDir()) }

// file writes a file at its path on the server.
func (s standIns) file(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(string(s), path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

// tool writes a shell script at a tool's path.
func (s standIns) tool(t *testing.T, path, script string) {
	t.Helper()
	s.file(t, path, "#!/bin/sh\n"+script, 0o755)
}

func (s standIns) runner() *hosttool.Runner { return &hosttool.Runner{Root: string(s)} }

// ubuntuSSHD is sshd -T on an Ubuntu 24.04 cloud image, cut to the lines the
// agent reads and a few around them, with the port moved to 2222.
const ubuntuSSHD = `port 2222
addressfamily any
listenaddress [::]:2222
listenaddress 0.0.0.0:2222
usepam yes
permitrootlogin without-password
pubkeyauthentication yes
passwordauthentication yes
kbdinteractiveauthentication no
authorizedkeysfile .ssh/authorized_keys .ssh/authorized_keys2
authorizedkeyscommand none
authorizedprincipalsfile none
trustedusercakeys none
gssapiauthentication no
`

// ubuntu is the files and tools of an Ubuntu 24.04 cloud image whose sshd
// takes passwords from cloud-init's drop-in, listens on 2222 by a drop-in of
// the owner's, and lets one account in with a password by a Match block.
func ubuntu(t *testing.T) standIns {
	s := newStandIns(t)
	s.tool(t, "/usr/sbin/sshd", `case "$1" in
-T) cat <<'EOF'
`+ubuntuSSHD+`EOF
;;
-V) echo 'OpenSSH_9.6p1 Ubuntu-3ubuntu13.5, OpenSSL 3.0.13 30 Jan 2024' >&2 ;;
esac
`)
	s.tool(t, "/usr/bin/systemctl", `[ "$*" = "is-active ssh.socket sshd.socket" ] || exit 9
echo active
echo inactive
exit 3
`)
	s.file(t, "/etc/ssh/sshd_config", `# This is the sshd server system-wide configuration file.

Include /etc/ssh/sshd_config.d/*.conf

#Port 22
#PermitRootLogin prohibit-password
KbdInteractiveAuthentication no
UsePAM yes
PasswordAuthentication no
Subsystem	sftp	/usr/lib/openssh/sftp-server

Match User deploy
	PasswordAuthentication yes
`, 0o644)
	s.file(t, "/etc/ssh/sshd_config.d/50-cloud-init.conf", "PasswordAuthentication yes\n", 0o644)
	s.file(t, "/etc/ssh/sshd_config.d/60-port.conf", "# moved by the owner\nPort=2222\n", 0o644)
	return s
}

func readSSH(t *testing.T, s standIns) *dockwellv1.ServerSsh {
	t.Helper()
	return readSSHAt(t, s, written, time.Time{})
}

// written is when a file's content was last written, alone: what a test sets
// with at. A test has no way to set when an inode changed, which is always
// now, so the tests of that give their own times.
func written(root fs.FS, name string) time.Time {
	info, err := fs.Stat(root, strings.TrimPrefix(name, "/"))
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// readSSHAt reads SSH with changed saying when each file changed, at now, or
// at the clock's now when it's zero.
func readSSHAt(t *testing.T, s standIns, changed func(fs.FS, string) time.Time, now time.Time) *dockwellv1.ServerSsh {
	t.Helper()
	r := newSSHReader(Config{Tools: s.runner()})
	r.changed = changed
	if !now.IsZero() {
		r.now = func() time.Time { return now }
	}
	got, err := r.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func sources(list []*dockwellv1.SshSource) string {
	var out []string
	for _, s := range list {
		out = append(out, fmt.Sprintf("%s=%s@%s:%d", s.GetKeyword(), s.GetValue(), s.GetFile(), s.GetLine()))
	}
	return strings.Join(out, " ")
}

// SSH is read as sshd says it runs: passwords from cloud-init's drop-in, root
// with keys alone, its port, its version and the socket unit that holds the
// port; with the line each value comes from, the first sshd reads, and a Match
// block that lets one account in otherwise.
func TestSSHIsReadAsSSHDSaysItRuns(t *testing.T) {
	got := readSSH(t, ubuntu(t))
	if got.GetUnreadable() != "" {
		t.Fatalf("unreadable: %s", got.GetUnreadable())
	}
	if got.GetPasswordLogin() != yes || got.GetKeyLogin() != yes || got.GetRootLogin() != dockwellv1.SshRootLogin_SSH_ROOT_LOGIN_KEYS_ONLY ||
		fmt.Sprint(got.GetPorts()) != "[2222]" {
		t.Errorf("passwords %v, keys %v, root %v, ports %v", got.GetPasswordLogin(), got.GetKeyLogin(), got.GetRootLogin(), got.GetPorts())
	}
	if got.GetVersion() != "OpenSSH_9.6p1 Ubuntu-3ubuntu13.5" || got.GetSocketUnit() != "ssh.socket" {
		t.Errorf("version %q, socket unit %q", got.GetVersion(), got.GetSocketUnit())
	}
	want := "PasswordAuthentication=yes@/etc/ssh/sshd_config.d/50-cloud-init.conf:1 Port=2222@/etc/ssh/sshd_config.d/60-port.conf:2 " +
		"KbdInteractiveAuthentication=no@/etc/ssh/sshd_config:7"
	if got := sources(got.GetSetIn()); got != want {
		t.Errorf("set in\n %s\nwant\n %s", got, want)
	}
	if len(got.GetMatches()) != 1 || got.GetMatches()[0].GetCriteria() != "User deploy" || got.GetMatches()[0].GetLine() != 12 ||
		fmt.Sprint(got.GetMatches()[0].GetSets()) != "[PasswordAuthentication]" {
		t.Errorf("matches %v", got.GetMatches())
	}
	if got.GetDropInFolder() != "/etc/ssh/sshd_config.d" || got.GetDockwellDropIn() {
		t.Errorf("drop-ins %q, Dockwell's %t", got.GetDropInFolder(), got.GetDockwellDropIn())
	}
	if len(got.GetAccessLists())+len(got.GetUnchecked()) != 0 {
		t.Errorf("access lists %v, unchecked %v", got.GetAccessLists(), got.GetUnchecked())
	}
}

// An sshd_config that includes no drop-in folder, as RHEL 8's, says so; an
// sshd older than -V says its version in its usage; who may sign in, and ways
// in Dockwell can't check, are said as sshd says them; and Dockwell's own
// drop-in is seen where it's there.
func TestSSHWithoutDropInsAndWithWaysInDockwellCantCheck(t *testing.T) {
	s := newStandIns(t)
	s.tool(t, "/usr/sbin/sshd", `case "$1" in
-T) printf 'port 22\npermitrootlogin yes\npasswordauthentication no\nchallengeresponseauthentication no\nallowusers deploy admin@198.51.100.0/24\nallowgroups wheel\nauthorizedkeyscommand /usr/bin/sss_ssh_authorizedkeys\ngssapiauthentication yes\n' ;;
-V) echo 'unknown option -- V' >&2; echo 'OpenSSH_8.0p1, OpenSSL 1.1.1k  FIPS 25 Mar 2021' >&2; echo 'usage: sshd [-46DdeiqTt] [-C connection_spec]' >&2; exit 1 ;;
esac
`)
	s.file(t, "/etc/ssh/sshd_config", "PermitRootLogin yes\nPasswordAuthentication no\nInclude sshd_config.d/*.conf\n", 0o644)
	got := readSSH(t, s)
	if got.GetPasswordLogin() != no || got.GetRootLogin() != dockwellv1.SshRootLogin_SSH_ROOT_LOGIN_YES || got.GetVersion() != "OpenSSH_8.0p1" {
		t.Errorf("passwords %v, root %v, version %q", got.GetPasswordLogin(), got.GetRootLogin(), got.GetVersion())
	}
	if got.GetDropInFolder() != "/etc/ssh/sshd_config.d" {
		t.Errorf("a relative Include reads as %q", got.GetDropInFolder())
	}
	if fmt.Sprint(got.GetAccessLists()) != "[allowusers deploy admin@198.51.100.0/24 allowgroups wheel]" ||
		fmt.Sprint(got.GetUnchecked()) != "[AuthorizedKeysCommand GSSAPIAuthentication]" {
		t.Errorf("access lists %v, unchecked %v", got.GetAccessLists(), got.GetUnchecked())
	}
	if got.GetSocketUnit() != "" {
		t.Errorf("with no systemctl, a socket unit %q", got.GetSocketUnit())
	}

	s.file(t, "/etc/ssh/sshd_config", "PermitRootLogin yes\nPasswordAuthentication no\n", 0o644)
	if got := readSSH(t, s); got.GetDropInFolder() != "" || sources(got.GetSetIn()) != "PermitRootLogin=yes@/etc/ssh/sshd_config:1 PasswordAuthentication=no@/etc/ssh/sshd_config:2" {
		t.Errorf("an sshd_config with no Include: drop-ins %q, set in %s", got.GetDropInFolder(), sources(got.GetSetIn()))
	}

	u := ubuntu(t)
	u.file(t, "/etc/ssh/sshd_config.d/00-dockwell.conf", "PasswordAuthentication no\n", 0o644)
	if got := readSSH(t, u); !got.GetDockwellDropIn() {
		t.Error("Dockwell's own drop-in isn't seen")
	}
}

// What can't be read says why: sshd -T failing, as it does before sshd's
// first connection under socket activation; no sshd; and Dropbear.
func TestSSHThatCantBeReadSaysWhy(t *testing.T) {
	s := newStandIns(t)
	s.tool(t, "/usr/sbin/sshd", "echo 'Missing privilege separation directory: /run/sshd' >&2\nexit 255\n")
	if got := readSSH(t, s).GetUnreadable(); !strings.Contains(got, "Missing privilege separation directory: /run/sshd") || !strings.Contains(got, "Run sshd -T on the server as root") {
		t.Errorf("sshd -T failing: %q", got)
	}
	none := newStandIns(t)
	if got := readSSH(t, none).GetUnreadable(); !strings.Contains(got, "no OpenSSH sshd") {
		t.Errorf("no sshd: %q", got)
	}
	none.tool(t, "/usr/sbin/dropbear", "exit 0\n")
	if got := readSSH(t, none).GetUnreadable(); !strings.Contains(got, "runs Dropbear") {
		t.Errorf("Dropbear: %q", got)
	}
}

// A read is cut off at its bound; one that's asked for while it runs shares it.
func TestSSHReadsShareOneRun(t *testing.T) {
	s := ubuntu(t)
	count := filepath.Join(string(s), "runs")
	s.tool(t, "/usr/sbin/sshd", fmt.Sprintf(`echo x >> %q
case "$1" in
-T) sleep 0.3; printf 'port 22\npasswordauthentication no\n' ;;
esac
`, count))
	r := newSSHReader(Config{Tools: s.runner()})
	done := make(chan *dockwellv1.ServerSsh, 2)
	for range 2 {
		go func() {
			got, _ := r.read(context.Background())
			done <- got
		}()
	}
	a, b := <-done, <-done
	if a != b {
		t.Error("two reads at once ran apart")
	}
	runs, _ := os.ReadFile(count)
	if n := strings.Count(string(runs), "x"); n != 2 { // -T and -V, once each
		t.Errorf("sshd ran %d times for two reads at once", n)
	}
}

// A Match block in a file sshd_config includes ends with that file, as sshd
// reads it: what sshd_config sets after the Include is everyone's, and is said
// where it's set, not as the block's.
func TestAMatchBlockEndsWithTheFileItsIn(t *testing.T) {
	s := newStandIns(t)
	s.file(t, "/etc/ssh/sshd_config", "Include /etc/ssh/sshd_config.d/*.conf\nPermitRootLogin no\nPort 2222\n", 0o644)
	s.file(t, "/etc/ssh/sshd_config.d/10-sftp.conf", "Match Group sftp\n\tPasswordAuthentication yes\n", 0o644)
	files := readSSHDConfig(os.DirFS(string(s)), nil)
	if got := sources(files.setIn); got != "PermitRootLogin=no@/etc/ssh/sshd_config:2 Port=2222@/etc/ssh/sshd_config:3" {
		t.Errorf("set in %q", got)
	}
	if len(files.matches) != 1 || files.matches[0].GetCriteria() != "Group sftp" || fmt.Sprint(files.matches[0].GetSets()) != "[PasswordAuthentication]" {
		t.Errorf("Match blocks %v", files.matches)
	}
}

// Keyboard-interactive is a password login only where PAM asks for it: sshd's
// default says yes even where it was built without PAM, as on Alpine.
func TestKeyboardInteractiveIsAPasswordOnlyWithPAM(t *testing.T) {
	for text, want := range map[string]dockwellv1.SshAnswer{
		"passwordauthentication no\nkbdinteractiveauthentication yes\nusepam yes\n":    yes,
		"passwordauthentication no\nkbdinteractiveauthentication yes\nusepam no\n":     no,
		"passwordauthentication no\nkbdinteractiveauthentication yes\n":                no,
		"passwordauthentication no\nchallengeresponseauthentication yes\nusepam yes\n": yes,
		"passwordauthentication yes\nkbdinteractiveauthentication no\nusepam no\n":     yes,
	} {
		out := &dockwellv1.ServerSsh{}
		effective(text, out)
		if out.GetPasswordLogin() != want {
			t.Errorf("%q reads password login %v, want %v", text, out.GetPasswordLogin(), want)
		}
	}
}

const (
	yes   = dockwellv1.SshAnswer_SSH_ANSWER_YES
	no    = dockwellv1.SshAnswer_SSH_ANSWER_NO
	stale = dockwellv1.SshLoaded_SSH_LOADED_STALE
	apart = dockwellv1.SshLoaded_SSH_LOADED_APART
)

// A line sshd -T doesn't print is "can't tell", never "no": each door the
// card reports stays unset without the line that would say, and output with
// none of them reads as unreadable, not as everything off.
func TestAMissingLineIsCantTellNeverOff(t *testing.T) {
	for text, want := range map[string]string{
		"port 22\n":                   "password UNSPECIFIED keys UNSPECIFIED root UNSPECIFIED ports [22]",
		"passwordauthentication no\n": "password UNSPECIFIED keys UNSPECIFIED root UNSPECIFIED ports []",
		"passwordauthentication no\nkbdinteractiveauthentication no\n":               "password NO keys UNSPECIFIED root UNSPECIFIED ports []",
		"kbdinteractiveauthentication no\npubkeyauthentication no\n":                 "password UNSPECIFIED keys NO root UNSPECIFIED ports []",
		"kbdinteractiveauthentication yes\nusepam yes\nport 22\n":                    "password YES keys UNSPECIFIED root UNSPECIFIED ports [22]",
		"passwordauthentication yes\npermitrootlogin no\npubkeyauthentication yes\n": "password YES keys YES root NO ports []",
	} {
		out := &dockwellv1.ServerSsh{}
		effective(text, out)
		short := func(s fmt.Stringer) string { return s.String()[strings.LastIndex(s.String(), "_")+1:] }
		got := fmt.Sprintf("password %s keys %s root %s ports %v", short(out.GetPasswordLogin()), short(out.GetKeyLogin()), short(out.GetRootLogin()), out.GetPorts())
		if got != want {
			t.Errorf("%q reads\n %s\nwant\n %s", text, got, want)
		}
	}
	for name, printed := range map[string]string{"nothing": "", "lines that say none of it": "addressfamily any\nusepam yes\n"} {
		s := newStandIns(t)
		s.tool(t, "/usr/sbin/sshd", fmt.Sprintf("printf %q\n", printed))
		got := readSSH(t, s)
		if !strings.Contains(got.GetUnreadable(), "printed none of them") || got.GetPasswordLogin() != 0 || got.GetRootLogin() != 0 {
			t.Errorf("sshd -T printing %s reads %v", name, got)
		}
	}
}

// running puts an sshd that listens among the stand's processes: started at
// started, holding a listening socket, with its PID file written at wrote, or
// none when wrote is zero.
func (s standIns) running(t *testing.T, started, wrote time.Time) {
	t.Helper()
	boot := started.Add(-time.Hour)
	s.file(t, "/proc/stat", fmt.Sprintf("cpu 1 2 3\nbtime %d\n", boot.Unix()), 0o644)
	s.file(t, "/proc/net/tcp", "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
		"   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001\n"+
		"   1: 0A0200C0:0016 077100CB:C822 01 00000000:00000000 00:00000000 00000000     0        0 1002\n", 0o644)
	ticks := int64(started.Sub(boot) / (time.Second / procTicks))
	s.file(t, "/proc/100/comm", "sshd\n", 0o644)
	s.file(t, "/proc/100/cmdline", "sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups\x00\x00", 0o644)
	if err := os.Symlink("/usr/sbin/sshd", filepath.Join(string(s), "proc", "100", "exe")); err != nil {
		t.Fatal(err)
	}
	s.file(t, "/proc/100/stat", fmt.Sprintf("100 (sshd) S 1 100 100 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 %d 15000000 1\n", ticks), 0o644)
	s.file(t, "/proc/1/comm", "systemd\n", 0o644)
	// A session's own process, which holds the session's socket and never the listener.
	s.file(t, "/proc/200/comm", "sshd\n", 0o644)
	s.file(t, "/proc/200/stat", fmt.Sprintf("200 (sshd) S 100 200 200 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 %d 15000000 1\n", ticks+500), 0o644)
	for pid, inode := range map[string]string{"100": "1001", "200": "1002"} {
		dir := filepath.Join(string(s), "proc", pid, "fd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("socket:["+inode+"]", filepath.Join(dir, "3")); err != nil {
			t.Fatal(err)
		}
	}
	if !wrote.IsZero() {
		s.file(t, "/run/sshd.pid", "100\n", 0o644)
		s.at(t, "/run/sshd.pid", wrote)
	}
}

// at sets when a file or folder last changed.
func (s standIns) at(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(filepath.Join(string(s), path), when, when); err != nil {
		t.Fatal(err)
	}
}

// filesAt sets when each of the Ubuntu stand's files and its drop-in folder last changed.
func (s standIns) filesAt(t *testing.T, when time.Time) {
	t.Helper()
	for _, p := range []string{"/etc/ssh/sshd_config", "/etc/ssh/sshd_config.d/50-cloud-init.conf", "/etc/ssh/sshd_config.d/60-port.conf", "/etc/ssh/sshd_config.d", "/etc/ssh"} {
		s.at(t, p, when)
	}
}

// For real, with nothing injected: a file whose written time is set back still
// reads as changed when its inode did, which touch, cp -p and rsync can't undo.
func TestAWrittenTimeSetBackDoesntHideAChange(t *testing.T) {
	s := newStandIns(t)
	s.file(t, "/etc/ssh/sshd_config", "PasswordAuthentication no\n", 0o644)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Now().Add(-2 * time.Second)
	s.at(t, "/etc/ssh/sshd_config", old)
	root := os.DirFS(string(s))
	if at := written(root, "/etc/ssh/sshd_config"); !at.Equal(old) {
		t.Fatalf("written at %v", at)
	}
	if at := fileChanged(root, "/etc/ssh/sshd_config"); at.Before(before) {
		t.Errorf("changed at %v, as if setting its written time back hid the change", at)
	}
	if at := fileChanged(root, "/etc/ssh/missing"); !at.IsZero() {
		t.Errorf("a file that isn't there changed at %v", at)
	}
	// Every file's folder is walked, /etc/ssh included.
	files := readSSHDConfig(root, nil)
	var paths []string
	for _, w := range files.walked {
		paths = append(paths, w.path)
	}
	if fmt.Sprint(paths) != "[/etc/ssh/sshd_config /etc/ssh]" {
		t.Errorf("walked %v", paths)
	}
}

// A keyword sshd -T prints twice with two values is "can't tell", never the first.
func TestALineSaidTwiceWithTwoValuesIsCantTell(t *testing.T) {
	out := &dockwellv1.ServerSsh{}
	effective("passwordauthentication no\npasswordauthentication yes\nkbdinteractiveauthentication no\npermitrootlogin no\npermitrootlogin yes\npubkeyauthentication yes\npubkeyauthentication yes\n", out)
	if out.GetPasswordLogin() != 0 || out.GetRootLogin() != 0 || out.GetKeyLogin() != yes {
		t.Errorf("password %v, root %v, keys %v", out.GetPasswordLogin(), out.GetRootLogin(), out.GetKeyLogin())
	}
}

// stood is the Ubuntu stand with its files written at day, and an sshd that
// listens and last wrote its PID file a minute after.
func stood(t *testing.T, day time.Time) standIns {
	t.Helper()
	s := ubuntu(t)
	s.filesAt(t, day)
	s.running(t, day.Add(-48*time.Hour), day.Add(time.Minute))
	return s
}

// inodesAt says when each path's inode changed, for a test, which can't set
// that on a real file; a path not named changed with its content.
func inodesAt(inodes map[string]time.Time) func(fs.FS, string) time.Time {
	return func(root fs.FS, name string) time.Time {
		at := written(root, name)
		if c, ok := inodes[name]; ok && c.After(at) {
			return c
		}
		return at
	}
}

// The read never says the sshd that listens has read its files: with nothing
// against the values it says nothing, which isn't a confirmation. Files and
// times can't prove it, and three reviews each found another way they lie.
func TestTheReadNeverSaysSSHDHasReadItsFiles(t *testing.T) {
	day := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	now := day.Add(24 * time.Hour)
	// With nothing against the values it says so, and says nothing more; where it
	// can't read what it would warn from, it says neither.
	nothing := dockwellv1.SshLoaded_SSH_LOADED_NOTHING_TO_WARN_OF
	for name, prepare := range map[string]func(t *testing.T, s standIns){
		"nothing changed since the PID file": func(*testing.T, standIns) {},
		"no PID file": func(t *testing.T, s standIns) {
			if err := os.Remove(filepath.Join(string(s), "run", "sshd.pid")); err != nil {
				t.Fatal(err)
			}
		},
		"a PID file that names another process":               func(t *testing.T, s standIns) { s.file(t, "/run/sshd.pid", "4242\n", 0o644) },
		"a PID file from a clock that ran ahead":              func(t *testing.T, s standIns) { s.at(t, "/run/sshd.pid", now.Add(365*24*time.Hour)) },
		"no listening socket, and a PID file that names sshd": func(t *testing.T, s standIns) { s.file(t, "/proc/net/tcp", "header\n", 0o644) },
		"no sshd at all": func(t *testing.T, s standIns) {
			s.file(t, "/proc/net/tcp", "header\n", 0o644)
			s.file(t, "/proc/100/comm", "nginx\n", 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := stood(t, day)
			prepare(t, s)
			got := readSSHAt(t, s, inodesAt(nil), now)
			want := dockwellv1.SshLoaded_SSH_LOADED_UNSPECIFIED
			// Without a listening socket, the PID file still names the sshd that is.
			if name == "nothing changed since the PID file" || name == "no listening socket, and a PID file that names sshd" {
				want = nothing
			}
			if got.GetLoaded() != want || len(got.GetChangedFiles()) != 0 || got.GetLoadedTime() != nil || got.GetReloadCommand() != "" {
				t.Errorf("loaded %v, why %q, changed %v, at %v", got.GetLoaded(), got.GetLoadedWhy(), got.GetChangedFiles(), got.GetLoadedTime())
			}
			if want == 0 && got.GetLoadedWhy() == "" {
				t.Error("no warning, and nothing says what couldn't be read")
			}
			for _, src := range got.GetSetIn() {
				if src.GetChanged() {
					t.Errorf("%s is marked as not in effect, with nothing said against it", src.GetKeyword())
				}
			}
			if got.GetPasswordLogin() != yes || fmt.Sprint(got.GetPorts()) != "[2222]" {
				t.Errorf("the values the files say: password %v, ports %v", got.GetPasswordLogin(), got.GetPorts())
			}
		})
	}
	t.Run("no /proc to read", func(t *testing.T) {
		if got := readSSH(t, ubuntu(t)); got.GetLoaded() != 0 || len(got.GetChangedFiles()) != 0 {
			t.Errorf("loaded %v, changed %v", got.GetLoaded(), got.GetChangedFiles())
		}
	})
}

// What the read can show, it warns of: a file, a link or a folder that changed
// after the sshd that listens last wrote its PID file, which it does each time
// it reads its files. Each value from such a file is marked, and no other.
func TestTheReadWarnsOfFilesChangedAfterSSHDLastReadThem(t *testing.T) {
	day := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	now := day.Add(24 * time.Hour)
	later := day.Add(2 * time.Hour)
	dropIn := "/etc/ssh/sshd_config.d/50-cloud-init.conf"
	for name, c := range map[string]struct {
		prepare func(t *testing.T, s standIns)
		inodes  map[string]time.Time
		want    string
	}{
		"a drop-in written after":                        {func(t *testing.T, s standIns) { s.at(t, dropIn, later) }, nil, "[" + dropIn + "]"},
		"a drop-in rewritten with its written time kept": {func(*testing.T, standIns) {}, map[string]time.Time{dropIn: later}, "[" + dropIn + "]"},
		"a drop-in taken away, which changes its folder": {func(t *testing.T, s standIns) { s.at(t, "/etc/ssh/sshd_config.d", later) }, nil, "[/etc/ssh/sshd_config.d/]"},
		"sshd_config put in place by a rename":           {func(t *testing.T, s standIns) { s.at(t, "/etc/ssh", later) }, nil, "[/etc/ssh/]"},
	} {
		t.Run(name, func(t *testing.T) {
			s := stood(t, day)
			c.prepare(t, s)
			got := readSSHAt(t, s, inodesAt(c.inodes), now)
			if got.GetLoaded() != stale || fmt.Sprint(got.GetChangedFiles()) != c.want || !got.GetLoadedTime().AsTime().Equal(day.Add(time.Minute)) {
				t.Fatalf("loaded %v, why %q, changed %v, at %v", got.GetLoaded(), got.GetLoadedWhy(), got.GetChangedFiles(), got.GetLoadedTime().AsTime())
			}
			for _, src := range got.GetSetIn() {
				if want := slices.Contains(got.GetChangedFiles(), src.GetFile()); src.GetChanged() != want {
					t.Errorf("%s from %s marked changed %t", src.GetKeyword(), src.GetFile(), src.GetChanged())
				}
			}
		})
	}

	// For real, with the times the file system keeps.
	t.Run("sshd_config a link pointed at another old file", func(t *testing.T) {
		s := ubuntu(t)
		s.file(t, "/etc/ssh/other_config", "PasswordAuthentication yes\n", 0o644)
		conf := filepath.Join(string(s), "etc", "ssh", "sshd_config")
		if err := os.Remove(conf); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("other_config", conf); err != nil {
			t.Fatal(err)
		}
		s.filesAt(t, day)
		s.at(t, "/etc/ssh/other_config", day)
		s.running(t, day.Add(-48*time.Hour), day.Add(time.Minute))
		got := readSSHAt(t, s, nil, time.Time{})
		if got.GetLoaded() != stale || !slices.Contains(got.GetChangedFiles(), "/etc/ssh/sshd_config") {
			t.Errorf("loaded %v, why %q, changed %v", got.GetLoaded(), got.GetLoadedWhy(), got.GetChangedFiles())
		}
	})

	// The last look's A: a link above the file pointed elsewhere, to files as old as the first.
	t.Run("a link above sshd_config pointed at another folder", func(t *testing.T) {
		root := t.TempDir()
		old := time.Now().Add(-240 * time.Hour)
		for _, v := range []string{"v1", "v2"} {
			dir := filepath.Join(root, "opt", v)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "sshd_config"), []byte("PasswordAuthentication no\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{filepath.Join(dir, "sshd_config"), dir} {
				if err := os.Chtimes(p, old, old); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := os.MkdirAll(filepath.Join(root, "etc", "ssh"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("v1", filepath.Join(root, "opt", "current")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../opt/current/sshd_config", filepath.Join(root, "etc", "ssh", "sshd_config")); err != nil {
			t.Fatal(err)
		}
		fsys := os.DirFS(root)
		if got := fmt.Sprint(linksOnTheWay(fsys, "etc/ssh/sshd_config", 0)); got != "[etc/ssh/sshd_config opt/current]" {
			t.Fatalf("links on the way: %s", got)
		}
		time.Sleep(1100 * time.Millisecond)
		mark := time.Now()
		time.Sleep(1100 * time.Millisecond)
		// Pointed at v2, whose file is as old as v1's: nothing of the file's own moves.
		if err := os.Remove(filepath.Join(root, "opt", "current")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("v2", filepath.Join(root, "opt", "current")); err != nil {
			t.Fatal(err)
		}
		if at := written(fsys, "/etc/ssh/sshd_config"); at.After(mark) {
			t.Fatalf("the file's own written time moved: %v", at)
		}
		if at := fileChanged(fsys, "/etc/ssh/sshd_config"); !at.After(mark) {
			t.Errorf("a link above the file pointed elsewhere reads as changed at %v, before it was", at)
		}
	})

	// The last look's M2: a file removed from a folder a wildcard matched.
	t.Run("a file taken from a folder a wildcard Include matches", func(t *testing.T) {
		s := newStandIns(t)
		s.file(t, "/etc/ssh/sshd_config", "Include /etc/ssh/conf.*/a.conf\n", 0o644)
		s.file(t, "/etc/ssh/conf.x/a.conf", "PasswordAuthentication no\n", 0o644)
		s.file(t, "/etc/ssh/conf.y/b.conf", "Port 2222\n", 0o644)
		files := readSSHDConfig(os.DirFS(string(s)), written)
		var paths []string
		for _, w := range files.walked {
			paths = append(paths, w.path)
		}
		for _, want := range []string{"/etc/ssh/conf.x", "/etc/ssh/conf.y", "/etc/ssh/conf.x/a.conf", "/etc/ssh"} {
			if !slices.Contains(paths, want) {
				t.Errorf("walked %v, without %s", paths, want)
			}
		}
	})
}

// The sshd that listens may not be the sshd the agent asked, or may have been
// started with what outranks its files. Then what the files say may not be
// what it runs with at all, which is said as that, in a sentence of its own.
func TestTheReadSaysWhenTheListenerIsntRunFromTheseFiles(t *testing.T) {
	day := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	now := day.Add(24 * time.Hour)
	// The last look's B and H2: a self-built sshd listens, and the agent asks the system's.
	t.Run("another sshd binary", func(t *testing.T) {
		s := stood(t, day)
		exe := filepath.Join(string(s), "proc", "100", "exe")
		if err := os.Remove(exe); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/usr/local/sbin/sshd", exe); err != nil {
			t.Fatal(err)
		}
		got := readSSHAt(t, s, inodesAt(nil), now)
		if got.GetLoaded() != apart || !strings.Contains(got.GetLoadedWhy(), "It's /usr/local/sbin/sshd, not the /usr/sbin/sshd these were read with.") {
			t.Errorf("loaded %v, why %q", got.GetLoaded(), got.GetLoadedWhy())
		}
	})
	t.Run("the same sshd, replaced by an upgrade since it started", func(t *testing.T) {
		s := stood(t, day)
		exe := filepath.Join(string(s), "proc", "100", "exe")
		if err := os.Remove(exe); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/usr/sbin/sshd (deleted)", exe); err != nil {
			t.Fatal(err)
		}
		if got := readSSHAt(t, s, inodesAt(nil), now); got.GetLoaded() != dockwellv1.SshLoaded_SSH_LOADED_NOTHING_TO_WARN_OF {
			t.Errorf("loaded %v, why %q", got.GetLoaded(), got.GetLoadedWhy())
		}
	})
	for name, cmdline := range map[string]string{
		"-o":        "sshd: /usr/sbin/sshd -D -o PasswordAuthentication=yes [listener] 0 of 10-100 startups",
		"-o run on": "sshd: /usr/sbin/sshd -D -oPasswordAuthentication=yes [listener] 0 of 10-100 startups",
		"-f":        "/usr/sbin/sshd\x00-D\x00-f\x00/etc/ssh/other_config",
		"-p":        "sshd: /usr/sbin/sshd -Dp 2222 [listener] 0 of 10-100 startups",
		"-C":        "sshd: /usr/sbin/sshd -D -C user=deploy [listener] 0 of 10-100 startups",
	} {
		t.Run("started with "+name, func(t *testing.T) {
			s := stood(t, day)
			s.file(t, "/proc/100/cmdline", cmdline, 0o644)
			got := readSSHAt(t, s, inodesAt(nil), now)
			if got.GetLoaded() != apart || !strings.Contains(got.GetLoadedWhy(), "was started with -") {
				t.Errorf("loaded %v, why %q", got.GetLoaded(), got.GetLoadedWhy())
			}
		})
	}
	t.Run("a command line that can't be read says nothing", func(t *testing.T) {
		s := stood(t, day)
		s.file(t, "/proc/100/cmdline", "", 0o644)
		if got := readSSHAt(t, s, inodesAt(nil), now); got.GetLoaded() != dockwellv1.SshLoaded_SSH_LOADED_NOTHING_TO_WARN_OF {
			t.Errorf("loaded %v, why %q", got.GetLoaded(), got.GetLoadedWhy())
		}
	})
}

// Every Match block that isn't "Match all" is listed, whatever it sets, as the
// values are only the defaults for every connection; and a keyword in quotes,
// which sshd takes as it takes one without, doesn't hide what a block sets
// (the last look's L2).
func TestEveryMatchBlockIsListedAndAQuotedKeywordIsRead(t *testing.T) {
	s := newStandIns(t)
	s.file(t, "/etc/ssh/sshd_config", `"PermitRootLogin" no
Match User deploy
	"PasswordAuthentication" yes
Match Address 198.51.100.0/24
	X11Forwarding yes
Match all
	Port 2200
`, 0o644)
	files := readSSHDConfig(os.DirFS(string(s)), written)
	var blocks []string
	for _, m := range files.matches {
		blocks = append(blocks, fmt.Sprintf("%s@%d%v", m.GetCriteria(), m.GetLine(), m.GetSets()))
	}
	if got := strings.Join(blocks, " "); got != "User deploy@2[PasswordAuthentication] Address 198.51.100.0/24@4[]" {
		t.Errorf("Match blocks: %s", got)
	}
	if got := sources(files.setIn); got != "PermitRootLogin=no@/etc/ssh/sshd_config:1 Port=2200@/etc/ssh/sshd_config:7" {
		t.Errorf("set in %s", got)
	}
}

// The warning says how to reload the sshd that listens, by how it runs: its
// systemd service, its OpenRC service, or the signal sshd reloads on.
func TestTheReloadCommandIsTheListenersOwn(t *testing.T) {
	day := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	now := day.Add(24 * time.Hour)
	for cgroup, want := range map[string]string{
		"0::/system.slice/ssh.service\n": "systemctl reload ssh.service",
		"12:pids:/system.slice/sshd.service\n1:name=systemd:/system.slice/sshd.service\n": "systemctl reload sshd.service",
		"0::/openrc.sshd\n":                   "rc-service sshd reload",
		"0::/\n":                              `kill -HUP "$(cat /run/sshd.pid)"`,
		"0::/system.slice/x;rm -rf.service\n": `kill -HUP "$(cat /run/sshd.pid)"`,
		"":                                    `kill -HUP "$(cat /run/sshd.pid)"`,
	} {
		s := stood(t, day)
		if cgroup != "" {
			s.file(t, "/proc/100/cgroup", cgroup, 0o644)
		}
		s.at(t, "/etc/ssh/sshd_config.d/50-cloud-init.conf", day.Add(time.Hour))
		got := readSSHAt(t, s, inodesAt(nil), now)
		if got.GetLoaded() != stale || got.GetReloadCommand() != want {
			t.Errorf("cgroup %q: loaded %v, reload %q, want %q", cgroup, got.GetLoaded(), got.GetReloadCommand(), want)
		}
	}
	if got := readSSHAt(t, stood(t, day), inodesAt(nil), now); got.GetReloadCommand() != "" {
		t.Errorf("with nothing to warn of, a reload command %q", got.GetReloadCommand())
	}
}

// The narrow check's M1: where /usr/sbin links to bin, as on Arch and on Fedora
// from 42, the kernel names the listener /usr/bin/sshd, which is the sshd the
// agent asked as /usr/sbin/sshd, and not another.
func TestAMergedUsrSbinIsTheSameSSHD(t *testing.T) {
	day := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	now := day.Add(24 * time.Hour)
	s := stood(t, day)
	usr := filepath.Join(string(s), "usr")
	if err := os.Rename(filepath.Join(usr, "sbin", "sshd"), filepath.Join(usr, "bin", "sshd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(usr, "sbin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bin", filepath.Join(usr, "sbin")); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(string(s), "proc", "100", "exe")
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/sshd", exe); err != nil {
		t.Fatal(err)
	}
	if got := readSSHAt(t, s, inodesAt(nil), now); got.GetLoaded() != dockwellv1.SshLoaded_SSH_LOADED_NOTHING_TO_WARN_OF {
		t.Errorf("loaded %v, why %q", got.GetLoaded(), got.GetLoadedWhy())
	}
	if got := resolved(os.DirFS(string(s)), "/usr/sbin/sshd"); got != "/usr/bin/sshd" {
		t.Errorf("resolved %q", got)
	}
	// Another sshd is still another, through the link or not.
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/local/sbin/sshd", exe); err != nil {
		t.Fatal(err)
	}
	if got := readSSHAt(t, s, inodesAt(nil), now); got.GetLoaded() != apart {
		t.Errorf("another sshd: loaded %v, why %q", got.GetLoaded(), got.GetLoadedWhy())
	}
}

// socketHolds makes systemd hold port in ssh.socket, on the Ubuntu stand.
func (s standIns) socketHolds(t *testing.T, ports ...string) {
	t.Helper()
	var listen strings.Builder
	for _, p := range ports {
		listen.WriteString("echo '[::]:" + p + " (Stream)'\n")
	}
	s.tool(t, "/usr/bin/systemctl", `case "$*" in
"is-active ssh.socket sshd.socket") echo active; echo inactive; exit 3 ;;
"show ssh.socket --property=Listen --value") `+listen.String()+` ;;
*) exit 9 ;;
esac
`)
}

// The narrow check's M3: on Ubuntu from 22.10, systemd holds SSH's port in
// ssh.socket, which its generator builds from sshd_config at daemon-reload. A
// Port moved in the files reaches nothing until then, whatever sshd read, so
// the warning goes by the ports alone, says the command that moves them, and
// stays after a reload of sshd has written its PID file again.
func TestAPortSystemdStillHoldsIsWarnedOfUntilTheSocketTakesTheFilesPorts(t *testing.T) {
	day := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	now := day.Add(24 * time.Hour)
	portFile := "/etc/ssh/sshd_config.d/60-port.conf"
	t.Run("the socket still holds 22, after sshd reloaded", func(t *testing.T) {
		s := stood(t, day)
		s.socketHolds(t, "22")
		s.file(t, "/usr/lib/systemd/system-generators/sshd-socket-generator", "#!/bin/sh\n", 0o755)
		s.at(t, "/run/sshd.pid", day.Add(3*time.Hour))
		got := readSSHAt(t, s, inodesAt(nil), now)
		if got.GetLoaded() != stale || fmt.Sprint(got.GetChangedFiles()) != "["+portFile+"]" || got.GetLoadedTime() != nil ||
			got.GetReloadCommand() != "systemctl daemon-reload && systemctl restart ssh.socket" {
			t.Fatalf("loaded %v, changed %v, at %v, command %q", got.GetLoaded(), got.GetChangedFiles(), got.GetLoadedTime(), got.GetReloadCommand())
		}
		for _, src := range got.GetSetIn() {
			if src.GetChanged() != (src.GetKeyword() == "Port") {
				t.Errorf("%s marked changed %t", src.GetKeyword(), src.GetChanged())
			}
		}
	})
	t.Run("a socket that names its own ports gets no command", func(t *testing.T) {
		s := stood(t, day)
		s.socketHolds(t, "22")
		got := readSSHAt(t, s, inodesAt(nil), now)
		if got.GetLoaded() != stale || got.GetReloadCommand() != "" {
			t.Errorf("loaded %v, command %q", got.GetLoaded(), got.GetReloadCommand())
		}
	})
	t.Run("the socket holds 22 beside 2222", func(t *testing.T) {
		s := stood(t, day)
		s.socketHolds(t, "22", "2222")
		if got := readSSHAt(t, s, inodesAt(nil), now); got.GetLoaded() != stale {
			t.Errorf("loaded %v", got.GetLoaded())
		}
	})
	t.Run("once it holds the files' ports, the PID file says the rest", func(t *testing.T) {
		s := stood(t, day)
		s.socketHolds(t, "2222")
		if got := readSSHAt(t, s, inodesAt(nil), now); got.GetLoaded() != dockwellv1.SshLoaded_SSH_LOADED_NOTHING_TO_WARN_OF {
			t.Errorf("loaded %v, changed %v", got.GetLoaded(), got.GetChangedFiles())
		}
		s.at(t, "/etc/ssh/sshd_config.d/50-cloud-init.conf", day.Add(time.Hour))
		got := readSSHAt(t, s, inodesAt(nil), now)
		if got.GetLoaded() != stale || got.GetReloadCommand() != `kill -HUP "$(cat /run/sshd.pid)"` {
			t.Errorf("loaded %v, command %q", got.GetLoaded(), got.GetReloadCommand())
		}
	})
	t.Run("no files set Port, and the socket holds another", func(t *testing.T) {
		s := stood(t, day)
		s.socketHolds(t, "2200")
		if err := os.Remove(filepath.Join(string(s), "etc", "ssh", "sshd_config.d", "60-port.conf")); err != nil {
			t.Fatal(err)
		}
		got := readSSHAt(t, s, inodesAt(nil), now)
		if got.GetLoaded() != stale || fmt.Sprint(got.GetChangedFiles()) != "[/etc/ssh/sshd_config]" {
			t.Errorf("loaded %v, changed %v", got.GetLoaded(), got.GetChangedFiles())
		}
	})
}

// The narrow check's L3: with PidFile none, sshd keeps no PID file, so an old
// one at the default place says nothing of when it last read its files.
func TestPidFileNoneTakesNoTime(t *testing.T) {
	day := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	now := day.Add(24 * time.Hour)
	s := stood(t, day)
	s.tool(t, "/usr/sbin/sshd", `case "$1" in
-T) cat <<'EOF'
`+ubuntuSSHD+`pidfile none
EOF
;;
esac
`)
	s.at(t, "/etc/ssh/sshd_config.d/50-cloud-init.conf", day.Add(time.Hour))
	got := readSSHAt(t, s, inodesAt(nil), now)
	if got.GetLoaded() != 0 || len(got.GetChangedFiles()) != 0 || !strings.Contains(got.GetLoadedWhy(), "keeps no PID file") {
		t.Errorf("loaded %v, why %q, changed %v", got.GetLoaded(), got.GetLoadedWhy(), got.GetChangedFiles())
	}
}

// The narrow check's L2: the signal goes to the process the PID file names as
// it's run, and a PID file whose path a shell would read otherwise is never
// named in the command.
func TestTheSignalNamesThePIDFile(t *testing.T) {
	proc := os.DirFS(t.TempDir())
	if got := reloadCommand(proc, "812", "/run/sshd.pid"); got != `kill -HUP "$(cat /run/sshd.pid)"` {
		t.Errorf("command %q", got)
	}
	for _, pidFile := range []string{"/run/ssh d.pid", "/run/$(x).pid", "run/sshd.pid", ""} {
		if got := reloadCommand(proc, "812", pidFile); got != "kill -HUP 812" {
			t.Errorf("%q: command %q", pidFile, got)
		}
	}
}
