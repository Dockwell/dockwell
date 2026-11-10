// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"io"
	"reflect"
	"testing"

	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/collect"
)

func lookupsFor(t *testing.T, args []string, env map[string]string) agent.Config {
	t.Helper()
	g := &globalFlags{}
	stdio := IO{Out: io.Discard, Err: io.Discard, Getenv: func(k string) string { return env[k] }}
	cmd := newRunCmd(stdio, g)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	cfg := agent.Config{Collector: collect.New()}
	lookups(&cfg, stdio, g)
	return cfg
}

func TestTheFlagAndTheVariableTurnTheLookupOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  string
		off  bool
	}{
		{"by default it's on", nil, "", false},
		{"the flag", []string{"--no-stun"}, "", true},
		{"the variable", nil, "1", true},
		{"the variable set to 0", nil, "0", false},
		{"the variable set to false", nil, "false", false},
		{"the flag, over a variable that says otherwise", []string{"--no-stun"}, "0", true},
		{"the flag set to false, over the variable", []string{"--no-stun=false"}, "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lookupsFor(t, tc.args, map[string]string{envNoStun: tc.env}).NoLookup; got != tc.off {
				t.Errorf("NoLookup = %t, want %t", got, tc.off)
			}
		})
	}
}

// A route out through a TUN isn't where the server is, as a Tailscale exit node's
// shared address would otherwise read as carrier NAT, so the agent goes by its own.
func TestTheRouteOutGoesThroughTheChecksForATUN(t *testing.T) {
	cfg := lookupsFor(t, nil, nil)
	if reflect.ValueOf(cfg.Collector.Outbound).Pointer() == reflect.ValueOf(collect.OutboundAddress).Pointer() {
		t.Error("it reads the route out directly")
	}
}

// A script or an e2e starts the real binary, where a test binary's guard doesn't reach,
// so one variable has it ask nobody, set by the Makefile and CI for all they start.
func TestATestAsksNobody(t *testing.T) {
	for value, off := range map[string]bool{"": false, "1": true, "true": true, "0": false, "false": false} {
		cfg := lookupsFor(t, nil, map[string]string{envTest: value})
		if cfg.NoLookup != off || (cfg.Collector.Metadata == collect.ClosedMetadata) != off {
			t.Errorf("with %q: NoLookup %t, Metadata %q", value, cfg.NoLookup, cfg.Collector.Metadata)
		}
	}
}

// A test runs none of the machine's own tools, such as nft or ufw: only
// stand-ins from the folder DOCKWELL_TEST_TOOLS names, or none at all.
func TestATestRunsNoneOfTheMachinesTools(t *testing.T) {
	getenv := func(env map[string]string) IO { return IO{Getenv: func(k string) string { return env[k] }} }
	if r := hostTools(getenv(nil)); r.Off || r.Root != "" {
		t.Errorf("a server runs %+v, want its own tools", r)
	}
	if r := hostTools(getenv(map[string]string{envTest: "1"})); !r.Off {
		t.Errorf("a test runs %+v, want none", r)
	}
	if r := hostTools(getenv(map[string]string{envTest: "1", envTestTools: "/tmp/tools"})); r.Off || r.Root != "/tmp/tools" {
		t.Errorf("a test with stand-ins runs %+v", r)
	}
	if r := hostTools(getenv(map[string]string{envTestTools: "/tmp/tools"})); r.Root != "" {
		t.Errorf("a server outside a test takes stand-ins: %+v", r)
	}
}
