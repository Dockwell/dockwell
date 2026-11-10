// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
)

// sshWait bounds each command the agent reads SSH with.
const sshWait = 10 * time.Second

// sshReadWait bounds a whole read.
const sshReadWait = 30 * time.Second

// sshTools are the tools the agent reads SSH with, at the paths the
// distributions put them. None is looked for on PATH.
var sshTools = map[string]hosttool.Tool{
	"sshd":      {Name: "sshd", Paths: []string{"/usr/sbin/sshd", "/usr/bin/sshd", "/sbin/sshd"}},
	"dropbear":  {Name: "dropbear", Paths: []string{"/usr/sbin/dropbear", "/usr/bin/dropbear"}},
	"systemctl": {Name: "systemctl", Paths: []string{"/usr/bin/systemctl", "/bin/systemctl"}},
}

// sshdConfig is sshd's own file, where it starts reading.
const sshdConfig = "/etc/ssh/sshd_config"

// dockwellDropIn is the drop-in Dockwell writes, which sshd reads first of its
// folder (ADR 0052).
const dockwellDropIn = "00-dockwell.conf"

// sshKeywords are the values the tab says, as sshd's files write them, by the
// lower case sshd reads them in. ChallengeResponseAuthentication is what
// OpenSSH before 8.7 called KbdInteractiveAuthentication.
var sshKeywords = map[string]string{
	"passwordauthentication":          "PasswordAuthentication",
	"kbdinteractiveauthentication":    "KbdInteractiveAuthentication",
	"challengeresponseauthentication": "ChallengeResponseAuthentication",
	"permitrootlogin":                 "PermitRootLogin",
	"port":                            "Port",
}

// sshReader says how SSH lets people in, reading what sshd's files say now,
// and the files that set it, and changes nothing (ADR 0052).
type sshReader struct {
	tools *hosttool.Runner
	// root is the server's files, where /etc/ssh is: the stand-ins' folder in a
	// test.
	root fs.FS
	now  func() time.Time
	// changed says when a file or folder last changed, for a test; nil reads
	// it from the file itself (fileChanged).
	changed func(root fs.FS, name string) time.Time

	reads shared[*dockwellv1.ServerSsh]
}

// newSSHReader reads with the agent's tools, or, with none given, as in a
// test, runs none of the machine's.
func newSSHReader(cfg Config) *sshReader {
	tools := cfg.Tools
	if tools == nil {
		tools = &hosttool.Runner{Off: true}
	}
	root := "/"
	if tools.Root != "" {
		root = tools.Root
	}
	return &sshReader{tools: tools, root: os.DirFS(root)}
}

// read reads SSH as it is now. Two tabs asking at once share one read.
func (r *sshReader) read(ctx context.Context) (*dockwellv1.ServerSsh, error) {
	if r == nil {
		return nil, errors.New("this agent can't read SSH")
	}
	return r.reads.read(ctx, sshReadWait, r.readNow)
}

// readNow reads what's there. What it can't read, it says in what it returns.
func (r *sshReader) readNow(ctx context.Context) *dockwellv1.ServerSsh {
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	out := &dockwellv1.ServerSsh{Time: timestamppb.New(now())}
	if _, ok := r.tools.Find(sshTools["sshd"]); !ok {
		out.Unreadable = "This server has no OpenSSH sshd, so Dockwell reads nothing of SSH here."
		if _, ok := r.tools.Find(sshTools["dropbear"]); ok {
			out.Unreadable = "This server runs Dropbear, not OpenSSH, and Dockwell reads only OpenSSH's sshd for now."
		}
		return out
	}
	res, err := r.tools.Run(ctx, sshTools["sshd"], sshWait, nil, "-T")
	if err == nil && res.Code != 0 {
		err = errors.New(hosttool.LastLines(res.Errs+"\n"+string(res.Out), 1))
	}
	if err != nil {
		out.Unreadable = fmt.Sprintf("sshd -T, which says the values sshd's files set, failed: %v. Run sshd -T on the server as root to see why.", err)
		return out
	}
	pidFile, said := effective(string(res.Out), out)
	if !said {
		out.Reset()
		out.Time = timestamppb.New(now())
		out.Unreadable = "sshd -T, which says the values sshd's files set, printed none of them, so Dockwell can't tell how SSH lets people in. Run sshd -T on the server as root to see what it prints."
		return out
	}
	out.Version = r.version(ctx)
	out.SocketUnit = r.socketUnit(ctx)
	socket := r.socket(ctx, out.SocketUnit)
	files := readSSHDConfig(r.root, r.changed)
	out.SetIn, out.Matches, out.DropInFolder = files.setIn, files.matches, files.dropIns
	asked, _ := r.tools.Find(sshTools["sshd"])
	if r.tools.Root != "" {
		// A test's tools stand in a folder of their own; the listener's is the server's path.
		asked = "/" + strings.TrimPrefix(strings.TrimPrefix(asked, r.tools.Root), "/")
	}
	r.warn(out, files.walked, pidFile, asked, socket, now())
	if out.DropInFolder != "" {
		if _, err := fs.Stat(r.root, strings.TrimPrefix(path.Join(out.DropInFolder, dockwellDropIn), "/")); err == nil {
			out.DockwellDropIn = true
		}
	}
	return out
}

// sshAnswer is yes, no, or neither where the line that would say isn't there.
func sshAnswer(value string) dockwellv1.SshAnswer {
	switch value {
	case "yes":
		return dockwellv1.SshAnswer_SSH_ANSWER_YES
	case "no":
		return dockwellv1.SshAnswer_SSH_ANSWER_NO
	}
	return dockwellv1.SshAnswer_SSH_ANSWER_UNSPECIFIED
}

// effective reads sshd -T's lines, a lower-case keyword and its value each:
// what sshd's files say now, which the sshd that listens runs with only once
// it has read them. A value whose line isn't there stays unset, which is
// "can't tell", never "no". It returns the PID file sshd writes, and whether
// sshd -T said any of the values at all.
func effective(text string, out *dockwellv1.ServerSsh) (pidFile string, said bool) {
	values := map[string][]string{}
	for line := range strings.SplitSeq(text, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			values[key] = append(values[key], strings.TrimSpace(value))
		}
	}
	// A keyword sshd -T prints twice with two values says nothing Dockwell can
	// stand on: it reads as not said, which is "can't tell".
	first := func(key string) string {
		v := values[key]
		if len(v) == 0 || slices.ContainsFunc(v, func(other string) bool { return other != v[0] }) {
			return ""
		}
		return v[0]
	}
	// Keyboard-interactive asks for a password only through PAM: without it,
	// sshd on Linux has nothing to ask with, though the keyword defaults to yes.
	// An sshd built without PAM prints no usepam line. OpenSSH before 8.7 calls
	// the keyword ChallengeResponseAuthentication; either line says it.
	password := sshAnswer(first("passwordauthentication"))
	kbd := sshAnswer(first("kbdinteractiveauthentication"))
	if kbd == dockwellv1.SshAnswer_SSH_ANSWER_UNSPECIFIED {
		kbd = sshAnswer(first("challengeresponseauthentication"))
	}
	switch {
	case password == dockwellv1.SshAnswer_SSH_ANSWER_YES:
		out.PasswordLogin = dockwellv1.SshAnswer_SSH_ANSWER_YES
	case kbd == dockwellv1.SshAnswer_SSH_ANSWER_YES && first("usepam") == "yes":
		out.PasswordLogin = dockwellv1.SshAnswer_SSH_ANSWER_YES
	case password == dockwellv1.SshAnswer_SSH_ANSWER_NO && kbd != dockwellv1.SshAnswer_SSH_ANSWER_UNSPECIFIED:
		out.PasswordLogin = dockwellv1.SshAnswer_SSH_ANSWER_NO
	}
	out.KeyLogin = sshAnswer(first("pubkeyauthentication"))
	switch first("permitrootlogin") {
	case "yes":
		out.RootLogin = dockwellv1.SshRootLogin_SSH_ROOT_LOGIN_YES
	case "prohibit-password", "without-password":
		out.RootLogin = dockwellv1.SshRootLogin_SSH_ROOT_LOGIN_KEYS_ONLY
	case "forced-commands-only":
		out.RootLogin = dockwellv1.SshRootLogin_SSH_ROOT_LOGIN_FORCED_COMMANDS_ONLY
	case "no":
		out.RootLogin = dockwellv1.SshRootLogin_SSH_ROOT_LOGIN_NO
	}
	for _, p := range values["port"] {
		if n, err := strconv.ParseUint(p, 10, 16); err == nil && n > 0 && !slices.Contains(out.Ports, uint32(n)) {
			out.Ports = append(out.Ports, uint32(n))
		}
	}
	for _, key := range []string{"allowusers", "allowgroups", "denyusers", "denygroups"} {
		for _, v := range values[key] {
			out.AccessLists = append(out.AccessLists, key+" "+v)
		}
	}
	for key, name := range map[string]string{
		"authorizedkeyscommand": "AuthorizedKeysCommand", "authorizedprincipalscommand": "AuthorizedPrincipalsCommand",
		"trustedusercakeys": "TrustedUserCAKeys", "authorizedprincipalsfile": "AuthorizedPrincipalsFile",
	} {
		if v := first(key); v != "" && v != "none" {
			out.Unchecked = append(out.Unchecked, name)
		}
	}
	if first("gssapiauthentication") == "yes" {
		out.Unchecked = append(out.Unchecked, "GSSAPIAuthentication")
	}
	if first("hostbasedauthentication") == "yes" {
		out.Unchecked = append(out.Unchecked, "HostbasedAuthentication")
	}
	slices.Sort(out.Unchecked)
	said = len(values["passwordauthentication"])+len(values["permitrootlogin"])+len(values["port"])+len(values["pubkeyauthentication"]) > 0
	return first("pidfile"), said
}

// version is the SSH server and its version, such as "OpenSSH_9.6p1
// Ubuntu-3ubuntu13.5", as sshd -V says it. An sshd older than -V prints it
// in its usage, which it prints for -V too.
func (r *sshReader) version(ctx context.Context) string {
	res, err := r.tools.Run(ctx, sshTools["sshd"], sshWait, nil, "-V")
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(res.Errs+"\n"+string(res.Out), "\n") {
		if i := strings.Index(line, "OpenSSH_"); i >= 0 {
			v, _, _ := strings.Cut(line[i:], ",")
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// socketUnit is the systemd socket unit that holds SSH's port, when one does.
func (r *sshReader) socketUnit(ctx context.Context) string {
	if _, ok := r.tools.Find(sshTools["systemctl"]); !ok {
		return ""
	}
	units := []string{"ssh.socket", "sshd.socket"}
	// is-active says a line for each unit, and exits non-zero unless all are.
	res, err := r.tools.Run(ctx, sshTools["systemctl"], sshWait, nil, append([]string{"is-active"}, units...)...)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(res.Out)), "\n")
	for i, unit := range units {
		if i < len(lines) && strings.TrimSpace(lines[i]) == "active" {
			return unit
		}
	}
	return ""
}

// sshSocket is what systemd holds for sshd in a socket unit, where it does.
type sshSocket struct {
	unit string
	// ports are the ports it listens on, as systemctl shows them; none when
	// it couldn't say.
	ports []uint32
	// generated says Ubuntu's generator builds its ports from sshd_config at
	// each daemon-reload, as on Ubuntu 22.10 and later; elsewhere the unit
	// names its ports itself.
	generated bool
}

// socketGenerators are where Ubuntu's sshd-socket-generator is, and the
// drop-in it writes for ssh.socket.
var socketGenerators = []string{
	"usr/lib/systemd/system-generators/sshd-socket-generator",
	"lib/systemd/system-generators/sshd-socket-generator",
	"run/systemd/generator/ssh.socket.d/addresses.conf",
}

// socket reads the ports systemd holds for sshd in unit, if any.
func (r *sshReader) socket(ctx context.Context, unit string) sshSocket {
	out := sshSocket{unit: unit}
	if unit == "" {
		return out
	}
	res, err := r.tools.Run(ctx, sshTools["systemctl"], sshWait, nil, "show", unit, "--property=Listen", "--value")
	if err != nil || res.Code != 0 {
		return out
	}
	// "0.0.0.0:22 (Stream)" and "[::]:22 (Stream)", a line each, or "22 (Stream)".
	for line := range strings.SplitSeq(string(res.Out), "\n") {
		addr, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		var port uint64
		if ap, err := netip.ParseAddrPort(addr); err == nil {
			port = uint64(ap.Port())
		} else if n, err := strconv.ParseUint(addr, 10, 16); err == nil {
			port = n
		}
		if port > 0 && !slices.Contains(out.ports, uint32(port)) {
			out.ports = append(out.ports, uint32(port))
		}
	}
	out.generated = unit == "ssh.socket" && slices.ContainsFunc(socketGenerators, func(name string) bool {
		_, err := fs.Stat(r.root, name)
		return err == nil
	})
	return out
}

// sshdFiles is what sshd's files set, read as sshd reads them.
type sshdFiles struct {
	setIn   []*dockwellv1.SshSource
	matches []*dockwellv1.SshMatch
	// dropIns is the folder sshd_config includes, where Dockwell would write.
	dropIns string
	// walked is every file sshd reads, sshd_config and each file an Include
	// names, and each folder an Include reads from, with when it last changed.
	walked []walkedFile
}

// fileChanged is when a file or folder last changed, as far as the server can
// say: the later of when its content was written and when its inode changed,
// of the name itself and of what it leads to where it's a link. The time of
// the content alone can be set back, or carried over, by cp -p, rsync -a, a
// restore or touch; the inode's can't, and a rename, a new link or a link
// pointed elsewhere moves it. Zero when it can't be read.
func fileChanged(root fs.FS, name string) time.Time {
	name = strings.TrimPrefix(name, "/")
	var latest time.Time
	take := func(info fs.FileInfo, err error) bool {
		if err != nil {
			return false
		}
		for _, t := range []time.Time{info.ModTime(), inodeChanged(info)} {
			if t.After(latest) {
				latest = t
			}
		}
		return true
	}
	if !take(fs.Stat(root, name)) {
		return time.Time{}
	}
	// What the name itself is, where the file system can say: a link's own times.
	take(fs.Lstat(root, name))
	// And every link on the way to it: one pointed elsewhere above the file
	// changes what the name reads, and none of the file's own times.
	for _, link := range linksOnTheWay(root, name, 0) {
		take(fs.Lstat(root, link))
	}
	return latest
}

// linksOnTheWay are the links met resolving name in root, each as a path in
// root: a part of the name that is a link, and the parts of where it leads,
// followed no more than a few deep. It reads nothing where the file system
// can't say what a link holds.
func linksOnTheWay(root fs.FS, name string, depth int) []string {
	var out []string
	if depth > 8 {
		return nil
	}
	at := ""
	for part := range strings.SplitSeq(name, "/") {
		if part == "" || part == "." {
			continue
		}
		at = path.Join(at, part)
		info, err := fs.Lstat(root, at)
		if err != nil {
			return out
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		out = append(out, at)
		target, err := fs.ReadLink(root, at)
		if err != nil {
			return out
		}
		// Where it leads, from the server's root or from the folder the link is in.
		if path.IsAbs(target) {
			at = strings.TrimPrefix(path.Clean(target), "/")
		} else {
			at = path.Join(path.Dir(at), target)
		}
		if strings.HasPrefix(at, "..") {
			return out
		}
		out = append(out, linksOnTheWay(root, at, depth+1)...)
	}
	return out
}

// inodeChanged is when a file's inode last changed, where the file system
// says, or the zero time.
func inodeChanged(info fs.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Ctim.Unix())
	}
	return time.Time{}
}

// walkedFile is a file or folder sshd's configuration comes from.
type walkedFile struct {
	path string
	// changed is when it last changed. Zero when it couldn't be read.
	changed time.Time
	// dir says it's a folder, which a warning names with "/" after it, as a
	// folder's own name reads as a file's.
	dir bool
}

// readSSHDConfig reads sshd_config as sshd does: line by line, each Include
// read where it stands, its files in name order, relative to /etc/ssh. The
// first value of each keyword wins, except Port, of which each adds one. A
// Match block lasts until the next Match or the end of the file it's in, and
// no further: sshd puts back the Match state of the file an Include stands in
// once it has read the files it names. What a block sets is said apart. A file
// it can't read is left out, as sshd -T has already said what the files set.
func readSSHDConfig(root fs.FS, changed func(root fs.FS, name string) time.Time) sshdFiles {
	if changed == nil {
		changed = fileChanged
	}
	var out sshdFiles
	seen := map[string]bool{}
	var match *dockwellv1.SshMatch
	var read func(file string, depth int)
	var walk func(file string)
	walk = func(file string) {
		if slices.ContainsFunc(out.walked, func(w walkedFile) bool { return w.path == file }) {
			return
		}
		info, err := fs.Stat(root, strings.TrimPrefix(file, "/"))
		out.walked = append(out.walked, walkedFile{path: file, changed: changed(root, file), dir: err == nil && info.IsDir()})
		// The folder it's in says what the file can't: one put in its place by a
		// rename, with an older time of its own, or taken away.
		if dir := path.Dir(file); dir != file && dir != "/" && dir != "/etc" {
			walk(dir)
		}
	}
	read = func(file string, depth int) {
		if depth > 16 {
			return
		}
		walk(file)
		f, err := root.Open(strings.TrimPrefix(file, "/"))
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		for n := 1; sc.Scan(); n++ {
			key, value := sshdLine(sc.Text())
			switch key {
			case "":
			case "include":
				for _, pattern := range strings.Fields(value) {
					pattern = strings.Trim(pattern, `"`)
					if !path.IsAbs(pattern) {
						pattern = path.Join("/etc/ssh", pattern)
					}
					if file == sshdConfig && out.dropIns == "" && path.Dir(pattern) == "/etc/ssh/sshd_config.d" {
						out.dropIns = path.Dir(pattern)
					}
					// A file taken out of a folder changes the folder alone: each folder
					// the pattern reads from, where its folder part has a wildcard too.
					dirs := []string{path.Dir(pattern)}
					if strings.ContainsAny(dirs[0], "*?[") {
						found, _ := fs.Glob(root, strings.TrimPrefix(dirs[0], "/"))
						dirs = dirs[:0]
						for _, d := range found {
							dirs = append(dirs, "/"+d)
						}
					}
					for _, dir := range dirs {
						if info, err := fs.Stat(root, strings.TrimPrefix(dir, "/")); err == nil && info.IsDir() {
							walk(dir)
						}
					}
					found, _ := fs.Glob(root, strings.TrimPrefix(pattern, "/"))
					for _, name := range found {
						// A file read in starts inside the Match block its Include
						// stands in, and a Match block of its own ends with it.
						outer := match
						read("/"+name, depth+1)
						match = outer
					}
				}
			case "match":
				match = nil
				if !strings.EqualFold(strings.TrimSpace(value), "all") {
					// Listed whatever it sets: a block Dockwell misreads still shows.
					match = &dockwellv1.SshMatch{File: file, Line: uint32(n), Criteria: strings.TrimSpace(value)}
					out.matches = append(out.matches, match)
				}
			default:
				name, tracked := sshKeywords[key]
				switch {
				case !tracked:
				case match != nil:
					if !slices.Contains(match.Sets, name) {
						match.Sets = append(match.Sets, name)
					}
				case key == "port" || !seen[key]:
					seen[key] = true
					out.setIn = append(out.setIn, &dockwellv1.SshSource{Keyword: name, File: file, Line: uint32(n), Value: firstWord(value)})
				}
			}
		}
	}
	read(sshdConfig, 0)
	return out
}

// sshdLine is a line's keyword, in lower case, and the rest, with a comment or
// a blank line read as no keyword. A keyword and its value may stand apart
// with spaces or an equals sign.
func sshdLine(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", ""
	}
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return strings.ToLower(strings.Trim(line, `"`)), ""
	}
	value := strings.TrimLeft(line[i:], " \t")
	value = strings.TrimLeft(strings.TrimPrefix(value, "="), " \t")
	// sshd takes a keyword in quotes as it takes one without.
	return strings.ToLower(strings.Trim(line[:i], `"`)), value
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return strings.Trim(f[0], `"`)
	}
	return ""
}
