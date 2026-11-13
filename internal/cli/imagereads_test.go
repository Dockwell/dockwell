// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"io"
	"testing"
)

// A test reads no image from a registry unless DOCKWELL_TEST_REGISTRY says where:
// a registry of stand-ins, or "public", each image's own as a server reads it, for a
// test on real Docker that pulls from them anyway (ADR 0062, ADR 0085). On a server
// it's ignored.
func TestATestReadsImagesOnlyWhereItsTold(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		off      bool
		standsIn string
	}{
		{"a server", map[string]string{envTestRegistry: "http://127.0.0.1:5000"}, false, ""},
		{"a test", map[string]string{envTest: "1"}, true, ""},
		{"a test with stand-ins", map[string]string{envTest: "1", envTestRegistry: "http://127.0.0.1:5000"}, false, "http://127.0.0.1:5000"},
		{"a test that pulls anyway", map[string]string{envTest: "1", envTestRegistry: testRegistryPublic}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := imageReads(IO{Out: io.Discard, Err: io.Discard, Getenv: func(k string) string { return tc.env[k] }})
			if (got.Off != "") != tc.off || got.Test != tc.standsIn {
				t.Errorf("imageReads = %+v, want off %v and stand-ins %q", got, tc.off, tc.standsIn)
			}
		})
	}
}
