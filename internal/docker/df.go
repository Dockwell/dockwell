// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// DiskUsage is what Docker says its data takes, as `docker system df` reads it:
// the fields Dockwell uses.
type DiskUsage struct {
	// LayersSize is what every image's layers take, each layer counted once.
	LayersSize int64
	Images     []DiskUsageImage
	Containers []DiskUsageContainer
	Volumes    []DiskUsageVolume
	BuildCache []DiskUsageCache
}

// DiskUsageImage is one image's share.
type DiskUsageImage struct {
	ID                    string `json:"Id"`
	RepoTags, RepoDigests []string
	Labels                map[string]string
	Created               int64
	// Size is all of its layers, and SharedSize those other images have too, or
	// -1 when Docker didn't work that out.
	Size, SharedSize int64
	// Containers is how many containers use it, or -1 when Docker didn't count.
	Containers int64
}

// DiskUsageContainer is one container's own layer.
type DiskUsageContainer struct {
	ID string `json:"Id"`
	// SizeRw is what it wrote on top of its image.
	SizeRw int64
}

// DiskUsageVolume is one volume's share.
type DiskUsageVolume struct {
	Name string
	// Mountpoint is where its files are on the server.
	Mountpoint string
	UsageData  *VolumeUsage
}

// VolumeUsage is what a volume holds, and who uses it.
type VolumeUsage struct {
	// Size is what it holds, or -1 when Docker can't say, as for a volume
	// another driver keeps.
	Size int64
	// RefCount is how many containers use it.
	RefCount int64
}

// DiskUsageCache is one record of the build cache.
type DiskUsageCache struct {
	ID         string
	Type       string
	UsageCount int
	CreatedAt  time.Time
	LastUsedAt *time.Time
	Size       int64
	InUse      bool
	Shared     bool
}

// DiskUsage asks Docker what its data takes. Docker adds up every volume to
// answer, which takes a while on a big disk.
func (c *Client) DiskUsage(ctx context.Context) (*DiskUsage, error) {
	var out DiskUsage
	if err := c.do(ctx, http.MethodGet, "/system/df", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CleanupDiskUsage reads the preview's bounded Engine snapshot.
func (c *Client) CleanupDiskUsage(ctx context.Context) (*DiskUsage, error) {
	var out DiskUsage
	if err := c.boundedJSON(ctx, http.MethodGet, "/system/df", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BuildCacheUsage verifies a prune without walking volumes or image storage.
func (c *Client) BuildCacheUsage(ctx context.Context) ([]DiskUsageCache, error) {
	var out DiskUsage
	err := c.do(ctx, http.MethodGet, "/system/df", url.Values{"type": {"build-cache"}}, nil, &out)
	return out.BuildCache, err
}
