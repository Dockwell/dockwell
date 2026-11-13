// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// What a build leaves of Docker's disk, so a build can't fill it and take down what
// runs there, its databases among them: a twentieth of the disk, within these. A
// build doesn't start under it, and stops as soon as the disk falls under it.
const (
	minFloor = 1 << 30
	maxFloor = 10 << 30
	// watchEvery is how often a build reads the disk: at 0.5 GB/s, and more on
	// NVMe, two seconds would let a step write past the floor.
	watchEvery = 250 * time.Millisecond
)

// Each environment's builds keep their layer and cache mounts in a volume of their
// own, so what one app's RUN --mount=type=cache leaves, no app of another
// environment reads. The cache's bound is split between them, and a server keeps
// the caches of the environments that built last.
const (
	cacheLabel = "dockwell.build-cache"
	maxCaches  = 8
	minShare   = 512 << 20
)

// Network is the network builds run on, where no build reaches another container,
// as Docker's default bridge would let it.
const Network = "dockwell-build"

// diskLow is why a build stopped that the disk fell under its floor for.
type diskLow struct{ free, floor int64 }

func (d diskLow) Error() string {
	return fmt.Sprintf("Docker's disk has %d bytes free, under the %d a build leaves", d.free, d.floor)
}

// dockerRoot is where Docker keeps its data, whose disk builds write to.
func (b *Builder) dockerRoot(ctx context.Context) string {
	if info, err := b.Client.Info(ctx); err == nil && info.DockerRootDir != "" {
		return info.DockerRootDir
	}
	return "/var/lib/docker"
}

// floor is what a build leaves free of a disk of total bytes.
func floor(total int64) int64 { return min(max(total/20, minFloor), maxFloor) }

// diskLeft says what's free of the disk at root, and the floor a build leaves.
func (b *Builder) diskLeft(root string) (free, least int64, ok bool) {
	size, freeOf := b.DiskSize, b.DiskFree
	if size == nil {
		size = statfsSize
	}
	if freeOf == nil {
		freeOf = statfsFree
	}
	total, err := size(root)
	if err != nil || total <= 0 {
		return 0, 0, false
	}
	free, err = freeOf(root)
	if err != nil {
		return 0, 0, false
	}
	return free, floor(total), true
}

// checkDisk refuses a build on a disk already under its floor.
func (b *Builder) checkDisk(root string) error {
	free, least, ok := b.diskLeft(root)
	if !ok || free >= least {
		return nil
	}
	return resource.PlainlyBecause(resource.ReasonBuild,
		fmt.Sprintf("This server has %s free on Docker's disk, and a build leaves %s of it, so it didn't start. Free some disk, or build the image in CI and deploy that", human.Size(free), human.Size(least)),
		diskLow{free, least})
}

// watchDisk stops the build through stop as soon as the disk falls under its floor,
// until the returned func is called.
func (b *Builder) watchDisk(root string, stop context.CancelCauseFunc) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(b.watchEvery())
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if free, least, ok := b.diskLeft(root); ok && free < least {
					stop(diskLow{free, least})
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

func (b *Builder) watchEvery() time.Duration {
	if b.WatchEvery > 0 {
		return b.WatchEvery
	}
	return watchEvery
}

// cacheVolume is the volume a build keeps its cache in: its environment's, or for a
// build given registry logins, one of its own, for that service and those logins,
// shared with nothing else. BuildKit serves an image pinned by digest from the
// content its cache holds, whatever pulled it, so a private image a login pulled
// stays where only a build with that same login reads (ADR 0086).
func cacheVolume(req service.BuildRequest) string {
	env := strings.ToLower(req.Service.Scope)
	if env == "" {
		env = "none"
	}
	if len(req.Logins) == 0 {
		return CacheVolume + "-" + env
	}
	logins := make([]string, 0, len(req.Logins))
	for _, a := range req.Logins {
		logins = append(logins, a.Name+"\x00"+a.Registry+"\x00"+a.Username)
	}
	slices.Sort(logins)
	sum := sha256.Sum256([]byte(req.Service.Scope + "\x00" + req.Service.Name + "\x00" + strings.Join(logins, "\x01")))
	return CacheVolume + "-" + env + "-logins-" + hex.EncodeToString(sum[:8])
}

// cacheShare readies the build's cache volume, and gives its share of the bound:
// the bound split between the server's build caches, and at least minShare.
func (b *Builder) cacheShare(ctx context.Context, volume string, bound int64) (int64, error) {
	if _, err := b.Client.CreateVolume(ctx, &docker.VolumeCreate{Name: volume, Labels: map[string]string{cacheLabel: "1"}}); err != nil {
		return 0, fmt.Errorf("docker can't make the volume a build keeps its cache in: %w", err)
	}
	caches, err := b.Client.ListVolumes(ctx, cacheLabel)
	if err != nil || len(caches) == 0 {
		return bound, nil //nolint:nilerr // without the count, the whole bound
	}
	return max(bound/int64(len(caches)), minShare), nil
}

// usedCache records that a build used volume, and removes the caches of the
// environments that built longest ago, past maxCaches.
func (b *Builder) usedCache(ctx context.Context, volume string) {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	used := b.readCacheUse()
	used[volume] = b.now().Unix()
	caches, err := b.Client.ListVolumes(ctx, cacheLabel)
	if err == nil && len(caches) > maxCaches {
		slices.SortFunc(caches, func(x, y docker.Volume) int { return int(used[x.Name] - used[y.Name]) })
		for _, v := range caches[:len(caches)-maxCaches] {
			if v.Name == volume {
				continue
			}
			// One a build uses now is refused, and stays.
			if err := b.Client.RemoveVolume(ctx, v.Name); err == nil {
				delete(used, v.Name)
				b.logger().Info("removed the build cache an environment used longest ago", "volume", v.Name)
			}
		}
	}
	b.writeCacheUse(used)
}

// cacheUseFile keeps when each cache was last built with, in StateDir.
const cacheUseFile = "build-caches.json"

func (b *Builder) readCacheUse() map[string]int64 {
	if b.cacheUse == nil {
		b.cacheUse = map[string]int64{}
		if b.StateDir != "" {
			if raw, err := os.ReadFile(filepath.Join(b.StateDir, cacheUseFile)); err == nil {
				_ = json.Unmarshal(raw, &b.cacheUse)
			}
		}
	}
	return b.cacheUse
}

func (b *Builder) writeCacheUse(used map[string]int64) {
	if b.StateDir == "" {
		return
	}
	raw, _ := json.Marshal(used)
	if err := os.WriteFile(filepath.Join(b.StateDir, cacheUseFile), raw, 0o600); err != nil {
		b.logger().Warn("can't keep when each build cache was used, so the next to go may not be the oldest", "error", err.Error())
	}
}

// pruneFirst says whether a cache's last build didn't end cleanly, so its next
// prunes it before building, back under its share.
func (b *Builder) pruneFirst(volume string) bool {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	return b.dirtyAll || b.dirty[volume]
}

// dropCache removes an environment's build cache, as one whose build stopped under
// the disk's floor holds what it wrote, and says whether it did. One a build uses
// now is refused, and stays.
func (b *Builder) dropCache(ctx context.Context, volume string) bool {
	if err := b.Client.RemoveVolume(ctx, volume); err != nil && !docker.IsNotFound(err) {
		b.logger().Warn("can't remove a build cache to free Docker's disk", "volume", volume, "error", err.Error())
		return false
	}
	b.logger().Info("removed a build cache to free Docker's disk", "volume", volume)
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	delete(b.dirty, volume)
	if b.cacheUse != nil {
		delete(b.cacheUse, volume)
	}
	return true
}

// endedCache records whether a build pruned its cache as it ended.
func (b *Builder) endedCache(volume string, pruned bool) {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.dirty == nil {
		b.dirty = map[string]bool{}
	}
	b.dirty[volume] = !pruned
	if pruned && b.dirtyAll {
		// What a crash left is pruned cache by cache, as each builds again.
		b.dirty[volume] = false
	}
}

// ensureNetwork has the network builds run on: a bridge of its own, on which no
// container reaches another.
func (b *Builder) ensureNetwork(ctx context.Context) error {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.networkReady {
		return nil
	}
	n, err := b.Client.NetworkNamed(ctx, Network)
	switch {
	case err == nil && n != nil && n.Options["com.docker.network.bridge.enable_icc"] != "false":
		return resource.PlainlyBecause(resource.ReasonBuild,
			"The network "+Network+" lets containers on it reach each other, and builds need one that doesn't. Remove it with docker network rm "+Network+", and Dockwell makes it again",
			errors.New("the build network allows inter-container traffic"))
	case err == nil && n != nil:
	case err != nil && !docker.IsNotFound(err):
		return err
	default:
		if _, err := b.Client.CreateNetwork(ctx, &docker.NetworkCreate{
			Name: Network, Driver: "bridge", CheckDuplicate: true,
			Options: map[string]string{"com.docker.network.bridge.enable_icc": "false"},
			Labels:  map[string]string{docker.LabelBuilder: "network"},
		}); err != nil && !docker.IsConflict(err) {
			return fmt.Errorf("docker can't make the network builds run on: %w", err)
		}
	}
	b.networkReady = true
	return nil
}

// statfsFree is how much of the filesystem path is on is free to anyone.
func statfsFree(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * st.Bsize, nil //nolint:gosec // a disk's size fits
}

// checkPlain refuses a repository over plain HTTP whose name leads anywhere but a
// private network from this server, as anyone between could choose what's built.
func (b *Builder) checkPlain(ctx context.Context, repo string) error {
	u, err := url.Parse(repo)
	if err != nil || u.Scheme != "http" {
		return nil //nolint:nilerr // the spec's check says what's wrong with a URL
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return nil // an address the spec's check has seen is private
	}
	lookup := b.LookupIP
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return resource.PlainlyBecause(resource.ReasonBuild, "This server can't find "+host+", which the build fetches from over plain HTTP: "+err.Error()+". Check the name, or use HTTPS", err)
	}
	for _, ip := range ips {
		if !ip.IsPrivate() && !ip.IsLoopback() {
			return resource.PlainlyBecause(resource.ReasonBuild,
				fmt.Sprintf("%s leads to %s, which isn't on a private network, and Dockwell fetches over plain HTTP only from one. Use HTTPS", host, ip),
				errors.New("a plain-HTTP repository resolves outside a private network"))
		}
	}
	return nil
}

// checkRef refuses a ref that isn't a branch or a tag by its full name, or a
// commit: the build fetches it as it is, and Git's short names reach further, as
// pull/7/head reaches a pull request's head, which a fork pushes. A plan pins the
// full name (revision.pinBuilds).
func checkRef(ref string) error {
	if strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/") || commitRef.MatchString(ref) {
		return nil
	}
	return resource.PlainlyBecause(resource.ReasonBuild,
		fmt.Sprintf("The build names %q, and builds only a branch or a tag by its full name, such as refs/heads/main, or a commit. Deploy again, and the plan names it in full", ref),
		errors.New("a build's ref isn't a branch or a tag by its full name"))
}

var commitRef = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
