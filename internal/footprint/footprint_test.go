// SPDX-License-Identifier: AGPL-3.0-only

package footprint

import (
	"slices"
	"testing"
)

// maps is /proc/self/maps of a dockwell that was replaced on disk while it
// ran, with the same inode on another device, and a SQLite file mapped.
const maps = `00400000-00fab000 r-xp 00000000 fd:01 1234567                            /usr/local/bin/dockwell (deleted)
00fab000-01e40000 r--p 00bab000 fd:01 1234567                            /usr/local/bin/dockwell (deleted)
01e40000-01f11000 rw-p 01a40000 fd:01 1234567                            /usr/local/bin/dockwell (deleted)
01f11000-03f60000 rw-p 00000000 00:00 0
c000000000-c000400000 rw-p 00000000 00:00 0
7f0000000000-7f0000010000 r--s 00000000 fd:02 1234567                    /var/lib/dockwell/control/state.db
7f0000010000-7f0000020000 r--p 00000000 fd:01 7654321                    /var/lib/dockwell/control/other
7ffd8a1d6000-7ffd8a1f7000 rw-p 00000000 00:00 0                          [stack]
7ffd8a1fa000-7ffd8a1fe000 r--p 00000000 00:00 0                          [vvar]
7ffd8a1fe000-7ffd8a200000 r-xp 00000000 00:00 0                          [vdso]
ffffffffff600000-ffffffffff601000 --xp 00000000 00:00 0                  [vsyscall]
`

func TestReadOnlySpansAreTheExecutablesCodeAndConstants(t *testing.T) {
	got, err := readOnlySpans(maps, 0xfd, 0x01, 1234567)
	if err != nil {
		t.Fatal(err)
	}
	want := []span{{0x400000, 0xfab000}, {0xfab000, 0x1e40000}}
	if !slices.Equal(got, want) {
		t.Errorf("readOnlySpans = %#x, want %#x: only the read-only mappings of this file", got, want)
	}
}

func TestReadOnlySpansOfAnotherFileAreNone(t *testing.T) {
	got, err := readOnlySpans(maps, 0xfd, 0x01, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("readOnlySpans = %#x, want none", got)
	}
}

func TestReadOnlySpansRefusesAGarbledRange(t *testing.T) {
	if _, err := readOnlySpans("00fab000-0040zz00 r--p 00000000 fd:01 1234567 /usr/local/bin/dockwell\n", 0xfd, 0x01, 1234567); err == nil {
		t.Error("readOnlySpans took an address range it can't read")
	}
}
