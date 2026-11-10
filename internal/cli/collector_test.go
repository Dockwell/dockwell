// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"runtime/debug"
	"testing"
)

// gcPercent reads the collector's target, which Go only tells when it's set.
func gcPercent() int {
	now := debug.SetGCPercent(100)
	debug.SetGCPercent(now)
	return now
}

// An agent on its own collects at half of Go's target, as an idle agent's heap
// was half garbage otherwise (ADR 0060), and what ran before it gets its own
// back when the agent stops.
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
