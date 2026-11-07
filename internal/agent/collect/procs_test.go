// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"testing/fstest"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

const containerHex = "4f2a1b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"

// stat is /proc/<pid>/stat for a process: its name, parent, flags, CPU ticks,
// start in ticks after boot, and resident pages.
func stat(pid int, name string, ppid, flags, ticks, start, rss uint64) string {
	return fmt.Sprintf("%d (%s) S %d 1 1 0 -1 %d 0 0 0 0 %d 0 0 0 20 0 1 0 %d 1000000 %d 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0\n",
		pid, name, ppid, flags, ticks, start, rss)
}

func procFixture() fstest.MapFS {
	return fstest.MapFS{
		"proc/uptime": {Data: []byte("1000.00 4000.00\n")},
		"etc/passwd":  {Data: []byte("root:x:0:0:root:/root:/bin/bash\nmira:x:1000:1000::/home/mira:/bin/bash\n")},
		// init
		"proc/1/stat":    {Data: []byte(stat(1, "systemd", 0, 0, 500, 1, 3000))},
		"proc/1/status":  {Data: []byte("Name:\tsystemd\nUid:\t0\t0\t0\t0\n")},
		"proc/1/cmdline": {Data: []byte("/sbin/init\x00splash\x00")},
		"proc/1/cgroup":  {Data: []byte("0::/init.scope\n")},
		// a kernel thread, by its flags and its parent
		"proc/2/stat":   {Data: []byte(stat(2, "kthreadd", 0, pfKThread, 0, 2, 0))},
		"proc/2/status": {Data: []byte("Uid:\t0\t0\t0\t0\n")},
		"proc/9/stat":   {Data: []byte(stat(9, "kworker/0:1-events", 2, pfKThread, 3, 9, 0))},
		// dockerd, under a unit
		"proc/812/stat":    {Data: []byte(stat(812, "dockerd", 1, 0, 2000, 300, 18000))},
		"proc/812/status":  {Data: []byte("Uid:\t0\t0\t0\t0\n")},
		"proc/812/cmdline": {Data: []byte("/usr/bin/dockerd\x00-H\x00fd://\x00")},
		"proc/812/cgroup":  {Data: []byte("0::/system.slice/docker.service\n")},
		// a process in a container, with a name a parenthesis and spaces in it
		"proc/1184/stat":    {Data: []byte(stat(1184, "postgres: walwriter (x)", 1170, 0, 6000, 5000, 80000))},
		"proc/1184/status":  {Data: []byte("Uid:\t999\t999\t999\t999\n")},
		"proc/1184/cmdline": {Data: []byte("postgres: walwriter \x00")},
		"proc/1184/cgroup":  {Data: []byte("0::/system.slice/docker-" + containerHex + ".scope\n")},
		// someone's shell
		"proc/2000/stat":    {Data: []byte(stat(2000, "bash", 1990, 0, 10, 90000, 1200))},
		"proc/2000/status":  {Data: []byte("Uid:\t1000\t1000\t1000\t1000\n")},
		"proc/2000/cmdline": {Data: []byte("-bash\x00")},
		"proc/2000/cgroup":  {Data: []byte("0::/user.slice/user-1000.slice/session-3.scope\n")},
		// not a process
		"proc/meminfo": {Data: []byte("MemTotal: 1 kB\n")},
	}
}

func TestProcesses(t *testing.T) {
	fsys := procFixture()
	c := &Collector{Root: fsys}
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	named := func(id string) *dockwellv1.ProcessContainer {
		if id != containerHex {
			return nil
		}
		return &dockwellv1.ProcessContainer{Id: id, Name: "shop-db-1", ComposeProject: "env_01abc", ComposeService: "db"}
	}
	userIn := func(container string, pid, uid uint32) string {
		if container == containerHex && uid == 999 {
			return "postgres"
		}
		return ""
	}
	first, prev := c.Processes(t0, nil, 2000, named, userIn)
	if first.GetTotal() != 4 || len(first.GetProcesses()) != 4 {
		t.Fatalf("listed %d of %d, want init, dockerd, postgres and bash, without kernel threads: %v", len(first.GetProcesses()), first.GetTotal(), first.GetProcesses())
	}
	if first.GetCpuWindow() != nil {
		t.Error("the first list has nothing to measure CPU against, and says it covers a window")
	}
	by := map[uint32]*dockwellv1.Process{}
	for _, p := range first.GetProcesses() {
		by[p.GetPid()] = p
		if p.GetCpuPercent() != 0 {
			t.Errorf("the first list says %d took %v%% CPU", p.GetPid(), p.GetCpuPercent())
		}
	}
	pg := by[1184]
	if pg.GetName() != "postgres: walwriter (x)" || pg.GetUser() != "postgres" || pg.GetUid() != 999 || pg.GetParentPid() != 1170 {
		t.Errorf("postgres = %v", pg)
	}
	if pg.GetContainer().GetName() != "shop-db-1" || pg.GetContainer().GetComposeService() != "db" || pg.GetUnit() != "" {
		t.Errorf("postgres runs in %v, unit %q", pg.GetContainer(), pg.GetUnit())
	}
	if want := uint64(80000 * os.Getpagesize()); pg.GetMemoryBytes() != want {
		t.Errorf("postgres holds %d bytes, want %d", pg.GetMemoryBytes(), want)
	}
	// Booted 1000 s before t0, and started 5000 ticks, 50 s, after.
	if got := pg.GetStartTime().AsTime(); !got.Equal(t0.Add(-950 * time.Second)) {
		t.Errorf("postgres started %v", got)
	}
	if d := by[812]; d.GetUnit() != "docker.service" || d.GetUser() != "root" || d.GetCommand() != "/usr/bin/dockerd -H fd://" || d.GetContainer() != nil {
		t.Errorf("dockerd = %v", d)
	}
	if b := by[2000]; b.GetUser() != "mira" || b.GetUnit() != "" || b.GetProtected() != "dockwell" {
		t.Errorf("the agent's own process = %v, want mira's and protected", b)
	}
	if by[1].GetProtected() != "init" || by[812].GetProtected() != "" {
		t.Errorf("protected: init %q, dockerd %q", by[1].GetProtected(), by[812].GetProtected())
	}

	// 2 s later, postgres took 100 more ticks, 1 s of CPU, and dockerd 20.
	fsys["proc/1184/stat"] = &fstest.MapFile{Data: []byte(stat(1184, "postgres: walwriter (x)", 1170, 0, 6100, 5000, 80000))}
	fsys["proc/812/stat"] = &fstest.MapFile{Data: []byte(stat(812, "dockerd", 1, 0, 2020, 300, 18000))}
	// bash ended, and its PID went to another process.
	fsys["proc/2000/stat"] = &fstest.MapFile{Data: []byte(stat(2000, "sleep", 1990, 0, 500, 99999, 100))}
	second, _ := c.Processes(t0.Add(2*time.Second), prev, 0, named, userIn)
	if second.GetCpuWindow().AsDuration() != 2*time.Second {
		t.Errorf("CPU window = %v", second.GetCpuWindow().AsDuration())
	}
	got := second.GetProcesses()
	if got[0].GetPid() != 1184 || got[0].GetCpuPercent() != 50 || got[1].GetPid() != 812 || got[1].GetCpuPercent() != 10 {
		t.Errorf("busiest first: %d at %v%%, %d at %v%%", got[0].GetPid(), got[0].GetCpuPercent(), got[1].GetPid(), got[1].GetCpuPercent())
	}
	for _, p := range got {
		if p.GetPid() == 2000 && p.GetCpuPercent() != 0 {
			t.Errorf("a new process on an old PID took %v%% CPU, measured against the one before", p.GetCpuPercent())
		}
	}

	if start, ok := c.ProcessStart(1184); !ok || start != 5000 {
		t.Errorf("ProcessStart(1184) = %d, %v", start, ok)
	}
	fsys["proc/1184/stat"] = &fstest.MapFile{Data: []byte("1184 (postgres) Z 1170 1 1 0 -1 0 0 0 0 0 6100 0 0 0 20 0 1 0 5000 0 0 0\n")}
	if _, ok := c.ProcessStart(1184); ok {
		t.Error("a process that has ended, and waits for its parent to read how, reads as running")
	}
	if _, ok := c.ProcessStart(4242); ok {
		t.Error("a process that isn't there reads as running")
	}
}

func TestCgroupOf(t *testing.T) {
	for in, want := range map[string][2]string{
		"0::/system.slice/docker-" + containerHex + ".scope\n":             {containerHex, ""},
		"0::/docker/" + containerHex + "\n":                                {containerHex, ""},
		"12:pids:/docker/" + containerHex + "\n1:name=systemd:/docker/x\n": {containerHex, ""},
		"0::/system.slice/caddy.service\n":                                 {"", "caddy.service"},
		"0::/system.slice/dockwell.service/agent\n":                        {"", "dockwell.service"},
		"0::/user.slice/user-1000.slice/session-3.scope\n":                 {"", ""},
		"": {"", ""},
	} {
		c, u := cgroupOf([]byte(in))
		if c != want[0] || u != want[1] {
			t.Errorf("cgroupOf(%q) = %q, %q; want %q, %q", in, c, u, want[0], want[1])
		}
	}
}

func TestProcessesOnThisMachine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("processes are read from Linux's /proc")
	}
	c := New()
	list, prev := c.Processes(time.Now(), nil, os.Getpid(), nil, nil)
	if list.GetTotal() == 0 {
		t.Fatal("no processes on this machine")
	}
	self := false
	for _, p := range list.GetProcesses() {
		if int(p.GetPid()) == os.Getpid() {
			self = p.GetProtected() == "dockwell" && p.GetMemoryBytes() > 0 && p.GetStartTime() != nil
		}
	}
	if !self {
		t.Error("this test's own process isn't listed, protected, with its memory and start")
	}
	time.Sleep(20 * time.Millisecond)
	again, _ := c.Processes(time.Now(), prev, os.Getpid(), nil, nil)
	if again.GetCpuWindow() == nil {
		t.Error("a second list measures no CPU")
	}
}
