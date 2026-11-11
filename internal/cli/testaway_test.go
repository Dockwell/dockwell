// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/agent"
)

// DOCKWELL_TEST_AWAY shortens how long the agent waits away before it acts, and
// how often it looks then, but only in a test: on a server it's ignored, so none
// waits less than ADR 0054 says (docs/testing/chaos.md).
func TestTheAwayTimeShortensOnlyInATest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		after time.Duration
		bad   bool
	}{
		{"unset", map[string]string{envTest: "1"}, 0, false},
		{"in a test", map[string]string{envTest: "1", envTestAway: "2s"}, 2 * time.Second, false},
		{"outside a test", map[string]string{envTestAway: "2s"}, 0, false},
		{"with DOCKWELL_TEST off", map[string]string{envTest: "0", envTestAway: "2s"}, 0, false},
		{"not a duration", map[string]string{envTest: "1", envTestAway: "soon"}, 0, true},
		{"no time at all", map[string]string{envTest: "1", envTestAway: "0s"}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg agent.Config
			stdio := IO{Out: io.Discard, Err: io.Discard, Getenv: func(k string) string { return tc.env[k] }}
			err := testAway(&cfg, stdio)
			if tc.bad {
				if err == nil || !strings.Contains(err.Error(), envTestAway) {
					t.Fatalf("testAway = %v, want an error that names %s", err, envTestAway)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.AwayAfter != tc.after || cfg.ReconcileEvery != tc.after {
				t.Errorf("AwayAfter %s, ReconcileEvery %s; want both %s", cfg.AwayAfter, cfg.ReconcileEvery, tc.after)
			}
		})
	}
}
