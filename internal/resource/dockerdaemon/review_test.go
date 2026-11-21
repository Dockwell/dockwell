// SPDX-License-Identifier: AGPL-3.0-only

package dockerdaemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/netguard"
)

func TestUndoPreservesAbsentDaemonKeysAfterSerialization(t *testing.T) {
	draft := &spec.DockerDaemon{
		Mirrors:      &spec.DockerRegistryMirrors{Urls: []string{"https://mirror.example"}},
		Logging:      &spec.DockerDaemonLogging{Driver: "json-file", MaxSizeBytes: 10 << 20, MaxFiles: 5},
		AddressPools: &spec.DockerDefaultAddressPools{Pools: []*spec.DockerAddressPool{{Base: "192.0.2.0/24", PrefixLength: 28}}},
	}
	cfg, err := decodeConfig([]byte(`{"debug":true,"log-driver":"local"}`))
	if err != nil {
		t.Fatal(err)
	}
	next, baseline, _, err := ownedOverlay(cfg, record{}, draft)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record{Owned: draft, Baseline: baseline})
	if err != nil {
		t.Fatal(err)
	}
	var held record
	if err := json.Unmarshal(encoded, &held); err != nil {
		t.Fatal(err)
	}
	restored, _, _, err := ownedOverlay(next, held, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := encodeConfig(cfg)
	got, _ := encodeConfig(restored)
	if !bytes.Equal(got, want) {
		t.Fatalf("Undo restored %s, want %s", got, want)
	}
}

func TestActivationUsesChangedDaemonKeys(t *testing.T) {
	tests := []struct {
		old, next        string
		changed          string
		action, recovery v1.DockerDaemonAction
	}{
		{`{}`, `{}`, "", 0, 0},
		{`{}`, `{"registry-mirrors":[]}`, "registry-mirrors", 0, 0},
		{`{}`, `{"registry-mirrors":["https://mirror.example"]}`, "registry-mirrors", reload, restart},
		{`{"registry-mirrors":["https://mirror.example"]}`, `{}`, "registry-mirrors", restart, reload},
		{`{"registry-mirrors":[]}`, `{"registry-mirrors":["https://mirror.example"]}`, "registry-mirrors", reload, reload},
		{`{}`, `{"log-opts":{"max-size":"10m"}}`, "log-opts", restart, restart},
		{`{}`, `{"default-address-pools":[]}`, "default-address-pools", restart, restart},
	}
	for _, tc := range tests {
		next, err := decodeConfig([]byte(tc.next))
		if err != nil {
			t.Fatal(err)
		}
		action, recovery := settingsActions(map[string]bool{tc.changed: true}, []byte(tc.old), next)
		if action != tc.action || recovery != tc.recovery {
			t.Errorf("%s to %s = %v, %v; want %v, %v", tc.old, tc.next, action, recovery, tc.action, tc.recovery)
		}
	}
}

func TestDaemonRejectsUnactivatedHandEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	active := []byte(`{"debug":false}`)
	pending := []byte(`{"debug":true}`)
	if err := os.WriteFile(path, pending, 0o600); err != nil {
		t.Fatal(err)
	}
	src := source{path: path, started: time.Now().Add(-time.Hour)}
	for _, stamp := range []time.Time{time.Now(), src.started.Add(-time.Hour)} {
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if err := checkConfigAge(src, pending, record{ConfigDigest: configDigest(active)}); !errors.Is(err, errPendingEdit) {
			t.Fatalf("pending edit at %s passed: %v", stamp, err)
		}
	}
	if err := checkConfigAge(src, pending, record{ConfigDigest: configDigest(pending)}); err != nil {
		t.Fatalf("committed settings were refused: %v", err)
	}
}

func TestUnitDurationParsesSystemdTimes(t *testing.T) {
	for input, want := range map[string]time.Duration{
		"infinity": 0, "0": 0, "1min 30s": 90 * time.Second, "250ms": 250 * time.Millisecond,
		"2h 15min 30ms 5us": 2*time.Hour + 15*time.Minute + 30*time.Millisecond + 5*time.Microsecond,
		"invalid":           -1,
	} {
		if got := unitDuration(input); got != want {
			t.Errorf("unitDuration(%q) = %s; want %s", input, got, want)
		}
	}
}

func TestLoggingSummaryUsesEffectiveDefaults(t *testing.T) {
	for _, tc := range []struct {
		logs     docker.LogConfig
		defaults *spec.DockerDaemonLogging
		same     bool
	}{
		{docker.LogConfig{Type: "local"}, &spec.DockerDaemonLogging{Driver: "local", MaxSizeBytes: 20 << 20, MaxFiles: 5}, true},
		{docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10mb"}}, &spec.DockerDaemonLogging{Driver: "json-file", MaxSizeBytes: 10_000_000, MaxFiles: 1}, true},
		{docker.LogConfig{Type: "json-file"}, &spec.DockerDaemonLogging{Driver: "json-file", MaxSizeBytes: 10 << 20, MaxFiles: 1}, false},
		{docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "20m", "max-file": "5"}}, &spec.DockerDaemonLogging{Driver: "local", MaxSizeBytes: 20 << 20, MaxFiles: 5}, false},
	} {
		if got := sameLogging(&tc.logs, tc.defaults); got != tc.same {
			t.Errorf("%v compared with %v = %v, want %v", tc.logs, &tc.defaults, got, tc.same)
		}
	}
}

func TestMirrorProbeExcludesNonPublicHosts(t *testing.T) {
	client := netguard.Client(netguard.Options{Timeout: time.Second, MaxBody: 1})
	for _, url := range []string{"https://localhost", "https://127.0.0.1", "https://[::1]", "https://10.1.2.3", "https://100.64.0.1", "https://[fd00::2]"} {
		if got := mirrorStatus(t.Context(), client, url); got != v1.DockerMirrorReachability_DOCKER_MIRROR_REACHABILITY_NOT_CHECKED {
			t.Errorf("%s: got %v, want unverified", url, got)
		}
	}
}

func TestCurrentDockerFileDiffShowsOnlyReviewableChanges(t *testing.T) {
	before, err := decodeConfig([]byte(`{"live-restore":true,"registry-mirrors":["https://mirror.example"],"log-opts":{"max-size":"10m","max-file":"5"}}`))
	if err != nil {
		t.Fatal(err)
	}
	edited, err := decodeConfig([]byte(`{"live-restore":true,"registry-mirrors":["https://other.example"],"log-opts":{"max-size":"20m","max-file":"5"}}`))
	if err != nil {
		t.Fatal(err)
	}
	diff, _, err := activeFileChanges(before, edited)
	if err != nil || len(diff) != 2 || diff[0].GetFrom() != `["https://mirror.example"]` ||
		diff[0].GetTo() != `["https://other.example"]` || diff[1].GetPath() != "log-opts.max-size" {
		t.Fatalf("diff = %v, error = %v", diff, err)
	}
	edited["live-restore"] = json.RawMessage("false")
	if _, _, err := activeFileChanges(before, edited); err == nil {
		t.Fatal("a read-only setting was offered for activation")
	}
}

func TestCurrentDockerFileIgnoresMetadataChangesAfterSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	active := []byte(`{"debug":false}`)
	if err := os.WriteFile(path, active, 0o600); err != nil {
		t.Fatal(err)
	}
	src := source{path: path, started: time.Now().Add(-time.Hour)}
	var held record
	held.setActive(active)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := checkConfigAge(src, active, held); err != nil {
		t.Fatalf("mode-only edit: %v", err)
	}
	if err := checkConfigAge(src, []byte(`{"debug":true}`), held); !errors.Is(err, errPendingEdit) {
		t.Fatalf("file content changed: %v", err)
	}
}

func TestStartupRecoveryDoesNotWaitForLiveWorker(t *testing.T) {
	dir := t.TempDir()
	live, err := openTransactionLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := tryTransactionLock(live); err != nil {
		t.Fatal(err)
	}
	startup, err := openTransactionLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer startup.Close()
	if err := tryTransactionLock(startup); !errors.Is(err, errWorkerActive) {
		t.Fatalf("startup acquired a live worker's lock: %v", err)
	}
}
