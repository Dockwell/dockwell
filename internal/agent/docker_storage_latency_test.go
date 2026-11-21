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
	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

func TestDockerStorageReadsFinishDuringDiskAccounting(t *testing.T) {
	engine := dockertest.New(t)
	engine.Mountpoints = true
	engine.AddNetwork(docker.Network{Name: "fixture-network", Scope: "local", Driver: "bridge"})
	volume := docker.Volume{
		Name: "fixture-volume", Driver: "local", Scope: "local", CreatedAt: "2025-01-01T00:00:00Z",
		Mountpoint: "/var/lib/docker/volumes/fixture-volume/_data",
	}
	engine.AddVolume(volume)
	engine.VolumeSizes = map[string]int64{volume.Name: 12345}
	networkID := engine.Network("fixture-network").ID
	container := engine.AddContainer(docker.Container{
		Name:   "/fixture-container",
		Mounts: []docker.MountPoint{{Type: "volume", Name: volume.Name, Destination: "/data"}},
	})
	client := engine.Client()
	t.Cleanup(client.Close)
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
				return req.Context().Err()
			}
			return nil
		}
		// Give the metadata endpoints a realistic delay while df stays blocked.
		timer := time.NewTimer(20 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-req.Context().Done():
			return req.Context().Err()
		case <-timer.C:
			return nil
		}
	})
	c := &calls{cfg: Config{Dir: t.TempDir(), Docker: client, disks: &diskUse{
		lifetime: t.Context(), docker: client, collector: collect.New(), now: time.Now,
	}}}
	bounded := func(name string, read func(context.Context) bool) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
			defer cancel()
			if !read(ctx) || ctx.Err() != nil {
				t.Fatalf("%s did not return its data while disk accounting was blocked", name)
			}
			t.Logf("%s returned in %s", name, time.Since(start).Round(time.Millisecond))
		})
	}
	bounded("progressive disk read", func(ctx context.Context) bool {
		use, err := c.dockerDiskUse(ctx, nil)
		return err == nil && use.GetTime() == nil
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the disk count did not start")
	}
	bounded("network list", func(ctx context.Context) bool {
		rows, err := c.dockerNetworkList(ctx, &agentv1.DockerInventoryListCall{
			Filter: &v1.DockerObjectFilter{Search: "fixture-network"},
		})
		return err == nil && len(rows.GetObjects()) == 1
	})
	bounded("network detail", func(ctx context.Context) bool {
		row, err := c.dockerNetworkDetail(ctx, networkID)
		return err == nil && row.GetObject().GetNetwork() != nil
	})
	bounded("volume list", func(ctx context.Context) bool {
		rows, err := c.dockerVolumeList(ctx, &agentv1.DockerInventoryListCall{})
		return err == nil && len(rows.GetObjects()) == 1 && rows.GetObjects()[0].GetVolume().SizeBytes == nil
	})
	bounded("volume detail", func(ctx context.Context) bool {
		row, err := c.dockerVolumeDetail(ctx, docker.VolumeInventoryKey(&volume))
		return err == nil && row.GetObject().GetVolume() != nil && row.GetObject().GetVolume().SizeBytes == nil
	})
	bounded("connect picker container list", func(ctx context.Context) bool {
		rows, err := dockerInventoryList(ctx, client, &agentv1.DockerInventoryListCall{})
		return err == nil && len(rows.GetObjects()) == 1
	})
	bounded("container volume links", func(ctx context.Context) bool {
		row, err := dockerInventoryDetail(ctx, client, &agentv1.DockerInventoryDetailCall{ContainerId: container.ID})
		if err != nil || row.GetObject().GetContainer() == nil {
			return false
		}
		body := row.GetObject().GetContainer()
		c.volumeLinks(ctx, body)
		return len(body.GetVolumeMounts()) == 1
	})
	bounded("network preflight", func(ctx context.Context) bool {
		result, err := c.checkDockerNetwork(ctx, &agentv1.CheckDockerNetworkCall{
			EngineName: "fixture-new-network", Spec: &spec.Network{Driver: "bridge"},
		})
		return err == nil && result != nil && len(result.GetFindings()) == 0
	})
	close(release)
	if _, _, err := c.cfg.disks.snapshot(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	rows, err := c.dockerVolumeList(t.Context(), &agentv1.DockerInventoryListCall{})
	if err != nil || len(rows.GetObjects()) != 1 || rows.GetObjects()[0].GetVolume().GetSizeBytes() != 12345 || counts.Load() != 1 {
		t.Fatalf("volume size did not arrive after the one shared count: %v, %v, %d counts", rows, err, counts.Load())
	}
}
