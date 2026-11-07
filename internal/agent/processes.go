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
	// signal sends a signal to a process, for a test. Nil sends it through a pidfd.
	signal func(pid uint32, sig syscall.Signal) error

	mu      sync.Mutex
	prev    *collect.ProcTimes
	forget  *time.Timer
	names   map[string]*dockwellv1.ProcessContainer
	namedAt time.Time
}

func newProcesses(c *collect.Collector, d *docker.Client) *processes {
	return &processes{collector: c, docker: d, self: os.Getpid(), now: time.Now}
}

// list reads what runs, with each process's CPU since the last list.
func (p *processes) list(ctx context.Context) (*dockwellv1.ProcessList, error) {
	if p == nil {
		return nil, errors.New("this agent can't list processes")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	names := p.containerNames(ctx, now)
	// Each container's users, read once a list, through its first process.
	users := map[string]map[uint32]string{}
	byID := func(id string) *dockwellv1.ProcessContainer { return names[id] }
	userIn := func(container string, pid, uid uint32) string {
		known, ok := users[container]
		if !ok {
			known = containerUsers(pid)
			users[container] = known
		}
		return known[uid]
	}
	list, next := p.collector.Processes(now, p.prev, p.self, byID, userIn)
	p.prev = next
	if p.forget != nil {
		p.forget.Stop()
	}
	p.forget = time.AfterFunc(forgetProcessesAfter, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.prev, p.names = nil, nil
	})
	return list, nil
}

// containerNames is what Docker says of its containers, by full ID, read again
// once it's older than containerNamesFor. Without Docker it's empty.
func (p *processes) containerNames(ctx context.Context, now time.Time) map[string]*dockwellv1.ProcessContainer {
	if p.names != nil && now.Sub(p.namedAt) < containerNamesFor {
		return p.names
	}
	names := map[string]*dockwellv1.ProcessContainer{}
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
	}
	p.names, p.namedAt = names, now
	return names
}

// containerUsers reads a container's own /etc/passwd through its process, as the
// container knows its users. The path is resolved inside the container's root, so
// a link it holds can't lead out of it. What can't be read names nobody.
func containerUsers(pid uint32) map[uint32]string {
	root, err := unix.Open("/proc/"+strconv.FormatUint(uint64(pid), 10)+"/root", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil
	}
	defer unix.Close(root)
	fd, err := unix.Openat2(root, "etc/passwd", &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return nil
	}
	f := os.NewFile(uintptr(fd), "passwd")
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
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
	was, _ = p.collector.Process(pid)
	if was != nil && was.GetContainer() != nil {
		p.mu.Lock()
		if known := p.containerNames(ctx, p.now())[was.GetContainer().GetId()]; known != nil {
			was.Container = known
		}
		p.mu.Unlock()
		was.User = containerUsers(pid)[was.GetUid()]
	}
	killed, err = p.signalEnd(ctx, pid, start)
	return was, killed, err
}

// signalEnd sends SIGTERM, then SIGKILL after endGrace.
func (p *processes) signalEnd(ctx context.Context, pid uint32, start uint64) (killed bool, err error) {
	switch {
	case pid == 1:
		return false, protected("PID 1 starts and stops everything else on the server, so Dockwell won't end it. Restart the server instead.")
	case int(pid) == p.self:
		return false, protected("Dockwell's agent runs in this process, and ending it would cut this server off. Restart Dockwell instead, with systemctl restart dockwell.")
	}
	signal := p.signal
	if signal == nil {
		fd, err := unix.PidfdOpen(int(pid), 0)
		if err != nil {
			if errors.Is(err, unix.ESRCH) {
				return false, gone()
			}
			// A kernel before 5.3 has no pidfds: the start time is checked just before.
			signal = func(pid uint32, sig syscall.Signal) error { return syscall.Kill(int(pid), sig) }
		} else {
			defer unix.Close(fd)
			signal = func(_ uint32, sig syscall.Signal) error { return unix.PidfdSendSignal(fd, sig, nil, 0) }
		}
	}
	// With the pidfd open, the process it names can't be another, so this check
	// holds for what it signals.
	if s, ok := p.collector.ProcessStart(pid); !ok || s != start {
		return false, gone()
	}
	if err := signal(pid, syscall.SIGTERM); err != nil {
		return false, fmt.Errorf("can't end process %d: %w", pid, err)
	}
	if p.waitGone(ctx, pid, start, endGrace) {
		return false, nil
	}
	if err := signal(pid, syscall.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
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
