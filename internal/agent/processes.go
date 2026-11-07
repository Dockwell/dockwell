// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// Timing of the process list (ADR 0041).
const (
	// forgetProcessesAfter is how long the agent keeps what the last list read,
	// to say each process's CPU on the next: a page asks every 2 s while it shows
	// them, and once nobody does, the agent holds nothing.
	forgetProcessesAfter = 30 * time.Second
	// containerNamesFor is how long the names of containers stand, so a page
	// that lists every 2 s asks Docker every 10 s at most.
	containerNamesFor = 10 * time.Second
	// endGrace is how long a process has to end on SIGTERM before SIGKILL.
	endGrace = 10 * time.Second
)

// processes lists what runs on the server and ends a process, only when asked.
type processes struct {
	collector *collect.Collector
	docker    *docker.Client
	self      int
	now       func() time.Time
	// For a test: pidfdOpen opens a pidfd, unix.PidfdOpen unless set; opened runs
	// once it has, before the process is checked, as a PID that gives way to
	// another then would; and signal sends a signal through the pidfd.
	pidfdOpen func(pid int, flags int) (int, error)
	opened    func(pid uint32)
	signal    func(pidfd int, sig syscall.Signal) error

	// mu guards what's below, and is never held while Docker or a container's
	// files are read, which either can make wait.
	mu      sync.Mutex
	prev    *collect.ProcTimes
	forget  *time.Timer
	names   map[string]*dockwellv1.ProcessContainer
	namedAt time.Time
	// policies are containers' restart policies, by full ID: each is asked of
	// Docker once, as a container's seldom changes.
	policies map[string]string
}

func newProcesses(c *collect.Collector, d *docker.Client) *processes {
	return &processes{collector: c, docker: d, self: os.Getpid(), now: time.Now}
}

// list reads what runs, with each process's CPU since the last list.
func (p *processes) list(ctx context.Context) (*dockwellv1.ProcessList, error) {
	if p == nil {
		return nil, errors.New("this agent can't list processes")
	}
	now := p.now()
	names := p.containerNames(ctx, now)
	p.mu.Lock()
	prev := p.prev
	p.mu.Unlock()
	// Each container's users, read once a list, through its first process.
	users := map[string]map[uint32]string{}
	byID := func(id string) *dockwellv1.ProcessContainer { return names[id] }
	userIn := func(container string, pid, uid uint32) string {
		known, ok := users[container]
		if !ok {
			known = containerUsersOf(pid)
			users[container] = known
		}
		return known[uid]
	}
	list, next := p.collector.Processes(now, prev, p.self, byID, userIn)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prev = next
	if p.forget != nil {
		p.forget.Stop()
	}
	p.forget = time.AfterFunc(forgetProcessesAfter, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.prev, p.names, p.policies = nil, nil, nil
	})
	return list, nil
}

// containerNames is what Docker says of its containers, by full ID, read again
// once it's older than containerNamesFor, with each one's restart policy. Without
// Docker it's empty.
func (p *processes) containerNames(ctx context.Context, now time.Time) map[string]*dockwellv1.ProcessContainer {
	p.mu.Lock()
	if p.names != nil && now.Sub(p.namedAt) < containerNamesFor {
		names := p.names
		p.mu.Unlock()
		return names
	}
	policies := p.policies
	p.mu.Unlock()
	names := map[string]*dockwellv1.ProcessContainer{}
	known := map[string]string{}
	if p.docker != nil {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_ = p.docker.EachContainer(ctx, nil, func(c *docker.ContainerSummary) error {
			name := ""
			if len(c.Names) > 0 {
				name = strings.TrimPrefix(c.Names[0], "/")
			}
			names[c.ID] = &dockwellv1.ProcessContainer{
				Id: c.ID, Name: name, ComposeProject: c.Labels[docker.LabelProject], ComposeService: c.Labels[docker.LabelService],
			}
			return nil
		})
		for id, c := range names {
			policy, ok := policies[id]
			if !ok {
				if full, err := p.docker.InspectContainer(ctx, id); err == nil {
					policy, ok = full.HostConfig.RestartPolicy.Name, true
					if policy == "" {
						policy = "no"
					}
				}
			}
			if ok {
				c.RestartPolicy, known[id] = policy, policy
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.names, p.namedAt, p.policies = names, now, known
	return names
}

// containerUsersOf reads a container's own /etc/passwd through one of its
// processes, as the container knows its users. What can't be read names nobody.
func containerUsersOf(pid uint32) map[uint32]string {
	dir, err := unix.Open("/proc/"+strconv.FormatUint(uint64(pid), 10), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil
	}
	defer unix.Close(dir)
	return containerUsers(dir)
}

// containerUsers reads a container's own /etc/passwd through dir, a handle on
// one of its processes' /proc directories. The path is resolved inside the
// container's root, so a link it holds can't lead out of it, and only a regular
// file is read: a FIFO would hold the agent up, and a device node would be
// opened by root on the server, both whatever the container put there.
func containerUsers(dir int) map[uint32]string {
	root, err := unix.Openat(dir, "root", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil
	}
	defer unix.Close(root)
	path, err := unix.Openat2(root, "etc/passwd", &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return nil
	}
	defer unix.Close(path)
	var st unix.Stat_t
	if err := unix.Fstat(path, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil
	}
	// Opened again through the handle, for reading, which still never waits.
	fd, err := unix.Open("/proc/self/fd/"+strconv.Itoa(path), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil
	}
	f := os.NewFile(uintptr(fd), "passwd")
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, collect.MaxPasswd))
	if err != nil {
		return nil
	}
	return collect.ParsePasswd(b)
}

// gone is a process that has gone, or whose PID another has now.
func gone() error {
	const why = "that process isn't running any more, so there's nothing to end. The list shows what runs now"
	return resource.PlainlyBecause(collect.ReasonProcessGone, why, errors.New(why))
}

// protected is a process Dockwell won't end, and why.
func protected(why string) error {
	return resource.PlainlyBecause(collect.ReasonProcessProtected, why, errors.New(why))
}

// end ends a process: SIGTERM, then SIGKILL if it's still there after endGrace.
// It refuses one that isn't the process named, and what Dockwell needs.
func (p *processes) end(ctx context.Context, pid uint32, start uint64) (was *dockwellv1.Process, killed bool, err error) {
	if p == nil {
		return nil, false, errors.New("this agent can't end processes")
	}
	if pid == 0 || pid > maxPID {
		return nil, false, fmt.Errorf("there's no PID %d: Linux gives PIDs from 1 to %d", pid, maxPID)
	}
	h, err := p.open(pid, start)
	if err != nil {
		return nil, false, err
	}
	defer h.close()
	was = h.found.Process
	if c := was.GetContainer(); c != nil {
		if known := p.containerNames(ctx, p.now())[c.GetId()]; known != nil {
			was.Container = known
		}
		was.User = containerUsers(h.dir)[was.GetUid()]
	}
	killed, err = p.signalEnd(ctx, h)
	return was, killed, err
}

// maxPID is the highest PID Linux gives, its pid_max at most. Past it, as a
// signal reads it, a PID can be negative, and -1 names every process.
const maxPID = 4194304

// handle is a process opened to be ended: a pidfd, which names that process and
// no other, whatever comes of its PID, and its /proc directory, through which
// it's checked.
type handle struct {
	pid, pidfd, dir int
	found           collect.Found
}

func (h *handle) close() {
	_ = unix.Close(h.pidfd)
	_ = unix.Close(h.dir)
}

// open opens the process pid names, checks it's the one that started at start,
// and refuses what Dockwell won't end. With the pidfd open first, a process found
// through its directory afterwards to have started then is the one the pidfd
// names: a later one given its PID would have started later.
func (p *processes) open(pid uint32, start uint64) (*handle, error) {
	pidfdOpen := p.pidfdOpen
	if pidfdOpen == nil {
		pidfdOpen = unix.PidfdOpen
	}
	fd, err := pidfdOpen(int(pid), 0)
	switch {
	case errors.Is(err, unix.ESRCH):
		return nil, gone()
	case errors.Is(err, unix.EINVAL):
		// A thread, which has a PID of its own and no pidfd of its own.
		if tgid := p.threadOf(pid); tgid != 0 {
			return nil, protected(fmt.Sprintf("PID %d is a thread of process %d, which only stops as a whole. End process %d instead.", pid, tgid, tgid))
		}
		return nil, fmt.Errorf("can't open process %d: %w", pid, err)
	case errors.Is(err, unix.ENOSYS):
		// A kernel before 5.3 has no pidfds, and a signal by PID alone could end
		// whatever took the PID in the meantime.
		return nil, protected(fmt.Sprintf("this server's kernel is older than 5.3, which Dockwell needs to end a process safely. End it over SSH instead, with sudo kill %d.", pid))
	case err != nil:
		return nil, fmt.Errorf("can't open process %d: %w", pid, err)
	}
	if p.opened != nil {
		p.opened(pid)
	}
	dir, err := unix.Open("/proc/"+strconv.FormatUint(uint64(pid), 10), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(fd)
		if errors.Is(err, unix.ENOENT) {
			return nil, gone()
		}
		return nil, fmt.Errorf("can't open process %d: %w", pid, err)
	}
	h := &handle{pid: int(pid), pidfd: fd, dir: dir}
	found, ok := p.collector.ProcessIn(os.DirFS("/proc/self/fd/"+strconv.Itoa(dir)), pid)
	if !ok || found.Zombie || found.Process.GetStartTicks() != start {
		h.close()
		return nil, gone()
	}
	h.found = found
	if err := p.refuse(found); err != nil {
		h.close()
		return nil, err
	}
	return h, nil
}

// threadOf is the process a thread is of, or 0 when pid isn't a thread.
func (p *processes) threadOf(pid uint32) uint32 {
	dir, err := unix.Open("/proc/"+strconv.FormatUint(uint64(pid), 10), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0
	}
	defer unix.Close(dir)
	found, ok := p.collector.ProcessIn(os.DirFS("/proc/self/fd/"+strconv.Itoa(dir)), pid)
	if !ok || found.Tgid == pid {
		return 0
	}
	return found.Tgid
}

// refuse says why Dockwell won't end a process, or nothing.
func (p *processes) refuse(f collect.Found) error {
	pid := f.Process.GetPid()
	if f.KernelThread {
		return protected(fmt.Sprintf("PID %d is a thread the kernel runs, which no signal ends.", pid))
	}
	if f.Tgid != pid {
		return protected(fmt.Sprintf("PID %d is a thread of process %d, which only stops as a whole. End process %d instead.", pid, f.Tgid, f.Tgid))
	}
	why, _ := p.collector.Protected(f, p.self)
	switch why {
	case "init":
		return protected("PID 1 starts and stops everything else on the server, so Dockwell won't end it. Restart the server instead.")
	case "dockwell":
		return protected("Dockwell's agent runs in this process, or under it, and ending it would cut this server off. Restart Dockwell instead, with systemctl restart dockwell.")
	case "ssh":
		return protected("this is the SSH server that takes new logins, and ending it would lock you out of SSH, a way back onto the server. Restart it instead, with systemctl restart ssh.")
	}
	return nil
}

// signalEnd sends SIGTERM, then SIGKILL after endGrace, through the pidfd.
func (p *processes) signalEnd(ctx context.Context, h *handle) (killed bool, err error) {
	signal := p.signal
	if signal == nil {
		signal = func(pidfd int, sig syscall.Signal) error { return unix.PidfdSendSignal(pidfd, sig, nil, 0) }
	}
	pid, start := uint32(h.pid), h.found.Process.GetStartTicks() //nolint:gosec // a PID checked against maxPID
	if err := signal(h.pidfd, syscall.SIGTERM); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return false, gone()
		}
		return false, fmt.Errorf("can't end process %d: %w", pid, err)
	}
	if p.waitGone(ctx, pid, start, endGrace) {
		return false, nil
	}
	if err := signal(h.pidfd, syscall.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return false, fmt.Errorf("process %d didn't end within %s of SIGTERM, and can't take SIGKILL: %w", pid, endGrace, err)
	}
	if !p.waitGone(ctx, pid, start, 5*time.Second) {
		return true, fmt.Errorf("process %d took SIGKILL and is still there, which a process stuck waiting on a disk or the network does until that answers", pid)
	}
	return true, nil
}

// waitGone waits up to d for a process to end, and says whether it did. One that
// has ended but whose parent hasn't read its exit yet counts as ended.
func (p *processes) waitGone(ctx context.Context, pid uint32, start uint64, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if s, ok := p.collector.ProcessStart(pid); !ok || s != start {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}
