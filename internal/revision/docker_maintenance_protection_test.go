// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"slices"
	"testing"
	"time"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestDockerCleanupProtectionScopedToServerAndKinds(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A", "srv_B")
		appA := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "app-a"}
		appB := resource.Ref{Kind: container.Kind, Scope: "srv_B", Name: "app-b"}
		apply(t, e, propose(t, e, "create workloads with volumes",
			Edit{
				Ref: appA,
				Spec: &spec.Container{
					Image:  "nginx:1.27",
					Mounts: []*spec.Mount{{Source: &spec.Mount_Volume{Volume: "vol-a"}, Target: "/data"}},
				},
			},
			Edit{
				Ref: appB,
				Spec: &spec.Container{
					Image:  "nginx:1.27",
					Mounts: []*spec.Mount{{Source: &spec.Mount_Volume{Volume: "vol-b"}, Target: "/data"}},
				},
			},
		))
		// Delete app-b on srv_B so vol-b sits inside srv_B's 7-day undo window only.
		apply(t, e, propose(t, e, "delete app-b", Edit{Ref: appB}))

		now := time.Now()

		// Image-only protection does not populate volume or network lists.
		imgOnly, err := DockerCleanupProtectionKinds(t.Context(), db, "srv_A", now, true, false, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(imgOnly.Volumes) != 0 || len(imgOnly.RollbackVolumes) != 0 || len(imgOnly.BackupVolumes) != 0 || len(imgOnly.Networks) != 0 {
			t.Fatalf("image-only protection leaked volumes/networks: %+v", imgOnly)
		}
		if len(imgOnly.Images) == 0 {
			t.Fatal("image-only protection omitted infrastructure image pin")
		}

		// Cache-only protection returns empty sets.
		cacheOnly, err := DockerCleanupProtectionKinds(t.Context(), db, "srv_A", now, false, false, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(cacheOnly.Images)+len(cacheOnly.Networks)+len(cacheOnly.Volumes)+len(cacheOnly.RollbackVolumes)+len(cacheOnly.BackupVolumes) != 0 {
			t.Fatalf("cache-only protection should be empty: %+v", cacheOnly)
		}

		// Volume protection on srv_A includes vol-a, and excludes vol-b superseded on srv_B.
		volProtA, err := DockerCleanupProtectionKinds(t.Context(), db, "srv_A", now, false, false, true)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(volProtA.Volumes, "vol-a") {
			t.Fatalf("srv_A volume protection missing vol-a: %+v", volProtA)
		}
		if slices.Contains(volProtA.Volumes, "vol-b") || slices.Contains(volProtA.RollbackVolumes, "vol-b") {
			t.Fatalf("srv_A volume protection leaked srv_B volume vol-b: %+v", volProtA)
		}

		// Volume protection on srv_B retains vol-b in RollbackVolumes for its 7-day undo window.
		volProtB, err := DockerCleanupProtectionKinds(t.Context(), db, "srv_B", now, false, false, true)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(volProtB.RollbackVolumes, "vol-b") {
			t.Fatalf("srv_B volume protection missing rollback vol-b: %+v", volProtB)
		}
	})
}
