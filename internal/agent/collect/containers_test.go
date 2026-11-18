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

	first := c.Stats(t.Context(), at).GetContainers()
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
	next := c.Stats(t.Context(), at.Add(5*time.Second)).GetContainers()
	if got := next[0].GetCpuPercent(); got != 50 {
		t.Errorf("web used 2.5 s of CPU in 5 s, read as %v%%, want 50", got)
	}

	// One that stops leaves nothing to measure from: back, it starts again at 0.
	ids = []string{db}
	c.Stats(t.Context(), at.Add(10*time.Second))
	ids = []string{web, db}
	root["sys/fs/cgroup/system.slice/docker-"+web+".scope/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec 9000000\n")}
	if got := c.Stats(t.Context(), at.Add(15*time.Second)).GetContainers()[0].GetCpuPercent(); got != 0 {
		t.Errorf("web back after a sample away reads %v%%, want 0 until the next", got)
	}
}

// Without a monitor's list, a sample carries no containers, and reads no cgroup.
func TestASampleWithoutContainersReadsNone(t *testing.T) {
	c := &Collector{Root: fstest.MapFS{}, Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil }}
	if got := c.Stats(t.Context(), time.Unix(1_800_000_000, 0)).GetContainers(); got != nil {
		t.Errorf("containers = %v, want none", got)
	}
}

// cgroup is a container's cgroup v2 files in a tree, at dir: what it has used of
// the CPU, and its memory.
func cgroup(tree fstest.MapFS, dir, usec string) {
	if _, ok := tree[dir]; !ok {
		tree[dir] = &fstest.MapFile{Mode: fs.ModeDir | 0o755, Sys: &syscall.Stat_t{}} // root's
	}
	tree[dir+"/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec " + usec + "\n")}
	tree[dir+"/memory.current"] = &fstest.MapFile{Data: []byte("1048576\n")}
}

// owned makes a tree's folder a user's.
func owned(tree fstest.MapFS, dir string, uid uint32) {
	tree[dir] = &fstest.MapFile{Mode: fs.ModeDir | 0o755, Sys: &syscall.Stat_t{Uid: uid}}
}

// pids is Docker's word on each container's process, for a test: the PIDs in
// next first, one an ask, then a PID or an error by ID, or no answer until the
// ask's context ends; and how many times it was asked.
type pids struct {
	of    map[string]int
	next  []int
	err   error
	hang  bool
	asked int
}

func (p *pids) pid(ctx context.Context, id string) (int, error) {
	p.asked++
	if p.hang {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if p.err != nil {
		return 0, p.err
	}
	if len(p.next) > 0 {
		pid := p.next[0]
		p.next = p.next[1:]
		return pid, nil
	}
	return p.of[id], nil
}

// rootless1001 is a Docker daemon run by user 1001, as rootless Docker is.
func rootless1001() (uint32, bool) { return 1001, true }

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
	owned(tree, user+"/docker-"+rootless+".scope", 1001)
	cgroup(tree, "apps.slice/docker-"+parented+".scope", "100")
	cgroup(tree, "system.slice/docker-"+nested+".scope", "100")
	// What another user made in their own subtree, named for the rootless container, sorting first.
	planted := "user.slice/user-1000.slice/user@1000.service/evil/docker-" + rootless + ".scope"
	cgroup(tree, planted, "100")
	owned(tree, planted, 1000)
	tree[planted+"/memory.current"] = &fstest.MapFile{Data: []byte("999999999\n")}
	root := fstest.MapFS{
		"proc/201/cgroup": {Data: []byte("0::/" + user + "/docker-" + rootless + ".scope\n")},
		"proc/301/cgroup": {Data: []byte("0::/apps.slice/docker-" + parented + ".scope\n")},
	}
	docker := &pids{of: map[string]int{rootless: 201, parented: 301}}
	c := &Collector{
		Root: root, Cgroups: tree, Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil },
		Containers: func() []string { return []string{rootful, rootless, parented} }, ContainerPID: docker.pid, DaemonUID: rootless1001,
	}
	got := map[string]uint64{}
	for _, u := range c.Stats(t.Context(), time.Unix(1_800_000_000, 0)).GetContainers() {
		got[u.GetContainerId()] = u.GetMemoryBytes()
	}
	if len(got) != 3 || got[rootless] != 1048576 || got[parented] != 1048576 {
		t.Errorf("the sample read %v, want all three at 1 MiB: the rootless one from its own cgroup, not the planted one", got)
	}
	// Twice for each found: once for its PID, and once more after the read, for the same.
	if docker.asked != 4 {
		t.Errorf("Docker was asked %d times, want 4: never of the one in the usual place", docker.asked)
	}
	// Found once, it's read again with no ask.
	c.Stats(t.Context(), time.Unix(1_800_000_005, 0))
	if docker.asked != 4 {
		t.Errorf("Docker was asked %d times by the second sample, want no more", docker.asked)
	}

	// A container that makes cgroups of its own keeps its process below its own cgroup, which is read.
	nestedTree := fstest.MapFS{"cgroup.controllers": {Data: []byte("memory\n")}}
	cgroup(nestedTree, "apps.slice/docker-"+nested+".scope", "100")
	c = &Collector{
		Root: fstest.MapFS{"proc/401/cgroup": {Data: []byte("0::/apps.slice/docker-" + nested + ".scope/init.scope\n")}}, Cgroups: nestedTree,
		Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil }, Containers: func() []string { return []string{nested} },
		ContainerPID: (&pids{of: map[string]int{nested: 401}}).pid,
	}
	if got := c.Stats(t.Context(), time.Unix(1_800_000_000, 0)).GetContainers(); len(got) != 1 {
		t.Errorf("a container with cgroups of its own read %v, want its own cgroup", got)
	}

	// OpenRC's hybrid mount: cgroup v2 at unified/, beside v1's hierarchies.
	hybrid := fstest.MapFS{"unified/cgroup.controllers": {Data: []byte("memory\n")}, "memory/docker/" + parented + "/memory.usage_in_bytes": {Data: []byte("5\n")}}
	cgroup(hybrid, "unified/docker/"+parented, "100")
	c = &Collector{
		Root: fstest.MapFS{"proc/301/cgroup": {Data: []byte("12:memory:/docker/" + parented + "\n0::/docker/" + parented + "\n")}}, Cgroups: hybrid,
		Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil }, Containers: func() []string { return []string{parented} },
		ContainerPID: (&pids{of: map[string]int{parented: 301}}).pid,
	}
	if got := c.Stats(t.Context(), time.Unix(1_800_000_000, 0)).GetContainers(); len(got) != 1 || got[0].GetMemoryBytes() != 1048576 {
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
	owned(tree, dir, 1001)
	// A login session's cgroup, which a reused PID may be in.
	cgroup(tree, "user.slice/user-1000.slice/session-3.scope", "100")
	owned(tree, "user.slice/user-1000.slice/session-3.scope", 1000)
	root := fstest.MapFS{"proc/201/cgroup": {Data: []byte("0::/" + dir + "\n")}}
	docker := &pids{of: map[string]int{id: 201}}
	c := &Collector{
		Root: root, Cgroups: tree, Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil },
		Containers: func() []string { return []string{id} }, ContainerPID: docker.pid, DaemonUID: rootless1001,
	}
	at := time.Unix(1_800_000_000, 0)
	read := func(at time.Time) int { return len(c.Stats(t.Context(), at).GetContainers()) }
	if read(at) != 1 {
		t.Fatal("the first sample didn't read the container")
	}

	// It restarted: PID 201 is a login shell's now, and 202 is the container's.
	root["proc/201/cgroup"] = &fstest.MapFile{Data: []byte("0::/user.slice/user-1000.slice/session-3.scope\n")}
	root["proc/202/cgroup"] = &fstest.MapFile{Data: []byte("0::/" + dir + "\n")}
	docker.of[id] = 202
	if read(at.Add(5*time.Second)) != 1 || docker.asked != 4 {
		t.Errorf("after its PID went to another process, Docker was asked %d times, want 4, and the new PID read", docker.asked)
	}

	// Its PID is reused, and Docker still names it: the process isn't in the container's cgroup, so it reads as none.
	root["proc/202/cgroup"] = &fstest.MapFile{Data: []byte("0::/user.slice/user-1000.slice/session-3.scope\n")}
	if read(at.Add(10*time.Second)) != 0 {
		t.Error("a PID in another cgroup was read as the container")
	}
	// It waits a minute before asking again, then twice as long.
	asked := docker.asked
	read(at.Add(15 * time.Second))
	if docker.asked != asked {
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

// A lookup takes a folder only where it's root's or the Docker daemon's user's,
// and only once Docker, asked again after /proc is read, still says the
// container runs with that PID. So a PID that goes, between the two, to a local
// user's process in a folder they made and named for the container isn't read as
// the container; and once a container found that way restarts, its old PID in
// such a folder is dropped and Docker asked again (#852's re-check, N1).
func TestALookupTakesOnlyDockersFolderAndAPIDDockerStillGives(t *testing.T) {
	id := strings.Repeat("5", 64)
	own := "apps.slice/docker-" + id + ".scope"
	planted := "user.slice/user-1000.slice/user@1000.service/" + id
	tree := fstest.MapFS{"cgroup.controllers": {Data: []byte("memory\n")}}
	cgroup(tree, own, "100")
	cgroup(tree, planted, "100")
	owned(tree, planted, 1000)
	tree[planted+"/memory.current"] = &fstest.MapFile{Data: []byte("999999999\n")}
	root := fstest.MapFS{
		"proc/201/cgroup": {Data: []byte("0::/" + own + "\n")},
		"proc/666/cgroup": {Data: []byte("0::/" + planted + "\n")},
	}
	docker := &pids{of: map[string]int{id: 666}}
	// A rootful daemon: its socket is root's, so only root's folders are its.
	c := &Collector{
		Root: root, Cgroups: tree, Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil },
		Containers: func() []string { return []string{id} }, ContainerPID: docker.pid, DaemonUID: func() (uint32, bool) { return 0, true },
	}
	at := time.Unix(1_800_000_000, 0)
	memory := func(at time.Time) []uint64 {
		var got []uint64
		for _, u := range c.Stats(t.Context(), at).GetContainers() {
			got = append(got, u.GetMemoryBytes())
		}
		return got
	}
	// The PID Docker gave is a user's process now, in their folder named for the container.
	if got := memory(at); len(got) != 0 {
		t.Errorf("a PID in a user's folder named for the container read %v, want none", got)
	}

	// Docker gives 201, but asked again after the read, 202: it restarted between, so this lookup takes nothing.
	docker.next = []int{201, 202}
	if got := memory(at.Add(time.Minute)); len(got) != 0 {
		t.Errorf("a PID Docker no longer gives after the read was taken: %v", got)
	}
	// The next lookup, with Docker giving 201 both times, takes it.
	docker.of[id] = 201
	if got := memory(at.Add(3 * time.Minute)); len(got) != 1 || got[0] != 1048576 {
		t.Fatalf("the container read %v, want its own cgroup's 1 MiB", got)
	}

	// It restarts: 201 goes to the user's process in their folder, and 202 is the container's.
	root["proc/201/cgroup"] = &fstest.MapFile{Data: []byte("0::/" + planted + "\n")}
	root["proc/202/cgroup"] = &fstest.MapFile{Data: []byte("0::/" + own + "\n")}
	docker.of[id] = 202
	asked := docker.asked
	if got := memory(at.Add(4 * time.Minute)); len(got) != 1 || got[0] != 1048576 || docker.asked != asked+2 {
		t.Errorf("after a restart the container read %v with %d asks, want its own 1 MiB, and Docker asked again", got, docker.asked-asked)
	}
}

// The path is cut at the first folder named for the container's ID, the one its
// daemon made: a container that can write its cgroup can't move its process
// into a child named for its ID and have the child read as all of it (#852's
// re-check, N2).
func TestAContainersCgroupIsTheFirstNamedForIt(t *testing.T) {
	id := strings.Repeat("6", 64)
	outer := "apps.slice/docker-" + id + ".scope"
	tree := fstest.MapFS{"cgroup.controllers": {Data: []byte("memory\n")}}
	cgroup(tree, outer, "100")
	cgroup(tree, outer+"/"+id, "1")
	tree[outer+"/"+id+"/memory.current"] = &fstest.MapFile{Data: []byte("7\n")}
	c := &Collector{
		Root: fstest.MapFS{"proc/301/cgroup": {Data: []byte("0::/" + outer + "/" + id + "\n")}}, Cgroups: tree,
		Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil }, Containers: func() []string { return []string{id} },
		ContainerPID: (&pids{of: map[string]int{id: 301}}).pid,
	}
	if got := c.Stats(t.Context(), time.Unix(1_800_000_000, 0)).GetContainers(); len(got) != 1 || got[0].GetMemoryBytes() != 1048576 {
		t.Errorf("a container with a child named for its ID read %v, want all of it, 1 MiB", got)
	}
}

// A sample's lookups share one budget of 2 s, within the send loop's context: a
// Docker that doesn't answer holds a sample 2 s at most, however many containers
// are due, and none once the loop ends (#852's re-check, N3).
func TestAHungDockerHoldsASampleTwoSecondsAtMost(t *testing.T) {
	tree := fstest.MapFS{"cgroup.controllers": {Data: []byte("memory\n")}}
	var ids []string
	for i := range 6 {
		ids = append(ids, strings.Repeat(string(rune('a'+i)), 64))
	}
	docker := &pids{hang: true}
	c := &Collector{
		Root: fstest.MapFS{}, Cgroups: tree, Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil },
		Containers: func() []string { return ids }, ContainerPID: docker.pid,
	}
	began := time.Now()
	c.Stats(t.Context(), time.Unix(1_800_000_000, 0))
	if took := time.Since(began); took > 3*time.Second {
		t.Errorf("a sample with Docker hung took %v, want 2 s at most", took)
	}
	if docker.asked != 1 {
		t.Errorf("Docker was asked %d times in the sample, want once: the budget was spent", docker.asked)
	}

	// The loop has ended: a sample asks nothing.
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	c = &Collector{
		Root: fstest.MapFS{}, Cgroups: tree, Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil },
		Containers: func() []string { return ids }, ContainerPID: docker.pid,
	}
	asked := docker.asked
	began = time.Now()
	c.Stats(ended, time.Unix(1_800_000_000, 0))
	if took := time.Since(began); took > 500*time.Millisecond || docker.asked != asked {
		t.Errorf("after the loop ended, a sample took %v and asked Docker %d times, want at once and none", took, docker.asked-asked)
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
	c.Stats(t.Context(), at)

	// It restarted, and its new run's time passed the old run's within the interval.
	tree[dir] = &fstest.MapFile{Mode: fs.ModeDir, Sys: &syscall.Stat_t{Ino: 2}}
	cgroup(tree, dir, "2000000")
	if got := c.Stats(t.Context(), at.Add(5*time.Second)).GetContainers()[0].GetCpuPercent(); got != 0 {
		t.Errorf("the sample after a restart reads %v%%, want 0 until the next", got)
	}
	cgroup(tree, dir, "4500000")
	if got := c.Stats(t.Context(), at.Add(10*time.Second)).GetContainers()[0].GetCpuPercent(); got != 50 {
		t.Errorf("the new run used 2.5 s of CPU in 5 s, read as %v%%, want 50", got)
	}
}
