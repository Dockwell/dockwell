// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/catalogsource"
	"github.com/dockwell/dockwell/internal/store"
)

// ReleaseWait is how long a catalog release must have been on an install's source
// before an automatic update takes it, counted from when the source moved to it,
// so a bad release a catalog bumped by itself can be withdrawn before it reaches
// what runs. A person's plan takes it at once (ADR 0080, for ADR 0100).
const ReleaseWait = 3 * 24 * time.Hour

// ReasonReleaseWaits is why an automatic update's upgrade is refused for now: the
// release it would take hasn't been on its source for ReleaseWait. It's
// Unavailable, as a later look in the same window may take it.
const ReasonReleaseWaits = "release_waits"

// settingReleaseSince is the setting that holds, for a source and an entry, the
// release the source holds of it, at which of the source's revisions, and since
// when, by "<source's ID>/<entry>" after it, and "/<entry>" for the catalog built
// into Dockwell. A source removed and added again is another resource, with records
// of its own (its review, M1).
const settingReleaseSince = "catalog.release."

// walkBack is how many of a source's revisions a read looks back over, to find when
// the source moved to the release it holds. One it can't look past counts from the
// oldest it looked at, which only makes the wait longer.
const walkBack = 10

// held is a release a source holds of an entry, at one of its revisions, and since
// when.
type held struct {
	release  uint32
	revision int
	since    time.Time
}

func (h held) encode() []byte {
	return []byte(strconv.FormatUint(uint64(h.release), 10) + " " + strconv.Itoa(h.revision) + " " + h.since.UTC().Format(time.RFC3339Nano))
}

func decodeHeld(b []byte) (held, bool) {
	parts := strings.Fields(string(b))
	if len(parts) != 3 {
		return held{}, false
	}
	release, err1 := strconv.ParseUint(parts[0], 10, 32)
	revision, err2 := strconv.Atoi(parts[1])
	since, err3 := time.Parse(time.RFC3339Nano, parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return held{}, false
	}
	return held{release: uint32(release), revision: revision, since: since}, true
}

// sourceRevision is one revision of a source, as a walk back reads it.
type sourceRevision struct {
	number int
	at     time.Time
	spec   *resourcev1.CatalogSource
}

// asked is what heldSince is asked of one source's entry: the source's name, as an
// install's pin names it, empty for the catalog built into Dockwell, and the
// release it holds now.
type asked struct {
	source, entry string
	release       uint32
}

// heldSince says since when each source has held the release of an entry it holds
// now, by "<source name>/<entry>", for the releases asked: from when the source moved
// to it, which a revision of the source is (ADR 0080).
//
// A record of the same release at the source's latest revision stands. Otherwise
// the source moved since: its revisions are read back from the latest, while the
// entry's release in them is the one it holds now, to the one before the record or
// walkBack of them, and the time is the first of that run's. So a release the
// source left and came back to counts from its return, and a source removed and
// added again from its own revisions (its review, M1). The catalog built into
// Dockwell, which no revision moves, holds a release from when the control plane
// first read it. A record that can't be written is logged, and read again next time,
// which only makes the wait longer.
func (h *Handler) heldSince(ctx context.Context, list map[string]asked, now time.Time) map[string]time.Time {
	out := make(map[string]time.Time, len(list))
	if len(list) == 0 {
		return out
	}
	fresh := map[string]held{}
	for key, a := range list {
		since, rec, recKey, err := h.since(ctx, a, now)
		if err != nil {
			// What can't be read counts from now, which only makes the wait longer.
			h.logger.WarnContext(ctx, "can't read since when a catalog source has held a release, so its wait counts from now", slog.String("source", a.source), slog.String("entry", a.entry), slog.String("error", err.Error()))
			since = now
		}
		out[key] = since
		if recKey != "" {
			fresh[recKey] = rec
		}
	}
	if len(fresh) == 0 {
		return out
	}
	if err := h.db.Update(ctx, func(tx *store.Tx) error {
		for key, rec := range fresh {
			if err := tx.PutSetting(ctx, settingReleaseSince+key, rec.encode()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil && ctx.Err() == nil {
		h.logger.WarnContext(ctx, "can't record since when catalog sources have held their releases; the next read works it out again", slog.String("error", err.Error()))
	}
	return out
}

// since works out one source's entry's time, and the record to keep of it, empty
// recKey where it's up to date already.
func (h *Handler) since(ctx context.Context, a asked, now time.Time) (since time.Time, rec held, recKey string, err error) {
	var (
		was     held
		known   bool
		latest  sourceRevision
		earlier []sourceRevision // newest first, down to the record's or walkBack
	)
	err = h.db.View(ctx, func(tx *store.Tx) error {
		var r *store.Resource
		if a.source != "" {
			list, err := tx.ListKind(ctx, string(catalogsource.Kind))
			if err != nil {
				return err
			}
			for i := range list {
				if list[i].Name == a.source {
					r = &list[i]
				}
			}
			if r == nil {
				return nil
			}
			recKey = r.ID + "/" + a.entry
		} else {
			recKey = "/" + a.entry
		}
		b, err := tx.GetSetting(ctx, settingReleaseSince+recKey)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil {
			was, known = decodeHeld(b)
		}
		if r == nil {
			return nil
		}
		read := func(number int) (sourceRevision, error) {
			rev, err := tx.GetRevision(ctx, r.ID, number)
			if err != nil || rev.Spec == nil {
				return sourceRevision{number: number, at: rev.CreatedAt}, err
			}
			spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, number), rev.Spec)
			if err != nil {
				return sourceRevision{}, err
			}
			s, _ := spec.(*resourcev1.CatalogSource)
			return sourceRevision{number: number, at: rev.CreatedAt, spec: s}, nil
		}
		if latest, err = read(r.Revision); err != nil {
			return err
		}
		if known && (was.revision > r.Revision || was.revision < 1) {
			known = false
		}
		if known && was.revision == r.Revision && was.release == a.release {
			return nil
		}
		floor := max(r.Revision-walkBack, 0)
		if known {
			floor = was.revision
		}
		for number := r.Revision - 1; number > floor; number-- {
			rev, err := read(number)
			if err != nil {
				return err
			}
			earlier = append(earlier, rev)
		}
		return nil
	})
	switch {
	case err != nil || recKey == "":
		return now, held{}, "", err
	case a.source == "":
		// The catalog built into Dockwell: from its first read of the release.
		if known && was.release == a.release {
			return was.since, held{}, "", nil
		}
		return now, held{release: a.release, since: now}, recKey, nil
	case known && was.revision == latest.number && was.release == a.release:
		return was.since, held{}, "", nil
	}
	// The run of revisions back from the latest that hold this release, by commit,
	// as each commit's read is the same.
	start, stopped := latest, false
	releases := map[string]uint32{}
	for _, rev := range earlier {
		if rev.spec == nil {
			stopped = true
			break
		}
		at := rev.spec.GetRepository() + "\x00" + rev.spec.GetPath() + "\x00" + rev.spec.GetCommit()
		release, ok := releases[at]
		if !ok {
			release = h.releaseAt(ctx, a.source, rev.spec, a.entry)
			releases[at] = release
		}
		if release != a.release {
			stopped = true
			break
		}
		start = rev
	}
	since = start.at
	if !stopped && known && was.release == a.release {
		// The run reaches back to the record's revision, which held it too.
		since = was.since
	}
	if since.After(now) {
		since = now
	}
	return since, held{release: a.release, revision: latest.number, since: since}, recKey, nil
}

// releaseAt is the release a source held of an entry at a revision's spec, from
// the commit it named; 0 for none, or a commit that can't be read, which ends a
// walk back there.
func (h *Handler) releaseAt(ctx context.Context, name string, spec *resourcev1.CatalogSource, entry string) uint32 {
	got, err := h.sources.readSource(ctx, name, spec, claims{})
	if err != nil {
		return 0
	}
	for _, e := range append(got.entries, got.also...) {
		if e.GetId() == entry {
			return e.GetRelease()
		}
	}
	return 0
}
