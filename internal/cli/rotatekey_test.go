// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"strings"
	"testing"
)

// dockwell rotate-key runs beside a running control plane, on its data
// directory, and nowhere else.
func TestRotateKeyThroughTheCLI(t *testing.T) {
	dir := t.TempDir()
	startRunWithOutput(t, dir, "/claim#")
	none := map[string]string{envLogFormat: "json"}
	for range 2 {
		r := run(t, none, "rotate-key", "--data-dir", dir)
		if r.code != exitOK || !strings.Contains(r.out, "data keys under a new key-encryption key, kek_") {
			t.Fatalf("dockwell rotate-key = %d:\n%s%s", r.code, r.out, r.err)
		}
	}
	r := run(t, none, "rotate-key", "--data-dir", t.TempDir())
	if r.code != exitError || !strings.Contains(r.err, "there's no control plane") {
		t.Errorf("dockwell rotate-key away from the control server = %d %q", r.code, r.err)
	}
}
