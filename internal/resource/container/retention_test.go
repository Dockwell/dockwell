// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

func TestRemovePersistsImageVolumeRetentionBeforeContainerDelete(t *testing.T) {
	ctx := t.Context()
	anonVol := strings.Repeat("a", 64)

	t.Run("write failure stops removal and keeps container intact", func(t *testing.T) {
		e := dockertest.New(t)
		// The ledger loads successfully, but opening its temporary write file fails.
		keptPath := filepath.Join(t.TempDir(), "kept.json")
		kept := &volume.Kept{Path: keptPath}
		if err := os.Mkdir(keptPath+".new", 0o700); err != nil {
			t.Fatal(err)
		}
		e.AddVolume(docker.Volume{Name: anonVol, Driver: "local", Scope: "local"})
		e.AddContainer(docker.Container{
			Name:   "/env_keep-app-1",
			State:  docker.State{Status: "running", Running: true},
			Config: docker.Config{Labels: map[string]string{docker.LabelProject: "env_keep", docker.LabelService: "app", docker.LabelNumber: "1"}},
			Mounts: []docker.MountPoint{{Type: "volume", Name: anonVol, Destination: "/var/lib/data"}},
		})
		drv := &container.Docker{Client: e.Client(), Deleted: kept}
		target := container.Target{Name: "env_keep-app", Project: "env_keep", Service: "app", Scoped: true}
		var untouched bool
		removeCtx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { untouched = out.Untouched })
		err := drv.Remove(removeCtx, target)
		if !untouched {
			t.Fatal("retention write failure did not report the workload untouched")
		}
		if err == nil || !strings.Contains(err.Error(), "retention ledger") || !strings.Contains(err.Error(), "Free disk space or restore write access") {
			t.Fatalf("Remove error did not explain how to recover the retention write: %v", err)
		}
		if live := e.Container("env_keep-app-1"); live == nil || !live.State.Running {
			t.Fatalf("retention write failure changed the running container: %v", live)
		}
		for _, request := range e.Seen() {
			if (strings.HasPrefix(request, "POST /") || strings.HasPrefix(request, "DELETE /")) && strings.Contains(request, "/containers/") {
				t.Fatalf("retention write failure allowed Docker mutation: %s", request)
			}
		}
	})

	t.Run("interrupted removal after retention write leaves volume durably retained", func(t *testing.T) {
		e := dockertest.New(t)
		keptPath := filepath.Join(t.TempDir(), "kept.json")
		kept := &volume.Kept{Path: keptPath}
		e.AddVolume(docker.Volume{Name: anonVol, Driver: "local", Scope: "local"})
		e.AddContainer(docker.Container{
			Name:   "/env_crash-app-1",
			State:  docker.State{Status: "exited"},
			Config: docker.Config{Labels: map[string]string{docker.LabelProject: "env_crash", docker.LabelService: "app", docker.LabelNumber: "1"}},
			Mounts: []docker.MountPoint{{Type: "volume", Name: anonVol, Destination: "/var/lib/data"}},
		})
		// Simulate crash/failure right at DELETE /containers/...
		failDelete := true
		e.Intercept(func(r *http.Request) error {
			if failDelete && r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/") {
				return errors.New("simulated crash before container delete")
			}
			return nil
		})
		drv := &container.Docker{Client: e.Client(), Deleted: kept}
		target := container.Target{Name: "env_crash-app", Project: "env_crash", Service: "app", Scoped: true}
		if err := drv.Remove(ctx, target); err == nil {
			t.Fatal("expected simulated delete failure")
		}
		// Reload Kept from disk to verify durable persistence across restart.
		reloaded := &volume.Kept{Path: keptPath}
		has, err := reloaded.Has(anonVol)
		if err != nil || !has {
			t.Fatalf("reloaded Kept.Has(%s) = %t, %v; want true", anonVol, has, err)
		}
		// Retry succeeds once the Engine answers again.
		failDelete = false
		if err := drv.Remove(ctx, target); err != nil {
			t.Fatalf("retry Remove failed: %v", err)
		}
		if e.Container("env_crash-app-1") != nil {
			t.Fatal("container still exists after successful retry")
		}
	})
}
