// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"bytes"
	"context"
	"io/fs"
	"strconv"
	"strings"
	"syscall"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// cpuUsage is what a container's cgroup said its CPU time was, at a sample, and
// which cgroup said it: a restart that keeps the container's ID makes a new one
// (#852).
type cpuUsage struct {
	at   time.Time
	usec uint64
	ino  uint64
}

// dockerPlaces are where a rootful Docker puts a container's cgroup, under
// systemd's driver and cgroupfs', read first: only root can make a folder there.
func dockerPlaces(id string) [2]string {
	return [2]string{"system.slice/docker-" + id + ".scope", "docker/" + id}
}

const (
	// pidAsksPerSample is how many containers a sample asks Docker of at most,
	// and pidAskTimeout how long each ask may take, as a sample waits for them.
	pidAsksPerSample = 4
	pidAskTimeout    = 2 * time.Second
	// pidRetryFirst is how long a container whose cgroup wasn't found waits
	// before the next try, twice as long after each that fails, up to pidRetryMost.
	pidRetryFirst = time.Minute
	pidRetryMost  = time.Hour
)

// pidCgroup is what's known of a container in neither of Docker's usual places:
// its process and the cgroup that process is in, or, after a try that found
// none, how many have failed and when to try again.
type pidCgroup struct {
	pid     int
	dir     string
	fails   int
	retryAt time.Time
}

// containerUsage reads what each running container uses from its cgroup, for an
// app's figures beside the server's (#797). The IDs are the ones the health
// monitor's latest list of Docker's has, so it asks Docker nothing: four small
// files of the kernel's for each, cpu.stat, memory.current, memory.stat and
// memory.max.
// It reads cgroup v2 alone, under systemd's driver ("system.slice/docker-<id>.scope")
// or cgroupfs' ("docker/<id>"), and elsewhere where the container's own process
// says (#852); a host without it reads as none.
func (c *Collector) containerUsage(now time.Time) []*dockwellv1.ContainerUsage {
	if c.Containers == nil {
		return nil
	}
	ids := c.Containers()
	seen := make(map[string]cpuUsage, len(ids))
	running := make(map[string]bool, len(ids))
	asks := 0
	var out []*dockwellv1.ContainerUsage
	for _, id := range ids {
		if !containerIDish(id) {
			continue
		}
		running[id] = true
		dir, current, ok := c.cgroupOf(id)
		if !ok {
			dir, current, ok = c.cgroupByPID(id, now, &asks)
		}
		if !ok {
			continue
		}
		u := &dockwellv1.ContainerUsage{ContainerId: id}
		if usec, ok := usageUsec(c.cgroupFile(dir + "/cpu.stat")); ok {
			ino := c.cgroupIno(dir)
			seen[id] = cpuUsage{at: now, usec: usec, ino: ino}
			// A cgroup made again since, as a restart that keeps the ID makes one,
			// counts from 0: its time can pass the last run's within one interval,
			// and the difference would mix two runs (#852).
			if p, ok := c.prevCPU[id]; ok && usec >= p.usec && ino == p.ino {
				if elapsed := now.Sub(p.at).Microseconds(); elapsed > 0 {
					u.CpuPercent = float64(usec-p.usec) / float64(elapsed) * 100
				}
			}
		}
		// As docker stats says it: what it holds, less the file cache the kernel can
		// take back at once (#848's review, M3).
		held, _ := number(current)
		if cache, ok := statOf(c.cgroupFile(dir+"/memory.stat"), "inactive_file"); ok && cache < held {
			held -= cache
		} else if ok {
			held = 0
		}
		u.MemoryBytes = held
		u.MemoryLimitBytes, _ = number(c.cgroupFile(dir + "/memory.max")) // "max" reads as none
		out = append(out, u)
	}
	// Only the containers that still run keep a sample to measure from, and what's known of their process.
	c.prevCPU = seen
	for id := range c.byPID {
		if !running[id] {
			delete(c.byPID, id)
		}
	}
	return out
}

// cgroupOf is where a container's cgroup is under either of Docker's drivers,
// with its memory.current, read once to find it (L2).
func (c *Collector) cgroupOf(id string) (string, []byte, bool) {
	if !containerIDish(id) {
		return "", nil, false
	}
	for _, dir := range dockerPlaces(id) {
		if current := c.cgroupFile(dir + "/memory.current"); len(current) > 0 {
			return dir, current, true
		}
	}
	return "", nil, false
}

// cgroupByPID finds the cgroup of a container in neither of Docker's usual
// places from its own process: rootless Docker's, a daemon's or a service's
// cgroup parent, and OpenRC's unified mount (#852). Docker's inspect says its
// PID, and /proc/<pid>/cgroup names the cgroup the kernel keeps it in, so a
// folder someone made elsewhere with the container's name is never read.
//
// What's found is checked at each sample, as a process that ended may leave its
// PID to another: the PID's cgroup must still be the one found, which has the
// container's ID in its name. Otherwise Docker is asked again. A try that finds
// nothing, as where /proc can't be read, waits a minute before the next, and
// twice as long after each that fails, up to an hour. A sample asks Docker of
// pidAsksPerSample containers at most.
func (c *Collector) cgroupByPID(id string, now time.Time, asks *int) (string, []byte, bool) {
	if c.ContainerPID == nil {
		return "", nil, false
	}
	if c.byPID == nil {
		c.byPID = map[string]pidCgroup{}
	}
	known := c.byPID[id]
	if known.dir != "" {
		if dir, ok := c.cgroupOfPID(id, known.pid); ok && dir == known.dir {
			if current := c.cgroupFile(dir + "/memory.current"); len(current) > 0 {
				return dir, current, true
			}
		}
		// Its process ended, or its PID is another's now.
		known = pidCgroup{}
		c.byPID[id] = known
	}
	if now.Before(known.retryAt) || *asks >= pidAsksPerSample {
		return "", nil, false
	}
	*asks++
	ctx, cancel := context.WithTimeout(context.Background(), pidAskTimeout)
	pid, err := c.ContainerPID(ctx, id)
	cancel()
	if err == nil && pid > 0 {
		if dir, ok := c.cgroupOfPID(id, pid); ok {
			if current := c.cgroupFile(dir + "/memory.current"); len(current) > 0 {
				c.byPID[id] = pidCgroup{pid: pid, dir: dir}
				return dir, current, true
			}
		}
	}
	c.byPID[id] = pidCgroup{fails: known.fails + 1, retryAt: now.Add(min(pidRetryFirst<<min(known.fails, 6), pidRetryMost))}
	return "", nil, false
}

// cgroupOfPID is the cgroup v2 folder in the tree that a process is in, cut at
// the folder named for the container's ID, as a container that makes cgroups of
// its own keeps them below it. A process in no folder named for the ID isn't
// the container's, as one whose PID was reused isn't.
func (c *Collector) cgroupOfPID(id string, pid int) (string, bool) {
	var at string
	found := false
	for line := range bytes.Lines(c.read("proc/" + strconv.Itoa(pid) + "/cgroup")) {
		if rest, ok := bytes.CutPrefix(line, []byte("0::")); ok {
			at, found = strings.TrimSpace(string(rest)), true
			break
		}
	}
	if !found {
		return "", false
	}
	parts := strings.Split(strings.Trim(at, "/"), "/")
	end := -1
	for i, part := range parts {
		if part == "docker-"+id+".scope" || part == id {
			end = i
		}
	}
	if end < 0 {
		return "", false
	}
	dir := strings.Join(parts[:end+1], "/")
	// On a hybrid host, as OpenRC's, cgroup v2 is mounted at unified/.
	if !c.cgroupHas("cgroup.controllers") && c.cgroupHas("unified/cgroup.controllers") {
		dir = "unified/" + dir
	}
	if !fs.ValidPath(dir) {
		return "", false
	}
	return dir, true
}

// cgroupHas reports a file of the cgroup tree that's there.
func (c *Collector) cgroupHas(name string) bool {
	tree := c.cgroupTree()
	if tree == nil {
		return false
	}
	_, err := fs.Stat(tree, name)
	return err == nil
}

// cgroupTree is the cgroup tree: Cgroups, or the root's sys/fs/cgroup.
func (c *Collector) cgroupTree() fs.FS {
	if c.Cgroups != nil {
		return c.Cgroups
	}
	tree, err := fs.Sub(c.Root, "sys/fs/cgroup")
	if err != nil {
		return nil
	}
	return tree
}

// cgroupIno is a cgroup folder's inode, which a cgroup made again has new, or 0
// where the tree doesn't say.
func (c *Collector) cgroupIno(dir string) uint64 {
	tree := c.cgroupTree()
	if tree == nil {
		return 0
	}
	fi, err := fs.Stat(tree, dir)
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// statOf reads one key's number from a cgroup's memory.stat: "key value" lines.
func statOf(b []byte, key string) (uint64, bool) {
	prefix := []byte(key + " ")
	for line := range bytes.Lines(b) {
		if rest, ok := bytes.CutPrefix(line, prefix); ok {
			return number(rest)
		}
	}
	return 0, false
}

// cgroupFile reads a file of the cgroup tree: Cgroups, or the root's sys/fs/cgroup.
func (c *Collector) cgroupFile(name string) []byte {
	if c.Cgroups == nil {
		return c.read("sys/fs/cgroup/" + name)
	}
	b, err := fs.ReadFile(c.Cgroups, name)
	if err != nil {
		return nil
	}
	return b
}

// containerIDish reports an ID Docker gives a container: 64 lowercase hex digits,
// so a path made of it stays where it should.
func containerIDish(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// usageUsec reads the CPU time a cgroup has used, in microseconds, from cpu.stat.
func usageUsec(b []byte) (uint64, bool) {
	for line := range bytes.Lines(b) {
		if rest, ok := bytes.CutPrefix(line, []byte("usage_usec ")); ok {
			return number(rest)
		}
	}
	return 0, false
}

// number reads one whole number, as a cgroup's file holds it.
func number(b []byte) (uint64, bool) {
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil
}
