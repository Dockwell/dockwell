// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
)

// A server's firewall is read through its agent, with the ports Docker publishes,
// and reading it changes nothing (ADR 0052).
func TestAServersFirewallIsReadThroughItsAgent(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.AddContainer(docker.Container{ID: "a1", Name: "/by-hand", HostConfig: docker.HostConfig{
		PortBindings: map[string][]docker.PortBinding{"80/tcp": {{HostIP: "0.0.0.0", HostPort: "8080"}}},
	}})
	fc := dockwellv1connect.NewFirewallServiceClient(b, b.origin, connect.WithProtoJSON())
	id := f.lastDocker.id.ServerID
	got, err := fc.GetServerFirewall(t.Context(), connect.NewRequest(&dockwellv1.GetServerFirewallRequest{ServerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	fw := got.Msg.GetFirewall()
	if fw.GetTime() == nil {
		t.Error("it says nothing of when it was read")
	}
	var published []string
	for _, p := range fw.GetPublished() {
		published = append(published, p.GetContainer()+":"+p.GetHostIp())
	}
	if !strings.Contains(strings.Join(published, " "), "by-hand:0.0.0.0") {
		t.Errorf("published %v, want by-hand's 8080", published)
	}

	if _, err := fc.GetServerFirewall(t.Context(), connect.NewRequest(&dockwellv1.GetServerFirewallRequest{ServerId: "srv_nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown server: %v", err)
	}
	f.lastDocker.stop()
	waitConnected(t, f, id, false)
	_, err = fc.GetServerFirewall(t.Context(), connect.NewRequest(&dockwellv1.GetServerFirewallRequest{ServerId: id}))
	if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "isn't connected") {
		t.Errorf("a server whose agent has gone: %v, want Unavailable, saying why", err)
	}
}
