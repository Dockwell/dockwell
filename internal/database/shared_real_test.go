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
// to PUBLIC left each later database's public schema closed to its own owner, and a
// server upgraded from before 15 keeps template1's that way: each new database's is
// given to its user as it's made, and one made closed before that fails its check,
// and is put right when its user is applied again.
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
			admin := func(db, sql string) {
				t.Helper()
				var said string
				ok := false
				// The image's first start ends with its setup server stopping.
				for range 60 {
					if said, ok = as("postgres", db, sql); ok {
						return
					}
					time.Sleep(time.Second)
				}
				t.Fatalf("%s: %s", sql, said)
			}
			// template1's public schema as the server's own and closed, as #596 left it
			// before 15, and as an upgrade from before 15 leaves it.
			admin("template1", "ALTER SCHEMA public OWNER TO postgres; REVOKE CREATE ON SCHEMA public FROM PUBLIC")
			apply := func(app string, action resource.Action) error {
				step := resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: env, Name: app}, Action: action}
				ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-for-" + app})
				return d.Apply(ctx, step, &resourcev1.DatabaseUser{Service: "db", Password: strings.ToUpper(app) + "_DATABASE_PASSWORD"})
			}
			for _, app := range []string{"shop", "blog"} {
				if err := apply(app, resource.ActionCreate); err != nil {
					t.Fatalf("making %s's user: %v", app, err)
				}
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

			// shop's database as #596 made it before 15: its check says so, and applying
			// its user again puts it right.
			admin("shop", "ALTER SCHEMA public OWNER TO postgres; REVOKE CREATE ON SCHEMA public FROM PUBLIC")
			ref := resource.Ref{Kind: UserKind, Scope: env, Name: "shop"}
			desired := &resourcev1.DatabaseUser{Service: "db", Password: "SHOP_DATABASE_PASSWORD"}
			if err := d.Verify(t.Context(), ref, desired); err == nil || !strings.Contains(err.Error(), "shop can't make tables in its database shop") {
				t.Errorf("checking shop with its public schema closed = %v", err)
			}
			if err := apply("shop", resource.ActionUpdate); err != nil {
				t.Fatal(err)
			}
			if err := d.Verify(t.Context(), ref, desired); err != nil {
				t.Errorf("applied again, shop checks as %v", err)
			}
			if said, ok := as("shop", "shop", "CREATE TABLE comments (id integer)"); !ok {
				t.Errorf("applied again, shop can't make a table: %s", said)
			}
		})
	}
}
