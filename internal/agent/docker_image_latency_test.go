// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/agent/collect"
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
		if row.GetImage().GetTotalBytes() != 42<<20 || row.GetImage().UniqueBytes != nil || row.GetObservedAt() != nil {
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
