// SPDX-License-Identifier: AGPL-3.0-only

package dockertest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
)

// PrivateDaemon prepares a private Docker data root and socket. Each launch
// returns a stop function for just that daemon, so tests can restart it.
func PrivateDaemon(t *testing.T) (*docker.Client, func() func()) {
	t.Helper()
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
	socket := filepath.Join(root, "docker.sock")
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
		//nolint:gosec // dockerd is found once on PATH and all arguments are private test fixtures.
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
	return client, launch
}
