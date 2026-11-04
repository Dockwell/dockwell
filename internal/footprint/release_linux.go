// SPDX-License-Identifier: AGPL-3.0-only

package footprint

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ReleaseStartupPages unmaps this process's read-only mappings of its own
// executable, its code and constant data, and then collects garbage once.
// It returns the process's RSS before and after, in bytes.
//
// Nothing is lost. The pages stay in the page cache, and the next touch of
// one maps it again from there. What stays resident is what the process
// touches from then on. The collection afterwards maps the pages each cycle
// reads now, rather than at whatever moment the next cycle would, so the
// figure after it is the steady one.
//
// Call it once startup is over.
func ReleaseStartupPages() (before, after int64, err error) {
	before, err = rss()
	if err != nil {
		return 0, 0, err
	}
	// The link names the executable even after it was replaced on disk,
	// as an upgrade does, and stat follows it to the file this process
	// runs.
	fi, err := os.Stat("/proc/self/exe")
	if err != nil {
		return 0, 0, fmt.Errorf("can't find this process's executable: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("can't find this process's executable: stat gave no inode")
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return 0, 0, fmt.Errorf("can't read this process's mappings: %w", err)
	}
	spans, err := readOnlySpans(string(maps), unix.Major(st.Dev), unix.Minor(st.Dev), st.Ino)
	if err != nil {
		return 0, 0, err
	}
	if len(spans) == 0 {
		return 0, 0, errors.New("found no read-only mappings of this process's executable in /proc/self/maps")
	}
	for _, s := range spans {
		// The raw call takes the range as numbers, so no pointer is made
		// from them. Advice on a read-only file mapping never loses data.
		if _, _, errno := syscall.Syscall(syscall.SYS_MADVISE, s.lo, s.hi-s.lo, syscall.MADV_DONTNEED); errno != 0 {
			return 0, 0, fmt.Errorf("can't release the pages of %#x-%#x: %w", s.lo, s.hi, errno)
		}
	}
	runtime.GC()
	after, err = rss()
	return before, after, err
}

// rss is this process's resident set size, in bytes.
func rss() (int64, error) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, fmt.Errorf("can't read this process's memory: %w", err)
	}
	// size resident shared text lib data dt, in pages
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, fmt.Errorf("can't read this process's memory: /proc/self/statm says %q", b)
	}
	pages, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("can't read this process's memory: %w", err)
	}
	return pages * int64(os.Getpagesize()), nil
}
