// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"testing"
	"testing/fstest"
)

func TestBuildMemoryIncludesEnclosingCgroupAndReclaimableCache(t *testing.T) {
	proc := fstest.MapFS{
		"meminfo":     {Data: []byte("MemTotal: 8388608 kB\nMemAvailable: 6291456 kB\n")},
		"self/cgroup": {Data: []byte("0::/test.slice/agent.scope\n")},
	}
	groups := fstest.MapFS{
		"test.slice/agent.scope/memory.max": {Data: []byte("max\n")},
		"test.slice/memory.max":             {Data: []byte("1073741824\n")},
		"test.slice/memory.current":         {Data: []byte("629145600\n")},
		"test.slice/memory.stat":            {Data: []byte("file 209715200\ninactive_file 104857600\n")},
	}
	available, total, err := ReadMemory(proc, groups)
	if err != nil || available != 524<<20 || total != 1<<30 {
		t.Fatalf("available %d, total %d, err %v, want 524 MiB available of 1 GiB", available, total, err)
	}
}
