// SPDX-License-Identifier: AGPL-3.0-only

// Package pageout reclaims clean executable pages of the standalone agent.
package pageout

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const interval = 5 * time.Second

var executable reclaimer

// Executable asks Linux to reclaim clean, read-only pages mapped from this
// process's executable. Faults bring those pages back as they are used.
func Executable() {
	executable.reclaim(time.Now(), ownMappings, advise)
}

type reclaimer struct {
	mu       sync.Mutex
	last     time.Time
	disabled bool
}

func (r *reclaimer) reclaim(now time.Time, mappings func() []mapping, advise func(mapping) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disabled || now.Sub(r.last) < interval {
		return
	}
	r.last = now
	for _, m := range mappings() {
		switch advise(m) {
		case unix.EINVAL, unix.ENOSYS, unix.EOPNOTSUPP, unix.EPERM: //nolint:errorlint // advise returns the kernel errno directly
			// Older kernels or a syscall filter may refuse MADV_PAGEOUT.
			r.disabled = true
			return
		}
	}
}

type mapping struct {
	start, end uintptr
}

func ownMappings() []mapping {
	var exe unix.Stat_t
	if err := unix.Stat("/proc/self/exe", &exe); err != nil {
		return nil
	}
	maps, err := os.Open("/proc/self/maps")
	if err != nil {
		return nil
	}
	defer maps.Close()
	candidates := executableMappings(maps, exe.Dev, exe.Ino)
	if len(candidates) == 0 {
		return nil
	}
	smaps, err := os.Open("/proc/self/smaps")
	if err != nil {
		return nil
	}
	defer smaps.Close()
	return cleanMappings(smaps, candidates)
}

// Match device and inode so an upgrade or another name for the same executable
// does not affect which file we recognize.
func executableMappings(r io.Reader, dev, inode uint64) []mapping {
	var mappings []mapping
	lines := bufio.NewScanner(r)
	for lines.Scan() {
		f := strings.Fields(lines.Text())
		if len(f) < 5 || (f[1] != "r-xp" && f[1] != "r--p") {
			continue
		}
		ino, err := strconv.ParseUint(f[4], 10, 64)
		if err != nil || ino == 0 || ino != inode {
			continue
		}
		major, minor, _ := strings.Cut(f[3], ":")
		ma, errMajor := strconv.ParseUint(major, 16, 32)
		mi, errMinor := strconv.ParseUint(minor, 16, 32)
		if errMajor != nil || errMinor != nil || ma != uint64(unix.Major(dev)) || mi != uint64(unix.Minor(dev)) {
			continue
		}
		if m, ok := addressRange(f[0]); ok {
			mappings = append(mappings, m)
		}
	}
	if lines.Err() != nil {
		return nil
	}
	return mappings
}

func addressRange(s string) (mapping, bool) {
	low, high, cut := strings.Cut(s, "-")
	if !cut {
		return mapping{}, false
	}
	start, errStart := strconv.ParseUint(low, 16, strconv.IntSize)
	end, errEnd := strconv.ParseUint(high, 16, strconv.IntSize)
	return mapping{uintptr(start), uintptr(end)}, errStart == nil && errEnd == nil && start < end
}

// A loader can make a formerly writable range read-only, with dirty or
// anonymous copy-on-write pages still in it. Check those counters in smaps.
func cleanMappings(r io.Reader, candidates []mapping) []mapping {
	want := make(map[mapping]struct{}, len(candidates))
	for _, m := range candidates {
		want[m] = struct{}{}
	}
	var clean []mapping
	var current mapping
	var eligible bool
	var zeros uint8
	lines := bufio.NewScanner(r)
	for lines.Scan() {
		f := strings.Fields(lines.Text())
		if len(f) < 2 {
			continue
		}
		if m, header := addressRange(f[0]); header {
			current, zeros = m, 0
			_, eligible = want[m]
			continue
		}
		if !eligible {
			continue
		}
		switch f[0] {
		case "Shared_Dirty:":
			zeros |= 1
		case "Private_Dirty:":
			zeros |= 2
		case "Anonymous:":
			zeros |= 4
		default:
			continue
		}
		if f[1] != "0" {
			eligible = false
		} else if zeros == 7 {
			clean = append(clean, current)
			eligible = false
		}
	}
	if lines.Err() != nil {
		return nil
	}
	return clean
}

func advise(m mapping) error {
	_, _, errno := unix.Syscall(unix.SYS_MADVISE, m.start, m.end-m.start, unix.MADV_PAGEOUT)
	if errno != 0 {
		return errno
	}
	return nil
}
