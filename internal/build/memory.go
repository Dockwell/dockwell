// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

const (
	buildMiB       = 1 << 20
	admissionFloor = 256 * buildMiB
	minReserve     = 128 * buildMiB
	maxReserve     = 256 * buildMiB
	minMemory      = 512 * buildMiB
	maxMemory      = 8 << 30
	memoryWait     = time.Minute
	memoryRetry    = 5 * time.Second
)

type buildLimits struct {
	memory, cpus, available int64
	tight                   bool
}

func memoryCap(available, total int64) (limit int64, tight bool) {
	reserve := min(max(total/8, minReserve), maxReserve)
	return min(max(available-reserve, minMemory), maxMemory), available-reserve < minMemory
}

type memoryLow struct{ available int64 }

func (m memoryLow) Error() string {
	return fmt.Sprintf("only %d MiB of memory available for build admission", m.available/buildMiB)
}

// waitForMemory shares the build's cancellation. The last reading is taken
// at the deadline too, so memory freed on the final interval can admit it.
func waitForMemory(ctx context.Context, read func() (int64, int64, error), wait bool) (int64, int64, error) {
	deadline := time.Now().Add(memoryWait)
	waiting := false
	defer func() {
		if waiting {
			resource.ReportBuild(context.WithoutCancel(ctx), resource.BuildUpdate{})
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		available, total, err := read()
		if err != nil {
			return 0, 0, fmt.Errorf("can't read how much memory this server has free: %w", err)
		}
		if !wait || available >= admissionFloor {
			return available, total, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return 0, 0, resource.PlainlyBecause(resource.ReasonBuildMemory,
				fmt.Sprintf("This server has %d MiB free and a build needs 256 MiB, so the build didn't start. Build in CI and deploy the image, or stop an app first", available/buildMiB),
				memoryLow{available})
		}
		if !waiting {
			waiting = true
			resource.Progress(ctx, "Waiting for memory to build")
			availableMiB := available / buildMiB
			resource.ReportBuild(ctx, resource.BuildUpdate{MemoryAvailableMiB: &availableMiB})
		}
		timer := time.NewTimer(min(left, memoryRetry))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return 0, 0, ctx.Err()
		}
	}
}

func (b *Builder) limits(ctx context.Context, wait bool) (buildLimits, error) {
	info, _ := b.Client.Info(ctx)
	read := b.Memory
	if read == nil {
		proc, groups := os.DirFS("/proc"), os.DirFS("/sys/fs/cgroup")
		parent := b.dockerCgroup(ctx, proc, info)
		read = func() (int64, int64, error) { return ReadMemory(proc, groups, parent) }
	}
	available, total, err := waitForMemory(ctx, read, wait)
	if err != nil {
		return buildLimits{}, err
	}
	memory, tight := memoryCap(available, total)
	n := runtime.NumCPU()
	switch {
	case b.CPUs != nil:
		n = b.CPUs()
	case info != nil && info.NCPU > 0:
		n = info.NCPU
	}
	return buildLimits{memory: memory, cpus: int64(max(n-1, 1)), available: available, tight: tight}, nil
}

// dockerCgroup finds the Engine's default container parent. Most engines do
// not report it directly. A running container with no explicit parent shows
// where that daemon puts containers by default, including inside a nested host.
func (b *Builder) dockerCgroup(ctx context.Context, proc fs.FS, info *docker.Info) string {
	if info == nil || info.CgroupVersion != "2" {
		return ""
	}
	if path.IsAbs(info.CgroupParent) {
		return path.Clean(info.CgroupParent)
	}
	var parent string
	stop := errors.New("finished looking for Docker's cgroup parent")
	checked := 0
	_ = b.Client.EachContainer(ctx, map[string][]string{"status": {"running"}}, func(listed *docker.ContainerSummary) error {
		checked++
		if checked > 8 {
			return stop
		}
		c, err := b.Client.InspectContainer(ctx, listed.ID)
		if err == nil && c != nil && c.HostConfig.CgroupParent == "" {
			parent = containerParent(proc, info.CgroupDriver, c.ID, c.State.Pid)
			if parent != "" {
				return stop
			}
		}
		return nil
	})
	return parent
}

// containerParent confirms the live PID belongs to this container before
// taking the parent. In a different PID namespace, even a PID that exists
// here can refer to a different process.
func containerParent(proc fs.FS, driver, id string, pid int) string {
	if pid <= 0 || !docker.IsFullID(id) {
		return ""
	}
	var leaf string
	switch driver {
	case "systemd":
		leaf = "docker-" + id + ".scope"
	case "cgroupfs":
		leaf = id
	default:
		return ""
	}
	raw, err := fs.ReadFile(proc, strconv.Itoa(pid)+"/cgroup")
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if dir, ok := strings.CutPrefix(line, "0::"); ok && path.IsAbs(dir) && path.Base(dir) == leaf {
			return path.Dir(path.Clean(dir))
		}
	}
	return ""
}

// ReadMemory bounds host MemAvailable by Docker's v2 cgroup parent and
// ancestors, when known. A limit on the agent's service is irrelevant here.
func ReadMemory(proc, groups fs.FS, parent string) (available, total int64, err error) {
	raw, err := fs.ReadFile(proc, "meminfo")
	if err != nil {
		return 0, 0, err
	}
	available, hasAvailable := memoryField(raw, "MemAvailable:")
	total, _ = memoryField(raw, "MemTotal:")
	if total == 0 || !hasAvailable {
		return 0, 0, errors.New("/proc/meminfo is missing MemAvailable or MemTotal")
	}
	available, total = available<<10, total<<10
	if groups == nil || !path.IsAbs(parent) {
		return available, total, nil
	}
	for dir := path.Clean(parent); ; dir = path.Dir(dir) {
		base := strings.TrimPrefix(dir, "/")
		limit, limited := cgroupBytes(groups, path.Join(base, "memory.max"))
		used, readable := cgroupBytes(groups, path.Join(base, "memory.current"))
		if limited && readable {
			stats, _ := fs.ReadFile(groups, path.Join(base, "memory.stat"))
			inactive, _ := memoryField(stats, "inactive_file")
			active, _ := memoryField(stats, "active_file")
			shmem, _ := memoryField(stats, "shmem")
			slab, _ := memoryField(stats, "slab_reclaimable")
			working := max(0, used-inactive-max(0, active-shmem)-slab)
			available = min(available, max(0, limit-working))
			total = min(total, limit)
		}
		if dir == "/" {
			break
		}
	}
	return available, total, nil
}

func memoryField(raw []byte, field string) (int64, bool) {
	for line := range strings.SplitSeq(string(raw), "\n") {
		words := strings.Fields(line)
		if len(words) >= 2 && words[0] == field {
			n, err := strconv.ParseInt(words[1], 10, 64)
			if err == nil && n >= 0 {
				return n, true
			}
			break
		}
	}
	return 0, false
}

func cgroupBytes(groups fs.FS, name string) (int64, bool) {
	raw, err := fs.ReadFile(groups, name)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return n, err == nil && n >= 0
}
