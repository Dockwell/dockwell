// SPDX-License-Identifier: AGPL-3.0-only

package probe_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
// input alone, and leaves nothing behind but the binary's one image, which the next
// check runs too (ADR 0099, #710).
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
	images := e.Images(docker.ProbeRepository)
	if len(images) != 1 || !strings.HasPrefix(images[0], docker.ProbeRepository+":") {
		t.Fatalf("the check left images %v, want this build's alone", images)
	}
	// Another check, by this runner or one this build starts again, runs the same image.
	for _, r := range []*probe.Runner{runner(e), runner(e)} {
		if _, err := r.Run(t.Context(), req, []string{appNetwork}); err != nil {
			t.Fatal(err)
		}
	}
	if again := e.Images(docker.ProbeRepository); !slices.Equal(again, images) {
		t.Errorf("checks after the first left images %v, want %v", again, images)
	}
	if ran := e.Helpers(); ran[len(ran)-1].Image != images[0] {
		t.Errorf("the last check ran %s, want %s", ran[len(ran)-1].Image, images[0])
	}
}

// An image by this build's name is run only when its layer is the one this agent
// would import: one made by other hands, which would be given a connection's keys,
// is made again first (#710 review L1). One that is this agent's isn't imported again.
func TestACheckRunsOnlyItsOwnImage(t *testing.T) {
	t.Parallel()
	e := dockertest.New(t)
	e.AddNetwork(docker.Network{Name: appNetwork, IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "127.0.0.0/8"}}}})
	req := bucket(s3test.New(t), false)
	if _, err := runner(e).Run(t.Context(), req, []string{appNetwork}); err != nil {
		t.Fatal(err)
	}
	tags := e.Images(docker.ProbeRepository)
	if len(tags) != 1 {
		t.Fatalf("a check left images %v", tags)
	}
	ours, err := e.Client().InspectImage(t.Context(), tags[0])
	if err != nil || len(ours.RootFS.Layers) != 1 {
		t.Fatalf("this build's image reads %+v, %v", ours, err)
	}
	imports := e.Imports
	if _, err := runner(e).Run(t.Context(), req, []string{appNetwork}); err != nil {
		t.Fatal(err)
	}
	if e.Imports != imports {
		t.Errorf("a check imported this build's image again: %d imports, want %d", e.Imports, imports)
	}

	// Another image under the same name.
	other := filepath.Join(t.TempDir(), "other")
	if err := os.WriteFile(other, []byte("#!/bin/sh\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.Client().ImportFiles(t.Context(), tags[0], []docker.ImageFile{{Name: "/dockwell", Path: other, Mode: 0o755}}); err != nil {
		t.Fatal(err)
	}
	imports = e.Imports
	if _, err := runner(e).Run(t.Context(), req, []string{appNetwork}); err != nil {
		t.Fatal(err)
	}
	now, err := e.Client().InspectImage(t.Context(), tags[0])
	if err != nil || !slices.Equal(now.RootFS.Layers, ours.RootFS.Layers) || e.Imports != imports+1 {
		t.Errorf("after an image by its name that isn't its own, the check's image reads %v (%v), with %d imports, want %v and one more",
			now.RootFS.Layers, err, e.Imports-imports, ours.RootFS.Layers)
	}
	if ran := e.Helpers(); ran[len(ran)-1].Image != tags[0] {
		t.Errorf("the check ran %s", ran[len(ran)-1].Image)
	}
}

// An agent runs MaxChecks at once, and refuses another as busy, at once, with
// nothing made, until one ends (#710).
func TestAnAgentRunsTwoChecksAtOnce(t *testing.T) {
	t.Parallel()
	e := dockertest.New(t)
	e.AddNetwork(docker.Network{Name: appNetwork, IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "127.0.0.0/8"}}}})
	// A store that takes connections and never answers holds each check.
	stall, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stall.Close() })
	go func() {
		for {
			conn, err := stall.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	req := probe.Request{Kind: probe.KindS3, S3: &probe.S3{
		Endpoint: "http://" + stall.Addr().String(), Bucket: "uploads", Region: "auto", AccessKey: "AKIDEXAMPLE", SecretKey: "the-secret",
	}}
	r := runner(e)
	ctx, cancel := context.WithCancel(t.Context())
	var running sync.WaitGroup
	for range probe.MaxChecks {
		running.Go(func() { _, _ = r.Run(ctx, req, []string{appNetwork}) })
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(e.Containers()) < probe.MaxChecks {
		if time.Now().After(deadline) {
			t.Fatalf("%d checks run, want %d", len(e.Containers()), probe.MaxChecks)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := r.Run(t.Context(), req, []string{appNetwork}); !errors.Is(err, probe.ErrBusy) || !strings.Contains(err.Error(), "Try again in a moment") {
		t.Errorf("a third check at once: %v", err)
	}
	if n := len(e.Containers()); n != probe.MaxChecks {
		t.Errorf("the busy check made a container: %d run", n)
	}
	cancel()
	running.Wait()
	if _, err := r.Run(t.Context(), bucket(s3test.New(t), false), []string{appNetwork}); err != nil {
		t.Errorf("a check once the others ended: %v", err)
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

// What a crash cut short is removed as the agent starts: the check's container and
// its network, and every probe image but this build's. A check of this start, which
// may run while Clean does, keeps its container and network.
func TestCleanRemovesWhatACrashLeft(t *testing.T) {
	t.Parallel()
	e := dockertest.New(t)
	c := e.Client()
	r := runner(e)
	r.Start = "now"
	made := func(run, start string) {
		t.Helper()
		if err := c.ImportFiles(t.Context(), docker.ProbeRepository+":"+run, []docker.ImageFile{{Name: "/dockwell", Path: "/proc/self/exe", Mode: 0o755}}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.CreateNetwork(t.Context(), &docker.NetworkCreate{Name: docker.ProbeRepository + "-" + run, Labels: map[string]string{docker.LabelProbe: start}}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.CreateContainer(t.Context(), docker.ProbeRepository+"-"+run, &docker.CreateRequest{Config: docker.Config{
			Image: docker.ProbeRepository + ":" + run, Labels: map[string]string{docker.LabelProbe: start},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	made("before-1", "before")
	made("now-1", "now")
	// This build's image, as a check of this start made it.
	if _, err := r.Run(t.Context(), probe.Request{Kind: probe.KindS3, S3: &probe.S3{Endpoint: "http://127.0.0.1:9", Bucket: "uploads", AccessKey: "a", SecretKey: "b"}}, nil); err != nil {
		t.Fatal(err)
	}
	var build string
	for _, tag := range e.Images(docker.ProbeRepository) {
		if tag != docker.ProbeRepository+":before-1" && tag != docker.ProbeRepository+":now-1" {
			build = tag
		}
	}
	if err := r.Clean(t.Context()); err != nil {
		t.Fatal(err)
	}
	if left := e.Containers(); !slices.Equal(left, []string{docker.ProbeRepository + "-now-1"}) {
		t.Errorf("Clean left containers %v, want this start's alone", left)
	}
	if left := e.Images(docker.ProbeRepository); build == "" || !slices.Equal(left, []string{build}) {
		t.Errorf("Clean left images %v, want this build's alone, %q", left, build)
	}
	if n := e.Network(docker.ProbeRepository + "-before-1"); n != nil {
		t.Errorf("Clean left the network %s", n.Name)
	}
	if n := e.Network(docker.ProbeRepository + "-now-1"); n == nil {
		t.Error("Clean removed the network of a check this start runs")
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
