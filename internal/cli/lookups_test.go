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

func TestAnEnvironmentCanSayWhereMetadataIs(t *testing.T) {
	const url = "http://127.0.0.1:9"
	if got := lookupsFor(t, nil, map[string]string{envMetadataURL: url}).Collector.Metadata; got != url {
		t.Errorf("Metadata = %q", got)
	}
}
