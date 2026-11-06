// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// registryBench is an engine whose registry asks are counted, by image, with a
// clock the test moves.
type registryBench struct {
	e     *dockertest.Engine
	l     resource.Local
	mu    sync.Mutex
	asked map[string]int
	now   time.Time
	// hang holds up an ask for any image it holds, until its caller gives up.
	hang func(ref string) bool
	rt   *container.Docker
}

func newRegistryBench(t *testing.T) *registryBench {
	t.Helper()
	b := &registryBench{e: dockertest.New(t), asked: map[string]int{}, now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	b.rt = &container.Docker{Client: b.e.Client(), Settle: 1, Poll: 1, Soak: -1, Now: func() time.Time {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.now
	}}
	b.l = resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: b.rt}})}
	b.e.Intercept(func(r *http.Request) error {
		_, ref, ok := strings.Cut(r.URL.Path, "/distribution/")
		if !ok {
			return nil
		}
		ref = strings.TrimSuffix(ref, "/json")
		b.mu.Lock()
		b.asked[ref]++
		hang := b.hang != nil && b.hang(ref)
		b.mu.Unlock()
		if hang {
			<-r.Context().Done()
		}
		return nil
	})
	return b
}

func (b *registryBench) times(ref string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.asked[ref]
}

func (b *registryBench) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, v := range b.asked {
		n += v
	}
	return n
}

func (b *registryBench) wait(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now = b.now.Add(d)
}

func (b *registryBench) plan(ctx context.Context, images ...string) ([]resource.Step, error) {
	return b.planned(ctx, false, images...)
}

// planned plans a service of each image, each one someone asked to run, as a deploy
// or a rollback does, or each only part of a file.
func (b *registryBench) planned(ctx context.Context, run bool, images ...string) ([]resource.Step, error) {
	var desired []resource.Desired
	for i, image := range images {
		desired = append(desired, resource.Desired{
			Ref:  resource.Ref{Kind: service.Kind, Scope: shapesEnv, Name: fmt.Sprintf("s%d", i)},
			Spec: svc(&resourcev1.Container{Image: image}), Run: run,
		})
	}
	return b.l.Plan(ctx, desired)
}

// Docker Hub counts the manifest GET Docker's /distribution makes as a pull, so the
// plan a change is proposed with asks for an image the server hasn't got, and the
// plan it is applied with doesn't: proposing and applying cost what pulling does,
// once, beside the pull itself. An answer is kept until the server pulls the image,
// and for 15 minutes at most. An answer that a registry won't give it isn't kept,
// as it costs no pull, and pushing the image then planning again has to work. The
// last check of #92 found three pulls for one apply.
func TestAPlanAsksForAnImageOnceUntilItIsPulled(t *testing.T) {
	b := newRegistryBench(t)
	const image = "acme/shop:1.4"
	shop := "sha256:" + strings.Repeat("5", 64)
	b.e.Registry[image] = dockertest.Image{Digest: shop, Config: docker.Config{Cmd: []string{"shop"}}}
	steps, err := b.plan(t.Context(), image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.plan(t.Context(), image); err != nil {
		t.Fatal(err)
	}
	if got := b.times(image); got != 1 {
		t.Errorf("proposing and planning again asked the registry %d times, want once", got)
	}
	if err := b.l.Apply(t.Context(), steps[0], svc(&resourcev1.Container{Image: image})); err != nil {
		t.Fatal(err)
	}
	if b.e.Pulls != 1 {
		t.Errorf("it pulled %d times, want once", b.e.Pulls)
	}

	// Once pulled, what the registry said is forgotten, so a plan after the image is
	// gone, from the server and the registry, finds out.
	if err := b.e.Client().RemoveContainer(t.Context(), b.e.Container(service.ContainerName(resource.Ref{Scope: shapesEnv, Name: "s0"})).ID, true); err != nil {
		t.Fatal(err)
	}
	b.e.Forget(shop)
	if _, err := b.plan(t.Context(), image); err == nil || !strings.Contains(err.Error(), "its registry won't give it to a pull") {
		t.Errorf("planning an image that is gone since it was pulled = %v, want it refused", err)
	}

	// A refusal isn't kept: pushed, it plans.
	const missing = "acme/later:1"
	for range 2 {
		if _, err := b.plan(t.Context(), missing); err == nil {
			t.Fatal("an image no registry has plans")
		}
	}
	if got := b.times(missing); got != 2 {
		t.Errorf("two plans of an image a registry hasn't got asked %d times, want each to ask", got)
	}
	b.e.Registry[missing] = dockertest.Image{Config: docker.Config{Cmd: []string{"later"}}}
	if _, err := b.plan(t.Context(), missing); err != nil {
		t.Errorf("once pushed, it plans = %v", err)
	}

	// An answer goes stale.
	const other = "acme/other:2"
	b.e.Registry[other] = dockertest.Image{Config: docker.Config{Cmd: []string{"other"}}}
	for range 2 {
		if _, err := b.plan(t.Context(), other); err != nil {
			t.Fatal(err)
		}
	}
	b.wait(16 * time.Minute)
	if _, err := b.plan(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if got := b.times(other); got != 2 {
		t.Errorf("planning within 15 minutes, then after, asked %d times, want 2", got)
	}
}

// One file from a token could spend a server's whole allowance of Docker Hub's
// pulls on plans alone, so a plan asks Docker Hub 20 times at most in 6 hours: a
// fifth of what an address that doesn't sign in may pull. Past that, a plan says
// nothing of an image there, and the pull finds out. Other registries don't count
// against it. The last check of #92 found nothing bounding it.
func TestPlansSpendABoundedShareOfDockerHubsPulls(t *testing.T) {
	b := newRegistryBench(t)
	var images []string
	for i := range 30 {
		image := fmt.Sprintf("acme/app%d:1", i)
		b.e.Registry[image] = dockertest.Image{Config: docker.Config{Cmd: []string{"app"}}}
		images = append(images, image)
	}
	if _, err := b.plan(t.Context(), images...); err != nil {
		t.Fatal(err)
	}
	if got := b.total(); got != 20 {
		t.Errorf("planning 30 images on Docker Hub asked it %d times, want 20", got)
	}
	// Past the budget, an image no one has isn't asked about, so it plans.
	if _, err := b.plan(t.Context(), "acme/no-such:1"); err != nil {
		t.Errorf("past the budget, an image no one has = %v, want it planned", err)
	}
	if _, err := b.plan(t.Context(), "ghcr.io/acme/no-such:1"); err == nil {
		t.Error("an image no one has on another registry plans, want it asked about and refused")
	}
	if _, err := b.plan(t.Context(), "docker.io/library/no-such:1"); err != nil {
		t.Errorf("Docker Hub by its full name is Docker Hub, past the budget too: %v", err)
	}
	b.wait(6*time.Hour + time.Minute)
	if _, err := b.plan(t.Context(), "acme/no-such:1"); err == nil {
		t.Error("6 hours later, an image no one has plans, want it asked about again")
	}
}

// Docker pings each endpoint it could pull from, so a registry it can't reach costs
// a plan the whole wait for each image. One that doesn't answer isn't asked again
// for 5 minutes, and a plan waits on registries for half the time it had at most, in
// all, so it never runs out of time waiting: the last check of #92 had seven images
// fail a plan at its minute, and the review of #135 found half of what was left, for
// each ask, took 99.4% of a plan, and that ten registries failed it.
func TestARegistryThatDoesntAnswerCostsAPlanOneWait(t *testing.T) {
	b := newRegistryBench(t)
	b.hang = func(ref string) bool { return true }
	var images []string
	for i := range 7 {
		images = append(images, fmt.Sprintf("acme/app%d:1", i))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := b.plan(ctx, images...); err != nil {
		t.Fatalf("planning with Docker Hub silent = %v, want it planned, saying nothing of the images", err)
	}
	if got := b.total(); got != 1 {
		t.Errorf("it asked a silent registry %d times, want once", got)
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("planning took %s, want at most half of its 2 s", took)
	}

	// Many registries, each silent, wait no longer than one does.
	var many []string
	for i := range 12 {
		many = append(many, fmt.Sprintf("registry%d.example:5000/app:1", i))
	}
	ctx, cancel = context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start = time.Now()
	if _, err := b.plan(ctx, many...); err != nil {
		t.Errorf("planning with twelve silent registries = %v, want it planned", err)
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("planning with twelve silent registries took %s, want at most half of its 2 s", took)
	}

	// Silence is forgotten.
	b.mu.Lock()
	b.hang = nil
	b.mu.Unlock()
	b.wait(6 * time.Minute)
	if _, err := b.plan(t.Context(), "acme/no-such:1"); err == nil {
		t.Error("once Docker Hub answers again, an image no one has plans, want it refused")
	}

	// What a file names doesn't pile up: a registry silent now takes the place of
	// those that were.
	b.mu.Lock()
	b.hang = func(string) bool { return true }
	b.mu.Unlock()
	ctx, cancel = context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := b.plan(ctx, "registry.example:5000/app:1"); err != nil {
		t.Fatal(err)
	}
	if got := b.rt.SilentRegistries(); got != 1 {
		t.Errorf("the agent holds %d silent registries, want the one still silent", got)
	}
}

// A plan that didn't ask for an image knows nothing of it, and mustn't read that as
// the image being there. The review of #135 found a rollback to a digest that's gone
// kept its pin once Docker Hub's limit was spent, or in a registry's silence, and could
// only fail. Those limits are for files. A deploy or a rollback names one image, so it
// asks past them, and a file's image still goes unasked.
func TestARollbackAsksForItsDigestPastTheLimits(t *testing.T) {
	const gone = "acme/app:1@sha256:0000000000000000000000000000000000000000000000000000000000000001"
	unasked := func(t *testing.T, b *registryBench, ref string) {
		t.Helper()
		before := b.times(ref)
		steps, err := b.plan(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if b.times(ref) != before || steps[0].ImageGone {
			t.Errorf("a file's image was asked for %d times more, and reads as gone: %t, want no ask, saying nothing", b.times(ref)-before, steps[0].ImageGone)
		}
	}
	asks := func(t *testing.T, b *registryBench, ref string) {
		t.Helper()
		before := b.times(ref)
		steps, err := b.planned(t.Context(), true, ref)
		if err != nil {
			t.Fatal(err)
		}
		if b.times(ref) != before+1 || !steps[0].ImageGone {
			t.Errorf("a rollback asked %d times, and its digest reads as gone: %t, want one ask, and gone", b.times(ref)-before, steps[0].ImageGone)
		}
	}

	t.Run("with Docker Hub's limit spent", func(t *testing.T) {
		b := newRegistryBench(t)
		for i := range 20 {
			if _, err := b.plan(t.Context(), fmt.Sprintf("acme/spent%d:1", i)); err == nil {
				t.Fatal("an image no one has planned")
			}
		}
		unasked(t, b, gone)
		asks(t, b, gone)
	})

	t.Run("in a registry's silence", func(t *testing.T) {
		b := newRegistryBench(t)
		b.mu.Lock()
		b.hang = func(string) bool { return true }
		b.mu.Unlock()
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		if _, err := b.plan(ctx, "acme/silent:1"); err != nil {
			t.Fatal(err)
		}
		b.mu.Lock()
		b.hang = nil
		b.mu.Unlock()
		unasked(t, b, gone)
		asks(t, b, gone)
	})

	// With a plan's time for registries used up, nothing can be said, so one someone
	// asked for is refused rather than planned on a guess.
	t.Run("with a plan's time used up", func(t *testing.T) {
		b := newRegistryBench(t)
		b.mu.Lock()
		b.hang = func(ref string) bool { return !strings.Contains(ref, "sha256") }
		b.mu.Unlock()
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		_, err := b.planned(ctx, true, "acme/slow:1", gone)
		if err == nil || !strings.Contains(err.Error(), "Plan it again") {
			t.Errorf("a pin no one could ask for = %v, want it refused, saying to plan again", err)
		}
	})

	// A registry asked that says nothing leaves a pin as it is, as a rollback has
	// nothing better to go on, and the pull finds out.
	t.Run("from a registry that says nothing", func(t *testing.T) {
		b := newRegistryBench(t)
		b.mu.Lock()
		b.hang = func(string) bool { return true }
		b.mu.Unlock()
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		steps, err := b.planned(ctx, true, gone)
		if err != nil {
			t.Fatal(err)
		}
		if steps[0].ImageGone {
			t.Error("a registry that said nothing reads as saying the image is gone")
		}
	})
}
