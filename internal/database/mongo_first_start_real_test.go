// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// On a real MongoDB's first start, the image's own server, which makes its root user
// and then stops, reads as the first start still running, from the container's
// processes, until the real server answers; a user made then is made at once, with
// no gap to wait out (#701).
func TestAMongoDBsFirstStartIsSeenOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	const image = "mongo:8.0"
	env := dockertest.Scope(t)
	project := docker.Project(env)
	dockertest.Cleanup(t, client, project)
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	id, err := client.CreateContainer(t.Context(), docker.ContainerName(project, "db"), &docker.CreateRequest{Config: docker.Config{
		Image:  image,
		Env:    []string{"MONGO_INITDB_ROOT_USERNAME=root", "MONGO_INITDB_ROOT_PASSWORD=the-administrators"},
		Labels: map[string]string{docker.LabelProject: project, docker.LabelService: "db", docker.LabelNumber: "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	e := Recognise(image)
	cmd, _ := e.reading(0)
	type state struct {
		Root, Answering, Open, Initializing bool
	}
	var seen []state
	ready := false
	for until := time.Now().Add(2 * time.Minute); time.Now().Before(until) && !ready; time.Sleep(200 * time.Millisecond) {
		said, err := Reader{Client: client}.ask(t.Context(), id, e, cmd, mongoOpen)
		if err != nil {
			continue // as while mongosh can't run yet
		}
		lines := strings.Split(strings.TrimSpace(string(said)), "\n")
		var s state
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &s); err != nil {
			t.Fatalf("mongoOpen said %q", said)
		}
		if len(seen) == 0 || seen[len(seen)-1] != s {
			seen = append(seen, s)
		}
		ready = s.Root && s.Answering && !s.Open && !s.Initializing
	}
	if !ready {
		t.Fatalf("the server never answered as ready: %+v", seen)
	}
	sawFirst := false
	for _, s := range seen {
		sawFirst = sawFirst || s.Initializing
	}
	if !sawFirst {
		t.Errorf("the image's first server was never seen running: %+v", seen)
	}
	// Ready, the real server takes a user at once: no wait, no gap.
	d := &UserDriver{Client: client, Dir: t.TempDir(), Wait: time.Nanosecond, Every: time.Nanosecond}
	step := resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: env, Name: "shop"}, Action: resource.ActionCreate}
	ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-for-shop"})
	if err := d.Apply(ctx, step, &resourcev1.DatabaseUser{Service: "db", Password: "SHOP_DATABASE_PASSWORD"}); err != nil {
		t.Errorf("making a user once the server reads as ready: %v (seen %+v)", err, seen)
	}
}
