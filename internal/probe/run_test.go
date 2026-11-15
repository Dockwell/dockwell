// SPDX-License-Identifier: AGPL-3.0-only

package probe_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/backup/s3"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/probe"
)

// appNetwork is an app's network whose subnet holds the fake S3, which listens on
// this machine's loopback: the probe, run here by the fake engine, reaches it only
// as one of the app's networks.
const appNetwork = "env_01jwd8r6q4wexz3c6v0wzm5k8g-default"

func runner(e *dockertest.Engine, own ...netip.Prefix) *probe.Runner {
	return &probe.Runner{
		Client: e.Client(), Binary: "/proc/self/exe",
		Own: func() ([]netip.Prefix, error) { return own, nil },
	}
}

// bucket is a check of the fake's bucket, its secrets on the probe's input alone.
func bucket(srv *s3test.Server, presign bool) probe.Request {
	return probe.Request{Kind: probe.KindS3, S3: &probe.S3{
		Endpoint: srv.URL(), Bucket: "uploads", Prefix: "outline/", Region: "auto",
		AccessKey: "AKIDEXAMPLE", SecretKey: "the-secret", Presign: presign,
	}}
}

// A check runs in a container made of the agent's own binary, on the app's
// network, with no capability, a read-only root, no log and its secrets on its
// input alone, and leaves nothing behind (ADR 0099).
func TestACheckRunsLockedDownOnTheAppsNetwork(t *testing.T) {
	t.Parallel()
	e := dockertest.New(t)
	e.AddNetwork(docker.Network{Name: appNetwork, IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "127.0.0.0/8"}}}})
	srv := s3test.New(t)
	req := bucket(srv, true)
	got, err := runner(e).Run(t.Context(), req, []string{appNetwork})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Passed {
		t.Fatalf("a working bucket on the app's network checks as %+v", got)
	}
	made := e.Helpers()
	if len(made) != 1 {
		t.Fatalf("the check made %d containers, want 1", len(made))
	}
	c := made[0]
	switch {
	case !slices.Equal(c.Cmd, []string{probe.Command}) || !slices.Equal(c.Entrypoint, []string{"/dockwell"}):
		t.Errorf("the check runs %v %v", c.Entrypoint, c.Cmd)
	case c.User != "65534:65534" || len(c.Env) != 0:
		t.Errorf("the check runs as %q with %v", c.User, c.Env)
	case c.HostConfig.NetworkMode != appNetwork:
		t.Errorf("the check runs on %q", c.HostConfig.NetworkMode)
	case !c.HostConfig.ReadonlyRootfs || !slices.Equal(c.HostConfig.CapDrop, []string{"ALL"}) ||
		!slices.Equal(c.HostConfig.SecurityOpt, []string{"no-new-privileges"}) || c.HostConfig.LogConfig == nil || c.HostConfig.LogConfig.Type != "none":
		t.Errorf("the check's container isn't locked down: %+v", c.HostConfig)
	}
	for _, field := range append(c.Cmd, c.Env...) {
		if strings.Contains(field, "the-secret") {
			t.Errorf("the check's container holds its secret outside its input: %q", field)
		}
	}
	if left := e.Containers(); len(left) != 0 {
		t.Errorf("the check left containers %v", left)
	}
	if left := e.Images(docker.ProbeRepository); len(left) != 0 {
		t.Errorf("the check left images %v", left)
	}
}

// A connection no app uses yet is checked on a network made for it, which goes
// with the check, and reaches nothing of the server's.
func TestACheckWithNoAppRunsOnItsOwnNetwork(t *testing.T) {
	t.Parallel()
	e := dockertest.New(t)
	srv := s3test.New(t)
	req := bucket(srv, false)
	got, err := runner(e).Run(t.Context(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Passed || got.Code != probe.CodeRefused {
		t.Errorf("this server's loopback, from a check of its own, checks as %+v", got)
	}
	made := e.Helpers()
	if len(made) != 1 || !strings.HasPrefix(made[0].HostConfig.NetworkMode, docker.ProbeRepository+"-") {
		t.Fatalf("the check ran on %+v", made)
	}
	if n := e.Network(made[0].HostConfig.NetworkMode); n != nil {
		t.Errorf("the check left its network %s", n.Name)
	}
}

// The server's own addresses are refused though an app's network holds them, as
// its bridge's gateway is the server.
func TestTheServersOwnAddressesAreRefusedOnAnAppsNetwork(t *testing.T) {
	t.Parallel()
	e := dockertest.New(t)
	e.AddNetwork(docker.Network{Name: appNetwork, IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "127.0.0.0/8"}}}})
	srv := s3test.New(t)
	req := bucket(srv, false)
	got, err := runner(e, netip.MustParsePrefix("127.0.0.1/32")).Run(t.Context(), req, []string{appNetwork})
	if err != nil {
		t.Fatal(err)
	}
	if got.Passed || got.Code != probe.CodeRefused || !strings.Contains(got.Words, "this server's own addresses") {
		t.Errorf("the server's own address on the app's network checks as %+v", got)
	}
}

// What a crash cut short is removed as the agent starts: the check's container,
// its network and its image.
func TestCleanRemovesWhatACrashLeft(t *testing.T) {
	t.Parallel()
	e := dockertest.New(t)
	c := e.Client()
	r := runner(e)
	if err := c.ImportFiles(t.Context(), docker.ProbeRepository+":left", []docker.ImageFile{{Name: "/dockwell", Path: "/proc/self/exe", Mode: 0o755}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateNetwork(t.Context(), &docker.NetworkCreate{Name: docker.ProbeRepository + "-left", Labels: map[string]string{docker.LabelProbe: "left"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateContainer(t.Context(), docker.ProbeRepository+"-left", &docker.CreateRequest{Config: docker.Config{
		Image: docker.ProbeRepository + ":left", Labels: map[string]string{docker.LabelProbe: "left"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Clean(t.Context()); err != nil {
		t.Fatal(err)
	}
	if left := e.Containers(); len(left) != 0 {
		t.Errorf("Clean left containers %v", left)
	}
	if left := e.Images(docker.ProbeRepository); len(left) != 0 {
		t.Errorf("Clean left images %v", left)
	}
	if n := e.Network(docker.ProbeRepository + "-left"); n != nil {
		t.Errorf("Clean left the network %s", n.Name)
	}
}

// A prefix that isn't folders in a bucket, such as one that climbs out of it, is
// refused before anything's made: a server, or a proxy before it, may read
// /uploads/../other/ as another bucket the keys reach.
func TestACheckRefusesAPrefixThatClimbsOut(t *testing.T) {
	t.Parallel()
	e := dockertest.New(t)
	srv := s3test.New(t)
	for _, prefix := range []string{"../other/", "a/../../other/", "a?b/", "outline"} {
		req := bucket(srv, false)
		req.S3.Prefix = prefix
		if _, err := runner(e).Run(t.Context(), req, nil); !errors.Is(err, s3.ErrPrefix) {
			t.Errorf("a check under %q: %v", prefix, err)
		}
	}
	if seen := e.Seen(); len(seen) != 0 {
		t.Errorf("a refused prefix asked Docker to %v", seen)
	}
	if got := srv.Objects(); len(got) != 0 {
		t.Errorf("a refused prefix wrote %v", got)
	}
}

// What the probe prints when it stops goes to the agent's log, without the
// request's keys, and the caller gets the code alone: a panic, or an error that
// says what it was given, never reaches whoever asked for the check.
func TestWhatTheProbePrintsStaysInTheAgentsLog(t *testing.T) {
	t.Parallel()
	e := dockertest.New(t)
	srv := s3test.New(t)
	var logged bytes.Buffer
	r := runner(e)
	r.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	req := bucket(srv, false)
	// A kind the probe doesn't check, which it says back, here the request's key.
	req.Kind = "the-secret"
	_, err := r.Run(t.Context(), req, nil)
	if err == nil || strings.Contains(err.Error(), "the-secret") || strings.Contains(err.Error(), "doesn't check") || !strings.Contains(err.Error(), "with code 1") {
		t.Errorf("the caller is told %v", err)
	}
	if got := logged.String(); strings.Contains(got, "the-secret") || !strings.Contains(got, "doesn't check") {
		t.Errorf("the agent logs %s", got)
	}
}
