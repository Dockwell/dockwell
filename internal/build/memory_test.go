// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

func TestBuildMemoryPolicy(t *testing.T) {
	for _, tt := range []struct {
		name                        string
		totalMiB, freeMiB, limitMiB int64
		tight                       bool
	}{
		{"1 GiB and 150 MiB free", 1024, 150, 0, false},
		{"1 GiB and 300 MiB free", 1024, 300, 512, true},
		{"1 GiB and 450 MiB free", 1024, 450, 512, true},
		{"2 GiB and 600 MiB free", 2048, 600, 512, true},
		{"4 GiB and 700 MiB free", 4096, 700, 512, true},
		{"1.5 GiB and 1000 MiB free", 1536, 1000, 808, false},
		{"16 GiB and 12 GiB free", 16384, 12288, 8192, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var shown []int64
				ctx := resource.WithBuilds(t.Context(), func(u resource.BuildUpdate) {
					if u.MemoryAvailableMiB != nil {
						shown = append(shown, *u.MemoryAvailableMiB)
					}
				})
				available, total, err := waitForMemory(ctx, func() (int64, int64, error) {
					return tt.freeMiB * buildMiB, tt.totalMiB * buildMiB, nil
				}, true)
				if tt.limitMiB == 0 {
					if resource.ReasonOf(err) != resource.ReasonBuildMemory || time.Since(start) != time.Minute ||
						len(shown) != 1 || shown[0] != tt.freeMiB {
						t.Fatalf("refusal after %v, shown %v, err %v", time.Since(start), shown, err)
					}
				} else if limit, tight := memoryCap(available, total); err != nil ||
					limit != tt.limitMiB*buildMiB || tight != tt.tight || time.Since(start) != 0 {
					t.Fatalf("cap %d MiB, tight %v after %v, err %v", limit/buildMiB, tight, time.Since(start), err)
				}
			})
		})
	}
}

func TestBuildMemoryWaitCanRecoverOrCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := 0
		available, _, err := waitForMemory(t.Context(), func() (int64, int64, error) {
			n++
			if n == 13 {
				return admissionFloor, 1 << 30, nil
			}
			return admissionFloor - 1, 1 << 30, nil
		}, true)
		if err != nil || available != admissionFloor {
			t.Fatalf("memory became available at the deadline, got %d, %v", available, err)
		}

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, _, err = waitForMemory(ctx, func() (int64, int64, error) { return 0, 1 << 30, nil }, true)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancelled wait returned %v", err)
		}
	})
}

func TestBuildMemoryReadsDockerParentAndIgnoresAgentLeaf(t *testing.T) {
	bench := newBench(t)
	info := &docker.Info{CgroupVersion: "2", CgroupDriver: "systemd"}
	ctr := bench.engine.AddContainer(docker.Container{Name: "/example", State: docker.State{
		Running: true, Status: "running", Pid: 42,
	}})
	proc := fstest.MapFS{
		"meminfo":     {Data: []byte("MemTotal: 16777216 kB\nMemAvailable: 12582912 kB\n")},
		"self/cgroup": {Data: []byte("0::/system.slice/dockwell.service\n")},
		"42/cgroup":   {Data: []byte("0::/system.slice/docker-" + ctr.ID + ".scope\n")},
	}
	groups := fstest.MapFS{
		"system.slice/dockwell.service/memory.max":     {Data: []byte("536870912\n")},
		"system.slice/dockwell.service/memory.current": {Data: []byte("419430400\n")},
		"system.slice/memory.max":                      {Data: []byte("max\n")},
		"memory.max":                                   {Data: []byte("max\n")},
	}
	parent := bench.builder.dockerCgroup(t.Context(), proc, info)
	if parent != "/system.slice" {
		t.Fatalf("Docker's parent is %q", parent)
	}
	available, total, err := ReadMemory(proc, groups, parent)
	if err != nil || available != 12<<30 || total != 16<<30 {
		t.Fatalf("agent's 512 MiB leaf changed Docker's memory: %d available of %d, %v", available, total, err)
	}
	if limit, _ := memoryCap(available, total); limit != maxMemory {
		t.Fatalf("builder limit is %d, want %d", limit, maxMemory)
	}

	proc["42/cgroup"] = &fstest.MapFile{Data: []byte("4:memory:/docker/" + ctr.ID + "\n")}
	if parent := bench.builder.dockerCgroup(t.Context(), proc, info); parent != "" {
		t.Fatalf("cgroup v1 was treated as a v2 parent: %q", parent)
	}
	// A kernel that gives both hierarchies names the unified one separately.
	proc["42/cgroup"] = &fstest.MapFile{Data: []byte("4:memory:/docker/" + ctr.ID + "\n0::/docker/" + ctr.ID + "\n")}
	if parent := containerParent(proc, "cgroupfs", ctr.ID, 42); parent != "/docker" {
		t.Fatalf("Docker's cgroupfs parent is %q", parent)
	}
	proc["42/cgroup"] = &fstest.MapFile{Data: []byte("0::/" + ctr.ID + "\n")}
	if parent := containerParent(proc, "cgroupfs", ctr.ID, 42); parent != "/" {
		t.Fatalf("Docker's parent in a cgroup namespace is %q", parent)
	}
}

func TestBuildMemoryAccountsForReclaimablePages(t *testing.T) {
	proc := fstest.MapFS{
		"meminfo": {Data: []byte("MemTotal: 8388608 kB\nMemAvailable: 6291456 kB\n")},
	}
	for _, tt := range []struct {
		name, stats string
		want        int64
	}{
		{"reclaimable file pages and slab", "inactive_file 104857600\nactive_file 104857600\nshmem 52428800\nslab_reclaimable 52428800\n", 624 << 20},
		{"memory.stat unreadable", "", 424 << 20},
		{"file cache raced the current reading", "inactive_file 1073741824\n", 1 << 30},
	} {
		t.Run(tt.name, func(t *testing.T) {
			groups := fstest.MapFS{
				"docker/memory.max": {Data: []byte("max\n")},
				"memory.max":        {Data: []byte("1073741824\n")},
				"memory.current":    {Data: []byte("629145600\n")},
			}
			if tt.stats != "" {
				groups["memory.stat"] = &fstest.MapFile{Data: []byte(tt.stats)}
			}
			available, total, err := ReadMemory(proc, groups, "/docker")
			if err != nil || available != tt.want || total != 1<<30 {
				t.Fatalf("available %d, total %d, err %v, want %d available of 1 GiB", available, total, err, tt.want)
			}
		})
	}
}

func TestStandaloneAdmissionRefusalKeepsItsBuildReason(t *testing.T) {
	var ended resource.BuildUpdate
	ctx := resource.WithBuilds(t.Context(), func(u resource.BuildUpdate) { ended = u })
	err := resource.PlainlyBecause(resource.ReasonBuildMemory, "The build didn't start", memoryLow{available: 150 * buildMiB})
	b := new(Builder)
	_ = b.ended(ctx, service.BuildRequest{}, facts{}, err)
	if ended.ErrorReason != resource.ReasonBuildMemory || ended.MemoryAvailableMiB == nil || *ended.MemoryAvailableMiB != 150 {
		t.Fatalf("build update lost memory refusal: %+v", ended)
	}
}

func TestKilledWorkerDoesNotClaimAnOOMWithoutEvidence(t *testing.T) {
	err := failure(service.BuildRequest{}, facts{}, 137, false)
	plain, _ := resource.Explain(err)
	if !strings.Contains(plain, "killed before it could deploy") {
		t.Fatalf("killed build says %q", plain)
	}
}
