// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// On a real MongoDB, two apps sharing one server each get a user that owns its own
// database: each writes there with its password, and neither reads the other's
// (ADR 0084).
func TestTwoAppsShareAMongoDBOnRealDocker(t *testing.T) {
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
	d := &UserDriver{Client: client, Dir: t.TempDir(), Wait: 2 * time.Minute, Every: time.Second}
	for _, app := range []string{"shop", "blog"} {
		step := resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: env, Name: app}, Action: resource.ActionCreate}
		ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-for-" + app})
		if err := d.Apply(ctx, step, &resourcev1.DatabaseUser{Service: "db", Password: strings.ToUpper(app) + "_DATABASE_PASSWORD"}); err != nil {
			t.Fatalf("making %s's user: %v", app, err)
		}
		if err := d.Verify(t.Context(), step.Ref, &resourcev1.DatabaseUser{Service: "db"}); err != nil {
			t.Errorf("%s checks as %v", app, err)
		}
	}
	// mongosh as an app, signed in to a database with its own password.
	as := func(user, db, script string) (string, bool) {
		var out, errs bytes.Buffer
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		url := "mongodb://" + user + ":a-password-for-" + user + "@127.0.0.1:27017/" + db
		code, err := client.Exec(ctx, id, []string{"mongosh", "--quiet", "--norc", url, "--eval", script}, docker.Streams{Stdout: &out, Stderr: &errs})
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(errs.String() + out.String()), code == 0
	}
	for _, app := range []string{"shop", "blog"} {
		if said, ok := as(app, app, `db.posts.insertOne({ title: "hello" }); print(db.posts.countDocuments())`); !ok || !strings.HasSuffix(said, "1") {
			t.Errorf("%s can't write in its own database: %s", app, said)
		}
	}
	if said, ok := as("blog", "blog", `print(db.getSiblingDB("shop").posts.countDocuments())`); ok {
		t.Errorf("blog reads shop's database: %s", said)
	}
}
