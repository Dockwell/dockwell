// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/ingress"
)

// A form asks what holds some ports: sshd on 22 outside Docker, a container that
// publishes 8090, and nothing on 8091; and is refused what isn't a port.
func TestPortsHeldSaysWhatHoldsEachPort(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	table := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		fmt.Sprintf("   0: 00000000:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 4242 1 0000000000000000 100 0 0 10 0\n", 22)
	if err := os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	sshd := filepath.Join(root, "777")
	if err := os.MkdirAll(filepath.Join(sshd, "fd"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"comm": "sshd\n", "cgroup": "0::/system.slice/ssh.service\n"} {
		if err := os.WriteFile(filepath.Join(sshd, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("socket:[4242]", filepath.Join(sshd, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	e := dockertest.New(t)
	e.AddContainer(docker.Container{
		ID: "c1", Name: "/legacy-site", Config: docker.Config{Image: "nginx:1.27"},
		HostConfig: docker.HostConfig{PortBindings: map[string][]docker.PortBinding{"80/tcp": {{HostPort: "8090"}}}},
	})
	h := &ingress.Holders{Client: e.Client(), Proc: root, PID: 99}

	got, err := portsHeld(context.Background(), h, []*dockwellv1.ServerPortAsk{{Port: 22}, {Port: 8090}, {Port: 8091}})
	if err != nil {
		t.Fatal(err)
	}
	ports := got.GetPorts()
	if len(ports) != 3 {
		t.Fatalf("got %d answers for 3 ports", len(ports))
	}
	if !ports[0].GetHeld() || !strings.Contains(ports[0].GetBy(), "sshd") {
		t.Errorf("port 22 reads %v, want held by sshd", ports[0])
	}
	if !ports[1].GetHeld() || !strings.Contains(ports[1].GetBy(), "legacy-site") {
		t.Errorf("port 8090 reads %v, want held by legacy-site", ports[1])
	}
	if ports[2].GetHeld() || ports[2].GetBy() != "" {
		t.Errorf("port 8091 reads %v, want free", ports[2])
	}

	if _, err := portsHeld(context.Background(), h, []*dockwellv1.ServerPortAsk{{Port: 0}}); err == nil {
		t.Error("port 0 wasn't refused")
	}
	many := make([]*dockwellv1.ServerPortAsk, maxPortsAsked+1)
	for i := range many {
		many[i] = &dockwellv1.ServerPortAsk{Port: uint32(8000 + i)}
	}
	if _, err := portsHeld(context.Background(), h, many); err == nil || !strings.Contains(err.Error(), "Ask for fewer") {
		t.Errorf("65 ports: %v", err)
	}
}
