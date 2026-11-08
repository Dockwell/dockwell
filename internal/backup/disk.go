// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// What a backup leaves alone on the disk: 5% of it, between these. A server with
// a full disk stops more than its backups, so a backup never takes the last of it.
const (
	minReserve = 256 << 20
	maxReserve = 2 << 30
)

// measureWait bounds finding out what's in a volume before anything stops for it.
const measureWait = 10 * time.Second

// disk is the disk that holds backups.
type disk struct {
	free, reserve int64
	filesystem    string // tells it from the disk a volume is on
}

// NoRoom is a disk with no room for a backup, for a test of what a full one does.
func NoRoom() disk { return disk{free: 1 << 20, reserve: 1 << 30} }

// disk reads what's free where backups go. Free is what the agent can write, past
// what the filesystem keeps for root.
func (r *Runner) disk() (disk, error) {
	if r.Disk != nil {
		return r.Disk(), nil
	}
	at := r.Root
	if err := os.MkdirAll(at, 0o700); err != nil {
		return disk{}, fmt.Errorf("can't make a place for backups: %w", err)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(at, &st); err != nil {
		return disk{}, fmt.Errorf("can't read how much is free on the disk: %w", err)
	}
	size := int64(st.Blocks) * int64(st.Bsize) //nolint:gosec,unconvert // a disk's size fits, and Bsize's type differs by architecture
	return disk{
		free:       int64(st.Bavail) * int64(st.Bsize), //nolint:gosec,unconvert // as above
		reserve:    min(max(size/20, minReserve), maxReserve),
		filesystem: fmt.Sprint(st.Fsid),
	}, nil
}

// lastSize is the size of a volume's newest backup, or zero with none. A backup says
// more of the next than a volume's size does, as it's compressed.
func (r *Runner) lastSize(ref resource.Ref) int64 {
	found, err := filepath.Glob(filepath.Join(r.dir(ref), "bkp_*.json"))
	if err != nil || len(found) == 0 {
		return 0
	}
	var newest time.Time
	var size int64
	for _, path := range found {
		if m, err := readManifest(path); err == nil && m.Taken.After(newest) {
			newest, size = m.Taken, m.Size
		}
	}
	return size
}

// estimate says about how much a backup of a volume will take: what the last took,
// or what's in it before there's been one. That's an upper bound, as a backup is
// compressed. Zero means it took too long to find out.
func (r *Runner) estimate(ctx context.Context, take Take, t volume.Target) (size int64, known bool) {
	if size := r.lastSize(take.Ref); size > 0 {
		return size, true
	}
	ctx, cancel := context.WithTimeout(ctx, measureWait)
	defer cancel()
	stats, err := r.tool(ctx, t, "measure", docker.Streams{})
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			r.logger().Warn("can't measure a volume", "volume", t.Name, "error", err.Error())
		}
		return 0, false
	}
	return stats.Bytes, true
}

// margin is how far a backup may outgrow the last, as data does.
const margin = 5 // a fifth

// roomFor refuses a backup the disk has no room for, before anything stops for it.
func (r *Runner) roomFor(ctx context.Context, take Take, t volume.Target) error {
	d, err := r.disk()
	if err != nil {
		return err
	}
	size, _ := r.estimate(ctx, take, t)
	needs := size + size/margin
	if d.free-needs >= d.reserve {
		return nil
	}
	return tooFull(d, needs)
}

// keepRoom stops a backup that's about to go below what it leaves alone.
func (r *Runner) keepRoom() error {
	d, err := r.disk()
	if err != nil {
		return err
	}
	if d.free >= d.reserve {
		return nil
	}
	return tooFull(d, 0)
}

// tooFull says a backup won't fit.
func tooFull(d disk, needs int64) error {
	left := "The disk that holds backups has " + human.Size(d.free) + " free"
	if needs > 0 {
		left += ", and this backup takes about " + human.Size(needs)
	}
	return fmt.Errorf("%s, and a backup leaves %s alone, so it stopped, with nothing changed. Delete old backups, keep fewer, or free some space, then try again", left, human.Size(d.reserve))
}

// roomToRestore refuses a restore the server has no room for, before anything stops
// for it. It takes a copy of what's in the volume where backups go, and what it
// brings back has to fit in the volume: beside what's there for a copy of files, as
// that goes only once all of it is in, and in place of it for a dump. Where both are
// on one disk, it needs room for both. A volume too large to measure in time goes
// ahead, and writing the copy watches the disk as a backup does. It says nothing was
// stopped only when began says that's so.
func (r *Runner) roomToRestore(ctx context.Context, ref resource.Ref, t volume.Target, m *Manifest, began bool) error {
	refused := func(fact, fix string) error {
		if began {
			return fmt.Errorf("%s. %s", fact, fix)
		}
		return fmt.Errorf("%s, so nothing was stopped or changed. %s", fact, fix)
	}
	d, err := r.disk()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, measureWait)
	defer cancel()
	v, err := r.tool(ctx, t, "measure", docker.Streams{})
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			r.logger().Warn("can't measure a volume", "volume", t.Name, "error", err.Error())
		}
		return nil
	}
	copying := v.Bytes + v.Bytes/margin
	if copying > 0 && d.free-copying < d.reserve {
		return refused(fmt.Sprintf("the disk that holds backups has %s free, and a copy of what's in %s takes up to %s, leaving %s alone",
			human.Size(d.free), ref.Name, human.Size(copying), human.Size(d.reserve)), "Delete old backups or free some space, then try again")
	}
	bringing := m.Unpacked
	if isDump(m.Format) {
		bringing = max(bringing-v.Bytes, 0)
	}
	free, keep := v.Free, int64(0)
	if v.Filesystem == d.filesystem {
		free, keep = free-copying, d.reserve
	}
	if bringing > 0 && free-bringing < keep {
		return refused(fmt.Sprintf("the disk that holds %s has %s free once a copy of what's there is taken, and this backup needs %s more there",
			ref.Name, human.Size(max(free, 0)), human.Size(bringing)), "Free some space on that disk, then try again")
	}
	return nil
}
