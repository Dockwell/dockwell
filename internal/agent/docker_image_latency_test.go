// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

func BenchmarkDockerImageListCold(b *testing.B) {
	engine := dockertest.New(b)
	for range 20 {
		engine.AddLocalImage("", dockertest.Image{Size: 2 << 30})
	}
	engine.Intercept(func(req *http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/system/df") {
			time.Sleep(650 * time.Millisecond)
		}
		return nil
	})
	for b.Loop() {
		c := &calls{cfg: Config{Docker: engine.Client(), disks: &diskUse{
			docker: engine.Client(), collector: collect.New(), now: time.Now,
		}}}
		if _, err := c.dockerImageList(b.Context(), &agentv1.DockerInventoryListCall{}); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if _, _, err := c.cfg.disks.snapshot(b.Context(), false); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

func TestDockerImagesAppearBeforeDiskAccounting(t *testing.T) {
	engine := dockertest.New(t)
	engine.AddLocalImage("fixture/image:latest", dockertest.Image{Size: 42 << 20})
	started, release := make(chan struct{}), make(chan struct{})
	defer closeUnlessClosed(release)
	var counts atomic.Int32
	engine.Intercept(func(req *http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/system/df") {
			if counts.Add(1) == 1 {
				close(started)
			}
			select {
			case <-release:
			case <-req.Context().Done():
			}
		}
		return nil
	})
	c := &calls{cfg: Config{Dir: t.TempDir(), Docker: engine.Client(), disks: &diskUse{
		lifetime: t.Context(), docker: engine.Client(), collector: collect.New(), now: time.Now,
	}}}
	// Docker cannot produce a disk count until this test releases it.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for range 4 {
		rows, err := c.dockerImageList(ctx, &agentv1.DockerInventoryListCall{})
		if err != nil || len(rows.GetObjects()) != 1 {
			t.Fatalf("image inventory waited for disk accounting: %v, %v", rows, err)
		}
		row := rows.GetObjects()[0]
		if row.GetImage().GetTotalBytes() != 42<<20 || row.GetImage().UniqueBytes != nil || row.GetObservedAt() != nil ||
			row.GetImage().GetState() != v1.DockerImageState_DOCKER_IMAGE_STATE_UNUSED {
			t.Fatalf("cold image row = %v", row)
		}
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("no shared count started")
	}
	if use, err := c.dockerDiskUse(ctx, nil); err != nil || use.GetTime() != nil {
		t.Fatalf("overview did not report a pending measurement: %v, %v", use, err)
	}
	if _, err := c.dockerCleanupPreview(ctx, &agentv1.DockerMaintenancePreviewCall{}); resource.ReasonOf(err) != resource.ReasonDiskMeasuring {
		t.Fatalf("cleanup did not report a pending measurement: %v", err)
	}
	close(release)
	if _, _, err := c.cfg.disks.snapshot(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	rows, err := c.dockerImageList(t.Context(), &agentv1.DockerInventoryListCall{})
	if err != nil || len(rows.GetObjects()) != 1 || rows.GetObjects()[0].GetImage().GetUniqueBytes() != 42<<20 ||
		rows.GetObjects()[0].GetObservedAt() == nil || counts.Load() != 1 {
		t.Fatalf("size was not filled with one shared count: %v, %v, %d counts", rows, err, counts.Load())
	}
}

func closeUnlessClosed(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func TestDockerImageStateFiltersBeforePagination(t *testing.T) {
	engine := dockertest.New(t)
	client := engine.Client()
	for _, ref := range []string{"fixture/first:latest", "fixture/second:latest", "fixture/third:latest"} {
		engine.AddLocalImage(ref, dockertest.Image{Size: 64})
	}
	images, err := client.InventoryImages(t.Context())
	if err != nil || len(images) != 3 {
		t.Fatalf("fixture images: %v, %v", images, err)
	}
	slices.SortFunc(images, func(a, b docker.InventoryImage) int { return strings.Compare(a.ID, b.ID) })
	engine.AddContainer(docker.Container{Config: docker.Config{Image: images[2].ID}, State: docker.State{Status: "exited"}})
	c := &calls{cfg: Config{Docker: client}}
	ask := &agentv1.DockerInventoryListCall{
		PageSize: 1, Filter: &v1.DockerObjectFilter{ImageState: v1.DockerImageState_DOCKER_IMAGE_STATE_USED},
	}
	used, err := c.dockerImageList(t.Context(), ask)
	if err != nil || len(used.GetObjects()) != 1 || used.GetObjects()[0].GetKey().GetEngineId() != images[2].ID || used.GetNextId() != "" {
		t.Fatalf("used image beyond the first unfiltered page: %v, %v", used, err)
	}
	ask.Filter.ImageState = v1.DockerImageState_DOCKER_IMAGE_STATE_UNUSED
	unused, err := c.dockerImageList(t.Context(), ask)
	if err != nil || len(unused.GetObjects()) != 1 || unused.GetNextId() != images[0].ID {
		t.Fatalf("unused first page: %v, %v", unused, err)
	}
	ask.AfterId, ask.Snapshot = unused.GetNextId(), unused.GetSnapshot()
	other, err := c.dockerImageList(t.Context(), ask)
	if err != nil || len(other.GetObjects()) != 1 || other.GetObjects()[0].GetKey().GetEngineId() != images[1].ID || other.GetNextId() != "" {
		t.Fatalf("unused second page: %v, %v", other, err)
	}
	engine.AddContainer(docker.Container{Config: docker.Config{Image: images[1].ID}})
	changed, err := c.dockerImageList(t.Context(), ask)
	if err != nil || !changed.GetResync() {
		t.Fatalf("a usage change must invalidate the cursor: %v, %v", changed, err)
	}
}

func TestDockerImageWithUnknownUsageStaysInEveryFilter(t *testing.T) {
	c, engine := cleanupFixture(t)
	engine.AddLocalImage("fixture/image:latest", dockertest.Image{Size: 64})
	if _, _, err := c.cfg.disks.snapshotMode(t.Context(), false, true); err != nil {
		t.Fatal(err)
	}
	engine.Intercept(func(req *http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/containers/json") {
			return errors.New("container list unavailable")
		}
		return nil
	})
	for _, state := range []v1.DockerImageState{
		v1.DockerImageState_DOCKER_IMAGE_STATE_UNSPECIFIED,
		v1.DockerImageState_DOCKER_IMAGE_STATE_USED,
		v1.DockerImageState_DOCKER_IMAGE_STATE_UNUSED,
		v1.DockerImageState_DOCKER_IMAGE_STATE_DANGLING,
	} {
		out, err := c.dockerImageList(t.Context(), &agentv1.DockerInventoryListCall{
			Filter: &v1.DockerObjectFilter{ImageState: state},
		})
		if err != nil || len(out.GetObjects()) != 1 || out.GetObjects()[0].GetImage().GetState() != 0 ||
			out.GetObjects()[0].GetObservedAt() == nil {
			t.Errorf("state filter %s with a measured size and unknown usage: %v, %v", state, out, err)
		}
	}
}
