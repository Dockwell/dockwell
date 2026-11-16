// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/resource/catalogsource"
	"github.com/dockwell/dockwell/internal/store"
)

// settingReleaseSince is the setting that holds, for a source and an entry, the
// release the source holds of it and since when, by "<source>/<entry>" after it:
// the catalog built into Dockwell is the source with no name. An automatic update
// takes a release only once its source has held it for 3 days (ADR 0080).
const settingReleaseSince = "catalog.release."

// held is a release a source holds of an entry, and since when.
type held struct {
	release uint32
	since   time.Time
}

func (h held) encode() []byte {
	return []byte(strconv.FormatUint(uint64(h.release), 10) + " " + h.since.UTC().Format(time.RFC3339Nano))
}

func decodeHeld(b []byte) (held, bool) {
	release, at, ok := strings.Cut(string(b), " ")
	n, err := strconv.ParseUint(release, 10, 32)
	if !ok || err != nil {
		return held{}, false
	}
	since, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return held{}, false
	}
	return held{release: uint32(n), since: since}, true
}

// heldSince says since when each source has held the release of an entry it holds
// now, by "<source>/<entry>", for the releases asked: as recorded where it's the
// same release, and otherwise from when the source moved to the commit it's at,
// its latest revision, which it records. A source that moves an entry again starts
// its time again, as an update takes only what the source holds now. The catalog
// built into Dockwell, which no revision moves, holds a release from when the
// control plane first read it. A later revision of a source that left the entry as
// it was, as one that moved another entry, is never read here: the record stands.
func (h *Handler) heldSince(ctx context.Context, asked map[string]uint32) (map[string]time.Time, error) {
	out := make(map[string]time.Time, len(asked))
	if len(asked) == 0 {
		return out, nil
	}
	now := h.sources.now()
	fresh := map[string]held{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		moved := map[string]time.Time{}
		list, err := tx.ListKind(ctx, string(catalogsource.Kind))
		if err != nil {
			return err
		}
		for i := range list {
			r := &list[i]
			rev, err := tx.GetRevision(ctx, r.ID, r.Revision)
			if err != nil {
				return err
			}
			moved[r.Name] = rev.CreatedAt
		}
		for key, release := range asked {
			b, err := tx.GetSetting(ctx, settingReleaseSince+key)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if was, ok := decodeHeld(b); err == nil && ok && was.release == release {
				out[key] = was.since
				continue
			}
			source, _, _ := strings.Cut(key, "/")
			at, ok := moved[source]
			if !ok || at.After(now) {
				at = now
			}
			fresh[key] = held{release: release, since: at}
			out[key] = at
		}
		return nil
	})
	if err != nil || len(fresh) == 0 {
		return out, err
	}
	return out, h.db.Update(ctx, func(tx *store.Tx) error {
		for key, rec := range fresh {
			if err := tx.PutSetting(ctx, settingReleaseSince+key, rec.encode()); err != nil {
				return err
			}
		}
		return nil
	})
}
