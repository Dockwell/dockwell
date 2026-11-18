// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"strings"
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
