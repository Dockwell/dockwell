// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"runtime"
	"runtime/debug"
	"strconv"
	"testing"
)

// gcPercent reads the collector's target, which Go only tells when it's set.
func gcPercent() int {
	now := debug.SetGCPercent(100)
	debug.SetGCPercent(now)
	return now
}

// An agent on its own collects at a quarter of Go's target, as an idle agent's
// heap was half garbage otherwise (ADR 0060, ADR 0109), and what ran before it
// gets its own back when the agent stops.
func TestAnAgentOnItsOwnCollectsSooner(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(100))
	restore := collectAsAnAgent(IO{Getenv: func(string) string { return "" }})
	if got := gcPercent(); got != agentGC {
		t.Errorf("the collector's target is %d while an agent runs on its own, want %d", got, agentGC)
	}
	restore()
	if got := gcPercent(); got != 100 {
		t.Errorf("the collector's target is %d once the agent stopped, want the 100 it was", got)
	}
}

// GOGC in the environment is the operator's word, and the runtime has applied it.
func TestAnOperatorsGOGCStays(t *testing.T) {
	for _, set := range []string{"200", "25", "off"} {
		t.Run(set, func(t *testing.T) {
			defer debug.SetGCPercent(debug.SetGCPercent(100))
			restore := collectAsAnAgent(IO{Getenv: func(k string) string {
				if k == "GOGC" {
					return set
				}
				return ""
			}})
			if got := gcPercent(); got != 100 {
				t.Errorf("with GOGC=%s, the collector's target went from 100 to %d", set, got)
			}
			restore()
			if got := gcPercent(); got != 100 {
				t.Errorf("with GOGC=%s, stopping left the collector's target at %d", set, got)
			}
		})
	}
}

// An agent on its own runs on at most agentCPUs, as Go keeps memory for each CPU
// it may run on (ADR 0109); a smaller server keeps its own count, and an
// operator's GOMAXPROCS stays as set.
func TestAnAgentOnItsOwnRunsOnFewCPUs(t *testing.T) {
	defer runtime.SetDefaultGOMAXPROCS()
	none := IO{Getenv: func(string) string { return "" }}

	runtime.GOMAXPROCS(agentCPUs * 4)
	restore := runOnAgentCPUs(none)
	if got := runtime.GOMAXPROCS(0); got != agentCPUs {
		t.Errorf("an agent on its own runs on %d CPUs, want %d", got, agentCPUs)
	}
	restore()
	got := runtime.GOMAXPROCS(0)
	runtime.SetDefaultGOMAXPROCS()
	if want := runtime.GOMAXPROCS(0); got != want {
		t.Errorf("once the agent stopped, Go runs on %d CPUs, want its own %d", got, want)
	}

	runtime.GOMAXPROCS(agentCPUs - 2)
	runOnAgentCPUs(none)()
	if got := runtime.GOMAXPROCS(0); got != agentCPUs-2 {
		t.Errorf("a server of %d CPUs runs the agent on %d", agentCPUs-2, got)
	}

	runtime.GOMAXPROCS(agentCPUs * 4)
	runOnAgentCPUs(IO{Getenv: func(k string) string {
		if k == "GOMAXPROCS" {
			return strconv.Itoa(agentCPUs * 4)
		}
		return ""
	}})()
	if got := runtime.GOMAXPROCS(0); got != agentCPUs*4 {
		t.Errorf("with GOMAXPROCS=%d, the agent runs on %d CPUs", agentCPUs*4, got)
	}
}
