// SPDX-License-Identifier: AGPL-3.0-only

package footprint

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestReleaseStartupPagesUnmapsTheBinaryAndKeepsRunning(t *testing.T) {
	// The collection that follows can grow the heap, with the race
	// detector more than without, so the binary's pages are what to
	// compare: they are the file-backed part of RSS.
	fileBefore := rssFile(t)
	before, after, err := ReleaseStartupPages()
	if err != nil {
		t.Fatal(err)
	}
	fileAfter := rssFile(t)
	if before <= 0 || after <= 0 {
		t.Errorf("RSS before and after = %d and %d, want both above zero", before, after)
	}
	if fileAfter >= fileBefore {
		t.Errorf("file-backed RSS went from %d to %d bytes, want less: the binary's startup pages are still mapped", fileBefore, fileAfter)
	}
	// Code and constants on released pages still work: they map again.
	if got := strings.Repeat("ok", 2); got != "okok" {
		t.Errorf("after releasing, strings.Repeat gave %q", got)
	}
	if _, _, err := ReleaseStartupPages(); err != nil {
		t.Errorf("a second release failed: %v", err)
	}
}

// rssFile is this process's file-backed resident memory, in bytes.
func rssFile(t *testing.T) int64 {
	t.Helper()
	f, err := os.Open("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "RssFile:"); ok {
			kb, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return kb * 1024
		}
	}
	t.Fatal("/proc/self/status has no RssFile")
	return 0
}
