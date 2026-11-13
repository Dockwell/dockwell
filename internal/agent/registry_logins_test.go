// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"log/slog"
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

// A build is never handed a login whose registry is on the server's loopback, which
// its steps share: a step listening there would get it. BuildKit reads no Docker
// mirrors, so a build keeps its Docker Hub login behind them. (Security review of
// #591, H2.)
func TestABuildGetsNoLoopbackLogin(t *testing.T) {
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Www-Authenticate", `Basic realm="r"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(reg.Close)
	e := dockertest.New(t)
	e.Mirrors = []string{"https://mirror.example.com/"}
	im := newImages(e.Client(), ImageReads{Test: reg.URL}, nil)
	var logins []registry.Auth
	for _, host := range []string{"localhost:5000", "127.0.0.1:5000", "[::1]:5000", "ghcr.io", registry.Hub} {
		logins = append(logins, registry.Auth{Name: "l", Registry: host, Username: "ci", Password: "token"})
	}
	kept, err := im.forBuild(t.Context(), logins, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range kept {
		got = append(got, a.Registry)
	}
	if len(got) != 2 || got[0] != "ghcr.io" || got[1] != registry.Hub {
		t.Errorf("a build is handed logins for %q", got)
	}
}
