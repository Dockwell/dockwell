// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"bytes"
	"io/fs"
	"strconv"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// cpuUsage is what a container's cgroup said its CPU time was, at a sample.
type cpuUsage struct {
	at   time.Time
	usec uint64
}

// containerUsage reads what each running container uses from its cgroup, for an
// app's figures beside the server's (#797). The IDs are the ones the health
// monitor's latest list of Docker's has, so it asks Docker nothing: four small
// files of the kernel's for each, cpu.stat, memory.current, memory.stat and
// memory.max.
// It reads cgroup v2 alone, under systemd's driver ("system.slice/docker-<id>.scope")
// or cgroupfs' ("docker/<id>"); a host without it reads as none.
func (c *Collector) containerUsage(now time.Time) []*dockwellv1.ContainerUsage {
	if c.Containers == nil {
		return nil
	}
	ids := c.Containers()
	seen := make(map[string]cpuUsage, len(ids))
	var out []*dockwellv1.ContainerUsage
	for _, id := range ids {
		dir, current, ok := c.cgroupOf(id)
		if !ok {
			continue
		}
		u := &dockwellv1.ContainerUsage{ContainerId: id}
		if usec, ok := usageUsec(c.cgroupFile(dir + "/cpu.stat")); ok {
			seen[id] = cpuUsage{at: now, usec: usec}
			if p, ok := c.prevCPU[id]; ok && usec >= p.usec {
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
	// Only the containers that still run keep a sample to measure from.
	c.prevCPU = seen
	return out
}

// cgroupOf is where a container's cgroup is, under either of Docker's drivers,
// with its memory.current, read once to find it (L2).
func (c *Collector) cgroupOf(id string) (string, []byte, bool) {
	if !containerIDish(id) {
		return "", nil, false
	}
	for _, dir := range []string{"system.slice/docker-" + id + ".scope", "docker/" + id} {
		if current := c.cgroupFile(dir + "/memory.current"); len(current) > 0 {
			return dir, current, true
		}
	}
	return "", nil, false
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
