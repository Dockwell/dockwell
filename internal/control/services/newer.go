// SPDX-License-Identifier: AGPL-3.0-only

package services

import (
	"context"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Nexter is ADR 0079's record of what's newer than each service runs, as
// serviceupdates.Keeper keeps it.
type Nexter interface {
	// Next is the update a service is offered: the first that a person neither
	// skipped nor set aside with a snooze that still runs, or nil for none.
	Next(ctx context.Context, serviceID string) (*dockwellv1.AvailableUpdate, error)
}

// NewerFrom reads what newer image each service has from ADR 0079's record, for
// an update that names none and for automatic updates (ADR 0080). A version a
// person skipped, and a service they snoozed, have none.
func NewerFrom(n Nexter) Updates { return nexter{n} }

type nexter struct{ n Nexter }

// kinds are how far each kind of update moves, as Newer.Kind says it.
var kinds = map[dockwellv1.UpdateKind]string{
	dockwellv1.UpdateKind_UPDATE_KIND_PATCH:   "patch",
	dockwellv1.UpdateKind_UPDATE_KIND_MINOR:   "minor",
	dockwellv1.UpdateKind_UPDATE_KIND_MAJOR:   "major",
	dockwellv1.UpdateKind_UPDATE_KIND_DIGEST:  "digest",
	dockwellv1.UpdateKind_UPDATE_KIND_RELEASE: "release",
	dockwellv1.UpdateKind_UPDATE_KIND_COMMIT:  "commit",
}

func (x nexter) Of(ctx context.Context, serviceID string) (Newer, bool, error) {
	a, err := x.n.Next(ctx, serviceID)
	if err != nil || a == nil {
		return Newer{}, false, err
	}
	out := Newer{Image: a.GetToImage(), Kind: kinds[a.GetKind()], Catalog: a.GetCatalogRelease() > 0}
	return out, out.Catalog || out.Image != "", nil
}
