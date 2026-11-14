// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v4"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog/snapshot"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// The catalog's mongo entry, as it runs: its pinned image, with its environment
// and its health check, passes that check, and its root user makes an app a user of
// its own, which signs in to its database with the URL an app is given and writes
// there (ADR 0084).
func TestTheCatalogsMongoDBMakesAnAppItsUserOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	raw, err := fs.ReadFile(snapshot.Entries(), "mongo.yaml")
	if err != nil {
		t.Fatalf("the catalog has no mongo entry: %v", err)
	}
	var entry struct {
		Compose string `yaml:"compose"`
	}
	if err := yaml.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Image       string            `yaml:"image"`
			Environment map[string]string `yaml:"environment"`
			Healthcheck struct {
				Test        []string `yaml:"test"`
				Interval    string   `yaml:"interval"`
				Timeout     string   `yaml:"timeout"`
				Retries     int      `yaml:"retries"`
				StartPeriod string   `yaml:"start_period"`
			} `yaml:"healthcheck"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(entry.Compose), &compose); err != nil {
		t.Fatal(err)
	}
	db, ok := compose.Services["db"]
	if !ok || db.Image == "" || len(db.Healthcheck.Test) == 0 {
		t.Fatalf("the mongo entry's db is %+v", db)
	}
	const root = "a-generated-root-password"
	var envs []string
	for k, v := range db.Environment {
		envs = append(envs, k+"="+strings.ReplaceAll(v, "${MONGO_ROOT_PASSWORD}", root))
	}
	duration := func(s string) time.Duration {
		d, _ := time.ParseDuration(s)
		return d
	}
	env := dockertest.Scope(t)
	project := docker.Project(env)
	dockertest.Cleanup(t, client, project)
	if err := client.PullImage(t.Context(), db.Image, nil); err != nil {
		t.Fatal(err)
	}
	id, err := client.CreateContainer(t.Context(), docker.ContainerName(project, "db"), &docker.CreateRequest{Config: docker.Config{
		Image: db.Image, Env: envs,
		Labels: map[string]string{docker.LabelProject: project, docker.LabelService: "db", docker.LabelNumber: "1"},
		Healthcheck: &docker.Healthcheck{
			Test: append([]string(nil), db.Healthcheck.Test...), Interval: duration(db.Healthcheck.Interval), Timeout: duration(db.Healthcheck.Timeout),
			Retries: db.Healthcheck.Retries, StartPeriod: duration(db.Healthcheck.StartPeriod),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	// Its own health check says it's up, as the plan waits for before its users.
	// One that doesn't says why: how the container stands, the end of what MongoDB
	// wrote, and what the check's own command says when run by hand.
	why := func(c *docker.Container) string {
		var b strings.Builder
		fmt.Fprintf(&b, "state %s, running %t, exit code %d, out of memory %t, error %q, health %+v", c.State.Status, c.State.Running, c.State.ExitCode, c.State.OOMKilled, c.State.Error, c.State.Health)
		if r, err := client.ContainerLogs(t.Context(), id, false, docker.LogOptions{Tail: 40}); err == nil {
			for {
				e, err := r.Next()
				if err != nil {
					break
				}
				fmt.Fprintf(&b, "\n  log: %s", e.Text)
			}
			_ = r.Close()
		}
		if c.State.Running && len(db.Healthcheck.Test) == 2 && db.Healthcheck.Test[0] == "CMD-SHELL" {
			var out bytes.Buffer
			code, err := client.Exec(t.Context(), id, []string{"sh", "-c", db.Healthcheck.Test[1]}, docker.Streams{Stdout: &out, Stderr: &out})
			fmt.Fprintf(&b, "\n  the check by hand: code %d, %v: %s", code, err, out.String())
		}
		return b.String()
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		c, err := client.InspectContainer(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if h := c.State.Health; h != nil && h.Status == "healthy" {
			break
		}
		if !c.State.Running && !c.State.Restarting {
			t.Fatalf("the entry's MongoDB stopped before its health check passed: %s", why(c))
		}
		if time.Now().After(deadline) {
			t.Fatalf("the entry's health check never passed: %s", why(c))
		}
		time.Sleep(2 * time.Second)
	}

	d := &UserDriver{Client: client, Dir: t.TempDir(), Wait: 2 * time.Minute, Every: time.Second}
	step := resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: env, Name: "notes"}, Action: resource.ActionCreate}
	ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-for-notes"})
	if err := d.Apply(ctx, step, &resourcev1.DatabaseUser{Service: "db", Password: "NOTES_DATABASE_PASSWORD"}); err != nil {
		t.Fatalf("making the app's user: %v", err)
	}
	if err := d.Verify(t.Context(), step.Ref, &resourcev1.DatabaseUser{Service: "db"}); err != nil {
		t.Errorf("the app's user checks as %v", err)
	}
	var out, errs bytes.Buffer
	run, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	url := "mongodb://notes:a-password-for-notes@127.0.0.1:27017/notes"
	code, err := client.Exec(run, id, []string{"mongosh", "--quiet", "--norc", url, "--eval", `db.notes.insertOne({ text: "hello" }); print(db.notes.countDocuments())`}, docker.Streams{Stdout: &out, Stderr: &errs})
	if err != nil {
		t.Fatal(err)
	}
	if said := strings.TrimSpace(errs.String() + out.String()); code != 0 || !strings.HasSuffix(said, "1") {
		t.Errorf("the app can't write in its own database: %s", said)
	}
}
