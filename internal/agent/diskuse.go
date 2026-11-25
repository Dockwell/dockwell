// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
)

// diskUseFor is how long an answer stands, so a page opened twice, or by two
// people, asks Docker once.
const diskUseFor = time.Minute

// diskUseWait bounds one answer. Docker adds up every volume to answer, which
// takes a while on a big disk. A test shortens it.
var diskUseWait = 60 * time.Second

// infoWait bounds asking Docker where it keeps its data, which a Docker that
// answers at all answers at once. A test shortens it.
var infoWait = 5 * time.Second

var errDiskMeasuring = resource.PlainlyBecause(resource.ReasonDiskMeasuring, "Measuring Docker disk usage", errors.New("the Docker disk count is still running"))

// diskUse adds up what takes the room on the disk that holds Docker's data, only
// when asked (ADR 0040). It asks Docker once at a time, as Docker refuses a
// second count while one runs, and keeps the answer for diskUseFor.
type diskUse struct {
	lifetime             context.Context
	docker               *docker.Client
	collector            *collect.Collector
	cleanupMu            sync.Mutex
	lifecycleMu          sync.Mutex      // serializes lifecycle receipt replays across connections
	imagePulls           map[string]int  // exact image IDs in staging, guarded by cleanupMu
	failedCleanupChanges map[string]bool // changes whose cleanup journal write failed, guarded by cleanupMu
	// logs reads Docker's data directory, for its containers' logs. Nil reads the
	// real one.
	logs func(root string) fs.FS
	now  func() time.Time

	mu                         sync.Mutex
	measured                   uint64
	generation                 uint64 // invalidates an in-flight count after an image action
	last                       *dockwellv1.ServerDiskUse
	lastDF                     *docker.DiskUsage
	at                         time.Time
	running                    *diskMeasurement
	expiry                     *time.Timer
	err                        error // what the last count failed with
	previewCount, previewError bool
	errAt                      time.Time // when it did, so a count that failed isn't tried again for diskUseFor
}

// read answers a disk use call.
func (d *diskUse) read(ctx context.Context) (*dockwellv1.ServerDiskUse, error) {
	use, _, err := d.snapshot(ctx, false)
	return use, err
}

// snapshot returns the measurement and its immutable Engine rows from the same
// count. A fresh read waits for a count already in flight before starting its own.
func (d *diskUse) snapshot(ctx context.Context, fresh bool) (*dockwellv1.ServerDiskUse, *docker.DiskUsage, error) {
	return d.snapshotMode(ctx, fresh, false)
}

func (d *diskUse) invalidateImages() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.generation++
	d.last, d.lastDF, d.err = nil, nil, nil
}

// markFailedCleanupChange records a change whose cleanup journal write failed
// so a replay cannot delete an unrecorded target. Caller holds cleanupMu.
func (d *diskUse) markFailedCleanupChange(changeID string) {
	if d == nil || changeID == "" {
		return
	}
	if len(d.failedCleanupChanges) >= 1000 {
		clear(d.failedCleanupChanges)
	}
	if d.failedCleanupChanges == nil {
		d.failedCleanupChanges = map[string]bool{}
	}
	d.failedCleanupChanges[changeID] = true
}

// isFailedCleanupChange reports whether an earlier apply of changeID failed
// while writing its cleanup receipt journal. Caller holds cleanupMu.
func (d *diskUse) isFailedCleanupChange(changeID string) bool {
	return d != nil && d.failedCleanupChanges[changeID]
}

// cached starts a shared count when needed and immediately returns its latest
// result. This keeps disk accounting off the inventory RPC's critical path.
func (d *diskUse) cached(ctx context.Context) (*dockwellv1.ServerDiskUse, *docker.DiskUsage, error) {
	if d == nil || d.docker == nil {
		return nil, nil, errDockerInventory
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	use, df, err, ok := d.recent(true)
	if ok {
		return use, df, err
	}
	if d.running == nil {
		d.measure(ctx, true)
	}
	return nil, nil, errDiskMeasuring
}

func (d *diskUse) measurement() uint64 {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.measured
}

// recent is called with the mutex held. A preview needs a bounded count.
func (d *diskUse) recent(preview bool) (*dockwellv1.ServerDiskUse, *docker.DiskUsage, error, bool) {
	now := d.now()
	if (!preview || d.previewCount) && d.last != nil && d.lastDF != nil && now.Sub(d.at) < diskUseFor {
		return d.last, d.lastDF, nil, true
	}
	if preview == d.previewError && d.err != nil && now.Sub(d.errAt) < diskUseFor {
		return nil, nil, d.err, true
	}
	return nil, nil, nil, false
}

func (d *diskUse) snapshotMode(ctx context.Context, fresh, preview bool) (*dockwellv1.ServerDiskUse, *docker.DiskUsage, error) {
	if d == nil || d.docker == nil {
		return nil, nil, errors.New("this agent can't read Docker's disk use, as it has no Docker driver")
	}
	for {
		d.mu.Lock()
		if !fresh {
			if use, df, err, ok := d.recent(preview); ok {
				d.mu.Unlock()
				return proto.CloneOf(use), df, err
			}
		}
		if run := d.running; run != nil {
			d.mu.Unlock()
			select {
			case <-run.done:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			continue
		}
		run := d.measure(ctx, preview)
		d.mu.Unlock()
		select {
		case <-run.done:
			if run.invalidated {
				continue
			}
			return proto.CloneOf(run.use), run.df, run.err
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

type diskMeasurement struct {
	done        chan struct{}
	use         *dockwellv1.ServerDiskUse
	df          *docker.DiskUsage
	err         error
	invalidated bool
}

// measure starts one count with the mutex held. A view leaving doesn't cancel
// work other views are awaiting, but the agent's shutdown does.
func (d *diskUse) measure(ctx context.Context, preview bool) *diskMeasurement {
	run := &diskMeasurement{done: make(chan struct{})}
	d.running = run
	started := d.generation
	lifetime := d.lifetime
	if lifetime == nil {
		lifetime = context.WithoutCancel(ctx)
	}
	go func() {
		count, cancel := context.WithTimeout(lifetime, diskUseWait)
		defer cancel()
		run.use, run.df, run.err = d.count(count, preview)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.running = nil
		defer close(run.done)
		if started != d.generation {
			run.invalidated = true
			return
		}
		d.err, d.errAt, d.previewError = run.err, d.now(), preview
		d.measured++
		if run.err == nil {
			d.last, d.lastDF, d.at, d.previewCount = run.use, run.df, d.now(), preview
			if d.expiry != nil {
				d.expiry.Stop()
			}
			d.expiry = time.AfterFunc(diskUseFor, func() {
				d.mu.Lock()
				defer d.mu.Unlock()
				if d.last == run.use {
					d.last, d.lastDF = nil, nil
				}
			})
		}
	}()
	return run
}

// count asks Docker, and adds up what its data takes on its disk.
func (d *diskUse) count(ctx context.Context, preview bool) (*dockwellv1.ServerDiskUse, *docker.DiskUsage, error) {
	asked, cancel := context.WithTimeout(ctx, infoWait)
	info, err := d.docker.Info(asked)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, fmt.Errorf("its Docker didn't say where it keeps its data within %s. Check that Docker is running with systemctl status docker", human.Duration(infoWait))
		}
		return nil, nil, err
	}
	read := d.docker.DiskUsage
	if preview {
		read = d.docker.CleanupDiskUsage
	}
	df, err := read(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, fmt.Errorf("its Docker took more than %s to add up its images and volumes, as it reads every volume to do that, which is slow on a big disk. Try again in a few minutes", human.Duration(diskUseWait))
		}
		return nil, nil, err
	}
	root := info.DockerRootDir
	if root == "" {
		root = "/var/lib/docker"
	}
	out := &dockwellv1.ServerDiskUse{Time: timestamppb.New(d.now()), DockerRoot: root}
	if mount, size, used, ok := d.collector.DiskOf(root); ok {
		out.MountPoint, out.SizeBytes, out.UsedBytes = mount, size, used
	}
	out.ImagesBytes = positive(df.LayersSize)
	out.Images = uint32(len(df.Images)) //nolint:gosec // a count of images fits
	for _, img := range df.Images {
		// What an unused image doesn't share with another image: about what
		// removing it would free, as the layers it shares stay for the others.
		if img.Containers == 0 {
			out.ImagesUnusedBytes += positive(img.Size - max(img.SharedSize, 0))
		}
	}
	out.Containers = uint32(len(df.Containers)) //nolint:gosec // a count of containers fits
	for _, c := range df.Containers {
		out.ContainersBytes += positive(c.SizeRw)
	}
	out.Volumes = uint32(len(df.Volumes)) //nolint:gosec // a count of volumes fits
	for _, v := range df.Volumes {
		if v.UsageData == nil {
			continue
		}
		out.VolumesBytes += positive(v.UsageData.Size)
		if v.UsageData.RefCount == 0 {
			out.VolumesUnusedBytes += positive(v.UsageData.Size)
		}
	}
	for _, b := range df.BuildCache {
		if !b.Shared {
			out.BuildCacheBytes += positive(b.Size)
		}
	}
	logs := d.logs
	if logs == nil {
		logs = os.DirFS
	}
	out.LogsBytes = logsSize(logs(root))
	d.dockerBreakdown(out, df)
	return out, df, nil
}

// logsSize adds up the logs Docker keeps for its containers, in its data
// directory: json-file's <id>-json.log and what it rotated, and the local
// driver's files. What it can't read counts as nothing.
func logsSize(fsys fs.FS) uint64 {
	var total uint64
	dirs, err := fs.ReadDir(fsys, "containers")
	if err != nil {
		return 0
	}
	add := func(dir string, match func(name string) bool) {
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !match(e.Name()) {
				continue
			}
			if info, err := e.Info(); err == nil {
				total += positive(info.Size())
			}
		}
	}
	for _, c := range dirs {
		if !c.IsDir() {
			continue
		}
		dir := path.Join("containers", c.Name())
		add(dir, func(name string) bool { return strings.Contains(name, "-json.log") })
		add(path.Join(dir, "local-logs"), func(string) bool { return true })
	}
	return total
}

func positive(n int64) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}
