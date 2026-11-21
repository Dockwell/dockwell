// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ImageIdentity describes the exact native manifest, independently of a local Engine ID.
type ImageIdentity struct {
	Index, Manifest, Config string
	Platform                Platform
}

// ResolveImage reads fresh metadata for an exact pull. It verifies the registry's
// digests against the response bytes before using them as pins.
func (c *Client) ResolveImage(ctx context.Context, r Ref, native Platform) (ImageIdentity, error) {
	m, top, err := c.verifiedManifest(ctx, r, r.Reference())
	if err != nil {
		return ImageIdentity{}, err
	}
	out := ImageIdentity{Manifest: top, Platform: native}
	if len(m.Manifests) != 0 {
		pick := pickPlatform(m.Manifests, native)
		if pick == nil {
			return out, ErrNotFound
		}
		out.Index = top
		m, out.Manifest, err = c.verifiedManifest(ctx, r, pick.Digest)
		if err != nil {
			return out, err
		}
	}
	if m.Config == nil || !IsDigest(m.Config.Digest) {
		return out, ErrBadDigest
	}
	out.Config = m.Config.Digest
	var platform struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	}
	if err := c.verifiedMetadata(ctx, r, "blobs/"+out.Config, "", out.Config, &platform); err != nil {
		return out, err
	}
	if platform.OS != native.OS || platform.Architecture != native.Architecture ||
		native.Variant != "" && platform.Variant != "" && native.Variant != platform.Variant {
		return out, ErrNotFound
	}
	out.Platform = Platform{OS: platform.OS, Architecture: platform.Architecture, Variant: platform.Variant}
	return out, nil
}

func (c *Client) verifiedManifest(ctx context.Context, r Ref, reference string) (*manifest, string, error) {
	var m manifest
	expected := ""
	if IsDigest(reference) {
		expected = reference
	}
	digest, err := c.readIdentity(ctx, r, "manifests/"+reference, manifestTypes, expected, &m)
	return &m, digest, err
}

func (c *Client) verifiedMetadata(ctx context.Context, r Ref, path, accept, expected string, into any) error {
	_, err := c.readIdentity(ctx, r, path, accept, expected, into)
	return err
}

func (c *Client) readIdentity(ctx context.Context, r Ref, path, accept, expected string, into any) (string, error) {
	response, err := c.get(ctx, http.MethodGet, r.Registry, r.Repository, "/v2/"+r.Repository+"/"+path, accept, strings.HasPrefix(path, "blobs/"))
	if err != nil {
		return "", err
	}
	defer drain(response)
	const identityLimit = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(response.Body, identityLimit+1))
	if err != nil {
		return "", ErrUnreachable
	}
	if len(raw) > identityLimit {
		return "", ErrTooLarge
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	header := response.Header.Get("Docker-Content-Digest")
	if expected != "" && expected != digest || header != "" && header != digest {
		return "", ErrBadDigest
	}
	if json.Unmarshal(raw, into) != nil {
		return "", ErrUnreachable
	}
	return digest, nil
}

// Repositories reads a bounded Distribution catalog page. A registry that hides
// its catalog can still be used with a repository name.
func (c *Client) Repositories(ctx context.Context, host, last string, size int) ([]string, string, error) {
	query := url.Values{"n": {strconv.Itoa(size)}}
	if last != "" {
		query.Set("last", last)
	}
	// The canonical registry, rather than a pull-through mirror, owns the catalog.
	bases := c.bases(host)
	response, err := c.getFrom(ctx, http.MethodGet, bases[len(bases)-1], "", "/v2/_catalog?"+query.Encode(), "", false)
	if err != nil {
		return nil, "", err
	}
	defer drain(response)
	var body struct {
		Repositories []string `json:"repositories"`
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 || json.Unmarshal(raw, &body) != nil || len(body.Repositories) > size {
		return nil, "", ErrTooLarge
	}
	var names []string
	for _, name := range body.Repositories {
		if r, err := Parse(host + "/" + name); err == nil && r.Registry == host && r.Repository == name && r.Tag == "" && r.Digest == "" {
			names = append(names, name)
		}
	}
	next := ""
	if len(body.Repositories) > 0 && response.Header.Get("Link") != "" {
		if candidate := body.Repositories[len(body.Repositories)-1]; candidate > last && len(candidate) <= 255 {
			next = candidate
		}
	}
	return names, next, nil
}
