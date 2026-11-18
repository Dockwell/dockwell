// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

// What each container uses comes from its cgroup, under either of Docker's
// drivers, for the containers the health monitor lists: its CPU as a share of one
// CPU since the sample before, its memory and its limit (#797).
func TestASampleCarriesWhatEachContainerUses(t *testing.T) {
	web, db, gone := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	root := fstest.MapFS{
		"sys/fs/cgroup/system.slice/docker-" + web + ".scope/cpu.stat":       {Data: []byte("usage_usec 1000000\nuser_usec 800000\n")},
		"sys/fs/cgroup/system.slice/docker-" + web + ".scope/memory.current": {Data: []byte("268435456\n")},
		"sys/fs/cgroup/system.slice/docker-" + web + ".scope/memory.max":     {Data: []byte("536870912\n")},
		"sys/fs/cgroup/system.slice/docker-" + web + ".scope/memory.stat":    {Data: []byte("anon 100000000\nfile 168435456\nactive_file 100000000\ninactive_file 68435456\n")},
		"sys/fs/cgroup/docker/" + db + "/cpu.stat":                           {Data: []byte("usage_usec 500\n")},
		"sys/fs/cgroup/docker/" + db + "/memory.current":                     {Data: []byte("1048576\n")},
		"sys/fs/cgroup/docker/" + db + "/memory.max":                         {Data: []byte("max\n")},
	}
	ids := []string{web, db, gone, "../../etc"}
	c := &Collector{Root: root, Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil }, Containers: func() []string { return ids }}
	at := time.Unix(1_800_000_000, 0)

	first := c.Stats(at).GetContainers()
	if len(first) != 2 {
		t.Fatalf("the first sample has %d containers, want web and db alone: what has no cgroup, or isn't an ID, is left out", len(first))
	}
	// What it holds is memory.current less the file cache the kernel takes back, as docker stats says it.
	if first[0].GetContainerId() != web || first[0].GetCpuPercent() != 0 || first[0].GetMemoryBytes() != 200000000 || first[0].GetMemoryLimitBytes() != 536870912 {
		t.Errorf("web's first sample = %v, want no CPU yet, 200 MB held of 512 MiB", first[0])
	}
	// With no memory.stat to read, memory.current is what it holds.
	if first[1].GetContainerId() != db || first[1].GetMemoryBytes() != 1048576 || first[1].GetMemoryLimitBytes() != 0 {
		t.Errorf("db under cgroupfs = %v, want 1 MiB and no limit", first[1])
	}

	// Five seconds on, web used half a CPU for it.
	root["sys/fs/cgroup/system.slice/docker-"+web+".scope/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec 3500000\n")}
	next := c.Stats(at.Add(5 * time.Second)).GetContainers()
	if got := next[0].GetCpuPercent(); got != 50 {
		t.Errorf("web used 2.5 s of CPU in 5 s, read as %v%%, want 50", got)
	}

	// One that stops leaves nothing to measure from: back, it starts again at 0.
	ids = []string{db}
	c.Stats(at.Add(10 * time.Second))
	ids = []string{web, db}
	root["sys/fs/cgroup/system.slice/docker-"+web+".scope/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec 9000000\n")}
	if got := c.Stats(at.Add(15 * time.Second)).GetContainers()[0].GetCpuPercent(); got != 0 {
		t.Errorf("web back after a sample away reads %v%%, want 0 until the next", got)
	}
}

// Without a monitor's list, a sample carries no containers, and reads no cgroup.
func TestASampleWithoutContainersReadsNone(t *testing.T) {
	c := &Collector{Root: fstest.MapFS{}, Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil }}
	if got := c.Stats(time.Unix(1_800_000_000, 0)).GetContainers(); got != nil {
		t.Errorf("containers = %v, want none", got)
	}
}

// cgroup is a container's cgroup v2 files in a tree, at dir: what it has used of
// the CPU, and its memory.
func cgroup(tree fstest.MapFS, dir, usec string) {
	tree[dir+"/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec " + usec + "\n")}
	tree[dir+"/memory.current"] = &fstest.MapFile{Data: []byte("1048576\n")}
}

// A container whose cgroup is in neither of a rootful Docker's places is found by
// a search of the tree: rootless Docker's, OpenRC's unified mount, a daemon's
// cgroup-parent. The place is kept, so another container there is read without
// one, and a search comes at most once a minute. A cgroup v1 hierarchy's folder,
// with no memory.current, isn't one (#852).
func TestACgroupInAnotherPlaceIsFoundAndKept(t *testing.T) {
	rootless, unified, parented, later, v1, elsewhere := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64),
		strings.Repeat("4", 64), strings.Repeat("5", 64), strings.Repeat("6", 64)
	const user = "user.slice/user-1000.slice/user@1000.service/user.slice"
	tree := fstest.MapFS{"memory/docker/" + v1 + "/memory.usage_in_bytes": {Data: []byte("1048576\n")}}
	cgroup(tree, user+"/docker-"+rootless+".scope", "100")
	cgroup(tree, "unified/docker/"+unified, "100")
	cgroup(tree, "apps.slice/docker-"+parented+".scope", "100")
	ids := []string{rootless, unified, parented, v1}
	c := &Collector{Root: fstest.MapFS{}, Cgroups: tree, Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil }, Containers: func() []string { return ids }}
	at := time.Unix(1_800_000_000, 0)

	read := func(at time.Time) map[string]bool {
		got := map[string]bool{}
		for _, u := range c.Stats(at).GetContainers() {
			got[u.GetContainerId()] = true
		}
		return got
	}
	if got := read(at); len(got) != 3 || !got[rootless] || !got[unified] || !got[parented] {
		t.Fatalf("the first sample read %v, want the rootless, unified and cgroup-parent containers alone", got)
	}

	// Another container in a place already found is read with no search.
	cgroup(tree, user+"/docker-"+later+".scope", "100")
	cgroup(tree, "elsewhere/"+elsewhere, "100")
	ids = []string{rootless, later, elsewhere}
	if got := read(at.Add(5 * time.Second)); !got[later] || got[elsewhere] {
		t.Errorf("5 s on, the sample read %v, want the one in the rootless place, and not one only a search finds", got)
	}
	// One in a new place waits for the next search, a minute after the last.
	if got := read(at.Add(time.Minute)); !got[elsewhere] {
		t.Errorf("a minute on, the sample read %v, want the one a search finds", got)
	}
}

// A container that restarts keeps its ID, but has a new cgroup, whose CPU time
// counts from 0. A sample that finds it made again starts over, rather than read
// the difference from the last run's time, which can mix two runs (#852).
func TestARestartThatKeepsTheIDStartsItsCPUOver(t *testing.T) {
	id := strings.Repeat("a", 64)
	dir := "system.slice/docker-" + id + ".scope"
	tree := fstest.MapFS{dir: {Mode: fs.ModeDir, Sys: &syscall.Stat_t{Ino: 1}}}
	cgroup(tree, dir, "1000000")
	c := &Collector{Root: fstest.MapFS{}, Cgroups: tree, Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil }, Containers: func() []string { return []string{id} }}
	at := time.Unix(1_800_000_000, 0)
	c.Stats(at)

	// It restarted, and its new run's time passed the old run's within the interval.
	tree[dir] = &fstest.MapFile{Mode: fs.ModeDir, Sys: &syscall.Stat_t{Ino: 2}}
	cgroup(tree, dir, "2000000")
	if got := c.Stats(at.Add(5 * time.Second)).GetContainers()[0].GetCpuPercent(); got != 0 {
		t.Errorf("the sample after a restart reads %v%%, want 0 until the next", got)
	}
	cgroup(tree, dir, "4500000")
	if got := c.Stats(at.Add(10 * time.Second)).GetContainers()[0].GetCpuPercent(); got != 50 {
		t.Errorf("the new run used 2.5 s of CPU in 5 s, read as %v%%, want 50", got)
	}
}
