// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"io"
	"strings"
	"sync"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

func service(image string) *resourcev1.Service {
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: image}}}
}

// MongoDB moves one major release at a time, raising the features its data may
// use first, and back one at most. Other moves are no move of MongoDB's.
func TestMongoMovesOneReleaseAtATime(t *testing.T) {
	for _, c := range []struct {
		from, to string
		raises   bool
		move     bool
		refused  string
	}{
		{"mongo:7.0", "mongo:8.0", true, true, ""},
		// As CI's Docker job moves it, by patch.
		{"mongo:6.0.28", "mongo:7.0.45", true, true, ""},
		{"mongo:7.0.45", "mongo:9.0", false, false, "Change its image to mongo:8.0 first, then to mongo:9.0 once that's healthy"},
		{"mongo:7.0.45", "mongo:5.0", false, false, "which goes back one major release at most. Change its image to mongo:6.0 first"},
		{"mongo:8.0.34", "mongo:9", true, true, ""},
		{"mongo:8.3", "mongo:9.0", true, true, ""},
		// The tag names no minor: the agent asks the server what it runs.
		{"mongo:8", "mongo:9", true, true, ""},
		// 9.0 moves only from 8.0 or 8.3.
		{"mongo:8.2", "mongo:9.0", false, false, "and 9.0 moves only from data at 8.0 or 8.3. Change its image to mongo:8.3 first"},
		{"mongo:8.1.2", "mongo:9", false, false, "Change its image to mongo:8.3 first"},
		{"mongo:8.0", "mongo:7.0", false, true, ""},
		{"mongo:6.0", "mongo:8.0", false, false, "Change its image to mongo:7.0 first, then to mongo:8.0 once that's healthy"},
		{"mongo:8.0", "mongo:6.0", false, false, "Change its image to mongo:7.0 first"},
		{"mongo:7.0", "mongo:7.0.14", false, false, ""},
		{"mongo:7.0", "mongo:latest", false, false, ""},
		{"mongo:7.0", "postgres:17", false, false, ""},
	} {
		m, err := MoveOf("docs", service(c.from), service(c.to))
		switch {
		case c.refused != "":
			if err == nil || !strings.Contains(err.Error(), c.refused) || !strings.HasPrefix(err.Error(), "docs runs MongoDB") {
				t.Errorf("%s to %s: %v, want it refused", c.from, c.to, err)
			}
		case err != nil:
			t.Errorf("%s to %s is refused: %v", c.from, c.to, err)
		case (m != nil) != c.move || m != nil && m.RaisesFeatures != c.raises:
			t.Errorf("%s to %s: %+v", c.from, c.to, m)
		}
	}
	if m, _ := MoveOf("docs", service("mongo:7.0.12"), service("mongo:8.0")); m.From != "7.0" || m.To != "8.0" {
		t.Errorf("the move names %q to %q", m.From, m.To)
	}
	if m, _ := MoveOf("docs", service("mongo:8.0"), service("mongo:7.0")); !m.Lowers || m.RaisesFeatures {
		t.Errorf("a move down is %+v", m)
	}
}

func TestReleaseIsWhatAFeatureCompatibilityVersionSays(t *testing.T) {
	for image, want := range map[string]string{"mongo:7": "7", "mongo:7.0": "7.0", "mongo:8.3.13-noble": "8.3", "mongo:7.0.12": "7.0"} {
		if got := Release(image); got != want {
			t.Errorf("%s: %q, want %q", image, got, want)
		}
	}
}

// Before a MongoDB moves to another release, its old container says what it runs
// and what its data uses, signing in from its environment. Moving up, it raises the
// feature compatibility version to the release it runs, which the tag can hide, and
// refuses a release the next doesn't move from; moving down, it refuses data newer
// than where it goes.
func TestAMongoMovingAsksTheServerFirst(t *testing.T) {
	e := dockertest.New(t)
	var mu sync.Mutex
	var raised []string
	runs, fcv, refuse := "7.0.14", "6.0", false
	e.AddLocalImage("mongo:7", dockertest.Image{Config: docker.Config{Cmd: []string{"mongod"}}, Exec: func(x dockertest.Exec) int {
		b, _ := io.ReadAll(x.Stdin)
		mu.Lock()
		defer mu.Unlock()
		script := strings.Join(x.Cmd, " ") + "\n" + string(b)
		if refuse {
			_, _ = io.WriteString(x.Stderr, "MongoServerError: Authentication failed.\n")
			return 1
		}
		if strings.Contains(script, "buildInfo") {
			_, _ = io.WriteString(x.Stdout, `{"version":"`+runs+`","fcv":"`+fcv+`"}`+"\n")
			return 0
		}
		raised = append(raised, script)
		return 0
	}})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_1-docs-1", &docker.CreateRequest{Config: docker.Config{Image: "mongo:7", Cmd: []string{"mongod", "--port", "27018"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	old, err := client.InspectContainer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	r := Reader{Client: client}
	set := func(version, features string, refusing bool) {
		mu.Lock()
		runs, fcv, refuse = version, features, refusing
		mu.Unlock()
	}
	for _, image := range []string{"mongo:7.0.14", "mongo:latest", "postgres:17"} {
		if _, err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: image}); err != nil {
			t.Errorf("to %s: %v", image, err)
		}
	}
	if len(raised) != 0 {
		t.Fatalf("a move to no other release raised %q", raised)
	}
	// Up: to the release it runs, whatever its tag says.
	if _, err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:8.0"}); err != nil {
		t.Fatal(err)
	}
	if len(raised) != 1 || !strings.Contains(raised[0], "DW_RELEASE=7.0") || !strings.Contains(raised[0], "DW_PORT=27018") ||
		!strings.Contains(raised[0], "setFeatureCompatibilityVersion") || !strings.Contains(raised[0], "cmd.confirm = true") || strings.Contains(raised[0], "--password") {
		t.Errorf("it raised %q", raised)
	}
	// A release the next doesn't move from: mongo:8 running 8.2 isn't moved to 9.
	set("8.2.1", "8.2", false)
	if _, err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:9.0"}); err == nil || !strings.Contains(err.Error(), "moves only from data at 8.0 or 8.3") {
		t.Errorf("8.2 to 9.0: %v", err)
	}
	set("8.3.13", "8.0", false)
	if _, err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:9"}); err != nil || !strings.Contains(raised[len(raised)-1], "DW_RELEASE=8.3") {
		t.Errorf("8.3 to 9: %v, raised %q", err, raised[len(raised)-1])
	}
	// Down: data at newer features than where it goes stays.
	set("8.0.34", "8.0", false)
	if _, err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:7.0"}); err == nil || !strings.Contains(err.Error(), "uses MongoDB 8.0's features, which 7.0 can't run") {
		t.Errorf("down with data at 8.0: %v", err)
	}
	set("8.0.34", "7.0", false)
	n := len(raised)
	if _, err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:7.0"}); err != nil || len(raised) != n {
		t.Errorf("down with data at 7.0: %v, raised %d", err, len(raised)-n)
	}
	set("7.0.14", "7.0", true)
	if _, err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:8.0"}); err == nil || !strings.Contains(err.Error(), "refused Dockwell's sign-in") {
		t.Errorf("a refused sign-in says %v", err)
	}
}

func TestClickHouseChecksPinnedUpgradeCompatibility(t *testing.T) {
	const repo = "clickhouse/clickhouse-server:"
	digest := "@sha256:" + strings.Repeat("a", 64)
	for _, c := range []struct {
		from, to string
		backup   bool
		refused  string
	}{
		{"25.8.33.6", "25.8.34.1" + digest, false, ""},
		{"25.8.33.6", "25.12.1.1" + digest, false, ""},
		{"25.8.33.6", "26.3.62.3" + digest, true, ""},
		{"25.8.33.6", "26.8.1.1" + digest, false, "intermediate"},
		{"26.3.62.3", "25.8.33.6" + digest, false, "can't downgrade"},
		{"26.3.62.3", "26.3.62.2" + digest, false, "can't downgrade"},
		{"25.8.33.6", "latest" + digest, false, "full version"},
		{"25.8.33.6", "26.3.62.3", false, "isn't pinned"},
	} {
		t.Run(c.from+"_"+c.to, func(t *testing.T) {
			got, err := MoveOf("events", service(repo+c.from), service(repo+c.to))
			if c.refused != "" {
				if err == nil || !strings.Contains(err.Error(), c.refused) {
					t.Fatalf("want %q, got %v", c.refused, err)
				}
				return
			}
			if err != nil || got == nil || got.Dumps || got.RequiresBackup != c.backup {
				t.Fatalf("move: %+v, %v", got, err)
			}
		})
	}
}

func TestClickHouseMirrorUsesTheDataMountAndRealVersions(t *testing.T) {
	const source = "mirror.example/analytics:25.8.33.6"
	digest := "@sha256:" + strings.Repeat("a", 64)
	target := "mirror.example/analytics:26.3.62.3" + digest
	current := service(source)
	current.GetApp().Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/var/lib/clickhouse"}}
	move, err := MoveOf("events", current, service(target))
	if err != nil || move == nil || !move.RequiresBackup || move.Volume != "data" {
		t.Fatalf("mirrored image bypassed the plan's native backup: %+v, %v", move, err)
	}
	unknown := "mirror.example/analytics:latest" + digest
	if _, err := MoveOf("events", current, service(unknown)); err == nil || !strings.Contains(err.Error(), unknown) || !strings.Contains(err.Error(), "full version") {
		t.Fatalf("a version that can't be read was accepted: %v", err)
	}
	e := dockertest.New(t)
	e.Registry[target] = dockertest.Image{Config: docker.Config{Labels: map[string]string{clickHouseVersionLabel: "25.1.3.4-alpine"}}}
	if err := e.Client().PullImage(t.Context(), target, nil); err != nil {
		t.Fatal(err)
	}
	old := &docker.Container{
		Config: docker.Config{Image: source, Labels: map[string]string{clickHouseVersionLabel: "25.8.33.6-alpine"}},
		Mounts: []docker.MountPoint{{Destination: "/var/lib/clickhouse"}},
	}
	r := Reader{Client: e.Client()}
	if _, err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: target}); err == nil || !strings.Contains(err.Error(), "can't downgrade") {
		t.Fatalf("a misleading mirror tag bypassed the downgrade check: %v", err)
	}
}

func TestClickHouseMirrorMajorMoveRequiresManagedData(t *testing.T) {
	const source = "mirror.example/analytics:25.8.33.6"
	const project = "env_fixture"
	target := "mirror.example/analytics:26.3.62.3@sha256:" + strings.Repeat("a", 64)
	managed := docker.ScopedName(project, "data")
	volumeMount := &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: clickHouseDataDir}
	hostMount := &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/events"}, Target: clickHouseDataDir}
	e := dockertest.New(t)
	e.Registry[target] = dockertest.Image{Config: docker.Config{Labels: map[string]string{clickHouseVersionLabel: "26.3.62.3-alpine"}}}
	r := Reader{Client: e.Client()}
	if err := r.Client.PullImage(t.Context(), target, nil); err != nil {
		t.Fatal(err)
	}
	e.AddVolume(docker.Volume{Name: managed, Labels: map[string]string{docker.LabelProject: project, docker.LabelVolume: "data"}})
	for _, tc := range []struct {
		name          string
		before, after *resourcev1.Mount
		mountType     string
		mounted       string
		allow         bool
	}{
		{name: "anonymous", mountType: "volume", mounted: "anonymous-volume"},
		{name: "host path", before: hostMount, after: hostMount, mountType: "bind"},
		{name: "another volume", before: volumeMount, after: volumeMount, mountType: "volume", mounted: "some-other-volume"},
		{name: "managed volume", before: volumeMount, after: volumeMount, mountType: "volume", mounted: managed, allow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current, desired := service(source), service(target)
			if tc.before != nil {
				current.GetApp().Mounts = []*resourcev1.Mount{tc.before}
			}
			if tc.after != nil {
				desired.GetApp().Mounts = []*resourcev1.Mount{tc.after}
			}
			move, err := MoveOf("events", current, desired)
			if err != nil || tc.before != nil && (move == nil || !move.RequiresBackup || move.Volume != tc.before.GetVolume()) {
				t.Fatalf("ClickHouse plan move = %+v, %v", move, err)
			}
			old := &docker.Container{
				Name: "events", Config: docker.Config{Image: source, Labels: map[string]string{clickHouseVersionLabel: "25.8.33.6", docker.LabelProject: project}},
				Mounts: []docker.MountPoint{{Type: tc.mountType, Name: tc.mounted, Destination: clickHouseDataDir}},
			}
			id, err := r.BeforeReplace(t.Context(), old, desired.GetApp())
			if tc.allow {
				if err != nil || !docker.IsImageID(id) {
					t.Fatalf("managed upgrade = %q, %v", id, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "can't back it up") {
				t.Fatalf("major upgrade without managed data was allowed: %q, %v", id, err)
			}
		})
	}
}

func TestClickHouseChecksWhenTheSameTagMoves(t *testing.T) {
	const image = "mirror.example/analytics:latest"
	e := dockertest.New(t)
	r := Reader{Client: e.Client()}
	build := func(version string) *docker.Image {
		t.Helper()
		e.AddLocalImage(image, dockertest.Image{Config: docker.Config{Labels: map[string]string{clickHouseVersionLabel: version}}})
		inspected, err := r.Client.InspectImage(t.Context(), image)
		if err != nil {
			t.Fatal(err)
		}
		return inspected
	}
	initial := build("25.8.33.6")
	old := &docker.Container{Image: initial.ID, Config: *initial.Config, Mounts: []docker.MountPoint{{Destination: clickHouseDataDir}}}
	old.Config.Image = image
	spec := &resourcev1.Container{Image: image}
	if id, err := r.BeforeReplace(t.Context(), old, spec); err != nil || id != initial.ID {
		t.Fatalf("unchanged image = %q, %v", id, err)
	}
	build("26.3.62.3")
	if _, err := r.BeforeReplace(t.Context(), old, spec); err == nil || !strings.Contains(err.Error(), image) || !strings.Contains(err.Error(), "full version") {
		t.Fatalf("moved tag was allowed: %v", err)
	}
}
