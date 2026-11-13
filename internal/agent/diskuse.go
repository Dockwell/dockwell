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
)

// diskUseFor is how long an answer stands, so a page opened twice, or by two
// people, asks Docker once.
const diskUseFor = 30 * time.Second

// diskUseWait bounds one answer. Docker adds up every volume to answer, which
// takes a while on a big disk. A test shortens it.
var diskUseWait = 60 * time.Second

// infoWait bounds asking Docker where it keeps its data, which a Docker that
// answers at all answers at once. A test shortens it.
var infoWait = 5 * time.Second

// diskUse adds up what takes the room on the disk that holds Docker's data, only
// when asked (ADR 0040). It asks Docker once at a time, as Docker refuses a
// second count while one runs, and keeps the answer for diskUseFor.
type diskUse struct {
	docker    *docker.Client
	collector *collect.Collector
	// logs reads Docker's data directory, for its containers' logs. Nil reads the
	// real one.
	logs func(root string) fs.FS
	now  func() time.Time

	mu      sync.Mutex
	last    *dockwellv1.ServerDiskUse
	at      time.Time
	running chan struct{} // closed when the count under way ends
	err     error         // what the last count failed with
	errAt   time.Time     // when it did, so a count that failed isn't tried again for diskUseFor
}

// read answers a disk use call.
func (d *diskUse) read(ctx context.Context) (*dockwellv1.ServerDiskUse, error) {
	if d == nil || d.docker == nil {
		return nil, errors.New("this agent can't read Docker's disk use, as it has no Docker driver")
	}
	for {
		d.mu.Lock()
		if d.last != nil && d.now().Sub(d.at) < diskUseFor {
			out := proto.CloneOf(d.last)
			d.mu.Unlock()
			return out, nil
		}
		// A Docker that failed to answer, or took too long, isn't asked again
		// at once by each page that opens.
		if d.err != nil && d.now().Sub(d.errAt) < diskUseFor {
			err := d.err
			d.mu.Unlock()
			return nil, err
		}
		if running := d.running; running != nil {
			d.mu.Unlock()
			select {
			case <-running:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			d.mu.Lock()
			err := d.err
			d.mu.Unlock()
			if err != nil {
				return nil, err
			}
			continue
		}
		running := make(chan struct{})
		d.running = running
		d.mu.Unlock()

		// The count goes on for whoever else waits, even if this caller leaves.
		count, cancel := context.WithTimeout(context.WithoutCancel(ctx), diskUseWait)
		out, err := d.count(count)
		cancel()
		d.mu.Lock()
		d.running, d.err, d.errAt = nil, err, d.now()
		if err == nil {
			d.last, d.at = out, d.now()
		}
		close(running)
		d.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return proto.CloneOf(out), nil
	}
}

// count asks Docker, and adds up what its data takes on its disk.
func (d *diskUse) count(ctx context.Context) (*dockwellv1.ServerDiskUse, error) {
	asked, cancel := context.WithTimeout(ctx, infoWait)
	info, err := d.docker.Info(asked)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("its Docker didn't say where it keeps its data within %s. Check that Docker is running with systemctl status docker", human.Duration(infoWait))
		}
		return nil, err
	}
	df, err := d.docker.DiskUsage(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("its Docker took more than %s to add up its images and volumes, as it reads every volume to do that, which is slow on a big disk. Try again in a few minutes", human.Duration(diskUseWait))
		}
		return nil, err
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
		use := &dockwellv1.VolumeDiskUse{Name: v.Name, SizeBytes: -1, Mountpoint: v.Mountpoint}
		out.VolumeUse = append(out.VolumeUse, use)
		if v.UsageData == nil {
			continue
		}
		use.SizeBytes = v.UsageData.Size
		use.Containers = uint32(max(v.UsageData.RefCount, 0)) //nolint:gosec // a count of containers fits
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
	return out, nil
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
