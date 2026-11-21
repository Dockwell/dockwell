// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// SaveImage streams the Engine's archive for one exact ID. Newer Engines accept
// a Docker platform object. Classic Engines before API 1.48 save their one platform.
func (c *Client) SaveImage(ctx context.Context, id, platform string) (io.ReadCloser, error) {
	if !IsImageID(id) {
		return nil, errors.New("select a full image ID")
	}
	api, err := c.imageArchiveAPI(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if platform != "" && !older(api, "1.48") {
		p, err := imageArchivePlatform(platform)
		if err != nil {
			return nil, err
		}
		q.Set("platform", p)
	}
	res, err := c.send(ctx, http.MethodGet, "/v"+api+"/images/"+escRef(id)+"/get", q, nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

func imageArchivePlatform(platform string) (string, error) {
	os, arch, _ := strings.Cut(platform, "/")
	arch, variant, _ := strings.Cut(arch, "/")
	p, err := json.Marshal(struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant,omitempty"`
	}{os, arch, variant})
	return string(p), err
}

// imageArchiveAPI lets these endpoints name a platform without changing the
// version used for established calls on the same connection.
func (c *Client) imageArchiveAPI(ctx context.Context) (string, error) {
	if _, err := c.versioned(ctx, ""); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if older(c.advertisedAPI, "1.49") {
		return c.advertisedAPI, nil
	}
	return "1.49", nil
}

func manifestDescriptor(d *ImageDescriptor, id string) bool {
	if d == nil || d.Digest != id || !IsImageID(id) {
		return false
	}
	switch d.MediaType {
	case "application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json":
		return true
	default:
		return false
	}
}

// ImagePlatformManifest reads the Engine's exact OCI manifest for a reviewed
// image and platform. On newer Engines the Engine selects the platform.
func (c *Client) ImagePlatformManifest(ctx context.Context, image *Image, platform string) (string, error) {
	if manifestDescriptor(image.Descriptor, image.ID) {
		return image.ID, nil
	}
	api, err := c.imageArchiveAPI(ctx)
	if err != nil {
		return "", err
	}
	path := "/v" + api + "/images/" + escRef(image.ID) + "/json"
	if !older(api, "1.49") {
		selected, err := imageArchivePlatform(platform)
		if err != nil {
			return "", err
		}
		var out Image
		if err := c.raw(ctx, http.MethodGet, path, url.Values{"platform": {selected}}, nil, &out); err != nil {
			return "", err
		}
		if !manifestDescriptor(out.Descriptor, out.ID) {
			return "", errors.New("the Engine did not identify the selected platform manifest")
		}
		return out.ID, nil
	}
	if !older(api, "1.48") {
		return c.matchArchiveManifest(ctx, path, image.ID, platform)
	}
	return "", errors.New("the Engine cannot identify this platform manifest")
}

func (c *Client) matchArchiveManifest(ctx context.Context, path, id, platform string) (string, error) {
	var out struct {
		ID        string `json:"Id"`
		Manifests []struct {
			ID        string
			Available bool
			Kind      string
			ImageData *struct {
				Platform struct{ OS, Architecture, Variant string }
			}
		}
	}
	if err := c.raw(ctx, http.MethodGet, path, url.Values{"manifests": {"1"}}, nil, &out); err != nil {
		return "", err
	}
	if out.ID != id {
		return "", errors.New("the Engine returned a different image")
	}
	var digest string
	for _, manifest := range out.Manifests {
		if !manifest.Available || manifest.Kind != "image" || manifest.ImageData == nil || !IsImageID(manifest.ID) {
			continue
		}
		p := manifest.ImageData.Platform
		found := strings.TrimSuffix(p.OS+"/"+p.Architecture+"/"+p.Variant, "/")
		if archivePlatformAlias(found) != archivePlatformAlias(platform) {
			continue
		}
		if digest != "" && digest != manifest.ID {
			return "", errors.New("the Engine reports more than one manifest for this platform")
		}
		digest = manifest.ID
	}
	if digest == "" {
		return "", errors.New("the Engine did not identify the manifest for this platform")
	}
	return digest, nil
}

func archivePlatformAlias(platform string) string {
	switch platform {
	case "linux/arm64/v8":
		return "linux/arm64"
	case "linux/amd64/v1":
		return "linux/amd64"
	default:
		return platform
	}
}
