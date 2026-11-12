// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
)

// A stand-in for the registries and Docker Hub's API, for a test's agent to
// read images from (DOCKWELL_TEST_REGISTRY, ADR 0062): a few images that
// self-hosters run, with the ports, variables and volumes their Dockerfiles
// declare, and their tags, newest first. Anything else isn't there. It hands
// out no token: a test reads as anyone may.

// fakeRepo is a repository the stand-in has.
type fakeRepo struct {
	// Tags, newest first. The first is where latest points.
	tags    []string
	ports   []string
	env     []string
	volumes []string
	cmd     []string
}

var fakeRepos = map[string]fakeRepo{
	"library/nginx": {
		// Not 1.27-alpine or 1.28-alpine, which the flows run as images the registry
		// says nothing of, so New app fills nothing in for them.
		tags: []string{"1.28", "1.27", "1.26"}, ports: []string{"80/tcp"},
		env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "NGINX_VERSION=1.28.0"},
		cmd: []string{"nginx", "-g", "daemon off;"},
	},
	"library/postgres": {
		tags: []string{"17.2", "17", "16.6", "16.4", "16"}, ports: []string{"5432/tcp"},
		env:     []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "PG_MAJOR=17", "PGDATA=/var/lib/postgresql/data"},
		volumes: []string{"/var/lib/postgresql/data"}, cmd: []string{"postgres"},
	},
	"library/redis": {
		tags: []string{"7.4", "7.2"}, ports: []string{"6379/tcp"}, volumes: []string{"/data"},
		env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "REDIS_VERSION=7.4.1"},
		cmd: []string{"redis-server"},
	},
	"library/ghost": {
		tags: []string{"5.96", "5.95"}, ports: []string{"2368/tcp"}, volumes: []string{"/var/lib/ghost/content"},
		env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "NODE_ENV=production", "url=http://localhost:2368"},
		cmd: []string{"node", "current/index.js"},
	},
	"louislam/uptime-kuma": {
		tags: []string{"1.23.16", "1.23"}, ports: []string{"3001/tcp"}, volumes: []string{"/app/data"},
		cmd: []string{"node", "server/server.js"},
	},
	"example/shop": {
		tags: []string{"3.2.0", "3.1.4"}, ports: []string{"3000/tcp"},
		env: []string{"NODE_ENV=production", "PORT=3000", "DATABASE_URL="},
		cmd: []string{"node", "server.js"},
	},
}

// fakeHub is what a search of Docker Hub finds.
var fakeHub = []docker.Found{
	{Name: "nginx", Description: "Official build of Nginx.", Official: true, Stars: 20890},
	{Name: "postgres", Description: "The PostgreSQL object-relational database system.", Official: true, Stars: 14210},
	{Name: "redis", Description: "Redis is the world's fastest data platform.", Official: true, Stars: 13020},
	{Name: "ghost", Description: "Ghost is a free and open source blogging platform.", Official: true, Stars: 1730},
	{Name: "louislam/uptime-kuma", Description: "A fancy self-hosted monitoring tool.", Stars: 690},
	{Name: "bitnami/nginx", Description: "Bitnami container image for NGINX.", Stars: 190},
}

func digestOfBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// registryDigest is what the stand-in calls the image a reference names, such
// as postgres:16.4, so what the engine pulls is what the stand-in says runs (ADR
// 0079): empty for an image it hasn't got.
func registryDigest(ref string) string {
	name, tag, _ := strings.Cut(ref, ":")
	if tag == "" {
		tag = "latest"
	}
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}
	repo, ok := fakeRepos[name]
	if !ok {
		return ""
	}
	if tag == "latest" {
		tag = repo.tags[0]
	}
	if !slices.Contains(repo.tags, tag) {
		return ""
	}
	_, m := configOf(name, repo, tag)
	return digestOfBytes(m)
}

// configOf is a repository's config at a tag, and its manifest, as a registry
// keeps them.
func configOf(repo string, r fakeRepo, tag string) (config, manifest []byte) {
	exposed := map[string]struct{}{}
	for _, p := range r.ports {
		exposed[p] = struct{}{}
	}
	volumes := map[string]struct{}{}
	for _, v := range r.volumes {
		volumes[v] = struct{}{}
	}
	config, _ = json.Marshal(map[string]any{
		"os": "linux", "architecture": "amd64",
		"config": map[string]any{"ExposedPorts": exposed, "Env": r.env, "Volumes": volumes, "Cmd": r.cmd, "Labels": map[string]string{"tag": repo + ":" + tag}},
	})
	manifest, _ = json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digestOfBytes(config), "size": len(config)},
	})
	return config, manifest
}

// serveRegistry serves the stand-in at addr, such as 127.0.0.1:8035.
func serveRegistry(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/{$}", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) })
	repoOf := func(r *http.Request) (string, fakeRepo, bool) {
		path := strings.TrimPrefix(r.URL.Path, "/v2/")
		for _, part := range []string{"/manifests/", "/blobs/", "/tags/list"} {
			if at := strings.Index(path, part); at > 0 {
				name := path[:at]
				repo, ok := fakeRepos[name]
				return name, repo, ok
			}
		}
		return "", fakeRepo{}, false
	}
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		name, repo, ok := repoOf(r)
		if !ok {
			http.Error(w, `{"errors":[{"code":"NAME_UNKNOWN"}]}`, http.StatusNotFound)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "tags": append([]string{"latest"}, repo.tags...)})
		case strings.Contains(r.URL.Path, "/manifests/"):
			ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			tag := ref
			if ref == "latest" {
				tag = repo.tags[0]
			}
			for _, t := range repo.tags {
				_, m := configOf(name, repo, t)
				if strings.HasPrefix(ref, "sha256:") && digestOfBytes(m) == ref {
					tag = t
				}
			}
			if !slices.Contains(repo.tags, tag) {
				http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound)
				return
			}
			_, m := configOf(name, repo, tag)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digestOfBytes(m))
			if r.Method != http.MethodHead {
				_, _ = w.Write(m)
			}
		case strings.Contains(r.URL.Path, "/blobs/"):
			want := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			for _, t := range repo.tags {
				if c, _ := configOf(name, repo, t); digestOfBytes(c) == want {
					_, _ = w.Write(c)
					return
				}
			}
			http.Error(w, `{"errors":[{"code":"BLOB_UNKNOWN"}]}`, http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	})
	// Docker Hub's own API, for a repository's tags with when each was pushed.
	mux.HandleFunc("GET /v2/namespaces/{ns}/repositories/{name}/tags", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("ns") + "/" + r.PathValue("name")
		repo, ok := fakeRepos[name]
		if !ok {
			http.Error(w, `{"message":"object not found"}`, http.StatusNotFound)
			return
		}
		var results []map[string]any
		at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
		for i, t := range repo.tags {
			_, m := configOf(name, repo, t)
			pushed := at.Add(-time.Duration(i) * 72 * time.Hour).Format(time.RFC3339)
			results = append(results, map[string]any{"name": t, "digest": digestOfBytes(m), "tag_last_pushed": pushed})
			if i == 0 {
				results = append(results, map[string]any{"name": "latest", "digest": digestOfBytes(m), "tag_last_pushed": pushed})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		log.Fatalf("can't serve the registry at the address FAKEDOCKER_REGISTRY names: %v", err)
	}
	go func() {
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		_ = srv.Serve(l)
	}()
}
