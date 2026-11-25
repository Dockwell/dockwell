// SPDX-License-Identifier: AGPL-3.0-only

package terminal

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestParseProcStatPgrpSid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		stat     string
		wantPgrp int
		wantSid  int
		wantOK   bool
	}{
		{
			name:     "standard shell",
			stat:     "1234 (bash) S 1000 1234 1234 34816 1234 4194304 0 0 0 0",
			wantPgrp: 1234,
			wantSid:  1234,
			wantOK:   true,
		},
		{
			name:     "comm with spaces and closing parens",
			stat:     "4321 (my (weird) proc) R 100 4320 4000 34816 4320",
			wantPgrp: 4320,
			wantSid:  4000,
			wantOK:   true,
		},
		{
			name:   "missing closing paren",
			stat:   "1234 (broken S 1 2 3",
			wantOK: false,
		},
		{
			name:   "too few fields after comm",
			stat:   "1234 (sh) S 1 2",
			wantOK: false,
		},
		{
			name:   "non-numeric pgrp",
			stat:   "1234 (sh) S 1 abc 3",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pgrp, sid, ok := parseProcStatPgrpSid(tc.stat)
			if ok != tc.wantOK || pgrp != tc.wantPgrp || sid != tc.wantSid {
				t.Errorf("parseProcStatPgrpSid(%q) = (%d, %d, %v), want (%d, %d, %v)",
					tc.stat, pgrp, sid, ok, tc.wantPgrp, tc.wantSid, tc.wantOK)
			}
		})
	}
}

func TestReadProcPgrpSidSelf(t *testing.T) {
	t.Parallel()

	pid := os.Getpid()
	wantPgrp, err := unix.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	wantSid, err := unix.Getsid(pid)
	if err != nil {
		t.Fatalf("Getsid: %v", err)
	}
	gotPgrp, gotSid, ok := readProcPgrpSid(pid)
	if !ok || gotPgrp != wantPgrp || gotSid != wantSid {
		t.Fatalf("readProcPgrpSid(%d) = (%d, %d, %v), want (%d, %d, true)",
			pid, gotPgrp, gotSid, ok, wantPgrp, wantSid)
	}
}
