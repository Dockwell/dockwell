// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// Opt in on a host that can run a second Docker daemon. The test restarts only
// the daemon it starts, using a private socket, data root and containerd.
func TestManagedStopSurvivesDaemonRestartOnRealDocker(t *testing.T) {
	if os.Getenv("DOCKWELL_TEST_ISOLATED_DOCKER") != "1" {
		t.Skip("set DOCKWELL_TEST_ISOLATED_DOCKER=1 to launch a private Docker daemon")
	}
	dockerd, err := exec.LookPath("dockerd")
	if err != nil {
		t.Skip("dockerd is unavailable")
	}
	// A short root leaves room for containerd's Unix socket path.
	root, err := os.MkdirTemp("", "dw-engine-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	config := filepath.Join(root, "daemon.json")
	if err := os.WriteFile(config, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket, runRecords := filepath.Join(root, "docker.sock"), t.TempDir()
	client := docker.New(socket)
	t.Cleanup(client.Close)

	launch := func() func() {
		t.Helper()
		log, err := os.CreateTemp(root, "daemon-*.log")
		if err != nil {
			t.Fatal(err)
		}
		defer log.Close()
		daemonCtx, stopContext := context.WithCancel(context.Background())
		cmd := exec.CommandContext(daemonCtx, dockerd,
			"--config-file", config, "--host", "unix://"+socket,
			"--data-root", filepath.Join(root, "data"), "--exec-root", filepath.Join(root, "exec"),
			"--pidfile", filepath.Join(root, "pid"), "--storage-driver", "vfs",
			"--bridge", "none", "--iptables=false", "--ip6tables=false", "--ip-forward=false", "--ip-masq=false",
		)
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			stopContext()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
			close(done)
		}()
		stop := sync.OnceFunc(func() {
			t.Helper()
			defer stopContext()
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("private Docker daemon stopped with an error: %v", err)
				}
			case <-time.After(20 * time.Second):
				_ = cmd.Process.Kill()
				t.Errorf("private Docker daemon didn't stop gracefully: %v", <-done)
			}
		})
		t.Cleanup(stop)
		ready, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			if client.Ping(ready) == nil {
				return stop
			}
			select {
			case err := <-done:
				t.Fatalf("private Docker daemon exited before it was ready: %v", err)
			case <-ready.Done():
				t.Fatal("private Docker daemon didn't become ready")
			case <-tick.C:
			}
		}
	}

	local := func() resource.Local {
		runtime := &container.Docker{Client: client, RunStateDir: runRecords}
		return resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: runtime}})}
	}
	stopDaemon := launch()
	ref := resource.Ref{Kind: service.Kind, Scope: dockertest.Scope(t), Name: "workload"}
	app := &resourcev1.Container{
		Image: "busybox:1.37", Command: []string{"sleep", "600"},
		Restart: resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS,
	}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}
	apply(t, local(), ref, spec)
	before, err := client.ContainerNamed(t.Context(), service.ContainerName(ref))
	if err != nil {
		t.Fatal(err)
	}
	app.DesiredRunState = resourcev1.DesiredRunState_DESIRED_RUN_STATE_STOPPED
	apply(t, local(), ref, spec)
	stopDaemon()
	_ = launch()

	after, err := client.InspectContainer(t.Context(), before.ID)
	if err != nil || after.State.Running || after.HostConfig.RestartPolicy.Name != "no" {
		t.Fatalf("the same container should remain stopped with Docker's policy disabled: %v", err)
	}
	unchanged(t, local(), ref, spec)
}
