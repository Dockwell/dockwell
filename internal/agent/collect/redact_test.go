// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// tokenFixture is a cloudflared connector token: base64 JSON whose "s" is the
// tunnel's secret.
const tokenFixture = "eyJhIjoiMjc0ZmFhMWQxMTFhZjcxNmEzMzkyMmIzNGVlNTM4MWQiLCJ0IjoiMzI3ZDlkNDEtYjAwOS00MTNkLTg0OTQtN2VlZWVmYjNlZDJhIiwicyI6InNlY3JldCJ9"

func TestRedactHidesSecretFlags(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{[]string{"cloudflared", "tunnel", "run", "--token", tokenFixture}, []string{"cloudflared", "tunnel", "run", "--token", Hidden}},
		{[]string{"cloudflared", "tunnel", "run", "--token=" + tokenFixture}, []string{"cloudflared", "tunnel", "run", "--token=" + Hidden}},
		{[]string{"cloudflared", "-token", tokenFixture, "tunnel", "run"}, []string{"cloudflared", "-token", Hidden, "tunnel", "run"}},
		{[]string{"cloudflared", "tunnel", "run", "--credentials-contents", `{"TunnelSecret":"x"}`}, []string{"cloudflared", "tunnel", "run", "--credentials-contents", Hidden}},
		{[]string{"sh", "-c", "exec cloudflared tunnel run --token " + tokenFixture + " --loglevel info"}, []string{"sh", "-c", "exec cloudflared tunnel run --token " + Hidden + " --loglevel info"}},
		// A token file names where the secret is, not the secret: it stays.
		{[]string{"cloudflared", "tunnel", "run", "--token-file", "/run/secrets/tunnel"}, []string{"cloudflared", "tunnel", "run", "--token-file", "/run/secrets/tunnel"}},
		// Cut off where only the start was read: hidden all the same.
		{[]string{"cloudflared", "tunnel", "run", "--token", tokenFixture[:20]}, []string{"cloudflared", "tunnel", "run", "--token", Hidden}},
		{[]string{"nginx", "-g", "daemon off;"}, []string{"nginx", "-g", "daemon off;"}},
	} {
		if got := redact(c.in); !slices.Equal(got, c.want) {
			t.Errorf("redact(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The process list the agent sends never holds a connector token, as a unit that
// "cloudflared service install" wrote runs one on its command line (ADR 0104, the
// review's M8).
func TestTheProcessListHoldsNoToken(t *testing.T) {
	dir := fstest.MapFS{
		"stat":    {Data: []byte("4242 (cloudflared) S 1 4242 4242 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 9 0 5000 1000 100 18446744073709551615")},
		"status":  {Data: []byte("Name:\tcloudflared\nTgid:\t4242\nUid:\t0\t0\t0\t0\n")},
		"cmdline": {Data: []byte("/usr/bin/cloudflared\x00--no-autoupdate\x00tunnel\x00run\x00--token\x00" + tokenFixture + "\x00")},
		"cgroup":  {Data: []byte("0::/system.slice/cloudflared.service\n")},
	}
	p, ok := readProcessIn(dir, 4242)
	if !ok {
		t.Fatal("not read")
	}
	if strings.Contains(p.command, tokenFixture) || strings.Contains(p.command, tokenFixture[:16]) {
		t.Fatalf("the command line holds the token: %q", p.command)
	}
	if p.command != "/usr/bin/cloudflared --no-autoupdate tunnel run --token "+Hidden || p.unit != "cloudflared.service" {
		t.Fatalf("read %q in %q", p.command, p.unit)
	}
}
