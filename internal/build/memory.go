// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
)

func hostMemory() (available, total int64, err error) {
	return ReadMemory(os.DirFS("/proc"), os.DirFS("/sys/fs/cgroup"))
}

// ReadMemory reads host memory and also checks the process's cgroup v2
// ancestors, if available. An agent often shares its enclosing cgroup with
// Docker and the apps even when its immediate cgroup has no limit of its own.
func ReadMemory(proc, groups fs.FS) (available, total int64, err error) {
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
	if groups == nil {
		return available, total, nil
	}
	cgroup, err := fs.ReadFile(proc, "self/cgroup")
	if err != nil {
		// Some hosts do not mount a unified hierarchy where the agent can see it.
		return available, total, nil //nolint:nilerr // host memory is still available
	}
	for line := range strings.SplitSeq(string(cgroup), "\n") {
		if dir, ok := strings.CutPrefix(line, "0::/"); ok {
			for dir = path.Clean("/" + dir); ; dir = path.Dir(dir) {
				base := strings.TrimPrefix(dir, "/")
				limit, limited := cgroupBytes(groups, path.Join(base, "memory.max"))
				used, readable := cgroupBytes(groups, path.Join(base, "memory.current"))
				if limited && readable {
					// The inactive file cache can be reclaimed. Shmem and active
					// pages still count against what the build can be given.
					stats, _ := fs.ReadFile(groups, path.Join(base, "memory.stat"))
					cache, _ := memoryField(stats, "inactive_file")
					available = min(available, max(0, limit-max(0, used-cache)))
					total = min(total, limit)
				}
				if dir == "/" {
					break
				}
			}
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
