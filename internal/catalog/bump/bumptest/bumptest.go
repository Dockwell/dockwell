// SPDX-License-Identifier: AGPL-3.0-only

// Package bumptest is a stand-in for what a catalog's bump reads (ADR 0100), for a
// test with no network: registries, Docker Hub's API, GitHub's API and the file
// docker-library's official-images keeps of each image, all on one local server.
package bumptest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/catalog/bump"
	"github.com/dockwell/dockwell/internal/registry"
)

// Tag is a tag of an image: its digest is made from its name and Build, and it runs
// on Platforms, linux/amd64 and linux/arm64 where none are given.
type Tag struct {
	Name string
	// Build tells two pushes of one tag apart: a new digest of the same tag.
	Build     int
	Pushed    time.Time
	Platforms []string
}

// Digest is the digest the stand-in gives a tag of a repository.
func Digest(repository string, t Tag) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s:%s#%d", repository, t.Name, t.Build))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Release is a GitHub release.
type Release struct {
	Draft, Prerelease bool
}

// Stand is the stand-in's server.
type Stand struct {
	URL string
	mu  sync.Mutex
	// Images are each repository's tags, by its repository as a registry.Ref
	// names it, such as library/postgres or plausible/community-edition, and Hub
	// says which are Docker Hub's.
	Images map[string][]Tag
	Hub    map[string]bool
	// Releases and Tags are GitHub's, by owner/name@tag.
	Releases map[string]Release
	Tags     map[string]bool
	// Official are docker-library's library files, by image name.
	Official map[string]string
	// Limited is how many requests to a registry answer 429 first.
	Limited int
	// Broken are the manifests that answer 500, by repository:tag, as a registry
	// that fails for a while.
	Broken map[string]bool
	// Asked are the paths it was asked for, in order.
	Asked []string
}

// New starts a stand-in, stopped when the test ends.
func New(t *testing.T) *Stand {
	t.Helper()
	s := &Stand{Images: map[string][]Tag{}, Hub: map[string]bool{}, Releases: map[string]Release{}, Tags: map[string]bool{}, Official: map[string]string{}, Broken: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Live are a bump's sources reading the stand-in alone.
func (s *Stand) Live() *bump.Live {
	c := &http.Client{Timeout: 10 * time.Second}
	return &bump.Live{
		Registry:  registry.New(registry.Options{Test: s.URL, Secure: c, Plain: c, Public: c}),
		HTTP:      c,
		GitHubAPI: s.URL + "/github",
		Raw:       s.URL + "/raw",
		Tries:     3,
		Wait:      time.Millisecond,
	}
}

func (s *Stand) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Asked = append(s.Asked, r.Method+" "+r.URL.Path)
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/github/"):
		s.github(w, strings.TrimPrefix(p, "/github"))
	case strings.HasPrefix(p, "/raw/docker-library/official-images/master/library/"):
		name := strings.TrimPrefix(p, "/raw/docker-library/official-images/master/library/")
		file, ok := s.Official[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(file))
	case strings.HasPrefix(p, "/v2/"):
		if s.Limited > 0 {
			s.Limited--
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		s.registry(w, r, strings.TrimPrefix(p, "/v2/"))
	default:
		http.NotFound(w, r)
	}
}

func (s *Stand) registry(w http.ResponseWriter, r *http.Request, p string) {
	if rest, ok := strings.CutPrefix(p, "namespaces/"); ok {
		// Docker Hub's API: namespaces/<ns>/repositories/<name>/tags.
		parts := strings.Split(rest, "/")
		if len(parts) != 4 || parts[1] != "repositories" || parts[3] != "tags" {
			http.NotFound(w, r)
			return
		}
		repo := parts[0] + "/" + parts[2]
		tags, ok := s.Images[repo]
		if !ok || !s.Hub[repo] {
			http.NotFound(w, r)
			return
		}
		filter := r.URL.Query().Get("name")
		type result struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
			Pushed string `json:"tag_last_pushed"`
		}
		var out []result
		for _, t := range tags {
			if filter == "" || strings.Contains(t.Name, filter) {
				out = append(out, result{t.Name, Digest(repo, t), t.Pushed.UTC().Format(time.RFC3339Nano)})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": out})
		return
	}
	if repo, ok := strings.CutSuffix(p, "/tags/list"); ok {
		tags, ok := s.Images[repo]
		if !ok {
			http.NotFound(w, r)
			return
		}
		names := make([]string, len(tags))
		for i, t := range tags {
			names[i] = t.Name
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": names})
		return
	}
	repo, ref, ok := strings.Cut(p, "/manifests/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if s.Broken[repo+":"+ref] {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	for _, t := range s.Images[repo] {
		d := Digest(repo, t)
		if t.Name != ref && d != ref {
			continue
		}
		platforms := t.Platforms
		if platforms == nil {
			platforms = []string{"linux/amd64", "linux/arm64"}
		}
		var list []map[string]any
		for _, pl := range platforms {
			os, arch, _ := strings.Cut(pl, "/")
			sum := sha256.Sum256([]byte(d + pl))
			list = append(list, map[string]any{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    "sha256:" + hex.EncodeToString(sum[:]),
				"size":      100,
				"platform":  map[string]string{"os": os, "architecture": arch},
			})
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Docker-Content-Digest", d)
		if r.Method == http.MethodHead {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": list,
		})
		return
	}
	http.NotFound(w, r)
}

func (s *Stand) github(w http.ResponseWriter, p string) {
	parts := strings.Split(strings.TrimPrefix(p, "/repos/"), "/")
	if len(parts) < 5 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	repo := parts[0] + "/" + parts[1]
	switch {
	case parts[2] == "releases" && parts[3] == "tags":
		tag := strings.Join(parts[4:], "/")
		rel, ok := s.Releases[repo+"@"+tag]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"draft": rel.Draft, "prerelease": rel.Prerelease, "tag_name": tag})
	case parts[2] == "git" && parts[3] == "ref" && parts[4] == "tags":
		tag := strings.Join(parts[5:], "/")
		if !s.Tags[repo+"@"+tag] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/tags/" + tag})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// AskedOf are the paths asked for that start with prefix.
func (s *Stand) AskedOf(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.Asked), func(a string) bool { return !strings.Contains(a, prefix) })
}
