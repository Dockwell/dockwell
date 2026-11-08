// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// envOf is a create request's environment value for name, and whether it has one.
func envOf(req docker.CreateRequest, name string) (string, bool) {
	for _, kv := range req.Env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

func TestABuildDoesntStartOnADiskUnderItsFloor(t *testing.T) {
	b := newBench(t)
	b.builder.DiskFree = func(string) (int64, error) { return 500 << 20, nil }
	commit := b.repo.Commit("main", "Something", map[string]string{"Dockerfile": nodeDockerfile})
	err := b.builder.Build(b.ctx(), b.request(commit, "chg_01KBZ00000000000000000000A"))
	plain, _ := resource.Explain(err)
	if !strings.Contains(plain, "This server has 500 MB free on Docker's disk, and a build leaves 2.5 GB of it, so it didn't start") {
		t.Errorf("a build on a full disk says %q", plain)
	}
	if n := len(b.engine.Builders()); n != 0 {
		t.Errorf("a build on a full disk made %d builders", n)
	}
	if f := b.final(); f.State != resource.BuildFailed {
		t.Errorf("the build ended %v, not failed", f.State)
	}
}

func TestABuildStopsWhenTheDiskFallsUnderItsFloorAndFreesWhatItWrote(t *testing.T) {
	b := newBench(t)
	req := b.request(b.repo.Commit("main", "Write a lot", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN echo writing && sleep 30\n"}), "chg_01KBZ00000000000000000000A")
	volume := cacheVolume(req)
	// What a step writes is in its environment's cache, so the disk stays under the
	// floor for as long as the cache is there, whatever becomes of the builder.
	var writing atomic.Bool
	b.builder.DiskFree = func(string) (int64, error) {
		if _, err := b.engine.Client().InspectVolume(context.Background(), volume); err != nil {
			// Gone, and what was written with it: one made anew is empty.
			writing.Store(false)
		}
		if writing.Load() {
			return 1 << 30, nil
		}
		return 40 << 30, nil
	}
	b.builder.WatchEvery = 20 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- b.builder.Build(b.ctx(), req) }()
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(b.lines(), "writing") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	writing.Store(true)
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the build went on with the disk under its floor")
	}
	plain, _ := resource.Explain(err)
	if !strings.Contains(plain, "The build stopped when Docker's disk had 1.0 GB free, under the 2.5 GB a build leaves, so nothing was deployed") ||
		!strings.Contains(plain, "Dockwell removed this environment's build cache, "+volume+", which held what it wrote") {
		t.Errorf("a build that filled the disk says %q", plain)
	}
	if left := b.engine.Containers(); len(left) != 0 {
		t.Errorf("the builder is still there: %v", left)
	}
	if _, err := b.engine.Client().InspectVolume(t.Context(), volume); err == nil {
		t.Errorf("the cache that holds what the build wrote, %s, is still there", volume)
	}

	// So the next build of the environment starts, and with nothing to prune.
	next := b.repo.Commit("main", "Smaller", map[string]string{"Dockerfile": nodeDockerfile})
	if err := b.builder.Build(b.ctx(), b.request(next, "chg_01KBZ00000000000000000000B")); err != nil {
		t.Fatalf("the build after one the disk stopped = %v", err)
	}
	builders := b.engine.Builders()
	if _, ok := envOf(builders[len(builders)-1], "DW_PRUNE_FIRST"); ok {
		t.Error("the build after a cache was removed prunes first")
	}
}

// A cache a build that stopped left goes first when the disk is under its floor, as
// its next build can't start to prune it.
func TestADiskUnderItsFloorFreesTheCacheABuildLeft(t *testing.T) {
	b := newBench(t)
	if err := b.builder.ensureImage(t.Context()); err != nil {
		t.Fatal(err)
	}
	req := b.request(b.repo.Commit("main", "Something", map[string]string{"Dockerfile": nodeDockerfile}), "chg_01KBZ00000000000000000000A")
	volume := cacheVolume(req)
	if _, err := b.engine.Client().CreateVolume(t.Context(), &docker.VolumeCreate{Name: volume, Labels: map[string]string{cacheLabel: "1"}}); err != nil {
		t.Fatal(err)
	}
	// What a crash left: a builder, and so a cache its build didn't prune.
	if _, err := b.engine.Client().CreateContainer(t.Context(), "dockwell-build-left", &docker.CreateRequest{
		Config: docker.Config{Image: Image, Labels: map[string]string{docker.LabelBuilder: "chg_01KBZ00000000000000000000Z"}},
	}); err != nil {
		t.Fatal(err)
	}
	b.builder.Clean(context.Background())
	b.builder.DiskFree = func(string) (int64, error) {
		if _, err := b.engine.Client().InspectVolume(context.Background(), volume); err == nil {
			return 1 << 30, nil
		}
		return 40 << 30, nil
	}
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Fatalf("a build with its cache holding the disk under the floor = %v", err)
	}
}

func TestEachEnvironmentBuildsWithACacheOfItsOwn(t *testing.T) {
	b := newBench(t)
	commit := b.repo.Commit("main", "Something", map[string]string{"Dockerfile": nodeDockerfile})
	for i := range maxCaches + 1 {
		req := b.request(commit, fmt.Sprintf("chg_01KBZ0000000000000000000%02d", i))
		req.Service.Scope = fmt.Sprintf("env_01KBZ00000000000000000%04d", i)
		req.Image = docker.BuiltImage(fmt.Sprintf("svc_01KBZ%02d", i), commit, req.Change)
		if err := b.builder.Build(b.ctx(), req); err != nil {
			t.Fatal(err)
		}
	}
	builders := b.engine.Builders()
	volumes := map[string]bool{}
	for _, req := range builders {
		for _, m := range req.HostConfig.Mounts {
			if m.Target == cacheMount {
				volumes[m.Source] = true
			}
		}
	}
	if len(volumes) != maxCaches+1 {
		t.Errorf("%d environments' builds used %d caches", maxCaches+1, len(volumes))
	}
	// The second's share is half the bound, of a 50 GiB disk's 5 GiB.
	if keep, _ := envOf(builders[1], "DW_KEEP"); keep != strconv.FormatInt(int64(5<<30)/2, 10) {
		t.Errorf("the second environment's cache keeps %s, not half the bound", keep)
	}
	caches, err := b.engine.Client().ListVolumes(t.Context(), cacheLabel)
	if err != nil || len(caches) != maxCaches {
		t.Fatalf("the server keeps %d caches, %v, not %d", len(caches), err, maxCaches)
	}
	if slices.ContainsFunc(caches, func(v docker.Volume) bool { return v.Name == CacheVolume+"-env_01kbz000000000000000000000" }) {
		t.Error("the cache of the environment that built longest ago is still there")
	}
}

func TestBuildsRunOnANetworkWhereNoContainerReachesAnother(t *testing.T) {
	b := newBench(t)
	commit := b.repo.Commit("main", "Something", map[string]string{"Dockerfile": nodeDockerfile})
	if err := b.builder.Build(b.ctx(), b.request(commit, "chg_01KBZ00000000000000000000A")); err != nil {
		t.Fatal(err)
	}
	if mode := b.engine.Builders()[0].HostConfig.NetworkMode; mode != Network {
		t.Errorf("the builder runs on %q, not %s", mode, Network)
	}
	n := b.engine.Network(Network)
	if n == nil || n.Options["com.docker.network.bridge.enable_icc"] != "false" {
		t.Fatalf("the build network is %+v, not a bridge without inter-container traffic", n)
	}

	// One someone made that lets containers reach each other is refused.
	other := newBench(t)
	if _, err := other.engine.Client().CreateNetwork(t.Context(), &docker.NetworkCreate{Name: Network, Driver: "bridge"}); err != nil {
		t.Fatal(err)
	}
	commit = other.repo.Commit("main", "Something", map[string]string{"Dockerfile": nodeDockerfile})
	err := other.builder.Build(other.ctx(), other.request(commit, "chg_01KBZ00000000000000000000A"))
	plain, _ := resource.Explain(err)
	if !strings.Contains(plain, "lets containers on it reach each other") {
		t.Errorf("a build network that lets containers reach each other = %q", plain)
	}
}

func TestStepsRunUnderDockersAppArmorProfileWhereThereIsOne(t *testing.T) {
	for _, c := range []struct {
		profiles string
		want     string
	}{
		{"docker-default (enforce)\nlsb_release (enforce)\n", "docker-default"},
		{"lsb_release (enforce)\n", ""},
	} {
		b := newBench(t)
		b.builder.AppArmorProfiles = func() ([]byte, error) { return []byte(c.profiles), nil }
		commit := b.repo.Commit("main", "Something", map[string]string{"Dockerfile": nodeDockerfile})
		if err := b.builder.Build(b.ctx(), b.request(commit, "chg_01KBZ00000000000000000000A")); err != nil {
			t.Fatal(err)
		}
		if got, _ := envOf(b.engine.Builders()[0], "DW_APPARMOR"); got != c.want {
			t.Errorf("with %q loaded, the steps run under %q, not %q", c.profiles, got, c.want)
		}
	}
	if !strings.Contains(script, `apparmor-profile = "%s"`) {
		t.Error("the script doesn't give BuildKit the profile")
	}
}

func TestAfterACrashEachCachePrunesFirst(t *testing.T) {
	b := newBench(t)
	if err := b.builder.ensureImage(t.Context()); err != nil {
		t.Fatal(err)
	}
	// What a crash left: a builder, still there.
	id, err := b.engine.Client().CreateContainer(t.Context(), "dockwell-build-left", &docker.CreateRequest{
		Config: docker.Config{Image: Image, Labels: map[string]string{docker.LabelBuilder: "chg_01KBZ00000000000000000000Z"}},
	})
	if err != nil || id == "" {
		t.Fatal(err)
	}
	b.builder.Clean(context.Background())
	commit := b.repo.Commit("main", "Something", map[string]string{"Dockerfile": nodeDockerfile})
	if err := b.builder.Build(b.ctx(), b.request(commit, "chg_01KBZ00000000000000000000A")); err != nil {
		t.Fatal(err)
	}
	builders := b.engine.Builders()
	if _, ok := envOf(builders[len(builders)-1], "DW_PRUNE_FIRST"); !ok {
		t.Error("the first build after a crash doesn't prune first")
	}
}

func TestAPlainHTTPNameMustLeadToAPrivateNetwork(t *testing.T) {
	b := newBench(t)
	b.builder.LookupIP = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.7")}, nil }
	req := b.request(strings.Repeat("a", 40), "chg_01KBZ00000000000000000000A")
	req.URL = "http://git.local/acme/web"
	err := b.builder.Build(b.ctx(), req)
	plain, _ := resource.Explain(err)
	if !strings.Contains(plain, "git.local leads to 203.0.113.7, which isn't on a private network") {
		t.Errorf("a plain-HTTP name that leads outside says %q", plain)
	}
	if n := len(b.engine.Builders()); n != 0 {
		t.Errorf("it made %d builders", n)
	}
}
