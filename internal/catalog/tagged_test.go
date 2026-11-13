// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

const pgDigest = "@sha256:748a18a54d3842d43bc156f0840d78029b8d349e0feca98c12b9c4ce3404a6b3"

// tagged is the built-in postgres entry as the control plane converts another
// store's: its image named by tag.
func tagged(t *testing.T) *catalogv1.Entry {
	t.Helper()
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := c.Entry("postgres")
	e := proto.CloneOf(base)
	if !strings.Contains(e.GetCompose(), pgDigest) {
		t.Fatal("postgres's entry moved on from the digest this test knows")
	}
	e.Compose = catalog.Unpin(e.GetCompose())
	return e
}

// An entry converted on the control plane names its images by a version's tag,
// written plainly, which CheckTagged takes and Check doesn't (ADR 0078).
func TestCheckTaggedTakesAVersionsTag(t *testing.T) {
	e := tagged(t)
	if err := catalog.Check(e); err == nil || !strings.Contains(err.Error(), "isn't pinned by digest") {
		t.Errorf("Check = %v, want it to refuse a tag alone", err)
	}
	if err := catalog.CheckTagged(e); err != nil {
		t.Errorf("CheckTagged = %v", err)
	}
	if !catalog.Tagged(e) {
		t.Error("Tagged says it's pinned")
	}
	image := catalog.TaggedImages(e.GetCompose())
	if len(image) == 0 || !strings.HasPrefix(image[0], "postgres:") {
		t.Fatalf("TaggedImages = %v", image)
	}
	// A tag that moves is pinned when an install is planned too, and its newer builds
	// are updates (ADR 0085).
	moving := proto.CloneOf(e)
	moving.Compose = strings.Replace(e.GetCompose(), "image: "+image[0], "image: postgres:latest", 1)
	if err := catalog.CheckTagged(moving); err != nil {
		t.Errorf("CheckTagged refused a moving tag: %v", err)
	}
	for name, edit := range map[string]func(string) string{
		"no tag": func(s string) string { return strings.Replace(s, "image: "+image[0], "image: postgres", 1) },
		"quoted": func(s string) string { return strings.Replace(s, "image: "+image[0], `image: "`+image[0]+`"`, 1) },
		"a comment": func(s string) string {
			return strings.Replace(s, "image: "+image[0], "image: "+image[0]+" # pinned later", 1)
		},
		"a registry": func(s string) string { return strings.Replace(s, "image: "+image[0], "image: registry:5000/pg", 1) },
		"two in a row": func(s string) string {
			return strings.Replace(s, "image: "+image[0], "image: "+image[0]+" "+image[0], 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := proto.CloneOf(e)
			bad.Compose = edit(e.GetCompose())
			if err := catalog.CheckTagged(bad); err == nil {
				t.Error("CheckTagged took it")
			}
		})
	}
}

// An install pins a tagged entry's images in its own file; an upgrade reads past the
// digests it chose, so only a change to the entry's file offers one.
func TestPinsOfATaggedEntry(t *testing.T) {
	e := tagged(t)
	images := catalog.TaggedImages(e.GetCompose())
	image := images[0]
	digest := strings.TrimPrefix(pgDigest, "@")
	digests := map[string]string{}
	for _, i := range images {
		digests[i] = digest
	}
	pinned := catalog.Pin(e.GetCompose(), digests)
	if !strings.Contains(pinned, "image: "+image+pgDigest+"\n") || catalog.Unpin(pinned) != e.GetCompose() || catalog.Tagged(&catalogv1.Entry{Compose: pinned}) {
		t.Errorf("Pin and Unpin don't round trip:\n%s", pinned)
	}
	if got := catalog.PinnedDigests(pinned); !maps.Equal(got, digests) {
		t.Errorf("PinnedDigests = %v", got)
	}
	pin := &resourcev1.CatalogPin{Release: e.GetRelease(), File: []byte(pinned)}
	newer := proto.CloneOf(e)
	newer.Release++
	if catalog.Upgrades(newer, pin) {
		t.Error("a higher release with the same file offers an upgrade, as the digest the install pinned reads as a change")
	}
	newer.Compose = strings.Replace(newer.GetCompose(), image, image+".1", 1)
	if !catalog.Upgrades(newer, pin) {
		t.Error("a higher release that moves the tag offers no upgrade")
	}
}

// A tag that moves names no version: latest, stable and the rest, and an image named
// with no tag, which Docker pulls as latest, with or without the digest an install
// pinned it to (ADR 0085).
func TestMoving(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	for image, want := range map[string]bool{
		"nginx": true, "nginx:latest": true, "nginx:latest" + digest: true, "acme/web:stable": true, "registry:5000/web": true,
		"nginx" + digest: false, "nginx:1.27": false, "nginx:1.27" + digest: false, "registry:5000/web:1.0": false, "acme/web:16-alpine": false,
	} {
		if got := catalog.Moving(image); got != want {
			t.Errorf("Moving(%q) = %v, want %v", image, got, want)
		}
	}
	compose := "services:\n  web:\n    image: nginx:latest\n  db:\n    image: postgres:16.4\n  cache:\n    image: redis:stable" + digest + "\n"
	if got := catalog.MovingImages(compose); !slices.Equal(got, []string{"nginx:latest", "redis:stable"}) {
		t.Errorf("MovingImages = %v", got)
	}
}
