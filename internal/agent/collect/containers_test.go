// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"context"
	"errors"
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

// pids is Docker's word on each container's process, for a test: a PID, or an
// error, by ID; and how many times it was asked.
type pids struct {
	of    map[string]int
	err   error
	asked int
}

func (p *pids) pid(_ context.Context, id string) (int, error) {
	p.asked++
	if p.err != nil {
		return 0, p.err
	}
	return p.of[id], nil
}

// A container in neither of a rootful Docker's places is found from its own
// process: Docker's inspect says its PID, and /proc/<pid>/cgroup names its
// cgroup. That covers rootless Docker, a cgroup parent of a daemon's or a
// service's, a container that makes cgroups of its own inside, and OpenRC's
// unified mount. A folder a user made elsewhere with a container's name is never
// read, as the review of #852 planted one (M1). One in the usual places asks
// Docker nothing.
func TestACgroupElsewhereIsFoundFromTheContainersProcess(t *testing.T) {
	rootful, rootless, parented, nested := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64)
	const user = "user.slice/user-1001.slice/user@1001.service/user.slice"
	tree := fstest.MapFS{"cgroup.controllers": {Data: []byte("cpu memory pids\n")}}
	cgroup(tree, "system.slice/docker-"+rootful+".scope", "100")
	cgroup(tree, user+"/docker-"+rootless+".scope", "100")
	cgroup(tree, "apps.slice/docker-"+parented+".scope", "100")
	cgroup(tree, "system.slice/docker-"+nested+".scope", "100")
	// What another user made in their own subtree, named for the rootless container, sorting first.
	planted := "user.slice/user-1000.slice/user@1000.service/evil/docker-" + rootless + ".scope"
	cgroup(tree, planted, "100")
	tree[planted+"/memory.current"] = &fstest.MapFile{Data: []byte("999999999\n")}
	root := fstest.MapFS{
		"proc/201/cgroup": {Data: []byte("0::/" + user + "/docker-" + rootless + ".scope\n")},
		"proc/301/cgroup": {Data: []byte("0::/apps.slice/docker-" + parented + ".scope\n")},
	}
	docker := &pids{of: map[string]int{rootless: 201, parented: 301}}
	c := &Collector{Root: root, Cgroups: tree, Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil },
		Containers: func() []string { return []string{rootful, rootless, parented} }, ContainerPID: docker.pid}
	got := map[string]uint64{}
	for _, u := range c.Stats(time.Unix(1_800_000_000, 0)).GetContainers() {
		got[u.GetContainerId()] = u.GetMemoryBytes()
	}
	if len(got) != 3 || got[rootless] != 1048576 || got[parented] != 1048576 {
		t.Errorf("the sample read %v, want all three at 1 MiB: the rootless one from its own cgroup, not the planted one", got)
	}
	if docker.asked != 2 {
		t.Errorf("Docker was asked %d times, want twice: never of the one in the usual place", docker.asked)
	}
	// Found once, it's read again with no ask.
	c.Stats(time.Unix(1_800_000_005, 0))
	if docker.asked != 2 {
		t.Errorf("Docker was asked %d times by the second sample, want no more", docker.asked)
	}

	// A container that makes cgroups of its own keeps its process below its own cgroup, which is read.
	nestedTree := fstest.MapFS{"cgroup.controllers": {Data: []byte("memory\n")}}
	cgroup(nestedTree, "apps.slice/docker-"+nested+".scope", "100")
	c = &Collector{Root: fstest.MapFS{"proc/401/cgroup": {Data: []byte("0::/apps.slice/docker-" + nested + ".scope/init.scope\n")}}, Cgroups: nestedTree,
		Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil }, Containers: func() []string { return []string{nested} },
		ContainerPID: (&pids{of: map[string]int{nested: 401}}).pid}
	if got := c.Stats(time.Unix(1_800_000_000, 0)).GetContainers(); len(got) != 1 {
		t.Errorf("a container with cgroups of its own read %v, want its own cgroup", got)
	}

	// OpenRC's hybrid mount: cgroup v2 at unified/, beside v1's hierarchies.
	hybrid := fstest.MapFS{"unified/cgroup.controllers": {Data: []byte("memory\n")}, "memory/docker/" + parented + "/memory.usage_in_bytes": {Data: []byte("5\n")}}
	cgroup(hybrid, "unified/docker/"+parented, "100")
	c = &Collector{Root: fstest.MapFS{"proc/301/cgroup": {Data: []byte("12:memory:/docker/" + parented + "\n0::/docker/" + parented + "\n")}}, Cgroups: hybrid,
		Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil }, Containers: func() []string { return []string{parented} },
		ContainerPID: (&pids{of: map[string]int{parented: 301}}).pid}
	if got := c.Stats(time.Unix(1_800_000_000, 0)).GetContainers(); len(got) != 1 || got[0].GetMemoryBytes() != 1048576 {
		t.Errorf("on a hybrid host the container read %v, want its unified cgroup", got)
	}
}

// A container's process is checked at each sample: one that ended may leave its
// PID to another process, which is never read as the container. Docker is asked
// again, and a new PID in the container's cgroup is read. A container Docker says
// no longer runs, or whose /proc can't be read, as on a rootless host that hides
// others' processes, reads as none, and Docker is asked again only after a wait
// that doubles (#852's review, M1).
func TestAContainersProcessIsCheckedAtEachSample(t *testing.T) {
	id := strings.Repeat("2", 64)
	dir := "user.slice/user-1001.slice/user@1001.service/user.slice/docker-" + id + ".scope"
	tree := fstest.MapFS{"cgroup.controllers": {Data: []byte("memory\n")}}
	cgroup(tree, dir, "100")
	// A login session's cgroup, which a reused PID may be in.
	cgroup(tree, "user.slice/user-1000.slice/session-3.scope", "100")
	root := fstest.MapFS{"proc/201/cgroup": {Data: []byte("0::/" + dir + "\n")}}
	docker := &pids{of: map[string]int{id: 201}}
	c := &Collector{Root: root, Cgroups: tree, Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil },
		Containers: func() []string { return []string{id} }, ContainerPID: docker.pid}
	at := time.Unix(1_800_000_000, 0)
	read := func(at time.Time) int { return len(c.Stats(at).GetContainers()) }
	if read(at) != 1 {
		t.Fatal("the first sample didn't read the container")
	}

	// It restarted: PID 201 is a login shell's now, and 202 is the container's.
	root["proc/201/cgroup"] = &fstest.MapFile{Data: []byte("0::/user.slice/user-1000.slice/session-3.scope\n")}
	root["proc/202/cgroup"] = &fstest.MapFile{Data: []byte("0::/" + dir + "\n")}
	docker.of[id] = 202
	if read(at.Add(5*time.Second)) != 1 || docker.asked != 2 {
		t.Errorf("after its PID went to another process, Docker was asked %d times, want once more, and the new PID read", docker.asked)
	}

	// Its PID is reused, and Docker still names it: the process isn't in the container's cgroup, so it reads as none.
	root["proc/202/cgroup"] = &fstest.MapFile{Data: []byte("0::/user.slice/user-1000.slice/session-3.scope\n")}
	if read(at.Add(10*time.Second)) != 0 {
		t.Error("a PID in another cgroup was read as the container")
	}
	// It waits a minute before asking again, then twice as long.
	asked := docker.asked
	if read(at.Add(15 * time.Second)); docker.asked != asked {
		t.Errorf("Docker was asked again 5 s after a try that failed")
	}
	read(at.Add(70 * time.Second))
	read(at.Add(75 * time.Second))
	if docker.asked != asked+1 {
		t.Errorf("Docker was asked %d more times in the minute after, want once", docker.asked-asked)
	}

	// /proc can't be read, as on a rootless host with hidepid: none, and no ask until the wait ends.
	delete(root, "proc/202/cgroup")
	docker.of[id] = 202
	if read(at.Add(10*time.Minute)) != 0 {
		t.Error("a container whose /proc can't be read reads as some")
	}
	// Docker fails, or says it doesn't run (PID 0): none, though /proc/0 is never read as its.
	root["proc/202/cgroup"] = &fstest.MapFile{Data: []byte("0::/" + dir + "\n")}
	docker.err = errors.New("can't reach Docker")
	asked = docker.asked
	if read(at.Add(5*time.Hour)) != 0 || docker.asked != asked+1 {
		t.Errorf("with Docker failing, the container reads as some, or Docker was asked %d times, want once", docker.asked-asked)
	}
	docker.err, docker.of[id] = nil, 0
	if read(at.Add(10*time.Hour)) != 0 || docker.asked != asked+2 {
		t.Errorf("with Docker saying it doesn't run, the container reads as some, or Docker wasn't asked")
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
