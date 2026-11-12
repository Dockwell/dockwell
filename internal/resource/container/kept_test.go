// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// The image a moved tag leaves keeps a name of Dockwell's own for the undo window,
// so a rollback finds it on the server, and goes after that (ADR 0080).
func TestTheImageAMovedTagLeavesIsKeptForItsRollback(t *testing.T) {
	e := dockertest.New(t)
	const image = "ghcr.io/acme/shop:latest"
	e.Registry[image] = dockertest.Image{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Config: docker.Config{Cmd: []string{"shop"}}}
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	kept := &container.KeptImages{Path: filepath.Join(t.TempDir(), "kept-images.json"), Client: e.Client(), Now: func() time.Time { return now }}
	d := &container.Driver{Runtime: &container.Docker{Client: e.Client(), Kept: kept}}
	ref := resource.Ref{Kind: container.Kind, Name: "web"}
	spec := &resourcev1.Container{Image: image}
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err != nil {
		t.Fatal(err)
	}
	old := e.Container("web").Image
	e.Registry[image] = dockertest.Image{Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Config: docker.Config{Cmd: []string{"shop"}}}
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionReplace, Pull: true}}, spec); err != nil {
		t.Fatal(err)
	}
	name := container.KeptName(image, old)
	kept1 := func() bool {
		img, err := e.Client().InspectImage(t.Context(), name)
		return err == nil && img.ID == old
	}
	if !kept1() {
		t.Fatalf("%s doesn't name the image that ran, %s", name, old)
	}

	logger := slog.New(slog.DiscardHandler)
	now = now.Add(container.KeepImagesFor - time.Hour)
	if err := kept.Prune(t.Context(), logger); err != nil || !kept1() {
		t.Errorf("within the undo window: %v, kept %t", err, kept1())
	}
	now = now.Add(2 * time.Hour)
	if err := kept.Prune(t.Context(), logger); err != nil || kept1() {
		t.Errorf("after the undo window: %v, kept %t", err, kept1())
	}
}

// A kept name is a tag Docker takes, whatever the repository was called.
func TestAKeptNameIsATagDockerTakes(t *testing.T) {
	for ref, want := range map[string]string{
		"ghcr.io/acme/Shop_App:latest": container.KeptRepo + "/shop_app:0123456789ab",
		"nginx":                        container.KeptRepo + "/nginx:0123456789ab",
		"registry.example.com:5000/x":  container.KeptRepo + "/x:0123456789ab",
	} {
		if got := container.KeptName(ref, "sha256:0123456789abcdef"); got != want {
			t.Errorf("KeptName(%s) = %s, want %s", ref, got, want)
		}
	}
}
