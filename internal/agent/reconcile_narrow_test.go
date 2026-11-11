// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/desired"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// The cases from the narrow check of #357: a container whose image keeps data in
// a volume the agent never saw must wait, however its image came to be absent,
// unreadable or another one, and what the agent writes down must be true.

const pgData = "/var/lib/postgresql/data"

func postgres() dockertest.Image {
	return dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Volumes: []string{pgData}}
}

// unseen runs a container whose image declares a volume, and removes it before
// the agent has looked at it, so its record names no volume: what a fill of an
// agent that kept nothing leaves.
func unseen(t *testing.T) (*stand, desired.Record) {
	t.Helper()
	s := newStand(t)
	s.e.Registry["postgres:17"] = postgres()
	s.run(t, webRef, &resourcev1.Container{Image: "postgres:17"}, nil)
	rec, _, _ := s.store.Read(webRef)
	if rec.Image.Digest == "" || len(rec.Volumes) != 0 {
		t.Fatalf("the record has digest %q and volumes %v; want a digest and no volumes", rec.Image.Digest, rec.Volumes)
	}
	s.e.Remove("web")
	return s, rec
}

func (s *stand) waitsUnmade(t *testing.T, reason string) {
	t.Helper()
	if c := s.e.Container("web"); c != nil {
		t.Fatalf("made again, on %q where its image keeps data, though the agent never saw that volume", volumeAt(c, pgData))
	}
	if got := s.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, reason) {
		t.Errorf("the journal says %+v; want it waiting, with %q", got, reason)
	}
}

// The image gone too, as after docker system prune -a, which leaves volumes: the
// agent pulls it by its digest, reads where the image it pulled keeps data, and
// waits (narrow check, H-A).
func TestAnImageVolumeNeverSeenWaitsWhenItsImageIsGoneToo(t *testing.T) {
	s, rec := unseen(t)
	if err := s.e.Client().RemoveImage(t.Context(), rec.Image.ID); err != nil {
		t.Fatal(err)
	}
	s.pass(t)
	s.waitsUnmade(t, "never saw the volume")
	// And again on the next look, with the image now here.
	s.pass(t)
	s.waitsUnmade(t, "never saw the volume")
}

// The image gone and not to be had: it waits for the pull, and makes nothing.
func TestAnImageVolumeNeverSeenWaitsWhenItsImageCantBePulled(t *testing.T) {
	s, rec := unseen(t)
	if err := s.e.Client().RemoveImage(t.Context(), rec.Image.ID); err != nil {
		t.Fatal(err)
	}
	s.e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/create") {
			return errors.New("the registry doesn't answer")
		}
		return nil
	})
	s.pass(t)
	s.waitsUnmade(t, "pulling it by its digest")
}

// Docker can't read the image: nothing says where it keeps data, so it waits,
// whether the read fails as the agent checks which image the tag names or as
// the driver reads its volumes (narrow check, H-A).
func TestAnImageDockerCantReadHoldsItsContainerBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		from int // which read of an image fails first
	}{
		{"every read", 1},
		{"the driver's read, after the tag was found right", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := unseen(t)
			reads := 0
			s.e.Intercept(func(r *http.Request) error {
				if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json") {
					reads++
					if reads >= tc.from {
						return errors.New("layer store is busy")
					}
				}
				return nil
			})
			s.pass(t)
			if reads < tc.from {
				t.Fatalf("the image was read %d times; the test wants at least %d", reads, tc.from)
			}
			s.waitsUnmade(t, "Docker can't say")
		})
	}
}

// One with no volume to lose is still held back when Docker can't read its
// image: the agent can't know it has none.
func TestAnImageDockerCantReadHoldsBackOneWithNoVolumesToo(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.e.Remove("web")
	reads := 0
	s.e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json") {
			reads++
			if reads >= 2 {
				return errors.New("layer store is busy")
			}
		}
		return nil
	})
	s.pass(t)
	if s.e.Container("web") != nil {
		t.Fatal("made again without knowing where its image keeps data")
	}
	s.e.Intercept(nil)
	s.pass(t)
	if s.e.Container("web") == nil {
		t.Fatalf("not made again once Docker could read its image: %+v", s.said(webRef))
	}
}

// The tag moved to an image that keeps no data in a volume: the agent moves the
// tag back first, and reads the volumes of the image its record names, not of
// the one the tag named (narrow check, H-A).
func TestAMovedTagDoesntHideWhereTheImageKeepsData(t *testing.T) {
	s, rec := unseen(t)
	s.e.Registry["postgres:17"] = dockertest.Image{Digest: "sha256:" + strings.Repeat("7e", 32), Config: docker.Config{Cmd: []string{"other"}}}
	if err := s.e.Client().PullImage(t.Context(), "postgres:17", nil); err != nil {
		t.Fatal(err)
	}
	if img, _ := s.e.Client().InspectImage(t.Context(), "postgres:17"); img.ID == rec.Image.ID {
		t.Fatal("the tag didn't move")
	}
	s.pass(t)
	s.waitsUnmade(t, "never saw the volume")
	if img, _ := s.e.Client().InspectImage(t.Context(), "postgres:17"); img.ID != rec.Image.ID {
		t.Errorf("the tag names %s; want it moved back to %s before the volumes were read", img.ID, rec.Image.ID)
	}
}

// The other way round: the tag moved to an image that declares a volume, and the
// one the record names declares none, so there is nothing to lose and it's made.
func TestAMovedTagsVolumesArentTakenForTheRecords(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	rec, _, _ := s.store.Read(webRef)
	s.e.Remove("web")
	moved := postgres()
	moved.Digest = "sha256:" + strings.Repeat("7e", 32)
	s.e.Registry["nginx:1.27"] = moved
	if err := s.e.Client().PullImage(t.Context(), "nginx:1.27", nil); err != nil {
		t.Fatal(err)
	}
	s.pass(t)
	if c := s.e.Container("web"); c == nil || c.Image != rec.Image.ID {
		t.Fatalf("made again as %+v, with the journal at %+v; want it made from %s", c, s.said(webRef), rec.Image.ID)
	}
}

// A pull that takes longer than a look has: the tag is still put on what was
// pulled, on a deadline of its own, and the container made (narrow check, M-B).
func TestATagAfterALongPullHasItsOwnDeadline(t *testing.T) {
	was := lookTimeout
	lookTimeout = 400 * time.Millisecond
	t.Cleanup(func() { lookTimeout = was })
	s := newStand(t)
	img := s.e.Registry["nginx:1.27"]
	img.Size, img.PullTime = 1<<20, 900*time.Millisecond // the fake engine takes its time over an image with a size
	s.e.Registry["nginx:1.27"] = img
	s.run(t, webRef, nginx, nil)
	rec, _, _ := s.store.Read(webRef)
	s.e.Remove("web")
	if err := s.e.Client().RemoveImage(t.Context(), rec.Image.ID); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	s.pass(t)
	if took := time.Since(began); took < lookTimeout {
		t.Fatalf("the pull took %s, within a look's %s, so this shows nothing", took, lookTimeout)
	}
	if s.e.Container("web") == nil {
		t.Fatalf("not made again after a pull longer than a look: %+v", s.said(webRef))
	}
	if got := s.said(webRef); got.What != desired.Made || !strings.Contains(got.Reason, "Pulled its image by its digest") {
		t.Errorf("the journal says %+v", got)
	}
}

// A start that fails is written down once, as what it was. Nothing later says
// the agent stopped during it, which it didn't (narrow check, M-A).
func TestAFailedStartIsntTakenForOneTheAgentStoppedIn(t *testing.T) {
	s := newStand(t)
	now := time.Now()
	s.r.now = func() time.Time { return now }
	s.run(t, webRef, nginx, nil)
	s.e.FailedToStart("web", "driver failed programming external connectivity: port is already allocated")
	s.e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start") {
			return errors.New("port is already allocated")
		}
		return nil
	})
	for range 3 {
		s.r.recoverActions(t.Context())
		s.pass(t)
		if doing := s.journal.Doing(); len(doing) != 0 {
			t.Fatalf("a failed start left its receipt pending: %+v", doing)
		}
		now = now.Add(lastRetry)
	}
	s.r.recoverActions(t.Context())
	entries, _, _ := s.journal.Pending()
	tried := 0
	for _, e := range entries {
		if strings.Contains(e.Reason, "The agent stopped during") {
			t.Errorf("the journal says the agent stopped, which it didn't: %+v", e)
		}
		if e.What == desired.Waiting && strings.Contains(e.Reason, "nor could the agent") {
			tried++
		}
	}
	if tried != 3 {
		t.Errorf("the journal has %d failed starts, want 3: %+v", tried, entries)
	}
	// Once it can start, it does, and says so.
	s.e.Intercept(nil)
	s.pass(t)
	if c := s.e.Container("web"); c == nil || !c.State.Running {
		t.Fatalf("not started once it could be: %+v", s.said(webRef))
	}
	if got := s.said(webRef); got.What != desired.Started {
		t.Errorf("the journal says %+v", got)
	}
}

// A making that fails with nothing made ends its receipt with why. When the
// control plane then applies the resource, nothing says the agent stopped during
// a creation (narrow check, M-A).
func TestAFailedMakeIsntTakenForOneTheAgentStoppedIn(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.e.Remove("web")
	s.e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create") {
			return errors.New("no space left on device")
		}
		return nil
	})
	s.pass(t)
	if got := s.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, "Making it again failed") {
		t.Fatalf("the journal says %+v", got)
	}
	if doing := s.journal.Doing(); len(doing) != 0 {
		t.Fatalf("a making that made nothing left its receipt pending: %+v", doing)
	}
	// The control plane comes back and applies it.
	s.e.Intercept(nil)
	s.run(t, webRef, nginx, nil)
	s.r.recoverActions(t.Context())
	s.pass(t)
	entries, _, _ := s.journal.Pending()
	for _, e := range entries {
		if strings.Contains(e.Reason, "The agent stopped during") {
			t.Errorf("the journal says the agent stopped, which it didn't: %+v", e)
		}
	}
}
