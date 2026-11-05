// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dockwell/dockwell/internal/store"
)

// Forgotten says which of a service's logs no reader may see any more, as it was
// deleted: those of revisions up to UpTo. A service keeps its resource and its ID when
// it is made again under the same name, so it is the revision that tells what the
// deleted one left from what the new one wrote. This is worked out from the
// revisions alone, so it holds at once on every server the logs are on, whether or
// not that server can be reached (ADR 0025).
type Forgotten struct {
	UpTo int
	// At is when the deletion that put them out of reach happened.
	At time.Time
	// Deleted says the service stands deleted, rather than made again.
	Deleted bool
}

// ForgottenOf works out what a service's deletions put out of reach. Undoing a
// deletion brings it back, and so does rolling back one that failed. Rolling back a
// first deploy that failed isn't a deletion, as what it left is what its reader comes
// for, but rolling back an undo is, as the service goes back to being deleted.
func ForgottenOf(ctx context.Context, tx *store.Tx, res store.Resource) (Forgotten, error) {
	deletions, err := tx.DeletionsOf(ctx, res.ID)
	if err != nil {
		return Forgotten{}, err
	}
	var f Forgotten
	for _, d := range deletions {
		c, err := tx.GetChange(ctx, d.ChangeID)
		if err != nil {
			return Forgotten{}, err
		}
		if IsRollback(c) {
			was, err := tx.GetChange(ctx, c.Reverts)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return Forgotten{}, err
			}
			if was.Reverts == "" {
				continue
			}
		}
		if d.Number >= res.Revision {
			f = Forgotten{UpTo: d.Number, At: d.CreatedAt, Deleted: true}
			continue
		}
		after, err := tx.RevisionChange(ctx, res.ID, d.Number+1)
		if err != nil {
			return Forgotten{}, err
		}
		next, err := tx.GetChange(ctx, after)
		if err != nil {
			return Forgotten{}, err
		}
		if next.Reverts == c.ID {
			continue
		}
		f = Forgotten{UpTo: d.Number, At: d.CreatedAt}
	}
	return f, nil
}

// ForgottenService is what a kind's deletions put out of reach, for telling servers.
type ForgottenService struct {
	Resource store.Resource
	Forgotten
}

// maxForgotten bounds what one look finds, the newest kept. Past it, what the oldest
// left leaves when a server's kept logs reach their own time limit, and no reader can
// see it either way.
const maxForgotten = 10_000

// ForgottenServices lists what deletions of a kind since a time put out of reach.
func ForgottenServices(ctx context.Context, tx *store.Tx, kind string, since time.Time) ([]ForgottenService, error) {
	ids, err := tx.DeletedSince(ctx, kind, since, maxForgotten)
	if err != nil {
		return nil, err
	}
	var out []ForgottenService
	for _, id := range ids {
		res, err := tx.GetResourceByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("can't read what was deleted: %w", err)
		}
		f, err := ForgottenOf(ctx, tx, res)
		if err != nil {
			return nil, err
		}
		if f.UpTo > 0 {
			out = append(out, ForgottenService{Resource: res, Forgotten: f})
		}
	}
	return out, nil
}
