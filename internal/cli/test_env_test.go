// SPDX-License-Identifier: AGPL-3.0-only

package cli

import "testing"

// A test's builds keep their secrets and registry logins on disk only where both
// DOCKWELL_TEST and a folder of their own say so: DOCKWELL_TEST alone, which a
// service could carry by mistake, keeps a server's builds in memory. (Security
// review of #591, L1.)
func TestBuildSecretsGoOnDiskOnlyWhereATestSaysSo(t *testing.T) {
	for what, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"a server":                    {env: map[string]string{}, want: ""},
		"DOCKWELL_TEST alone":         {env: map[string]string{envTest: "1"}, want: ""},
		"the folder alone":            {env: map[string]string{envTestBuildSecrets: "/tmp/b"}, want: ""},
		"the folder, DOCKWELL_TEST=0": {env: map[string]string{envTest: "0", envTestBuildSecrets: "/tmp/b"}, want: ""},
		"a test":                      {env: map[string]string{envTest: "1", envTestBuildSecrets: "/tmp/b"}, want: "/tmp/b"},
	} {
		if got := testBuildSecrets(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("%s: %q", what, got)
		}
	}
}

// DOCKWELL_TEST under systemd or OpenRC is a mistake, and the log says so as the
// server starts.
func TestDockwellTestUnderAServiceManager(t *testing.T) {
	for what, c := range map[string]struct {
		env  map[string]string
		want bool
	}{
		"a test":                        {env: map[string]string{envTest: "1"}, want: false},
		"systemd with DOCKWELL_TEST":    {env: map[string]string{envTest: "1", "INVOCATION_ID": "0123abcd"}, want: true},
		"OpenRC with DOCKWELL_TEST":     {env: map[string]string{envTest: "1", "RC_SVCNAME": "dockwell"}, want: true},
		"systemd without DOCKWELL_TEST": {env: map[string]string{"INVOCATION_ID": "0123abcd"}, want: false},
	} {
		if got := testUnderService(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("%s: %v", what, got)
		}
	}
}

// A test's builds reach its registry of stand-ins over plain HTTP, where the test
// names one so, and only under DOCKWELL_TEST.
func TestATestsBuildsReachItsOwnRegistry(t *testing.T) {
	for what, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"a server":                        {env: map[string]string{envTestRegistry: "http://192.0.2.10:5000"}, want: ""},
		"a test with no registry":         {env: map[string]string{envTest: "1"}, want: ""},
		"a test's registry":               {env: map[string]string{envTest: "1", envTestRegistry: "http://192.0.2.10:5000"}, want: "192.0.2.10:5000"},
		"a test's registry, with a slash": {env: map[string]string{envTest: "1", envTestRegistry: "http://192.0.2.10:5000/"}, want: "192.0.2.10:5000"},
		"one over HTTPS":                  {env: map[string]string{envTest: "1", envTestRegistry: "https://registry.example.com"}, want: ""},
		"one with a path":                 {env: map[string]string{envTest: "1", envTestRegistry: "http://192.0.2.10:5000/v2"}, want: ""},
	} {
		if got := testPlainRegistry(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("%s: %q", what, got)
		}
	}
}
