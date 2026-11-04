// SPDX-License-Identifier: AGPL-3.0-only

package dockertest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
)

// EnvReal turns on the tests against a real Docker Engine: "1" for the
// one at docker.DefaultSocket, or a socket's path. CI's Docker job sets
// it.
const EnvReal = "DOCKWELL_TEST_DOCKER"

// Real returns a client for a real Docker Engine, or skips the test, with
// how to run it, unless EnvReal is set. Once it is set, an engine that
// doesn't answer fails the test.
func Real(t testing.TB) *docker.Client {
	t.Helper()
	v := os.Getenv(EnvReal)
	if v == "" {
		t.Skipf("skipped: this test runs against a real Docker Engine. Set %s=1 to run it against %s, or to a socket's path", EnvReal, docker.DefaultSocket)
	}
	socket := docker.DefaultSocket
	if v != "1" {
		socket = v
	}
	c := docker.New(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("%s is set, but Docker doesn't answer: %v", EnvReal, err)
	}
	t.Cleanup(c.Close)
	return c
}

// Scope returns a new environment ID, so what a test makes on a real
// engine has a Compose project of its own, which Cleanup removes.
func Scope(t testing.TB) string {
	t.Helper()
	b := make([]byte, 13)
	_, _ = rand.Read(b)
	return "env_" + strings.ToUpper(hex.EncodeToString(b))[:26]
}

// Cleanup removes, when the test ends, every container, volume and
// network a real engine has in a Compose project.
func Cleanup(t testing.TB, c *docker.Client, project string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := exec.LookPath("docker"); err != nil {
			return
		}
		// docker compose down needs no file to remove a project's
		// containers, networks and volumes.
		cmd := exec.Command("docker", "compose", "-p", project, "down", "--volumes", "--remove-orphans", "--timeout", "1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("can't clean up the Compose project %s: %v\n%s", project, err, out)
		}
	})
}
