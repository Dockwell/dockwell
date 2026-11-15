// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	// What each read found, in order, each new state once, and each error a read
	// skipped, with how often, so a failure says what it saw (#716).
	var seen []state
	var saw []string
	skipped := map[string]int{}
	began := time.Now()
	ready := false
	for until := time.Now().Add(2 * time.Minute); time.Now().Before(until) && !ready; time.Sleep(200 * time.Millisecond) {
		said, err := Reader{Client: client}.ask(t.Context(), id, e, cmd, mongoOpen)
		if err != nil {
			// As while mongosh can't run yet.
			if skipped[err.Error()] == 0 {
				saw = append(saw, fmt.Sprintf("%s: skipped %v", time.Since(began).Round(time.Millisecond), err))
			}
			skipped[err.Error()]++
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(said)), "\n")
		var s state
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &s); err != nil {
			t.Fatalf("mongoOpen said %q", said)
		}
		if len(seen) == 0 || seen[len(seen)-1] != s {
			seen = append(seen, s)
			saw = append(saw, fmt.Sprintf("%s: %+v", time.Since(began).Round(time.Millisecond), s))
		}
		ready = s.Root && s.Answering && !s.Open && !s.Initializing
	}
	if !ready {
		t.Fatalf("the server never answered as ready: %+v\nwhat each read saw, in order:\n  %s\nerrors skipped, by how often: %v\n%s",
			seen, strings.Join(saw, "\n  "), skipped, firstStartWhy(t, client, id))
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

// firstStartWhy is how a MongoDB container stands, for a first start that never
// read as ready: its state, the end of its log, and, while it runs, each of its
// processes with its state and command line, which says what read as the image's
// first server still running.
func firstStartWhy(t *testing.T, client *docker.Client, id string) string {
	t.Helper()
	var b strings.Builder
	c, err := client.InspectContainer(t.Context(), id)
	if err != nil {
		return fmt.Sprintf("can't inspect the container: %v", err)
	}
	fmt.Fprintf(&b, "the container: state %s, running %t, exit code %d, out of memory %t, error %q, restarts %d, started %s, finished %s",
		c.State.Status, c.State.Running, c.State.ExitCode, c.State.OOMKilled, c.State.Error, c.RestartCount, c.State.StartedAt, c.State.FinishedAt)
	if r, err := client.ContainerLogs(t.Context(), id, false, docker.LogOptions{Tail: 60}); err == nil {
		for {
			entry, err := r.Next()
			if err != nil {
				break
			}
			fmt.Fprintf(&b, "\n  log: %s", entry.Text)
		}
		_ = r.Close()
	}
	if c.State.Running {
		// Each process: its ID, its state (Z for one that ended and wasn't reaped),
		// and its command line, which a zombie's is empty.
		const list = `for p in /proc/[0-9]*; do n=${p#/proc/}; s=$(sed -n 's/^State:[[:space:]]*//p' "$p/status" 2>/dev/null); c=$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null); echo "$n [$s] $c"; done`
		var out bytes.Buffer
		code, err := client.Exec(t.Context(), id, []string{"sh", "-c", list}, docker.Streams{Stdout: &out, Stderr: &out})
		fmt.Fprintf(&b, "\n  its processes (code %d, %v):\n%s", code, err, out.String())
	}
	return b.String()
}
