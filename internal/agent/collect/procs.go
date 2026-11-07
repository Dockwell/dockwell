// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"bufio"
	"bytes"
	"cmp"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

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

// maxCommand bounds a command line, which can run to megabytes.
const maxCommand = 512

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
	pid, ppid       uint32
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
	users := parsePasswd(c.read("etc/passwd"))
	next := &ProcTimes{at: now, ticks: map[procKey]uint64{}}
	window := time.Duration(0)
	if prev != nil && now.After(prev.at) {
		window = now.Sub(prev.at)
	}
	pageSize := uint64(os.Getpagesize()) //nolint:gosec // a page size is positive
	var found []*dockwellv1.Process
	byContainer := map[string]*dockwellv1.ProcessContainer{}
	for _, e := range entries {
		pid, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil || !e.IsDir() {
			continue
		}
		p, ok := c.readProcess(uint32(pid))
		if !ok || p.kthread {
			continue
		}
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
		switch {
		case p.pid == 1:
			out.Protected = "init"
		case self > 0 && int(p.pid) == self:
			out.Protected = "dockwell"
		}
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

// Process reads one process as a list would have it, without its CPU, and with
// its user as the server names it: what a record of it needs. false for one that
// has gone.
func (c *Collector) Process(pid uint32) (*dockwellv1.Process, bool) {
	p, ok := c.readProcess(pid)
	if !ok || p.kthread {
		return nil, false
	}
	out := &dockwellv1.Process{Pid: p.pid, ParentPid: p.ppid, Name: p.name, Command: p.command, Uid: p.uid, StartTicks: p.start, Unit: p.unit}
	if p.containerID != "" {
		out.Container = &dockwellv1.ProcessContainer{Id: p.containerID}
	} else {
		out.User = parsePasswd(c.read("etc/passwd"))[p.uid]
	}
	return out, true
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
	dir := path.Join("proc", strconv.FormatUint(uint64(pid), 10))
	stat := c.read(path.Join(dir, "stat"))
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
	for line := range strings.SplitSeq(string(c.read(path.Join(dir, "status"))), "\n") {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				uid, _ := strconv.ParseUint(f[0], 10, 32)
				p.uid = uint32(uid)
			}
			break
		}
	}
	cmdline := c.read(path.Join(dir, "cmdline"))
	if len(cmdline) > maxCommand*2 {
		cmdline = cmdline[:maxCommand*2]
	}
	p.command = strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))
	if len(p.command) > maxCommand {
		p.command = strings.ToValidUTF8(p.command[:maxCommand], "")
	}
	p.containerID, p.unit = cgroupOf(c.read(path.Join(dir, "cgroup")))
	return p, true
}

// containerID finds a Docker container's ID in a cgroup path: systemd's
// "docker-<id>.scope", or cgroupfs' "/docker/<id>".
var containerID = regexp.MustCompile(`(?:docker-|/docker/)([0-9a-f]{64})(?:\.scope)?(?:/|$)`)

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
		if m := containerID.FindStringSubmatch(p); m != nil {
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

// parsePasswd reads /etc/passwd's names by user ID.
func parsePasswd(b []byte) map[uint32]string {
	out := map[uint32]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) < 3 || strings.HasPrefix(f[0], "#") {
			continue
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

// ParsePasswd reads /etc/passwd's names by user ID, for a container's own.
func ParsePasswd(b []byte) map[uint32]string { return parsePasswd(b) }
