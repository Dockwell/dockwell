// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Registry is what the converter asks of the registries an app's images are in.
type Registry interface {
	// Pin says what a tag points at now: the digest of its multi-architecture index
	// where it has one, so each server pulls its own.
	Pin(ctx context.Context, image string) (string, error)
	// Built says when the image a digest names was built, from its config.
	Built(ctx context.Context, image, digest string) (time.Time, error)
	// Platforms says what the image a digest names runs on, such as linux/arm64.
	Platforms(ctx context.Context, image, digest string) ([]string, error)
}

// ErrNotAsked says the converter didn't ask, to keep within what a registry allows.
var ErrNotAsked = errors.New("not asked, to keep within what the registry allows an address in a few hours")

// What the converter accepts for an image's manifest, an index first.
var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// OCI asks registries over the distribution API, as Docker does, with the token
// each one's challenge names, and nothing signed in.
type OCI struct {
	Client *http.Client
	// HubGets bounds the manifest GETs asked of Docker Hub, which counts each as a
	// pull: 100 in 6 hours from an address that doesn't sign in. A HEAD costs
	// nothing, so pinning a tag is never bounded, and only Built is.
	HubGets int
	// HubAPI is Docker Hub's API, https://hub.docker.com unless a test sets it.
	HubAPI string

	mu     sync.Mutex
	tokens map[string]string
	gets   int
	hub    map[string]hubTag
}

// hubTag is what Docker Hub's API says of a tag: when it was last pushed, the index it
// names, and the platforms that index holds.
type hubTag struct {
	Pushed  time.Time `json:"tag_last_pushed"`
	Updated time.Time `json:"last_updated"`
	Digest  string    `json:"digest"`
	Images  []struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"images"`
}

// Pin asks for the digest a tag points at, with a HEAD, which no registry counts.
func (o *OCI) Pin(ctx context.Context, image string) (string, error) {
	r, err := parseRef(image)
	if err != nil {
		return "", err
	}
	if r.digest != "" {
		return r.digest, nil
	}
	resp, err := o.do(ctx, r, http.MethodHead, "manifests/"+r.tag, manifestTypes)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	if d := resp.Header.Get("Docker-Content-Digest"); strings.HasPrefix(d, "sha256:") {
		return d, nil
	}
	// A registry that says no digest on a HEAD says it of what a GET gives.
	resp, err = o.get(ctx, r, "manifests/"+r.tag, manifestTypes)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("reading %s's manifest: %w", image, err)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Built reads the build time from the config of the image's linux/amd64 manifest,
// or its first, which every platform's build shares closely enough for an age in days.
func (o *OCI) Built(ctx context.Context, image, digest string) (time.Time, error) {
	r, err := parseRef(image)
	if err != nil {
		return time.Time{}, err
	}
	if r.host == dockerHub && r.tag != "" {
		return o.hubPushed(ctx, r)
	}
	var m struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := o.getJSON(ctx, r, "manifests/"+digest, manifestTypes, &m); err != nil {
		return time.Time{}, err
	}
	if len(m.Manifests) > 0 {
		pick := m.Manifests[0].Digest
		for _, one := range m.Manifests {
			if one.Platform.OS == "linux" && one.Platform.Architecture == "amd64" {
				pick = one.Digest
				break
			}
		}
		m.Manifests = nil
		if err := o.getJSON(ctx, r, "manifests/"+pick, manifestTypes, &m); err != nil {
			return time.Time{}, err
		}
	}
	if m.Config.Digest == "" {
		return time.Time{}, fmt.Errorf("%s's manifest names no config", image)
	}
	var config struct {
		Created time.Time `json:"created"`
	}
	if err := o.getJSON(ctx, r, "blobs/"+m.Config.Digest, "*/*", &config); err != nil {
		return time.Time{}, err
	}
	if config.Created.IsZero() {
		return time.Time{}, fmt.Errorf("%s's config says no build time", image)
	}
	return config.Created, nil
}

// hubPushed asks Docker Hub's own API when a tag was last pushed, which it doesn't
// count as a pull, as a manifest GET is. A rebuild of the version pushes it again,
// so it's no earlier than the build's own date, and holds a build back no less.
func (o *OCI) hubPushed(ctx context.Context, r ref) (time.Time, error) {
	tag, err := o.hubTag(ctx, r)
	if err != nil {
		return time.Time{}, err
	}
	pushed := tag.Pushed
	if pushed.IsZero() {
		pushed = tag.Updated
	}
	if pushed.IsZero() {
		return time.Time{}, fmt.Errorf("no push time for %s:%s on Docker Hub", r.name, r.tag)
	}
	return pushed, nil
}

// hubTag asks Docker Hub's API of a tag, once a run.
func (o *OCI) hubTag(ctx context.Context, r ref) (hubTag, error) {
	key := r.name + ":" + r.tag
	o.mu.Lock()
	tag, ok := o.hub[key]
	o.mu.Unlock()
	if ok {
		return tag, nil
	}
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	namespace, repo, _ := strings.Cut(r.name, "/")
	u := o.HubAPI
	if u == "" {
		u = "https://hub.docker.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"/v2/namespaces/"+namespace+"/repositories/"+repo+"/tags/"+url.PathEscape(r.tag), http.NoBody)
	if err != nil {
		return hubTag{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return hubTag{}, fmt.Errorf("asking Docker Hub for %s: %w", r.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return hubTag{}, fmt.Errorf("asking Docker Hub for %s:%s: it answered %s", r.name, r.tag, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&tag); err != nil {
		return hubTag{}, fmt.Errorf("reading Docker Hub's answer for %s: %w", r.name, err)
	}
	o.mu.Lock()
	if o.hub == nil {
		o.hub = map[string]hubTag{}
	}
	o.hub[key] = tag
	o.mu.Unlock()
	return tag, nil
}

// Platforms says what an image runs on: from Docker Hub's API where the tag still
// names that build, which counts nothing, and from its index elsewhere.
func (o *OCI) Platforms(ctx context.Context, image, digest string) ([]string, error) {
	r, err := parseRef(image)
	if err != nil {
		return nil, err
	}
	if r.host == dockerHub && r.tag != "" {
		if tag, err := o.hubTag(ctx, r); err == nil && tag.Digest == digest && len(tag.Images) > 0 {
			var out []string
			for _, img := range tag.Images {
				out = append(out, img.OS+"/"+img.Architecture)
			}
			return compactPlatforms(out), nil
		}
	}
	var m struct {
		Manifests []struct {
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := o.getJSON(ctx, r, "manifests/"+digest, manifestTypes, &m); err != nil {
		return nil, err
	}
	var out []string
	for _, one := range m.Manifests {
		out = append(out, one.Platform.OS+"/"+one.Platform.Architecture)
	}
	if len(m.Manifests) == 0 && m.Config.Digest != "" {
		var config struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		}
		if err := o.getJSON(ctx, r, "blobs/"+m.Config.Digest, "*/*", &config); err != nil {
			return nil, err
		}
		out = append(out, config.OS+"/"+config.Architecture)
	}
	return compactPlatforms(out), nil
}

// compactPlatforms sorts platforms, once each, leaving out the attestations an index
// lists as unknown/unknown.
func compactPlatforms(list []string) []string {
	list = slices.DeleteFunc(list, func(p string) bool { return p == "unknown/unknown" || p == "/" })
	slices.Sort(list)
	return slices.Compact(list)
}

func (o *OCI) getJSON(ctx context.Context, r ref, path, accept string, into any) error {
	resp, err := o.get(ctx, r, path, accept)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(into); err != nil {
		return fmt.Errorf("reading %s from %s: %w", path, r.name, err)
	}
	return nil
}

// get asks with a GET, which Docker Hub counts when it's for a manifest.
func (o *OCI) get(ctx context.Context, r ref, path, accept string) (*http.Response, error) {
	if r.host == dockerHub && strings.HasPrefix(path, "manifests/") {
		o.mu.Lock()
		over := o.gets >= o.HubGets
		if !over {
			o.gets++
		}
		o.mu.Unlock()
		if over {
			return nil, ErrNotAsked
		}
	}
	return o.do(ctx, r, http.MethodGet, path, accept)
}

// do asks once, and again with a token when the registry's challenge names one.
func (o *OCI) do(ctx context.Context, r ref, method, path, accept string) (*http.Response, error) {
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	scope := "repository:" + r.name + ":pull"
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, method, r.base()+"/v2/"+r.name+"/"+path, http.NoBody)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		o.mu.Lock()
		token := o.tokens[r.host+" "+scope]
		o.mu.Unlock()
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("asking %s for %s: %w", r.host, r.name, err)
		}
		if resp.StatusCode == http.StatusUnauthorized && try == 0 {
			challenge := resp.Header.Get("WWW-Authenticate")
			_ = resp.Body.Close()
			token, err := o.token(ctx, client, challenge, scope)
			if err != nil {
				return nil, fmt.Errorf("signing in to %s for %s: %w", r.host, r.name, err)
			}
			o.mu.Lock()
			if o.tokens == nil {
				o.tokens = map[string]string{}
			}
			o.tokens[r.host+" "+scope] = token
			o.mu.Unlock()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("%s answered %s for %s's %s", r.host, resp.Status, r.name, path)
		}
		return resp, nil
	}
}

// token fetches what a Bearer challenge asks for, with no account, as a public
// image needs none.
func (o *OCI) token(ctx context.Context, client *http.Client, challenge, scope string) (string, error) {
	kind, params, ok := strings.Cut(challenge, " ")
	if !ok || !strings.EqualFold(kind, "Bearer") {
		return "", fmt.Errorf("its challenge is %q, and the converter signs in with a Bearer token only", challenge)
	}
	values := map[string]string{}
	for _, p := range splitParams(params) {
		k, v, _ := strings.Cut(p, "=")
		values[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	realm, err := url.Parse(values["realm"])
	if err != nil || realm.Scheme != "https" && realm.Scheme != "http" {
		return "", fmt.Errorf("its challenge names no realm to ask: %q", challenge)
	}
	q := realm.Query()
	if s := values["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", realm.Host, resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", fmt.Errorf("reading %s's token: %w", realm.Host, err)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	if t.Token == "" {
		return "", fmt.Errorf("%s gave no token", realm.Host)
	}
	return t.Token, nil
}

// splitParams splits a challenge's parameters at the commas outside quotes.
func splitParams(s string) []string {
	var out []string
	quoted, start := false, 0
	for i, c := range s {
		switch {
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

const dockerHub = "docker.io"

// ref is an image reference, read as Docker reads one.
type ref struct {
	host, name, tag, digest string
}

func (r ref) base() string {
	if r.host == dockerHub {
		return "https://registry-1.docker.io"
	}
	if strings.HasPrefix(r.host, "localhost") || strings.HasPrefix(r.host, "127.0.0.1") {
		return "http://" + r.host
	}
	return "https://" + r.host
}

// parseRef reads an image as Docker does: a first part with a dot or a colon, or
// localhost, is its registry, Docker Hub otherwise, where a name alone is an
// official image under library/.
func parseRef(image string) (ref, error) {
	var r ref
	rest := image
	if at := strings.Index(rest, "@"); at >= 0 {
		r.digest, rest = rest[at+1:], rest[:at]
	}
	if slash := strings.LastIndex(rest, "/"); strings.LastIndex(rest, ":") > slash {
		colon := strings.LastIndex(rest, ":")
		r.tag, rest = rest[colon+1:], rest[:colon]
	}
	first, after, hasSlash := strings.Cut(rest, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.host, r.name = first, after
	} else {
		r.host, r.name = dockerHub, rest
		if !hasSlash {
			r.name = "library/" + rest
		}
	}
	if r.host == "index.docker.io" || r.host == "registry-1.docker.io" {
		r.host = dockerHub
	}
	if r.name == "" || r.tag == "" && r.digest == "" {
		return ref{}, fmt.Errorf("the image %q names no tag. Name its version", image)
	}
	return r, nil
}
