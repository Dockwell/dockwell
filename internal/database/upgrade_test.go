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
		if err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: image}); err != nil {
			t.Errorf("to %s: %v", image, err)
		}
	}
	if len(raised) != 0 {
		t.Fatalf("a move to no other release raised %q", raised)
	}
	// Up: to the release it runs, whatever its tag says.
	if err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:8.0"}); err != nil {
		t.Fatal(err)
	}
	if len(raised) != 1 || !strings.Contains(raised[0], "DW_RELEASE=7.0") || !strings.Contains(raised[0], "DW_PORT=27018") ||
		!strings.Contains(raised[0], "setFeatureCompatibilityVersion") || !strings.Contains(raised[0], "cmd.confirm = true") || strings.Contains(raised[0], "--password") {
		t.Errorf("it raised %q", raised)
	}
	// A release the next doesn't move from: mongo:8 running 8.2 isn't moved to 9.
	set("8.2.1", "8.2", false)
	if err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:9.0"}); err == nil || !strings.Contains(err.Error(), "moves only from data at 8.0 or 8.3") {
		t.Errorf("8.2 to 9.0: %v", err)
	}
	set("8.3.13", "8.0", false)
	if err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:9"}); err != nil || !strings.Contains(raised[len(raised)-1], "DW_RELEASE=8.3") {
		t.Errorf("8.3 to 9: %v, raised %q", err, raised[len(raised)-1])
	}
	// Down: data at newer features than where it goes stays.
	set("8.0.34", "8.0", false)
	if err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:7.0"}); err == nil || !strings.Contains(err.Error(), "uses MongoDB 8.0's features, which 7.0 can't run") {
		t.Errorf("down with data at 8.0: %v", err)
	}
	set("8.0.34", "7.0", false)
	n := len(raised)
	if err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:7.0"}); err != nil || len(raised) != n {
		t.Errorf("down with data at 7.0: %v, raised %d", err, len(raised)-n)
	}
	set("7.0.14", "7.0", true)
	if err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:8.0"}); err == nil || !strings.Contains(err.Error(), "refused Dockwell's sign-in") {
		t.Errorf("a refused sign-in says %v", err)
	}
}
