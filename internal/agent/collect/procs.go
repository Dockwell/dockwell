// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"bufio"
	"bytes"
	"cmp"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dockwell/dockwell/internal/lazyre"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Reasons a process can't be ended, as the agent says them and the control plane
// reads them.
const (
	// ReasonProcessGone is a process that has gone, or whose PID another has now.
	ReasonProcessGone = "process_gone"
	// ReasonProcessProtected is what Dockwell needs, or PID 1.
	ReasonProcessProtected = "process_protected"
)

// MaxProcesses is the most a list holds, busiest first. The rest are counted.
const MaxProcesses = 1000

// maxCommand bounds a command line, which can run to megabytes, so only its
// start, twice this, is ever read.
const maxCommand = 512

// maxPasswd bounds what's read of an /etc/passwd, a container's included, which
// whoever runs the container writes.
const maxPasswd = 256 << 10

// maxUserName bounds a user's name, as useradd does.
const maxUserName = 32

// userHZ is how many clock ticks the kernel counts a second in /proc, which is
// 100 on every architecture Linux runs on for userspace.
const userHZ = 100

// pfKThread marks a kernel thread in /proc/<pid>/stat's flags.
const pfKThread = 0x00200000

// ProcTimes is what a process list remembers of each process for the next, to
// say how much CPU each took in between: its CPU ticks by PID and start, and
// when they were read.
type ProcTimes struct {
	at    time.Time
	ticks map[procKey]uint64
}

type procKey struct {
	pid   uint32
	start uint64
}

// Process is one process as /proc says it, before it's named for people.
type process struct {
	pid, ppid, tgid uint32
	name, command   string
	uid             uint32
	ticks, start    uint64
	rss             uint64
	containerID     string
	unit            string
	kthread, zombie bool
}

// ContainerOf says what the agent knows of a container by its full ID: its name
// and labels. Nil, or a container it doesn't know, leaves the name empty.
type ContainerOf func(id string) *dockwellv1.ProcessContainer

// UserIn names a user ID as a container knows it, through one of its processes,
// or "" when it can't say.
type UserIn func(container string, pid, uid uint32) string

// Processes reads what runs on the server, with each process's CPU over the time
// since prev, which it returns the next prev for. It reads /proc and nothing
// else, so it costs nothing until it's called. self is the agent's own PID, which
// can't be ended here, nor PID 1.
func (c *Collector) Processes(now time.Time, prev *ProcTimes, self int, container ContainerOf, userIn UserIn) (*dockwellv1.ProcessList, *ProcTimes) {
	entries, err := fs.ReadDir(c.Root, "proc")
	if err != nil {
		return &dockwellv1.ProcessList{Time: timestamppb.New(now)}, nil
	}
	boot := c.bootTime(now)
	users := parsePasswd(readAtMost(c.Root, "etc/passwd", maxPasswd))
	next := &ProcTimes{at: now, ticks: map[procKey]uint64{}}
	window := time.Duration(0)
	if prev != nil && now.After(prev.at) {
		window = now.Sub(prev.at)
	}
	pageSize := uint64(os.Getpagesize()) //nolint:gosec // a page size is positive
	var found []*dockwellv1.Process
	byContainer := map[string]*dockwellv1.ProcessContainer{}
	var all []process
	byPID := map[uint32]process{}
	for _, e := range entries {
		pid, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil || !e.IsDir() {
			continue
		}
		p, ok := c.readProcess(uint32(pid))
		if !ok || p.kthread {
			continue
		}
		all = append(all, p)
		byPID[p.pid] = p
	}
	g := guard{self: self, of: func(pid uint32) (process, bool) { p, ok := byPID[pid]; return p, ok }}
	for _, p := range all {
		key := procKey{p.pid, p.start}
		next.ticks[key] = p.ticks
		out := &dockwellv1.Process{
			Pid: p.pid, ParentPid: p.ppid, Name: p.name, Command: p.command, Uid: p.uid,
			MemoryBytes: p.rss * pageSize, StartTicks: p.start, Unit: p.unit,
		}
		if !boot.IsZero() {
			out.StartTime = timestamppb.New(boot.Add(time.Duration(p.start) * time.Second / userHZ)) //nolint:gosec // ticks since boot fit
		}
		if window > 0 {
			if before, ok := prev.ticks[key]; ok && p.ticks >= before {
				out.CpuPercent = float64(p.ticks-before) / userHZ / window.Seconds() * 100
			}
		}
		if p.containerID != "" {
			pc, ok := byContainer[p.containerID]
			if !ok {
				pc = &dockwellv1.ProcessContainer{Id: p.containerID}
				if container != nil {
					if known := container(p.containerID); known != nil {
						pc = known
					}
				}
				byContainer[p.containerID] = pc
			}
			out.Container = pc
			if userIn != nil {
				out.User = userIn(p.containerID, p.pid, p.uid)
			}
		} else {
			out.User = users[p.uid]
		}
		if out.User == "" {
			if p.uid == 0 {
				out.User = "root"
			} else {
				out.User = strconv.FormatUint(uint64(p.uid), 10)
			}
		}
		out.Protected, out.Stops, out.ContainerMain = g.protected(p), stopsWith(p), g.containerMain(p)
		found = append(found, out)
	}
	slices.SortStableFunc(found, func(a, b *dockwellv1.Process) int {
		return cmp.Or(
			cmp.Compare(b.GetCpuPercent(), a.GetCpuPercent()),
			cmp.Compare(b.GetMemoryBytes(), a.GetMemoryBytes()),
			cmp.Compare(a.GetPid(), b.GetPid()),
		)
	})
	list := &dockwellv1.ProcessList{Time: timestamppb.New(now), Total: uint32(len(found))} //nolint:gosec // a count of processes fits
	if window > 0 {
		list.CpuWindow = durationpb.New(window)
	}
	list.Processes = found[:min(len(found), MaxProcesses)]
	return list, next
}

// Found is one process as ending it reads it: what a record of it needs, and what
// says whether it may be ended.
type Found struct {
	// Process is the process as a list would have it, without its CPU, and with
	// its user as the server names it.
	Process *dockwellv1.Process
	// Tgid is the process a thread is of, the PID itself for a process.
	Tgid uint32
	// KernelThread is a thread the kernel runs, which no signal ends.
	KernelThread bool
	// Zombie is a process that has ended and only waits for its parent to read how.
	Zombie bool
}

// ProcessIn reads one process from its /proc directory, as dir has it: a handle on
// that directory, which names that process and no later one given its PID. false
// for one that has gone.
func (c *Collector) ProcessIn(dir fs.FS, pid uint32) (Found, bool) {
	p, ok := readProcessIn(dir, pid)
	if !ok {
		return Found{}, false
	}
	out := &dockwellv1.Process{Pid: p.pid, ParentPid: p.ppid, Name: p.name, Command: p.command, Uid: p.uid, StartTicks: p.start, Unit: p.unit}
	if p.containerID != "" {
		out.Container = &dockwellv1.ProcessContainer{Id: p.containerID}
	} else {
		out.User = parsePasswd(readAtMost(c.Root, "etc/passwd", maxPasswd))[p.uid]
	}
	return Found{Process: out, Tgid: p.tgid, KernelThread: p.kthread, Zombie: p.zombie}, true
}

// Protected says why a process can't be ended, as a list says it, and what else
// stops with it, reading the processes above it from /proc. self is the agent's
// own PID.
func (c *Collector) Protected(f Found, self int) (protected, stops string) {
	g := guard{self: self, of: c.readProcess}
	p := process{pid: f.Process.GetPid(), ppid: f.Process.GetParentPid(), tgid: f.Tgid, name: f.Process.GetName()}
	return g.protected(p), stopsWith(p)
}

// ProcessStart says when a process started, in clock ticks after boot, and false
// for one that has gone, or has ended and only waits for its parent to read how.
func (c *Collector) ProcessStart(pid uint32) (uint64, bool) {
	p, ok := c.readProcess(pid)
	if !ok || p.zombie {
		return 0, false
	}
	return p.start, true
}

// guard says which processes can't be ended, from what of looks up by PID.
type guard struct {
	self int
	of   func(pid uint32) (process, bool)
}

// protected is why p can't be ended, or "": PID 1; the agent, any thread of it,
// and what it runs under, such as the shell, service or container that started
// it; and the SSH server that takes new logins, as ending that locks out a way
// back onto the server.
func (g guard) protected(p process) string {
	switch {
	case p.pid == 1:
		return "init"
	case g.self > 0 && (int(p.pid) == g.self || int(p.tgid) == g.self):
		return "dockwell"
	case g.underAgent(p.pid):
		return "dockwell"
	case sshd(p.name):
		if parent, ok := g.of(p.ppid); !ok || !sshd(parent.name) {
			return "ssh"
		}
	}
	return ""
}

// underAgent reports whether the agent runs under pid, however far down.
func (g guard) underAgent(pid uint32) bool {
	if g.self <= 0 {
		return false
	}
	self, ok := g.of(uint32(g.self)) //nolint:gosec // a PID, positive and at most pid_max
	// A few dozen levels at most, as a loop in what /proc says ends it.
	for at, hops := self.ppid, 0; ok && at > 1 && hops < 64; hops++ {
		if at == pid {
			return true
		}
		var up process
		if up, ok = g.of(at); ok {
			at = up.ppid
		}
	}
	return false
}

// containerMain reports whether p is the process its container runs, which the
// container's runtime shim started.
func (g guard) containerMain(p process) bool {
	if p.containerID == "" {
		return false
	}
	parent, ok := g.of(p.ppid)
	return ok && strings.HasPrefix(parent.name, "containerd-shim")
}

// sshd reports an SSH server's process: the one that listens, and those it
// starts for each login.
func sshd(name string) bool { return name == "sshd" || strings.HasPrefix(name, "sshd-") }

// stopsWith is what else stops with p: every container, with Docker's daemons,
// or its container, with the shim that runs it.
func stopsWith(p process) string {
	switch {
	case p.name == "dockerd" || p.name == "containerd":
		return "containers"
	case strings.HasPrefix(p.name, "containerd-shim"):
		return "container"
	}
	return ""
}

// bootTime is when the server booted, from /proc/uptime.
func (c *Collector) bootTime(now time.Time) time.Time {
	f := strings.Fields(string(c.read("proc/uptime")))
	if len(f) == 0 {
		return time.Time{}
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return time.Time{}
	}
	return now.Add(-time.Duration(secs * float64(time.Second)))
}

// readProcess reads one process from /proc. ok is false for one that has gone.
func (c *Collector) readProcess(pid uint32) (p process, ok bool) {
	dir, err := fs.Sub(c.Root, path.Join("proc", strconv.FormatUint(uint64(pid), 10)))
	if err != nil {
		return p, false
	}
	return readProcessIn(dir, pid)
}

// readProcessIn reads one process from its /proc directory, dir.
func readProcessIn(dir fs.FS, pid uint32) (p process, ok bool) {
	read := func(name string) []byte {
		b, err := fs.ReadFile(dir, name)
		if err != nil {
			return nil
		}
		return b
	}
	stat := read("stat")
	// The name is in parentheses and can hold anything, spaces and ")" too, so
	// what follows is after the last ")".
	open, end := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
	if open < 0 || end < open {
		return p, false
	}
	rest := strings.Fields(string(stat[end+1:]))
	// rest[0] is field 3 of proc(5), so field n is rest[n-3].
	if len(rest) < 22 {
		return p, false
	}
	field := func(n int) uint64 {
		v, _ := strconv.ParseUint(rest[n-3], 10, 64)
		return v
	}
	p.pid, p.name = pid, string(stat[open+1:end])
	p.zombie = rest[0] == "Z"
	p.ppid = uint32(field(4)) //nolint:gosec // PIDs fit in 32 bits
	p.kthread = field(9)&pfKThread != 0 || pid == 2 || p.ppid == 2
	p.ticks = field(14) + field(15)
	p.start = field(22)
	p.rss = field(24)
	p.tgid = pid
	for line := range strings.SplitSeq(string(read("status")), "\n") {
		if rest, ok := strings.CutPrefix(line, "Tgid:"); ok {
			if tgid, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 32); err == nil {
				p.tgid = uint32(tgid)
			}
		}
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				uid, _ := strconv.ParseUint(f[0], 10, 32)
				p.uid = uint32(uid)
			}
		}
	}
	// Only the start of a command line is read, as one can run to megabytes.
	cmdline := readAtMost(dir, "cmdline", maxCommand*2)
	p.command = strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))
	if len(p.command) > maxCommand {
		p.command = strings.ToValidUTF8(p.command[:maxCommand], "")
	}
	p.containerID, p.unit = cgroupOf(read("cgroup"))
	return p, true
}

// readAtMost reads the first n bytes of a file, or nothing if it can't.
func readAtMost(fsys fs.FS, name string, n int64) []byte {
	f, err := fsys.Open(name)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, n))
	return b
}

// containerID finds a Docker container's ID in a cgroup path: systemd's
// "docker-<id>.scope", or cgroupfs' "/docker/<id>".
var containerID = lazyre.Regexp(`(?:docker-|/docker/)([0-9a-f]{64})(?:\.scope)?(?:/|$)`)

// cgroupOf reads a process's cgroup: the Docker container it runs in, if any,
// and otherwise the systemd unit it runs under, such as "docker.service".
func cgroupOf(b []byte) (container, unit string) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		// "hierarchy:controllers:path"; on cgroup v2 one line, "0::/path".
		parts := strings.SplitN(sc.Text(), ":", 3)
		if len(parts) != 3 {
			continue
		}
		p := parts[2]
		if m := containerID().FindStringSubmatch(p); m != nil {
			return m[1], ""
		}
		for elem := range strings.SplitSeq(p, "/") {
			if strings.HasSuffix(elem, ".service") {
				unit = elem
			}
		}
	}
	return "", unit
}

// parsePasswd reads /etc/passwd's names by user ID. A container's is whatever its
// image wrote, so a name is at most maxUserName bytes of what prints, and one
// that holds anything else names nobody.
func parsePasswd(b []byte) map[uint32]string {
	out := map[uint32]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	// A line runs as long as the file, or the scanner would stop at the first long
	// one, and every user after it would name nobody.
	sc.Buffer(nil, max(len(b), bufio.MaxScanTokenSize)+1)
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) < 3 || strings.HasPrefix(f[0], "#") || !printable(f[0]) {
			continue
		}
		if len(f[0]) > maxUserName {
			f[0] = strings.ToValidUTF8(f[0][:maxUserName], "")
		}
		uid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		if _, ok := out[uint32(uid)]; !ok {
			out[uint32(uid)] = f[0]
		}
	}
	return out
}

// ParsePasswd reads /etc/passwd's names by user ID, for a container's own, of
// which it reads MaxPasswd bytes at most.
func ParsePasswd(b []byte) map[uint32]string { return parsePasswd(b[:min(len(b), maxPasswd)]) }

// MaxPasswd is the most of an /etc/passwd that's read.
const MaxPasswd = maxPasswd

// printable reports a name made only of what prints, as a user's is.
func printable(name string) bool {
	if name == "" || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if r < 0x21 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return false
		}
	}
	return true
}
