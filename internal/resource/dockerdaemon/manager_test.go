// SPDX-License-Identifier: AGPL-3.0-only

package dockerdaemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonValidatorUsesTheInstalledProxy(t *testing.T) {
	dir := t.TempDir()
	proxy := filepath.Join(dir, "docker-proxy")
	if err := os.WriteFile(proxy, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	dockerd := filepath.Join(dir, "dockerd")
	if err := os.WriteFile(dockerd, []byte(`#!/bin/sh
if [ "$(command -v docker-proxy)" = "${0%/*}/docker-proxy" ]; then
    exit 0
fi
echo 'invalid userland-proxy-path: userland-proxy is enabled, but userland-proxy-path is not set' >&2
exit 1
`), 0o700); err != nil {
		t.Fatal(err)
	}
	m := systemd{}
	src := source{binary: dockerd}
	if err := m.validate(t.Context(), src, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(proxy); err != nil {
		t.Fatal(err)
	}
	if err := m.validate(t.Context(), src, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "could not find docker-proxy") {
		t.Fatalf("missing helper: %v", err)
	}
}

func TestDaemonValidatorReportsConflictingFlagWithoutValues(t *testing.T) {
	dockerd := filepath.Join(t.TempDir(), "dockerd")
	if err := os.WriteFile(dockerd, []byte(`#!/bin/sh
echo 'the following directives are specified both as a flag and in the configuration file: registry-mirrors: (from flag: [flag-secret], from file: [file-secret])' >&2
exit 1
`), 0o700); err != nil {
		t.Fatal(err)
	}
	err := (systemd{}).validate(t.Context(), source{binary: dockerd}, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "--registry-mirror both at startup and in daemon.json") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("conflicting flag: %v", err)
	}
}

func TestDaemonValidationOutputIsBounded(t *testing.T) {
	var out validationOutput
	for range 3 {
		block := bytes.Repeat([]byte("x"), 3000)
		if n, err := out.Write(block); err != nil || n != len(block) {
			t.Fatalf("writing stderr: %d, %v", n, err)
		}
	}
	if len(out) != 4096 {
		t.Fatalf("saved %d bytes of stderr, want 4096", len(out))
	}
}
