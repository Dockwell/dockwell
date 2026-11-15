// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"

	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/probe"
	"github.com/dockwell/dockwell/internal/store"
)

// The control plane asks a server's agent to check a connection from where an app
// is: the request goes sealed, the agent runs the check on the app's own networks,
// and what comes back is codes. With no app, the check runs on a network of its own,
// which reaches nothing the app's would (ADR 0099).
func TestAConnectionIsCheckedThroughItsAgent(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	e := f.addDockerServer(t)
	const env = "env_01jwd8r6q4wexz3c6v0wzm5k8g"
	project := docker.Project(env)
	// The app's network holds this machine's loopback, where the fake S3 listens.
	e.AddNetwork(docker.Network{Name: project + "_default", IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "127.0.0.0/8"}}}})
	e.AddLocalImage("ghcr.io/acme/notes:1", dockertest.Image{Config: docker.Config{Cmd: []string{"notes"}}})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), project+"-notes-1", &docker.CreateRequest{
		Config: docker.Config{Image: "ghcr.io/acme/notes:1", Labels: map[string]string{
			docker.LabelProject: project, docker.LabelService: "notes", docker.LabelNumber: "1",
		}},
		HostConfig: docker.HostConfig{NetworkMode: project + "_default"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	session, ok := f.agents.Session(f.lastDocker.id.ServerID)
	if !ok || !session.ProbesConnections() {
		t.Fatalf("the server's agent checks no connections: %v", ok)
	}
	srv := s3test.New(t)
	req := probe.Request{Kind: probe.KindS3, S3: &probe.S3{
		Endpoint: srv.URL(), Region: "auto", Bucket: "uploads", Prefix: "notes/",
		AccessKey: "AKIDEXAMPLE", SecretKey: "the-secret", Presign: true,
	}}
	got, err := session.ProbeConnection(t.Context(), env, "notes", req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.GetPassed() {
		t.Errorf("the bucket, from the app's network, checks as %v", got)
	}
	got, err = session.ProbeConnection(t.Context(), env, "", req)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetPassed() || got.GetCode() != probe.CodeRefused {
		t.Errorf("the bucket, from a network of the check's own, checks as %v", got)
	}
}
