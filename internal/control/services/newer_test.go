// SPDX-License-Identifier: AGPL-3.0-only

package services

import (
	"context"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

type next map[string]*dockwellv1.AvailableUpdate

func (n next) Next(_ context.Context, id string) (*dockwellv1.AvailableUpdate, error) {
	return n[id], nil
}

// What the keeper offers is what an update with no image takes: its image and how
// far it moves, and for a catalog install, that it upgrades the install, with no
// image of its own (ADR 0080).
func TestNewerIsWhatTheKeeperOffers(t *testing.T) {
	u := NewerFrom(next{
		"svc_db":    {Kind: dockwellv1.UpdateKind_UPDATE_KIND_PATCH, ToImage: "postgres:16.4"},
		"svc_site":  {Kind: dockwellv1.UpdateKind_UPDATE_KIND_DIGEST, ToImage: "nginx:latest"},
		"svc_notes": {Kind: dockwellv1.UpdateKind_UPDATE_KIND_RELEASE, ToImage: "ghcr.io/example/notes:2.0@sha256:22", CatalogRelease: 2},
	})
	for id, want := range map[string]Newer{
		"svc_db":    {Image: "postgres:16.4", Kind: "patch"},
		"svc_site":  {Image: "nginx:latest", Kind: "digest"},
		"svc_notes": {Image: "ghcr.io/example/notes:2.0@sha256:22", Kind: "release", Catalog: true},
	} {
		got, ok, err := u.Of(context.Background(), id)
		if err != nil || !ok || got != want {
			t.Errorf("%s: %+v, %t, %v, want %+v", id, got, ok, err, want)
		}
	}
	if _, ok, err := u.Of(context.Background(), "svc_none"); ok || err != nil {
		t.Errorf("a service with nothing newer: %t, %v", ok, err)
	}
}
