// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// pgImage is a fake PostgreSQL of a major release: its data is one file of SQL,
// which its tools dump, load and count as a move asks them to.
func pgImage(major string) dockertest.Image {
	return dockertest.Image{
		Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=" + major}},
		Exec: func(x dockertest.Exec) int {
			file := filepath.Join(x.Volumes["/var/lib/postgresql/data"], "data.sql")
			script := strings.Join(x.Cmd, " ")
			switch {
			case strings.Contains(script, "noconnect"):
			case strings.Contains(script, "pg_roles"):
				data, _ := os.ReadFile(file)
				rows := 0
				for _, l := range strings.Split(string(data), "\n") {
					if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "--") {
						rows++
					}
				}
				_, _ = fmt.Fprintf(x.Stdout, "database fake %d\n", rows)
			case strings.Contains(script, "pg_dumpall"):
				data, err := os.ReadFile(file)
				if err != nil {
					return 1
				}
				_, _ = x.Stdout.Write(data)
				_, _ = io.WriteString(x.Stdout, "\n-- PostgreSQL database cluster dump complete\n")
			case strings.Contains(script, "psql"):
				data, err := io.ReadAll(x.Stdin)
				if err != nil || os.WriteFile(file, data, 0o600) != nil {
					return 1
				}
			}
			return 0
		},
	}
}

// crashed is a crash at a point of a rollout, as the agent dying there would leave it.
type crashed struct{ at string }

// N1: a move cut short by a crash after its replace made the new container, made
// and not started or started with the old one still there, finishes on resume: the
// replace completes, the new release is checked, and only then does what stopped
// start again, never the old release on its old volume.
func TestAMoveCutShortInItsReplaceFinishesIt(t *testing.T) {
	for _, at := range []string{container.PointCreated, container.PointHealthy} {
		t.Run(at, func(t *testing.T) {
			e := dockertest.New(t)
			e.AddLocalImage("postgres:16.4", pgImage("16"))
			e.AddLocalImage("postgres:17.2", pgImage("17"))
			e.AddLocalImage("nginx:1", dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}})
			client := e.Client()
			ctx := t.Context()
			vol := func(name string) volume.Target {
				return volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: env, Name: name})
			}
			for _, name := range []string{"data", "data-17"} {
				if _, err := client.CreateVolume(ctx, &docker.VolumeCreate{Name: vol(name).Name}); err != nil {
					t.Fatal(err)
				}
			}
			helper := filepath.Join(t.TempDir(), "dockwell")
			if err := os.WriteFile(helper, []byte("#!agent\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			rt := &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: 2 * time.Second, ListenWait: 2 * time.Second}
			runner := &backup.Runner{Client: client, Root: t.TempDir(), Binary: helper, Work: 1000}
			local := resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: rt}, Movers: runner})}
			db := resource.Ref{Kind: service.Kind, Scope: env, Name: "db"}
			on := func(image, data string) *resourcev1.Service {
				return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
					Image: image, Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: data}, Target: "/var/lib/postgresql/data"}},
				}}}
			}
			deploy := func(want resource.Desired) {
				t.Helper()
				steps, err := local.Plan(ctx, []resource.Desired{want})
				if err != nil {
					t.Fatal(err)
				}
				if err := local.Apply(ctx, steps[0], want.Spec); err != nil {
					t.Fatal(err)
				}
			}
			deploy(resource.Desired{Ref: db, Spec: on("postgres:16.4", "data")})
			deploy(resource.Desired{Ref: webRef, Spec: app("nginx:1")})
			if err := os.WriteFile(filepath.Join(e.VolumeDir(vol("data").Name), "data.sql"), []byte("CREATE TABLE orders (id int);\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			move := &resource.MoveAsked{Backup: "bak_move", From: "data", To: "data-17", Change: "chg_move"}
			want := resource.Desired{Ref: db, Spec: on("postgres:17.2", "data-17"), Move: move}
			steps, err := local.Plan(ctx, []resource.Desired{want})
			if err != nil {
				t.Fatal(err)
			}
			if steps[0].Move == nil || steps[0].Move.Finishing {
				t.Fatalf("the plan is %+v, want a move", steps[0].Move)
			}
			rt.Points = func(name string) {
				if name == at {
					panic(crashed{at})
				}
			}
			func() {
				defer func() {
					if c, ok := recover().(crashed); !ok || c.at != at {
						t.Fatalf("the move didn't stop at %s", at)
					}
				}()
				_ = local.Apply(ctx, steps[0], want.Spec)
			}()
			rt.Points = nil

			// The resume finishes what the crash left, and what stopped starts once
			// the new release runs, on its own.
			steps, err = local.Plan(ctx, []resource.Desired{want})
			if err != nil {
				t.Fatal(err)
			}
			if steps[0].Move == nil || !steps[0].Move.Finishing {
				t.Fatalf("the resume's plan is %+v, want it to finish the move", steps[0].Decision)
			}
			if err := local.Apply(ctx, steps[0], want.Spec); err != nil {
				t.Fatal(err)
			}
			list, err := client.ListContainers(ctx, map[string][]string{"label": {docker.LabelService + "=db"}})
			if err != nil {
				t.Fatal(err)
			}
			var images []string
			for i := range list {
				c, err := client.InspectContainer(ctx, list[i].ID)
				if err != nil {
					t.Fatal(err)
				}
				images = append(images, fmt.Sprintf("%s running=%v", c.Config.Image, c.State.Running))
			}
			if len(images) != 1 || images[0] != "postgres:17.2 running=true" {
				t.Errorf("after the resume, db runs %v, want postgres:17.2 alone", images)
			}
			webs, err := client.ListContainers(ctx, map[string][]string{"label": {docker.LabelService + "=web"}})
			if err != nil || len(webs) != 1 || webs[0].State != "running" {
				t.Errorf("web after the resume: %+v, %v", webs, err)
			}
		})
	}
}
