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
		{"mongo:8.0.34", "mongo:9", true, true, ""},
		{"mongo:8.3", "mongo:9.0", true, true, ""},
		{"mongo:8.0", "mongo:7.0", false, true, ""},
		{"mongo:6.0", "mongo:8.0", false, false, "Change its image to mongo:7.0 first, then to mongo:8.0 once that's healthy"},
		{"mongo:8.0", "mongo:6.0", false, false, "Change its image to mongo:7.0 first"},
		{"mongo:7.0", "mongo:7.0.14", false, false, ""},
		{"mongo:7.0", "mongo:latest", false, false, ""},
		{"postgres:16", "postgres:17", false, false, ""},
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
}

func TestReleaseIsWhatAFeatureCompatibilityVersionSays(t *testing.T) {
	for image, want := range map[string]string{"mongo:7": "7.0", "mongo:7.0": "7.0", "mongo:8.3.13-noble": "8.3", "mongo:7.0.12": "7.0"} {
		if got := Release(image); got != want {
			t.Errorf("%s: %q, want %q", image, got, want)
		}
	}
}

// Before a MongoDB moves to the next release, its old container raises its feature
// compatibility version to the release it runs, signing in from its environment;
// nothing else asks it to.
func TestAMongoMovingOnRaisesItsFeaturesFirst(t *testing.T) {
	e := dockertest.New(t)
	var mu sync.Mutex
	var asked []string
	refuse := false
	e.AddLocalImage("mongo:7.0", dockertest.Image{Config: docker.Config{Cmd: []string{"mongod"}}, Exec: func(x dockertest.Exec) int {
		b, _ := io.ReadAll(x.Stdin)
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, strings.Join(x.Cmd, " ")+"\n"+string(b))
		if refuse {
			_, _ = io.WriteString(x.Stderr, "MongoServerError: Authentication failed.\n")
			return 1
		}
		return 0
	}})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_1-docs-1", &docker.CreateRequest{Config: docker.Config{Image: "mongo:7.0", Cmd: []string{"mongod", "--port", "27018"}}})
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
	for _, image := range []string{"mongo:7.0.14", "mongo:latest", "postgres:17"} {
		if err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: image}); err != nil {
			t.Errorf("to %s: %v", image, err)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("a move to no next release asked MongoDB %q", asked)
	}
	if err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:8.0"}); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "DW_RELEASE=7.0") || !strings.Contains(asked[0], "DW_PORT=27018") ||
		!strings.Contains(asked[0], "setFeatureCompatibilityVersion") || !strings.Contains(asked[0], "cmd.confirm = true") || strings.Contains(asked[0], "--password") {
		t.Errorf("it asked %q", asked)
	}
	mu.Lock()
	refuse = true
	mu.Unlock()
	if err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "mongo:8.0"}); err == nil || !strings.Contains(err.Error(), "refused Dockwell's sign-in") {
		t.Errorf("a refused sign-in says %v", err)
	}
}
