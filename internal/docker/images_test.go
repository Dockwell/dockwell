// SPDX-License-Identifier: AGPL-3.0-only

package docker_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

func TestImageImportKeepsRegistryPortsInTheRepository(t *testing.T) {
	for _, ref := range []struct{ input, repository, tag string }{
		{"fixture/helper:seed", "fixture/helper", "seed"},
		{"127.0.0.1:5000/fixture/image:review", "127.0.0.1:5000/fixture/image", "review"},
		{"localhost:5000/fixture/image", "localhost:5000/fixture/image", ""},
		{"[::1]:5000/fixture/image:review", "[::1]:5000/fixture/image", "review"},
	} {
		t.Run(ref.input, func(t *testing.T) {
			engine := dockertest.New(t)
			engine.Intercept(func(r *http.Request) error {
				if strings.HasSuffix(r.URL.Path, "/images/create") {
					q := r.URL.Query()
					if q.Get("fromSrc") != "-" || q.Get("repo") != ref.repository || q.Get("tag") != ref.tag {
						return fmt.Errorf("import sends %v, want repo %q and tag %q", q, ref.repository, ref.tag)
					}
				}
				return nil
			})
			if err := engine.Client().ImportImage(t.Context(), ref.input, strings.NewReader("fixture tar")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestImagePullAndTagKeepRegistryPorts(t *testing.T) {
	const repo = "127.0.0.1:5000/fixture/image"
	digest := "sha256:" + strings.Repeat("a", 64)
	pinned := repo + "@" + digest
	engine := dockertest.New(t)
	engine.Registry[pinned] = dockertest.Image{Digest: digest}
	engine.Intercept(func(r *http.Request) error {
		q := r.URL.Query()
		switch {
		case strings.HasSuffix(r.URL.Path, "/images/create"):
			if q.Get("fromImage") != pinned || q.Get("tag") != "" {
				return fmt.Errorf("pull sends %v, want the full pinned reference", q)
			}
		case strings.HasSuffix(r.URL.Path, "/tag"):
			if q.Get("repo") != repo || q.Get("tag") != "review" {
				return fmt.Errorf("tag sends %v, want repo %q and tag review", q, repo)
			}
		}
		return nil
	})
	client := engine.Client()
	if err := client.PullImage(t.Context(), pinned, nil); err != nil {
		t.Fatal(err)
	}
	image, err := client.InspectImage(t.Context(), pinned)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.TagImage(t.Context(), image.ID, repo+":review"); err != nil {
		t.Fatal(err)
	}
}

// What a registry calls an image comes from that image's repository, however a
// reference writes it. ADR 0021 says what taking the first of several comes to.
func TestDigestComesFromTheImagesOwnRepository(t *testing.T) {
	const (
		a = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		b = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	for _, c := range []struct {
		name, ref string
		digests   []string
		want      string
	}{
		{"a tag, with one digest", "ghcr.io/acme/shop:1", []string{"ghcr.io/acme/shop@" + a}, a},
		{"another repository's digest isn't this one's", "ghcr.io/acme/shop:1", []string{"ghcr.io/other/shop@" + b, "ghcr.io/acme/shop@" + a}, a},
		{"several of one repository: the first", "ghcr.io/acme/shop:1", []string{"ghcr.io/acme/shop@" + a, "ghcr.io/acme/shop@" + b}, a},
		{"Docker Hub's name, however it's written", "docker.io/library/nginx:1.27", []string{"nginx@" + a}, a},
		{"a reference's own digest, over any list", "ghcr.io/acme/shop:1@" + b, []string{"ghcr.io/acme/shop@" + a}, b},
		{"an image built here has none", "acme/built-here:1", nil, ""},
		{"nor does one only another repository has", "acme/built-here:1", []string{"ghcr.io/acme/shop@" + a}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := (&docker.Image{RepoDigests: c.digests}).Digest(c.ref); got != c.want {
				t.Errorf("Digest(%s) of %v = %q, want %q", c.ref, c.digests, got, c.want)
			}
		})
	}
}

// A registry reached over plain HTTP: one daemon.json names, by its name or its
// network, or one on the server itself.
func TestRegistriesReachedOverPlainHTTP(t *testing.T) {
	r := &docker.RegistryConfig{
		IndexConfigs: map[string]struct {
			Secure bool
		}{"registry.example.com:5000": {Secure: false}, "docker.io": {Secure: true}},
		InsecureRegistryCIDRs: []string{"10.0.0.0/8", "not a network"},
	}
	for host, want := range map[string]bool{
		"registry.example.com:5000": true, "docker.io": false, "10.1.2.3:5000": true, "10.1.2.3": true,
		"192.0.2.10:5000": false, "127.0.0.1:5000": true, "localhost:5000": true, "ghcr.io": false,
	} {
		if got := r.Insecure(host); got != want {
			t.Errorf("Insecure(%s) = %v, want %v", host, got, want)
		}
	}
	if !(*docker.RegistryConfig)(nil).Insecure("127.0.0.1") || (*docker.RegistryConfig)(nil).Insecure("10.1.2.3") {
		t.Errorf("with no config, only the server itself is reached over plain HTTP")
	}
}
