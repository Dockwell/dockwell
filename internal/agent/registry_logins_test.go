// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/registry"
)

// Docker sends a Docker Hub login to the mirrors its daemon.json names too, so a
// server whose Docker has any is handed no Docker Hub login: what it pulls from
// Docker Hub, it pulls as anyone. A login for another registry goes as before.
// (Security review of #584, L4.)
func TestDockerGetsNoHubLoginBehindMirrors(t *testing.T) {
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Www-Authenticate", `Basic realm="r"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(reg.Close)
	hub := registry.Auth{Name: "hub", Registry: registry.Hub, Username: "acme", Password: "hub-token"}
	ghcr := registry.Auth{Name: "ghcr", Registry: "ghcr.io", Username: "acme", Password: "ghcr-token"}

	e := dockertest.New(t)
	im := newImages(e.Client(), ImageReads{Test: reg.URL}, nil)
	if ok, err := im.challenge(t.Context(), hub); !ok || err != nil {
		t.Errorf("with no mirrors, Docker isn't handed the Docker Hub login: %v, %v", ok, err)
	}

	mirrored := dockertest.New(t)
	mirrored.Mirrors = []string{"https://mirror.example.com/"}
	im = newImages(mirrored.Client(), ImageReads{Test: reg.URL}, nil)
	if ok, err := im.challenge(t.Context(), hub); ok || err != nil {
		t.Errorf("behind a mirror, Docker is handed the Docker Hub login: %v, %v", ok, err)
	}
	if ok, err := im.challenge(t.Context(), ghcr); !ok || err != nil {
		t.Errorf("behind a Docker Hub mirror, Docker isn't handed GHCR's login: %v, %v", ok, err)
	}
}
