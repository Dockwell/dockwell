// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"cmp"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// Finding nothing newer is only "already so" of a container that's running: one
// that stopped since the plan was made still has to be started, whatever the plan
// said of it.
func TestAPullWithNothingNewerStartsWhatStoppedSinceItsPlan(t *testing.T) {
	e := dockertest.New(t)
	const image = "ghcr.io/acme/shop:latest"
	e.Registry[image] = dockertest.Image{Config: docker.Config{Cmd: []string{"shop"}}}
	d := &container.Driver{Runtime: &container.Docker{Client: e.Client()}}
	ref := resource.Ref{Kind: container.Kind, Name: "web"}
	spec := &resourcev1.Container{Image: image}
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err != nil {
		t.Fatal(err)
	}
	pull := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionReplace, Pull: true}}

	running := e.Container("web").ID
	if err := d.Apply(t.Context(), pull, spec); !errors.Is(err, resource.ErrAlreadySo) {
		t.Fatalf("with it running = %v, want it already so", err)
	}
	if e.Container("web").ID != running {
		t.Error("with it running and nothing newer, it was replaced")
	}

	e.Stop("web")
	if err := d.Apply(t.Context(), pull, spec); err != nil {
		t.Fatalf("with it stopped = %v", err)
	}
	if c := e.Container("web"); c == nil || c.ID == running || !c.State.Running {
		t.Errorf("with it stopped, it's now %+v", c)
	}
}

// pullStep is a deploy of the tag a container already names, and what it reported.
func pullStep(t *testing.T, d *container.Driver, ref resource.Ref, spec *resourcev1.Container) (resource.Outcome, error) {
	t.Helper()
	var told resource.Outcome
	ctx := resource.WithOutcome(t.Context(), func(o resource.Outcome) {
		told.Note, told.Digest, told.Untouched = cmp.Or(told.Note, o.Note), cmp.Or(o.Digest, told.Digest), told.Untouched || o.Untouched
	})
	err := d.Apply(ctx, resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionReplace, Pull: true}}, spec)
	return told, err
}

// An image built on the server has no registry, and a pull of it fails, which read
// the same as a registry that's down. A registry saying it hasn't got an image that
// came from none says nothing is wrong: what the server has under the tag stands. But
// a new build under that tag can't take over: only the tag named what ran, so nothing
// could go back to it. So that's refused, with what to do, whether or not it's running.
func TestAnImageBuiltHereStaysUnderItsOwnTagAndABuildOverItIsRefused(t *testing.T) {
	e := dockertest.New(t)
	const image = "acme/built-here:1"
	e.AddLocalImage(image, dockertest.Image{Config: docker.Config{Cmd: []string{"old"}}})
	d := &container.Driver{Runtime: &container.Docker{Client: e.Client()}}
	ref := resource.Ref{Kind: container.Kind, Name: "web"}
	spec := &resourcev1.Container{Image: image}
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err != nil {
		t.Fatal(err)
	}
	running := e.Container("web").ID

	told, err := pullStep(t, d, ref, spec)
	if !errors.Is(err, resource.ErrAlreadySo) {
		t.Fatalf("with nothing built since = %v, want it already so", err)
	}
	if e.Container("web").ID != running {
		t.Error("with nothing built since, it was replaced")
	}
	if !strings.Contains(told.Note, "built or loaded on this server") || told.Digest != "" || told.Untouched {
		t.Errorf("it told %+v, want it saying where the image came from, with no digest, and not a failed step", told)
	}

	// Built again under the same tag, nothing replaces what's there.
	e.AddLocalImage(image, dockertest.Image{Config: docker.Config{Cmd: []string{"new"}}})
	told, err = pullStep(t, d, ref, spec)
	plain, _ := resource.Explain(err)
	if !strings.Contains(plain, "a rollback would have nothing to go back to") || !strings.Contains(plain, "and what runs goes on running") ||
		!strings.Contains(plain, "Give the build a tag of its own, then deploy that") || resource.ReasonOf(err) != resource.ReasonImage {
		t.Errorf("a build over what runs says %q, for the reason %q", plain, resource.ReasonOf(err))
	}
	if !told.Untouched {
		t.Errorf("it told %+v, want nothing touched", told)
	}
	if c := e.Container("web"); c.ID != running || !c.State.Running || c.Config.Cmd[0] != "old" {
		t.Errorf("after a refused build, it's %+v", c)
	}

	// Stopped, nothing runs to go on running, and what's there is still all there is.
	e.Stop("web")
	_, err = pullStep(t, d, ref, spec)
	plain, _ = resource.Explain(err)
	if !strings.Contains(plain, "Nothing was replaced. Give the build a tag of its own") || strings.Contains(plain, "goes on running") {
		t.Errorf("a build over what's stopped says %q", plain)
	}
	if c := e.Container("web"); c.ID != running {
		t.Errorf("a refused build replaced what was stopped: %+v", c)
	}
}

// What came from a registry is still waiting on one: only an image from none is
// let through, so a registry that's down, or one that has lost what it gave, still
// fails the step, with what runs left running.
func TestAPullOfAnImageFromARegistryStillFails(t *testing.T) {
	const image = "ghcr.io/acme/shop:1"
	for _, c := range []struct{ name, says string }{
		{"down", "Get https://ghcr.io/v2/: dial tcp: i/o timeout"},
		{"it lost the image", "manifest unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := dockertest.New(t)
			e.Registry[image] = dockertest.Image{Digest: "sha256:aa", Config: docker.Config{Cmd: []string{"shop"}}}
			d := &container.Driver{Runtime: &container.Docker{Client: e.Client()}}
			ref := resource.Ref{Kind: container.Kind, Name: "web"}
			spec := &resourcev1.Container{Image: image}
			if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err != nil {
				t.Fatal(err)
			}
			running := e.Container("web").ID
			e.Registry[image] = dockertest.Image{PullError: c.says}
			told, err := pullStep(t, d, ref, spec)
			if err == nil || !told.Untouched || e.Container("web").ID != running {
				t.Errorf("the pull = %v, it told %+v, and it runs %s; want it failed, with %s running", err, told, e.Container("web").ID, running)
			}
		})
	}
}

// A built image with a registry that's down can't say more than that: it stays a
// failure, as nothing says there's nothing newer.
func TestAnImageBuiltHereStillFailsWhileItsRegistryIsDown(t *testing.T) {
	e := dockertest.New(t)
	const image = "registry.acme.dev/built-here:1"
	e.AddLocalImage(image, dockertest.Image{Config: docker.Config{Cmd: []string{"app"}}})
	d := &container.Driver{Runtime: &container.Docker{Client: e.Client()}}
	ref := resource.Ref{Kind: container.Kind, Name: "web"}
	spec := &resourcev1.Container{Image: image}
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err != nil {
		t.Fatal(err)
	}
	e.Registry[image] = dockertest.Image{PullError: "Get https://registry.acme.dev/v2/: dial tcp: i/o timeout"}
	if told, err := pullStep(t, d, ref, spec); err == nil || !told.Untouched {
		t.Errorf("the pull = %v, and it told %+v; want it failed", err, told)
	}
}

// A tag can move at any moment, by a pull or a build someone runs on the server. So
// what goes on record is read from the container, not from wherever the tag has got
// to by the time the step reads it back, as a rollback goes by that digest.
func TestWhatRanIsWhatTheContainerRunsWhereverItsTagWent(t *testing.T) {
	const (
		image = "ghcr.io/acme/shop:latest"
		ran   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		moved = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	e := dockertest.New(t)
	e.Registry[image] = dockertest.Image{Digest: ran, Config: docker.Config{Cmd: []string{"shop"}}}
	d := &container.Driver{Runtime: &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: time.Second}}
	var once atomic.Bool
	e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start") && once.CompareAndSwap(false, true) {
			e.Registry[image] = dockertest.Image{Digest: moved, Config: docker.Config{Cmd: []string{"shop"}}}
			return e.Client().PullImage(r.Context(), image, nil)
		}
		return nil
	})
	var told resource.Outcome
	ctx := resource.WithOutcome(t.Context(), func(o resource.Outcome) { told.Digest = cmp.Or(o.Digest, told.Digest) })
	spec := &resourcev1.Container{Image: image}
	ref := resource.Ref{Kind: container.Kind, Name: "web"}
	if err := d.Apply(ctx, resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err != nil {
		t.Fatal(err)
	}
	if !once.Load() {
		t.Fatal("the tag never moved, so this proves nothing")
	}
	// A step reports what ran as it checks it, once it has started.
	if err := d.Verify(ctx, ref, spec); err != nil {
		t.Fatal(err)
	}
	if told.Digest != ran {
		t.Errorf("the step reported %s, want %s, which is what the container runs", told.Digest, ran)
	}
}
