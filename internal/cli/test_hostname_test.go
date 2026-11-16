// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import "testing"

// A test names the control server with DOCKWELL_TEST_HOSTNAME only under
// DOCKWELL_TEST: a server that has the one without the other keeps its host name.
func TestATestNamesTheControlServerOnlyAsATest(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{envTestHostname: "a-long-name"}, ""},
		{map[string]string{envTest: "1", envTestHostname: "a-long-name"}, "a-long-name"},
		{map[string]string{envTest: "1"}, ""},
	} {
		if got := testHostname(IO{Getenv: func(k string) string { return c.env[k] }}); got != c.want {
			t.Errorf("with %v, the control server is named %q, want %q", c.env, got, c.want)
		}
	}
}
