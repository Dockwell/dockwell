// SPDX-License-Identifier: AGPL-3.0-only

package backup_test

import (
	"bytes"
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/catalog/folder"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

func TestPlausibleClickHouseNativeBackupOnRealDocker(t *testing.T) {
	cat, err := catalog.BuiltIn("")
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := cat.Entry("plausible")
	if !ok {
		t.Fatal("the catalog has no Plausible entry")
	}
	stack, err := folder.ReadStack(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	var app *resourcev1.Container
	for _, service := range stack.Services {
		if service.Name == "events" {
			app = service.Spec.GetApp()
			break
		}
	}
	if app == nil {
		t.Fatal("the Plausible entry has no events service")
	}
	if !strings.Contains(app.GetImage(), "@sha256:") || app.GetEnv()["PLAUSIBLE_SERVER_XML"] == "" || app.GetEnv()["PLAUSIBLE_USERS_XML"] == "" {
		t.Fatal("the Plausible entry must provide a pinned ClickHouse image and its server and user configuration")
	}
	client := dockertest.Real(t)
	pull, stopPull := context.WithTimeout(t.Context(), 5*time.Minute)
	err = client.PullImage(pull, app.GetImage(), nil)
	stopPull()
	if err != nil {
		t.Fatalf("pull Plausible's ClickHouse pin %s: %v", app.GetImage(), err)
	}

	helper := filepath.Join(t.TempDir(), "dockwell")
	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-tags", "agent", "-o", helper, "./cmd/dockwell")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the volume helper: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	ref := resource.Ref{Kind: volume.Kind, Scope: dockertest.Scope(t), Name: "events"}
	data := volume.TargetOf(ref)
	dockertest.Cleanup(t, client, data.Project)
	if _, err := client.CreateVolume(ctx, &docker.VolumeCreate{
		Name: data.Name, Labels: map[string]string{docker.LabelProject: data.Project, docker.LabelVolume: ref.Name},
	}); err != nil {
		t.Fatal(err)
	}
	env := make([]string, 0, len(app.GetEnv()))
	for _, key := range slices.Sorted(maps.Keys(app.GetEnv())) {
		env = append(env, key+"="+app.GetEnv()[key])
	}
	id, err := client.CreateContainer(ctx, docker.ContainerName(data.Project, "events"), &docker.CreateRequest{
		Config: docker.Config{
			Image: app.GetImage(), Cmd: app.GetCommand(), Entrypoint: app.GetEntrypoint(), Env: env,
			Labels: map[string]string{docker.LabelProject: data.Project, docker.LabelService: "events", docker.LabelNumber: "1"},
		},
		HostConfig: docker.HostConfig{
			NetworkMode: "none", NanoCPUs: 1e9,
			Mounts: []docker.Mount{{Type: "volume", Source: data.Name, Target: "/var/lib/clickhouse"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	ready, stop := context.WithTimeout(ctx, 90*time.Second)
	defer stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		code, err := client.Exec(ready, id, []string{"clickhouse-client", "--query=SELECT 1"}, docker.Streams{})
		if err == nil && code == 0 {
			break
		}
		select {
		case <-ticker.C:
		case <-ready.Done():
			t.Fatalf("Plausible's ClickHouse did not become ready: %v", ready.Err())
		}
	}
	var said bytes.Buffer
	code, err := client.Exec(ctx, id, []string{"clickhouse-client", "--multiquery"}, docker.Streams{
		Stdin:  strings.NewReader("CREATE DATABASE analytics; CREATE TABLE analytics.fixture (n UInt64) ENGINE=MergeTree ORDER BY n; INSERT INTO analytics.fixture VALUES (42)"),
		Stderr: &said,
	})
	if err != nil || code != 0 {
		t.Fatalf("seed Plausible's ClickHouse: %d, %v, %s", code, err, &said)
	}
	r := &backup.Runner{Client: client, Root: t.TempDir(), Binary: helper}
	got, err := r.Take(ctx, backup.Take{ID: "bkp_plausible_native", Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != resource.BackupDump || got.Database != "ClickHouse" || got.Size == 0 {
		t.Fatalf("want a verified ClickHouse dump for %s, got %+v", app.GetImage(), got)
	}
}
