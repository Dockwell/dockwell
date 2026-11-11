// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bufio"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// sshd reads its files as it starts and as it reloads, and never between, so
// what sshd -T says is what the files say now, not what the sshd that listens
// runs with. Nothing the agent reads of files and times can prove the two are
// the same: three rounds of review each found another way to change what sshd
// would read without moving what the agent compared. So this file never says
// they are. What it finds is used only to warn (ADR 0052):
//
//   - a file changed after the sshd that listens last wrote its PID file, which
//     it does each time it reads its files, a reload included (a reload runs
//     sshd again in place, with the same process ID and start time, and writes
//     the PID file anew);
//   - the sshd that listens isn't the sshd the agent asked, or was started with
//     a flag that sets what it runs with apart from its files.
//
// Whatever it can't read leaves nothing said. A clock that ran ahead and was
// set right, and a PID file something else touched, hide a change from it;
// only an answer from the daemon itself would not (#423).

// overridingFlag reads how a process was started, and returns the first of
// its flags that sets what sshd runs with apart from its files: -o, -f, -p
// or -C, alone or run together with others, as in -Dp. False when the command
// line can't be read. The listener's title keeps its arguments (sshd.c:
// "sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups").
func overridingFlag(proc fs.FS, pid string) (string, bool) {
	b, err := fs.ReadFile(proc, pid+"/cmdline")
	if err != nil || len(b) == 0 {
		return "", false
	}
	for _, word := range strings.FieldsFunc(string(b), func(r rune) bool { return r == 0 || r == ' ' || r == '\t' }) {
		if !strings.HasPrefix(word, "-") || strings.HasPrefix(word, "--") {
			continue
		}
		// Flags run together end at the first that takes a value.
		for _, c := range word[1:] {
			if strings.ContainsRune("ofpC", c) {
				return "-" + string(c), true
			}
			if !strings.ContainsRune("46DdeiqTtGV", c) {
				break
			}
		}
	}
	return "", true
}

// sshFamily are the names of sshd's processes: the one that listens, and from
// OpenSSH 9.8 and 10.0 those it starts for each session and sign-in.
var sshFamily = []string{"sshd", "sshd-session", "sshd-auth"}

// defaultPIDFiles are where sshd writes its process ID when sshd -T names no
// file.
var defaultPIDFiles = []string{"/run/sshd.pid", "/var/run/sshd.pid"}

// sshDaemon is a process that listens for SSH.
type sshDaemon struct {
	pid string
	// started is when the process began, zero when /proc didn't say.
	started time.Time
}

// warn says what the agent can show against the values, if anything: that a
// file changed after the sshd that listens last read its files, with each
// value from such a file marked; or that the listener doesn't take what it
// runs with from these files alone. It never says the listener has read them.
// asked is the sshd the agent ran -T with.
func (r *sshReader) warn(out *dockwellv1.ServerSsh, walked []walkedFile, pidFile, asked string, now time.Time) {
	silent := func(why string) {
		out.Loaded, out.LoadedWhy, out.ChangedFiles = dockwellv1.SshLoaded_SSH_LOADED_UNSPECIFIED, why, nil
	}
	proc, err := fs.Sub(r.root, "proc")
	if err != nil {
		silent("Dockwell can't read the server's processes.")
		return
	}
	daemons, err := sshDaemons(proc)
	if err != nil {
		silent("Dockwell can't read the server's processes in /proc.")
		return
	}
	candidates := defaultPIDFiles
	if pidFile != "" && pidFile != "none" {
		candidates = []string{pidFile}
	}
	// The PID file counts where it names a process that is sshd: one found by
	// its listening socket, or, where /proc showed no socket of its, one named
	// sshd that isn't a session's.
	var wrote time.Time
	for _, name := range candidates {
		b, err := fs.ReadFile(r.root, strings.TrimPrefix(name, "/"))
		info, serr := fs.Stat(r.root, strings.TrimPrefix(name, "/"))
		if err != nil || serr != nil {
			continue
		}
		pid := strings.TrimSpace(string(b))
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		known := slices.ContainsFunc(daemons, func(d sshDaemon) bool { return d.pid == pid })
		if !known && procComm(proc, pid) == "sshd" && !slices.Contains(sshFamily, procComm(proc, procParent(proc, pid))) {
			daemons = append(daemons, sshDaemon{pid: pid, started: procStarted(proc, pid)})
			known = true
		}
		if known && len(daemons) == 1 {
			wrote = info.ModTime()
		}
		break
	}
	if len(daemons) == 0 {
		silent("No sshd that listens was found among the server's processes.")
		return
	}
	// The listener may not be the sshd the agent asked, or may have been started
	// with what outranks its files: -o sets a value, -f names another file, -p a
	// port, -C a connection to test with. Debian's unit passes whatever
	// /etc/default/ssh puts in SSHD_OPTS. Then the files read here may not be
	// what it runs with at all, which is said as that, not as files changed.
	for _, d := range daemons {
		if exe := listenerExe(proc, d.pid); exe != "" && asked != "" && exe != asked {
			out.Loaded = dockwellv1.SshLoaded_SSH_LOADED_APART
			out.LoadedWhy = "It's " + exe + ", not the " + asked + " these were read with."
			return
		}
		if flag, ok := overridingFlag(proc, d.pid); ok && flag != "" {
			out.Loaded = dockwellv1.SshLoaded_SSH_LOADED_APART
			out.LoadedWhy = "It was started with " + flag + ", which sets values apart from its files."
			return
		}
	}
	switch {
	case wrote.IsZero():
		silent("sshd keeps no PID file Dockwell can read, so nothing says when it last read its files.")
		return
	case wrote.After(now):
		silent("sshd's PID file has a time that hasn't come yet, as when the server's clock ran ahead.")
		return
	}
	for _, w := range walked {
		if !w.changed.IsZero() && w.changed.After(wrote) {
			out.ChangedFiles = append(out.ChangedFiles, w.path)
		}
	}
	if len(out.GetChangedFiles()) == 0 {
		silent("")
		return
	}
	out.Loaded, out.LoadedTime = dockwellv1.SshLoaded_SSH_LOADED_STALE, timestamppb.New(wrote)
	out.ReloadCommand = reloadCommand(proc, daemons[0].pid)
	for _, s := range out.GetSetIn() {
		s.Changed = slices.Contains(out.GetChangedFiles(), s.GetFile())
	}
}

// reloadCommand is how to reload the sshd that listens, by how it runs: the
// systemd service or OpenRC service its cgroup names, or, with neither, the
// signal sshd reloads on, sent to its process.
func reloadCommand(proc fs.FS, pid string) string {
	b, _ := fs.ReadFile(proc, pid+"/cgroup")
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		// "0::/system.slice/ssh.service", or "1:name=systemd:/system.slice/sshd.service".
		_, at, _ := strings.Cut(line, ":")
		_, at, _ = strings.Cut(at, ":")
		last := path.Base(at)
		switch {
		case strings.HasSuffix(last, ".service") && serviceName.MatchString(last):
			return "systemctl reload " + last
		case strings.HasPrefix(last, "openrc.") && serviceName.MatchString(strings.TrimPrefix(last, "openrc.")):
			return "rc-service " + strings.TrimPrefix(last, "openrc.") + " reload"
		}
	}
	return "kill -HUP " + pid
}

// serviceName is a unit's or a service's name a command may name as it is.
var serviceName = regexp.MustCompile(`^[A-Za-z0-9@._-]+$`)

// listenerExe is the program a process runs, as /proc says, without the mark
// the kernel adds once its file is replaced, as a package's upgrade does. ""
// when it can't be read.
func listenerExe(proc fs.FS, pid string) string {
	exe, err := fs.ReadLink(proc, pid+"/exe")
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(exe, " (deleted)")
}

// sshDaemons are the processes that listen for SSH: named sshd, not started
// by another of sshd's processes, and holding a TCP socket that listens. One
// that systemd handed its socket holds it too.
func sshDaemons(proc fs.FS) ([]sshDaemon, error) {
	listening := map[string]bool{}
	for _, file := range []string{"net/tcp", "net/tcp6"} {
		f, err := proc.Open(file)
		if err != nil {
			if file == "net/tcp6" {
				continue // the kernel has no IPv6
			}
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // the header
		for sc.Scan() {
			if fields := strings.Fields(sc.Text()); len(fields) >= 10 && fields[3] == "0A" {
				listening[fields[9]] = true
			}
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	entries, err := fs.ReadDir(proc, ".")
	if err != nil {
		return nil, err
	}
	var out []sshDaemon
	for _, e := range entries {
		pid := e.Name()
		if _, err := strconv.Atoi(pid); err != nil || procComm(proc, pid) != "sshd" {
			continue
		}
		if slices.Contains(sshFamily, procComm(proc, procParent(proc, pid))) {
			continue
		}
		fds, err := fs.ReadDir(proc, pid+"/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			rl, ok := proc.(fs.ReadLinkFS)
			if !ok {
				break
			}
			target, err := rl.ReadLink(path.Join(pid, "fd", fd.Name()))
			inode, isSocket := strings.CutPrefix(target, "socket:[")
			if err == nil && isSocket && listening[strings.TrimSuffix(inode, "]")] {
				out = append(out, sshDaemon{pid: pid, started: procStarted(proc, pid)})
				break
			}
		}
	}
	return out, nil
}

// procComm is a process's name, or "" once it's gone.
func procComm(proc fs.FS, pid string) string {
	if pid == "" {
		return ""
	}
	b, err := fs.ReadFile(proc, pid+"/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// procStatFields are a process's stat fields after its name, which stands in
// brackets and may hold anything: its state first, its parent's ID second, and
// when it started, in clock ticks after boot, twentieth.
func procStatFields(proc fs.FS, pid string) []string {
	b, err := fs.ReadFile(proc, pid+"/stat")
	if err != nil {
		return nil
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return nil
	}
	return strings.Fields(string(b[i+1:]))
}

func procParent(proc fs.FS, pid string) string {
	if f := procStatFields(proc, pid); len(f) >= 2 {
		return f[1]
	}
	return ""
}

// procTicks is how many clock ticks the kernel counts a second in /proc, which
// is 100 on every Linux Dockwell runs on, whatever the kernel's own timer.
const procTicks = 100

// procStarted is when a process began: the boot time /proc/stat says, and the
// ticks after it the process's stat says. Zero when either can't be read.
func procStarted(proc fs.FS, pid string) time.Time {
	f := procStatFields(proc, pid)
	if len(f) < 20 {
		return time.Time{}
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return time.Time{}
	}
	b, err := fs.ReadFile(proc, "stat")
	if err != nil {
		return time.Time{}
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}
			}
			return time.Unix(boot, 0).Add(time.Duration(ticks) * time.Second / procTicks)
		}
	}
	return time.Time{}
}
