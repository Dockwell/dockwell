// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"fmt"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
)

// What a pull leaves free on the disk where Docker keeps images, beside what the
// image takes: 5% of the disk, between these, as a backup leaves (ADR 0033).
const (
	minPullReserve = 256 << 20
	maxPullReserve = 2 << 30
)

// roomFor checks there's room to pull ref, an image the server hasn't got, to
// replace what runs at t. A pull of the tag it runs, which may bring nothing, is
// left to Docker.
func (d *Docker) roomFor(ctx context.Context, t Target, ref string) error {
	if d.Room == nil {
		return nil
	}
	// It's here, or Docker will say why not as it's asked for it.
	if _, err := d.Client.InspectImage(ctx, docker.WithTag(ref)); !docker.IsNotFound(err) {
		return nil
	}
	old, err := d.current(ctx, t)
	if err != nil {
		return err
	}
	return d.roomToPull(ctx, ref, old)
}

// roomToPull refuses a pull that would replace old with ref when the disk where
// Docker keeps images hasn't room for it, before anything is pulled or stopped
// (ADR 0080). Docker would fail it partway, after minutes, and could leave that
// disk too full for anything else. What ref takes isn't known before the pull,
// so it reads it as what old's image takes plus a quarter, as a new release is
// about as large as the last. With nothing to replace, an image the server has,
// or no way to read the disk, it checks nothing, and Docker's own error says it.
func (d *Docker) roomToPull(ctx context.Context, ref string, old *docker.Container) error {
	if d.Room == nil || old == nil {
		return nil
	}
	was, err := d.Client.InspectImage(ctx, old.Image)
	if err != nil || was.Size <= 0 {
		return nil //nolint:nilerr // nothing to compare with: Docker's own error says it
	}
	free, size, err := d.Room(ctx)
	if err != nil || size == 0 {
		d.logger().Warn("can't read the room on the disk where Docker keeps images, so a pull goes ahead unchecked", "error", err)
		return nil
	}
	reserve := min(max(int64(size/20), minPullReserve), maxPullReserve) //nolint:gosec // a disk's size fits
	need := was.Size + was.Size/4 + reserve
	if int64(free) >= need { //nolint:gosec // a disk's free space fits
		return nil
	}
	plain := fmt.Sprintf("This server has %s free where Docker keeps images, and pulling %s needs about %s, so nothing was pulled or stopped. Free some space, such as with docker image prune, then try again.",
		human.Size(int64(free)), ref, human.Size(need)) //nolint:gosec // a disk's free space fits
	return resource.PlainlyBecause(resource.ReasonDisk, plain,
		fmt.Errorf("not enough room to pull %s: %d bytes free, about %d needed", ref, free, need))
}
