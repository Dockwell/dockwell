// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

const srv = "srv_01KAT4ZR6E3V0Y5XBKZB5D9Q6M"

// A container someone ran by hand, without the labels Dockwell gives its
// own, is never taken over by a container of the same name: planning one
// is refused, saying what is there and what to do, and it stays as it is.
func TestAContainerDockwellDidntMakeIsLeftAlone(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	e.AddContainer(docker.Container{
		Name: "/db", Image: "postgres:16",
		Config: docker.Config{Image: "postgres:16", Labels: map[string]string{"mine": "yes"}},
	})
	before := e.Container("db").ID
	l := resource.Local{Drivers: drivers(e)}
	ref := resource.Ref{Kind: container.Kind, Scope: srv, Name: "db"}
	for _, spec := range []*resourcev1.Container{{Image: "postgres:17"}, nil} {
		if spec != nil {
			_ = (&container.Driver{}).Validate(spec)
		}
		var desired []resource.Desired
		if spec == nil {
			desired = []resource.Desired{{Ref: ref}}
		} else {
			desired = []resource.Desired{{Ref: ref, Spec: spec}}
		}
		_, err := l.Plan(t.Context(), desired)
		if err == nil || !strings.Contains(err.Error(), "a container named db is on this server, but Dockwell didn't make it") ||
			!strings.Contains(err.Error(), "docker rename db") || !strings.Contains(err.Error(), "docker rm -f db") {
			t.Errorf("planning over a container run by hand = %v, want it refused, saying how to rename or remove it", err)
		}
	}
	// Applying anyway, as a plan made before someone started it would, is
	// refused too.
	for _, step := range []resource.Step{
		{Ref: ref, Decision: resource.Decision{Action: resource.ActionReplace}},
		{Ref: ref, Decision: resource.Decision{Action: resource.ActionUpdate}},
		{Ref: ref, Decision: resource.Decision{Action: resource.ActionDelete}},
	} {
		var spec *resourcev1.Container
		if step.Action != resource.ActionDelete {
			spec = &resourcev1.Container{Image: "postgres:17"}
			_ = (&container.Driver{}).Validate(spec)
		}
		var err error
		if spec == nil {
			err = l.Apply(t.Context(), step, nil)
		} else {
			err = l.Apply(t.Context(), step, spec)
		}
		if err == nil || !strings.Contains(err.Error(), "Dockwell didn't make it") {
			t.Errorf("applying a %s over a container run by hand = %v, want it refused", step.Action, err)
		}
	}
	if c := e.Container("db"); c == nil || c.ID != before {
		t.Error("the container run by hand is gone")
	}
}

// keptDrivers are the drivers with one list of kept volumes and a clock
// both share, as the agent has them.
func keptDrivers(e *dockertest.Engine, now *time.Time) (*resource.Registry, *volume.Docker) {
	clock := func() time.Time { return *now }
	kept := &volume.Kept{Now: clock}
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: time.Second, Deleted: kept}
	c := container.Driver{Runtime: rt}
	vols := &volume.Docker{Client: e.Client(), Kept: kept, Now: clock}
	return resource.NewRegistry(&c, &service.Driver{Containers: c}, &volume.Driver{Runtime: vols}), vols
}

// mountedAt is the volume mounted at pgdata.
func mountedAt(c *docker.Container) string {
	for _, m := range c.Mounts {
		if m.Destination == pgdata {
			return m.Name
		}
	}
	return ""
}

func postgres() *resourcev1.Service {
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "postgres:17"}}}
}

const pgdata = "/var/lib/postgresql/data"

// Deleting a service whose image keeps its data in a volume Docker made
// destroys that data, and the plan says so, with where; the server keeps
// the volume for 7 days, so undoing the deletion mounts it again, data
// and all, and the plan says it restores.
func TestDeletingAServiceKeepsItsImagesData(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reg, vols := keptDrivers(e, &now)
	l := resource.Local{Drivers: reg}
	ref := resource.Ref{Kind: service.Kind, Scope: env, Name: "db"}
	apply(t, l, ref, postgres())
	name := service.ContainerName(ref)
	first := mountedAt(e.Container(name))

	del := apply(t, l, ref, nil)
	if !del.Destroys || !slices.Equal(del.Kept, []string{pgdata}) {
		t.Errorf("deleting the service = destroys %v, kept %v; want it to destroy the data at %s", del.Destroys, del.Kept, pgdata)
	}
	if e.Volume(first) == nil {
		t.Fatal("the image's volume went with the container")
	}
	undo := apply(t, l, ref, postgres())
	if undo.Action != resource.ActionCreate || !undo.Restores {
		t.Errorf("undoing the deletion = %s, restores %v; want a create that restores", undo.Action, undo.Restores)
	}
	if second := mountedAt(e.Container(name)); second != first {
		t.Errorf("the undo mounts %s at %s, not %s, which has the data", second, pgdata, first)
	}
	unchanged(t, l, ref, postgres())

	// An undo planned within the 7 days and applied after them, once the
	// volume is pruned, fails, rather than making the service empty; one
	// planned then makes it anew, and its plan differs.
	apply(t, l, ref, nil)
	again := postgres()
	_ = (&service.Driver{}).Validate(again)
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: again}})
	if err != nil || !steps[0].Restores {
		t.Fatalf("planning the undo on day 6 = %v, %+v", err, steps)
	}
	now = now.Add(8 * 24 * time.Hour)
	if err := vols.Prune(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if e.Volume(first) != nil {
		t.Fatal("the prune left the image's volume")
	}
	if err := l.Apply(t.Context(), steps[0], again); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Errorf("applying the undo planned before the prune = %v, want it refused, as the data is gone", err)
	}
	if e.Container(name) != nil {
		t.Error("the refused undo made the container anyway")
	}
	if step := apply(t, l, ref, again); step.Action != resource.ActionCreate || step.Restores {
		t.Errorf("planning the undo after the prune = %s, restores %v; want a create of its own", step.Action, step.Restores)
	}
}

// Docker's default keeps a log without a limit, so a new container gets rotation,
// and the deploy that made it. Neither is part of its spec, so it doesn't read as
// drift, and a container without them isn't recreated.
func TestANewContainerHasRotationAndItsDeploy(t *testing.T) {
	e := dockertest.New(t)
	e.Registry["app:1"] = dockertest.Image{Config: docker.Config{Cmd: []string{"app"}}}
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond}
	target := container.Target{Name: "web", Project: "p", Service: "web"}
	spec := &resourcev1.Container{Image: "app:1", Restart: resourcev1.RestartPolicy_RESTART_POLICY_NO}
	ctx := resource.WithDeploy(t.Context(), resource.Deploy{Revision: 12, Change: "chg_12"})
	if err := rt.Run(ctx, target, spec, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	c := e.Container("web")
	if lc := c.HostConfig.LogConfig; lc == nil || lc.Type != docker.LogDriverJSONFile || lc.Config["max-size"] != "10m" || lc.Config["max-file"] != "5" || lc.Config["compress"] != "true" {
		t.Errorf("log config = %+v, want json-file with rotation", lc)
	}
	if c.Config.Labels[container.LabelRevision] != "12" || c.Config.Labels[container.LabelChange] != "chg_12" {
		t.Errorf("labels = %v, want the deploy's", c.Config.Labels)
	}
	read, err := rt.Inspect(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(read, spec) {
		t.Errorf("reading it back = %v, want %v: the log config and the deploy's labels aren't part of its spec", read, spec)
	}
	// The same spec reads the same with neither, as on a container from before.
	e.Edit("web", func(c *docker.Container) {
		c.HostConfig.LogConfig = &docker.LogConfig{Type: docker.LogDriverJSONFile}
		delete(c.Config.Labels, container.LabelRevision)
		delete(c.Config.Labels, container.LabelChange)
	})
	read, err = rt.Inspect(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(read, spec) {
		t.Errorf("without them, reading it back = %v", read)
	}
}

// An operator who chose another log driver for Docker keeps it.
func TestAnotherLogDriverStands(t *testing.T) {
	e := dockertest.New(t)
	e.LoggingDriver = "journald"
	e.Registry["app:1"] = dockertest.Image{}
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond}
	if err := rt.Run(t.Context(), container.Target{Name: "web", Project: "p", Service: "web"}, &resourcev1.Container{Image: "app:1"}, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	if lc := e.Container("web").HostConfig.LogConfig; lc != nil {
		t.Errorf("log config = %+v, want Docker's own choice left alone", lc)
	}
}

// logKeeper records what a replace keeps, and in what order.
type logKeeper struct {
	began, finished []string
	failed          []bool
	running         []bool
}

func (k *logKeeper) Begin(_ context.Context, c *docker.Container, failed bool) container.LogKeep {
	k.began = append(k.began, c.ID)
	k.failed = append(k.failed, failed)
	k.running = append(k.running, c.State.Running)
	return keeping{k}
}

type keeping struct{ k *logKeeper }

func (k keeping) Finish(_ context.Context, c *docker.Container) {
	k.k.finished = append(k.k.finished, c.ID)
	k.k.running = append(k.k.running, c.State.Running)
}

// A replace keeps the old container's log: most of it while it still runs, so
// downtime doesn't wait for that, and the rest once it has stopped, before it is
// removed. What the deploy a rollback rolls back made, or what wasn't well, counts as
// failed.
func TestAReplaceKeepsTheOldContainersLog(t *testing.T) {
	e := dockertest.New(t)
	e.Registry["app:1"] = dockertest.Image{}
	e.Registry["app:2"] = dockertest.Image{}
	e.Registry["app:bad"] = dockertest.Image{Exits: true}
	keeper := &logKeeper{}
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Logs: keeper}
	target := container.Target{Name: "web", Project: "p", Service: "web"}
	run := func(ctx context.Context, image string) string {
		t.Helper()
		if err := rt.Run(ctx, target, &resourcev1.Container{Image: image}, resource.Decision{}); err != nil {
			t.Fatal(err)
		}
		return e.Container("web").ID
	}
	first := run(t.Context(), "app:1")
	if len(keeper.began) != 0 {
		t.Fatalf("kept a log with nothing replaced: %v", keeper.began)
	}
	run(resource.WithDeploy(t.Context(), resource.Deploy{Revision: 2, Change: "chg_2"}), "app:2")
	if len(keeper.began) != 1 || keeper.began[0] != first || keeper.finished[0] != first {
		t.Fatalf("began %v and finished %v, want the old container's", keeper.began, keeper.finished)
	}
	if keeper.failed[0] {
		t.Error("an ordinary replace counted as failed")
	}
	if !keeper.running[0] || keeper.running[1] {
		t.Errorf("running at begin and finish = %v, want running, then stopped", keeper.running)
	}

	// A rollback's replace: what the deploy it rolls back made failed, whatever its state.
	run(resource.WithDeploy(t.Context(), resource.Deploy{Revision: 3, Change: "chg_3", Rollback: true}), "app:1")
	if !keeper.failed[1] {
		t.Error("what a rollback replaced didn't count as failed")
	}

	// What ran before that deploy didn't fail, though a rollback can replace it too, as
	// one pinned to a digest does: its log mustn't take the failed one's place.
	run(resource.WithDeploy(t.Context(), resource.Deploy{Revision: 5, Change: "chg_5", Rollback: true}), "app:2")
	if keeper.failed[2] {
		t.Error("what a deploy didn't make, replaced by a rollback, counted as failed")
	}

	// One that exited with an error counts as failed by itself.
	run(t.Context(), "app:bad")
	run(t.Context(), "app:1")
	if !keeper.failed[len(keeper.failed)-1] {
		t.Error("a container that had exited with an error didn't count as failed")
	}

	// Deleting one keeps its log too.
	last := e.Container("web").ID
	if err := rt.Remove(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if keeper.began[len(keeper.began)-1] != last {
		t.Errorf("deleting didn't keep the log: began %v", keeper.began)
	}
}

// A plan to delete says what happens to the logs its server kept.
func TestAPlanToDeleteSaysWhatHappensToKeptLogs(t *testing.T) {
	e := dockertest.New(t)
	e.Registry["app:1"] = dockertest.Image{}
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Logs: &logKeeper{}}
	target := container.Target{Name: "web", Project: "p", Service: "web", Scoped: true}
	if err := rt.Run(t.Context(), target, &resourcev1.Container{Image: "app:1"}, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	dec := resource.Decision{Action: resource.ActionDelete}
	if err := rt.Judge(t.Context(), target, nil, &dec); err != nil {
		t.Fatal(err)
	}
	if !dec.HidesLogs {
		t.Error("the plan to delete a service doesn't say its kept logs go")
	}
	replace := resource.Decision{Action: resource.ActionReplace}
	if err := rt.Judge(t.Context(), target, &resourcev1.Container{Image: "app:1"}, &replace); err != nil {
		t.Fatal(err)
	}
	if replace.HidesLogs {
		t.Error("the plan to replace it says its kept logs go")
	}
}
