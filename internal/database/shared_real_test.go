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

// On a real PostgreSQL, before 15 and after, two apps sharing one server each get a
// user and a database, and each makes a table in its own database's public schema,
// while neither connects to the other's, nor makes anything in template1, which every
// later database is made from (ADR 0084). Before 15, closing template1's public schema
// to PUBLIC left each later database's public schema closed to its own owner.
func TestTwoAppsShareAPostgresOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	for _, image := range []string{"postgres:14-alpine", "postgres:17-alpine"} {
		t.Run(image, func(t *testing.T) {
			env := dockertest.Scope(t)
			project := docker.Project(env)
			dockertest.Cleanup(t, client, project)
			if err := client.PullImage(t.Context(), image, nil); err != nil {
				t.Fatal(err)
			}
			id, err := client.CreateContainer(t.Context(), docker.ContainerName(project, "db"), &docker.CreateRequest{Config: docker.Config{
				Image:  image,
				Env:    []string{"POSTGRES_PASSWORD=the-administrators"},
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
			}
			// psql as a user, on the server's own socket, which signs anyone in: what
			// each may do is its roles', not how it signed in.
			as := func(user, db, sql string) (string, bool) {
				var out, errs bytes.Buffer
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				code, err := client.Exec(ctx, id, []string{"psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", db, "-c", sql}, docker.Streams{Stdout: &out, Stderr: &errs})
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(errs.String() + out.String()), code == 0
			}
			for _, app := range []string{"shop", "blog"} {
				if said, ok := as(app, app, "CREATE TABLE posts (id integer)"); !ok {
					t.Errorf("%s can't make a table in its own database: %s", app, said)
				}
			}
			if said, ok := as("blog", "shop", "SELECT 1"); ok || !strings.Contains(said, "permission denied") {
				t.Errorf("blog connects to shop's database: %s", said)
			}
			if said, ok := as("shop", "template1", "SELECT 1"); ok || !strings.Contains(said, "permission denied") {
				t.Errorf("shop connects to template1: %s", said)
			}
		})
	}
}
