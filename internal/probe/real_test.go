// SPDX-License-Identifier: AGPL-3.0-only

package probe_test

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/backup/s3"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/probe"
)

// storageImage is the S3 server the app's network holds, as scripts/catalog-e2e.sh
// pins it.
const storageImage = "rustfs/rustfs:1.0.2@sha256:7e948dad4748bda48f8985fda7195f8cf2e4259427011f6120acc2c9acf51587"

const (
	storageAccess = "probe-access-key"
	storageSecret = "probe-secret-key-0123456789"
)

// On a real Docker, a check runs from the agent's own binary in a container of
// its own on the app's network: it reaches the app's storage by the name the app
// uses, signs in, writes, reads, fetches by a presigned URL and deletes; a wrong
// secret comes back as the service's code alone; without the app's network the
// name reaches nothing; the server itself and the metadata service are refused;
// and nothing of the check is left (ADR 0099).
func TestAProbeChecksFromAnAppsNetworkOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	binary := agentBinary(t)
	network := "dwprobe-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := client.CreateNetwork(t.Context(), &docker.NetworkCreate{Name: network, CheckDuplicate: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.RemoveNetwork(context.Background(), network) })
	if err := client.PullImage(t.Context(), storageImage, nil); err != nil {
		t.Fatal(err)
	}
	id, err := client.CreateContainer(t.Context(), network+"-storage", &docker.CreateRequest{
		Config:     docker.Config{Image: storageImage, Env: []string{"RUSTFS_ACCESS_KEY=" + storageAccess, "RUSTFS_SECRET_KEY=" + storageSecret}},
		HostConfig: docker.HostConfig{NetworkMode: network},
		NetworkingConfig: docker.NetworkingConfig{EndpointsConfig: map[string]*docker.EndpointSettings{
			network: {Aliases: []string{"storage"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.RemoveContainer(context.Background(), id, true) })
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	c, err := client.InspectContainer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	addr := c.NetworkSettings.Networks[network].IPAddress
	makeBucket(t, "http://"+addr+":9000", "uploads")
	n, err := client.InspectNetwork(t.Context(), network)
	if err != nil || len(n.IPAM.Config) == 0 {
		t.Fatalf("the app's network reads %v, %v", n, err)
	}
	gateway := n.IPAM.Config[0].Gateway

	r := &probe.Runner{Client: client, Binary: binary}
	check := func(endpoint, secret string, networks []string) probe.Result {
		t.Helper()
		got, err := r.Run(t.Context(), probe.Request{Kind: probe.KindS3, S3: &probe.S3{
			Endpoint: endpoint, Region: "us-east-1", Bucket: "uploads", Prefix: "app/",
			AccessKey: storageAccess, SecretKey: secret, Presign: true,
		}}, networks)
		if err != nil {
			t.Fatalf("checking %s: %v", endpoint, err)
		}
		return got
	}
	if got := check("http://storage:9000", storageSecret, []string{network}); !got.Passed {
		t.Errorf("the app's storage, by the name it uses, checks as %+v", got)
	}
	if got := check("http://storage:9000", "not-the-secret", []string{network}); got.Passed || got.Code != "SignatureDoesNotMatch" || got.Message != "" {
		t.Errorf("a wrong secret checks as %+v", got)
	}
	if got := check("http://storage:9000", storageSecret, nil); got.Passed || got.Code != probe.CodeUnreachable {
		t.Errorf("the app's storage, from a check of its own, checks as %+v", got)
	}
	if gateway != "" {
		if got := check("http://"+gateway+":9000", storageSecret, []string{network}); got.Passed || got.Code != probe.CodeRefused {
			t.Errorf("the server, at its bridge's address, checks as %+v", got)
		}
	}
	if got := check("http://169.254.169.254", storageSecret, []string{network}); got.Passed || got.Code != probe.CodeRefused {
		t.Errorf("the metadata service checks as %+v", got)
	}

	left, err := client.ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProbe}})
	if err != nil || len(left) != 0 {
		t.Errorf("the checks left containers %v, %v", left, err)
	}
	if tags, err := client.ImageTags(t.Context(), docker.ProbeRepository); err != nil || len(tags) != 0 {
		t.Errorf("the checks left images %v, %v", tags, err)
	}
}

// agentBinary builds the agent, static, as the probe's container runs it.
func agentBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "dockwell")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-tags", "agent", "-o", out, "github.com/dockwell/dockwell/cmd/dockwell")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if said, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the agent: %v\n%s", err, said)
	}
	return out
}

// makeBucket makes a bucket from here, waiting for the server to start.
func makeBucket(t *testing.T, endpoint, name string) {
	t.Helper()
	const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	var last string
	for range 60 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, endpoint+"/"+name, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		s3.SignV4(req, emptyHash, storageAccess, storageSecret, "us-east-1", "s3", time.Now())
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusConflict {
				return
			}
			last = res.Status
		} else {
			last = err.Error()
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("the storage didn't make its bucket: %s", last)
}
