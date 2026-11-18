// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"bytes"
	"io/fs"
	"path"
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

// cgroupPlace is where Docker puts a container's cgroup: the folder that holds
// it, and its name around the container's ID, "docker-<id>.scope" under
// systemd's driver and "<id>" under cgroupfs'.
type cgroupPlace struct {
	parent, prefix, suffix string
}

func (p cgroupPlace) of(id string) string { return p.parent + "/" + p.prefix + id + p.suffix }

// dockerPlaces are where a rootful Docker puts containers' cgroups, under
// systemd's driver or cgroupfs', read first.
var dockerPlaces = []cgroupPlace{{"system.slice", "docker-", ".scope"}, {"docker", "", ""}}

const (
	// cgroupLookEvery is how often, at most, the cgroup tree is searched for a
	// running container that no known place holds: rootless Docker's, a daemon's
	// cgroup-parent, a service's cgroup_parent, or OpenRC's unified mount (#852).
	cgroupLookEvery = time.Minute
	// cgroupLookDirs and cgroupLookDepth bound one search: a rootless Docker's
	// cgroups are 5 deep, and a server has a few hundred.
	cgroupLookDirs  = 20_000
	cgroupLookDepth = 8
	// cgroupPlacesKept bounds what's kept of what searches found.
	cgroupPlacesKept = 16
)

// containerUsage reads what each running container uses from its cgroup, for an
// app's figures beside the server's (#797). The IDs are the ones the health
// monitor's latest list of Docker's has, so it asks Docker nothing: four small
// files of the kernel's for each, cpu.stat, memory.current, memory.stat and
// memory.max.
// It reads cgroup v2 alone, under systemd's driver ("system.slice/docker-<id>.scope")
// or cgroupfs' ("docker/<id>"), and wherever a search of the tree found one
// (#852); a host without it reads as none.
func (c *Collector) containerUsage(now time.Time) []*dockwellv1.ContainerUsage {
	if c.Containers == nil {
		return nil
	}
	ids := c.Containers()
	seen := make(map[string]cpuUsage, len(ids))
	type found struct {
		dir     string
		current []byte
	}
	at := make(map[string]found, len(ids))
	var missing []string
	for _, id := range ids {
		if !containerIDish(id) {
			continue
		}
		if dir, current, ok := c.cgroupOf(id); ok {
			at[id] = found{dir, current}
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 && now.Sub(c.cgroupLookedAt) >= cgroupLookEvery {
		c.cgroupLookedAt = now
		c.lookForCgroups(missing)
		for _, id := range missing {
			if dir, current, ok := c.cgroupOf(id); ok {
				at[id] = found{dir, current}
			}
		}
	}
	var out []*dockwellv1.ContainerUsage
	for _, id := range ids {
		f, ok := at[id]
		if !ok {
			continue
		}
		dir, current := f.dir, f.current
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
	// Only the containers that still run keep a sample to measure from.
	c.prevCPU = seen
	return out
}

// cgroupOf is where a container's cgroup is, under either of Docker's drivers or
// a place a search found, with its memory.current, read once to find it (L2).
func (c *Collector) cgroupOf(id string) (string, []byte, bool) {
	if !containerIDish(id) {
		return "", nil, false
	}
	for _, places := range [][]cgroupPlace{dockerPlaces, c.cgroupPlaces} {
		for _, p := range places {
			dir := p.of(id)
			if current := c.cgroupFile(dir + "/memory.current"); len(current) > 0 {
				return dir, current, true
			}
		}
	}
	return "", nil, false
}

// lookForCgroups searches the cgroup tree, within its bounds, for the cgroups of
// the containers given, and keeps the place each is in, so the next sample reads
// them, and any other container there, without a search (#852). A cgroup counts
// where it has a memory.current, as cgroup v2's do; a v1 hierarchy's don't.
func (c *Collector) lookForCgroups(ids []string) {
	tree := c.cgroupTree()
	if tree == nil {
		return
	}
	names := make(map[string]cgroupPlace, 2*len(ids))
	for _, id := range ids {
		names["docker-"+id+".scope"] = cgroupPlace{prefix: "docker-", suffix: ".scope"}
		names[id] = cgroupPlace{}
	}
	dirs := 0
	_ = fs.WalkDir(tree, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			// A folder that can't be read is passed over.
			return fs.SkipDir
		}
		if !d.IsDir() {
			return nil
		}
		dirs++
		if dirs > cgroupLookDirs {
			return fs.SkipAll
		}
		if name == "." {
			return nil
		}
		if strings.Count(name, "/") >= cgroupLookDepth {
			return fs.SkipDir
		}
		p, ok := names[d.Name()]
		if !ok {
			return nil
		}
		// What's under a container's cgroup is its own, and not another's.
		if b, err := fs.ReadFile(tree, name+"/memory.current"); err == nil && len(b) > 0 && len(c.cgroupPlaces) < cgroupPlacesKept {
			p.parent = path.Dir(name)
			if p.parent != "." && !c.knows(p) {
				c.cgroupPlaces = append(c.cgroupPlaces, p)
			}
		}
		return fs.SkipDir
	})
}

// knows reports a place already read.
func (c *Collector) knows(p cgroupPlace) bool {
	for _, places := range [][]cgroupPlace{dockerPlaces, c.cgroupPlaces} {
		for _, q := range places {
			if q == p {
				return true
			}
		}
	}
	return false
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
