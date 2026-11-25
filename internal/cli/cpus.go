// SPDX-License-Identifier: AGPL-3.0-only

package cli

import "runtime"

// agentCPUs is the most CPUs an agent on its own runs Go code on at once. Go
// keeps memory for each CPU it may run on, and an idle agent's few goroutines
// wake on any of them: on a server of 16 CPUs, an idle agent read 1.5 MB lower
// with 4, and a transfer's peak about 2 MB lower. An agent's work waits on the
// network, the disk and Docker more than on the CPU: four uploads and four
// downloads at once took as long with 4 as with 16
// (docs/adr/0109-the-agents-memory-headroom.md).
const agentCPUs = 4

// runOnAgentCPUs holds an agent on its own to agentCPUs, unless the environment
// sets GOMAXPROCS, and returns what puts it back. A server of agentCPUs or fewer,
// or a container limited to them, keeps Go's own count, which follows the limit
// as it changes.
func runOnAgentCPUs(stdio IO) (restore func()) {
	if stdio.Getenv("GOMAXPROCS") != "" || runtime.GOMAXPROCS(0) <= agentCPUs {
		return func() {}
	}
	runtime.GOMAXPROCS(agentCPUs)
	return runtime.SetDefaultGOMAXPROCS
}
