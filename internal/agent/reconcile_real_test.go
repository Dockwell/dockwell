// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/desired"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// Docker's own declared-volume image must retain its data when the agent makes
// it again, and wait when that volume is gone (#357's first review, H2).
func TestAnImagesDataSurvivesReconcileOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	scope := dockertest.Scope(t)
	dockertest.Cleanup(t, client, docker.Project(scope))
	const image = "redis:7-alpine"
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	drivers := resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: &container.Docker{Client: client}}})
	local := resource.Local{Drivers: drivers}
	ref := resource.Ref{Kind: service.Kind, Scope: scope, Name: "data"}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: image}}}
	steps, err := local.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Apply(t.Context(), steps[0], spec); err != nil {
		t.Fatal(err)
	}
	state, err := local.Reviver(ref).RunState(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	vol := state.Volumes["/data"]
	if vol == "" {
		t.Fatal("Docker didn't mount the image's declared /data volume")
	}
	name := service.ContainerName(ref)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = client.RemoveContainer(ctx, name, true)
		_ = client.RemoveVolume(ctx, vol)
	})
	if code, err := client.Exec(t.Context(), name, []string{"sh", "-c", "printf retained > /data/probe"}, docker.Streams{}); err != nil || code != 0 {
		t.Fatalf("write its data: exit %d, %v", code, err)
	}
	dir := t.TempDir()
	store, err := desired.Open(dir, "srv_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := resource.Encode(spec)
	if err != nil {
		t.Fatal(err)
	}
	img, err := local.RunsImage(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Keep(desired.Record{Ref: ref, Revision: 1, Spec: b, Image: img}); err != nil {
		t.Fatal(err)
	}
	if err := store.Seen(ref, desired.Standing{Volumes: state.Volumes}, time.Now()); err != nil {
		t.Fatal(err)
	}
	journal, err := desired.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := newReconciler(Config{Logger: slog.New(slog.DiscardHandler), Drivers: drivers, Desired: store, Journal: journal, Docker: client},
		newPresence(time.Now().Add(-time.Hour)), newUnfinished())
	if err := client.RemoveContainer(t.Context(), name, true); err != nil {
		t.Fatal(err)
	}
	r.pass(t.Context(), true)
	var data bytes.Buffer
	if code, err := client.Exec(t.Context(), name, []string{"cat", "/data/probe"}, docker.Streams{Stdout: &data}); err != nil || code != 0 || data.String() != "retained" {
		t.Fatalf("read after recreation: %q, exit %d, %v", data.String(), code, err)
	}
	if err := client.RemoveContainer(t.Context(), name, true); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveVolume(t.Context(), vol); err != nil {
		t.Fatal(err)
	}
	r.pass(t.Context(), true)
	if st, err := local.Reviver(ref).RunState(t.Context(), ref); err != nil || st.Exists {
		t.Fatalf("with its data volume gone: %+v, %v; want no container", st, err)
	}
}
