// SPDX-License-Identifier: AGPL-3.0-only

package dockerdaemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

type installedWorker struct {
	systemd
	binary string
}

func (w installedWorker) launch(ctx context.Context, dir, name string) error {
	return launchWorker(ctx, dir, name, w.binary)
}

func realSystemd(t *testing.T) (*Driver, installedWorker) {
	t.Helper()
	binary := os.Getenv("DOCKWELL_TEST_DAEMON_SYSTEMD")
	if binary == "" {
		t.Skip("requires a disposable systemd runner with Docker and the installed agent")
	}
	dir, err := os.MkdirTemp("/run", "dockwell-settings-ci-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	m := installedWorker{systemd{docker.DefaultSocket}, binary}
	client := docker.New(docker.DefaultSocket)
	t.Cleanup(client.Close)
	before, _, mode, err := readConfig(defaultConfig)
	if err != nil || before == nil {
		t.Fatalf("systemd fixture config: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := atomicFile(defaultConfig, before, mode); err != nil {
			t.Error(err)
		} else if err := m.action(ctx, restart); err != nil || !waitHealthy(ctx, client, nil) {
			t.Errorf("restore fixture Docker: %v", err)
		}
	})
	return &Driver{Client: client, Dir: dir, manager: m}, m
}

func systemdSource(t *testing.T, m installedWorker) source {
	t.Helper()
	src, err := m.source(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func systemdMirror() *spec.DockerDaemon {
	return &spec.DockerDaemon{Mirrors: &spec.DockerRegistryMirrors{Urls: []string{"https://mirror.example"}}}
}

func reviewedSystemdStep(t *testing.T, d *Driver, want *spec.DockerDaemon, action v1.DockerDaemonAction) resource.Step {
	t.Helper()
	p, err := d.prepare(t.Context(), want)
	if err != nil || p.plan.GetImpact().GetAction() != action {
		t.Fatalf("plan = %v, err = %v; want %v", p.plan, err, action)
	}
	return resource.Step{DockerDaemon: p.plan}
}

func applySystemd(t *testing.T, d *Driver, step resource.Step, want *spec.DockerDaemon) {
	t.Helper()
	ctx := resource.WithDeploy(t.Context(), resource.Deploy{Change: "reviewed-settings"})
	if err := d.Apply(ctx, step, want); err != nil {
		t.Fatal(err)
	}
	// Resuming an acknowledged change must read the same durable outcome.
	if err := d.Apply(ctx, step, want); err != nil {
		t.Fatalf("resume: %v", err)
	}
}

func TestDockerSettingsReloadOnSystemd(t *testing.T) {
	d, m := realSystemd(t)
	before := systemdSource(t, m)
	want := systemdMirror()
	applySystemd(t, d, reviewedSystemdStep(t, d, want, reload), want)
	info, err := d.Client.Info(t.Context())
	if err != nil || info.RegistryConfig == nil || !slices.EqualFunc(info.RegistryConfig.Mirrors, want.Mirrors.Urls, func(a, b string) bool {
		return strings.TrimRight(a, "/") == b
	}) || before.pid != systemdSource(t, m).pid {
		t.Fatalf("reload did not apply in the same process: %v", err)
	}
	p, err := d.prepare(t.Context(), want)
	if err != nil || p.plan != nil {
		t.Fatalf("identical settings: plan = %v, err = %v", p.plan, err)
	}
}

func dockerFixture(t *testing.T, args ...string) string {
	t.Helper()
	out, err := cleanCommand(t.Context(), "/usr/bin/docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

func systemdWorkload(t *testing.T, run bool) string {
	t.Helper()
	args := []string{"create", "--pull=never", "--network=none", "busybox:1.37", "sleep", "300"}
	if run {
		args[0] = "run"
		args = append(args[:1], append([]string{"-d"}, args[1:]...)...)
	}
	id := dockerFixture(t, args...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := cleanCommand(ctx, "/usr/bin/docker", "rm", "-f", id).Run(); err != nil {
			t.Error(err)
		}
	})
	return id
}

func systemdLogging() *spec.DockerDaemon {
	return &spec.DockerDaemon{Logging: &spec.DockerDaemonLogging{Driver: "json-file", MaxSizeBytes: 10 << 20, MaxFiles: 5}}
}

func TestDockerSettingsRestartOnSystemd(t *testing.T) {
	d, m := realSystemd(t)
	systemdWorkload(t, false)
	before := systemdSource(t, m)
	want := systemdLogging()
	applySystemd(t, d, reviewedSystemdStep(t, d, want, restart), want)
	if before.pid == systemdSource(t, m).pid {
		t.Fatal("Docker did not restart")
	}
	created := systemdWorkload(t, false)
	container, err := d.Client.InspectContainer(t.Context(), created)
	if err != nil || container.HostConfig.LogConfig.Config["max-size"] != "10485760" || container.HostConfig.LogConfig.Config["max-file"] != "5" {
		t.Fatalf("new container did not inherit reviewed limits: %v", err)
	}
}

func failingSystemdCandidate(t *testing.T, d *Driver, m installedWorker) (*transaction, string) {
	t.Helper()
	src := systemdSource(t, m)
	before, _, mode, err := readConfig(src.path)
	if err != nil {
		t.Fatal(err)
	}
	timing, err := m.timing(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Only the disposable test runner stages this candidate. The JSON is
	// valid, but startup cannot create a data root under a character device.
	candidate := []byte(`{"data-root":"/dev/null/dockwell-ci","live-restore":true}`)
	if err := m.validate(t.Context(), src, candidate); err != nil {
		t.Fatal(err)
	}
	tx := &transaction{
		Config: defaultConfig, Socket: docker.DefaultSocket, Binary: src.binary, PID: src.pid, Args: src.args,
		Started: src.started, Previous: before, Candidate: candidate, Mode: mode, Action: restart, Recovery: restart, Timing: timing,
	}
	const name = "90e734824f1d4ef894c8c13855f30ac8"
	dir := filepath.Join(d.Dir, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeRecord(filepath.Join(dir, "transaction.json"), tx); err != nil {
		t.Fatal(err)
	}
	return tx, dir
}

func waitForMarker(t *testing.T, dir, name string) {
	t.Helper()
	wait, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for !marker(dir, name) {
		select {
		case <-wait.Done():
			t.Fatalf("%s did not appear: %v", name, wait.Err())
		case <-poll.C:
		}
	}
}

func requireSystemdRestore(t *testing.T, d *Driver, tx *transaction, dir string) {
	t.Helper()
	waitForMarker(t, dir, "result.json")
	var result outcome
	if err := readRecord(filepath.Join(dir, "result.json"), &result); err != nil || result.State != "restored" {
		t.Fatalf("recovery = %+v, err = %v", result, err)
	}
	actual, _, mode, err := readConfig(tx.Config)
	if err != nil || mode != tx.Mode || !slices.Equal(actual, tx.Previous) || d.Client.Ping(t.Context()) != nil {
		t.Fatalf("Docker or previous file was not restored: %v", err)
	}
}

func TestDockerSettingsRestoreFailedStartOnSystemd(t *testing.T) {
	d, m := realSystemd(t)
	tx, dir := failingSystemdCandidate(t, d, m)
	if err := m.launch(t.Context(), dir, filepath.Base(dir)); err != nil {
		t.Fatal(err)
	}
	requireSystemdRestore(t, d, tx, dir)
}

func TestDockerSettingsRestoresAfterStartLimitOnSystemd(t *testing.T) {
	d, m := realSystemd(t)
	const dropIn = "/run/systemd/system/docker.service.d/98-dockwell-ci.conf"
	if err := os.MkdirAll(filepath.Dir(dropIn), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dropIn, []byte("[Unit]\nStartLimitIntervalSec=60s\nStartLimitBurst=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(os.Remove(dropIn), cleanCommand(context.Background(), systemctl, "daemon-reload").Run()); err != nil {
			t.Error(err)
		}
	})
	if err := cleanCommand(t.Context(), systemctl, "daemon-reload").Run(); err != nil {
		t.Fatal(err)
	}
	// Consume the unit's single start before the forward transaction. The
	// failed candidate consumes it again before recovery.
	if err := m.action(t.Context(), restart); err != nil {
		t.Fatal(err)
	}
	tx, dir := failingSystemdCandidate(t, d, m)
	if tx.Timing.Restart < 60*time.Second {
		t.Fatalf("start limit was not read: %+v", tx.Timing)
	}
	if err := m.launch(t.Context(), dir, filepath.Base(dir)); err != nil {
		t.Fatal(err)
	}
	requireSystemdRestore(t, d, tx, dir)
}

func TestDockerSettingsLiveRestoreOnSystemd(t *testing.T) {
	d, m := realSystemd(t)
	id := systemdWorkload(t, true)
	before, err := d.Client.InspectContainer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	src := systemdSource(t, m)
	want := systemdLogging()
	step := reviewedSystemdStep(t, d, want, restart)
	if !step.DockerDaemon.GetImpact().GetLiveRestore() {
		t.Fatal("the review should say that live restore is on")
	}
	applySystemd(t, d, step, want)
	after, err := d.Client.InspectContainer(t.Context(), id)
	if err != nil || !after.State.Running || before.State.Pid != after.State.Pid || src.pid == systemdSource(t, m).pid {
		t.Fatalf("Docker did not restart or the running container was interrupted: %v", err)
	}
}

func TestDockerSettingsWorkerLossOnSystemd(t *testing.T) {
	d, m := realSystemd(t)
	want := systemdMirror()
	step := reviewedSystemdStep(t, d, want, reload)
	const name = "3c9507f205e34fd49d8102feff12b8ba"
	dir := filepath.Join(d.Dir, name)
	// Stage without an acknowledging call, as when the agent loses its stream.
	if err := d.stage(t.Context(), step, want, dir, name); err != nil {
		t.Fatal(err)
	}
	waitForMarker(t, dir, "ready")
	unit := "dockwell-docker-" + name + ".service"
	if err := cleanCommand(t.Context(), systemctl, "kill", "--kill-who=main", "--signal=KILL", unit).Run(); err != nil {
		t.Fatal(err)
	}
	if err := cleanCommand(t.Context(), systemctl, "stop", unit).Run(); err != nil {
		t.Fatal(err)
	}
	if marker(dir, "result.json") {
		t.Fatal("worker finished before startup recovery could take over")
	}
	var tx transaction
	if err := readRecord(filepath.Join(dir, "transaction.json"), &tx); err != nil {
		t.Fatal(err)
	}
	if current, _, _, err := readConfig(defaultConfig); err != nil || !slices.Equal(current, tx.Candidate) {
		t.Fatalf("worker loss did not leave the staged candidate: %v", err)
	}
	// This is the startup entry point. No control plane needs to reconnect.
	d.Recover(t.Context(), slog.New(slog.DiscardHandler))
	requireSystemdRestore(t, d, &tx, dir)
	if current := systemdSource(t, m); current.pid != tx.PID {
		t.Fatalf("a reload-only recovery restarted Docker: PID %s → %s", tx.PID, current.pid)
	}
}

func TestDockerSettingsAgentRestartAcknowledgesLiveWorkerOnSystemd(t *testing.T) {
	d, manager := realSystemd(t)
	systemdWorkload(t, false)
	want := systemdLogging()
	step := reviewedSystemdStep(t, d, want, restart)
	const changeID = "restarted-agent"
	token := sha256.Sum256([]byte(changeID))
	name := hex.EncodeToString(token[:16])
	dir := filepath.Join(d.Dir, name)
	before := systemdSource(t, manager)
	if err := d.stage(t.Context(), step, want, dir, name); err != nil {
		t.Fatal(err)
	}
	waitForMarker(t, dir, "ready")
	restartedDocker := systemdSource(t, manager)
	if restartedDocker.pid == before.pid {
		t.Fatal("the worker did not restart Docker")
	}
	// A fresh agent has no evidence key from the original call. It can
	// acknowledge the staged review while the original worker still owns it.
	fresh := &Driver{Dir: d.Dir, Client: d.Client, manager: manager}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	recovered := make(chan struct{})
	go func() {
		defer close(recovered)
		fresh.Recover(ctx, slog.New(slog.DiscardHandler))
	}()
	if err := fresh.Apply(resource.WithDeploy(ctx, resource.Deploy{Change: changeID}), step, want); err != nil {
		t.Error(err)
	}
	select {
	case <-recovered:
	case <-time.After(5 * time.Second):
		t.Error("startup recovery did not observe the worker finishing")
		cancel()
		<-recovered
	}
	if issue := fresh.RecoveryAttention(); issue != "" {
		t.Fatal(issue)
	}
	if current := systemdSource(t, manager); current.pid != restartedDocker.pid {
		t.Fatalf("resuming the call restarted Docker again: %s -> %s", restartedDocker.pid, current.pid)
	}
}

func TestDockerSettingsReviewsTheCurrentFileOnSystemd(t *testing.T) {
	d, manager := realSystemd(t)
	d.Recover(t.Context(), slog.New(slog.DiscardHandler))
	before := systemdSource(t, manager)
	original, cfg, mode, err := readConfig(defaultConfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg["registry-mirrors"] = []byte(`["https://mirror.example"]`)
	pending, err := encodeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicFile(defaultConfig, pending, mode); err != nil {
		t.Fatal(err)
	}
	if checked := d.Check(t.Context(), systemdMirror()); len(checked.Findings) != 1 || !checked.Findings[0].GetReviewCurrentFile() {
		t.Fatalf("pending file did not offer review: %v", checked)
	}
	want := &spec.DockerDaemon{ReviewCurrentFile: true}
	step := reviewedSystemdStep(t, d, want, reload)
	if len(step.DockerDaemon.FileChanges) != 1 || step.DockerDaemon.FileChanges[0].GetFrom() != "[]" {
		t.Fatalf("review did not show the loaded configuration: %v", step.DockerDaemon)
	}
	// A permission-only change after review does not invalidate the bytes,
	// and activation does not rewrite the file.
	if err := os.Chmod(defaultConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	applySystemd(t, d, step, want)
	current, _, currentMode, err := readConfig(defaultConfig)
	if err != nil || currentMode != 0o600 || !slices.Equal(current, pending) ||
		slices.Equal(current, original) || before.pid != systemdSource(t, manager).pid {
		t.Fatalf("activation rewrote the file or restarted Docker: %v", err)
	}
	held, err := d.record()
	if err != nil || held.Owned != nil || !slices.Equal(held.Active, pending) {
		t.Fatalf("activation took ownership or missed its snapshot: %v", err)
	}
}
