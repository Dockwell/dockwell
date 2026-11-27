// SPDX-License-Identifier: AGPL-3.0-only

package pageout

import (
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestExecutableMappings(t *testing.T) {
	const maps = `00400000-00401000 r-xp 00000000 08:02 42 /path/to/dockwell
00401000-00402000 r--p 00001000 08:02 42 /path/to/dockwell (deleted)
00402000-00403000 rw-p 00002000 08:02 42 /path/to/dockwell
00403000-00404000 r-xs 00003000 08:02 42 /path/to/dockwell
00404000-00405000 r--p 00000000 08:02 43 /path/to/dockwell
00405000-00406000 r--p 00000000 09:02 42 /path/to/dockwell
00406000-00407000 r--p 00000000 00:00 0
00408000-00407000 r-xp 00000000 08:02 42 /path/to/dockwell
00408000-00409000 ---p 00000000 08:02 42 /path/to/dockwell
`
	got := executableMappings(strings.NewReader(maps), unix.Mkdev(8, 2), 42)
	want := []mapping{{0x400000, 0x401000}, {0x401000, 0x402000}}
	if !slices.Equal(got, want) {
		t.Errorf("executable mappings = %#v, want %#v", got, want)
	}
}

func TestCleanExecutableMappings(t *testing.T) {
	const smaps = `00400000-00401000 r-xp 00000000 08:02 42 /path/to/dockwell
Shared_Dirty:          0 kB
Private_Dirty:         0 kB
Anonymous:             0 kB
00401000-00402000 r--p 00001000 08:02 42 /path/to/dockwell
Shared_Dirty:          0 kB
Private_Dirty:         4 kB
Anonymous:             0 kB
00402000-00403000 r--p 00002000 08:02 42 /path/to/dockwell
Shared_Dirty:          0 kB
Private_Dirty:         0 kB
Anonymous:             4 kB
00403000-00404000 r--p 00003000 08:02 42 /path/to/dockwell
Shared_Dirty:          4 kB
Private_Dirty:         0 kB
Anonymous:             0 kB
00404000-00405000 r--p 00004000 08:02 42 /path/to/dockwell
Shared_Dirty:          0 kB
Private_Dirty:         0 kB
`
	candidates := []mapping{
		{0x400000, 0x401000},
		{0x401000, 0x402000},
		{0x402000, 0x403000},
		{0x403000, 0x404000},
		{0x404000, 0x405000},
	}
	got := cleanMappings(strings.NewReader(smaps), candidates)
	if !slices.Equal(got, candidates[:1]) {
		t.Errorf("clean executable mappings = %#v, want %#v", got, candidates[:1])
	}
}

func TestUnsupportedPageoutSkipsFurtherCalls(t *testing.T) {
	var r reclaimer
	at := time.Unix(1, 0)
	advised, lookedUp := 0, 0
	mappings := func() []mapping {
		lookedUp++
		return []mapping{{0x400000, 0x401000}, {0x401000, 0x402000}}
	}
	advise := func(mapping) error {
		advised++
		return unix.EINVAL
	}
	r.reclaim(at, mappings, advise)
	r.reclaim(at.Add(interval), mappings, advise)
	if advised != 1 || lookedUp != 1 {
		t.Errorf("unsupported advice called %d times after %d lookups, want 1 of each", advised, lookedUp)
	}
}

func TestRateLimitsPageout(t *testing.T) {
	var r reclaimer
	at := time.Unix(1, 0)
	advised := 0
	mappings := func() []mapping { return []mapping{{0x400000, 0x401000}} }
	advise := func(mapping) error { advised++; return nil }
	r.reclaim(at, mappings, advise)
	r.reclaim(at.Add(time.Second), mappings, advise)
	r.reclaim(at.Add(interval), mappings, advise)
	if advised != 2 {
		t.Errorf("advice called %d times, want 2", advised)
	}
}
