// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
)

const privateBase = "ghcr.io/acme/base:1.0"

var ghcrLogin = registry.Auth{Name: "ghcr-ci", Registry: "ghcr.io", Username: "ci", Password: "s3cret-registry-token"}

// A Dockerfile's private base image pulls with the login it's given, which BuildKit's
// client reads from Docker's config.json, in memory, read-only, and never from the
// builder's environment, its command or its log; it's gone once the build ends
// (ADR 0086).
func TestABuildsBaseImagePullsWithItsLogin(t *testing.T) {
	b := newBench(t)
	b.engine.Registry[privateBase] = dockertest.Image{Login: "ci:s3cret-registry-token"}
	commit := b.repo.Commit("main", "Our own base", map[string]string{"Dockerfile": "FROM " + privateBase + "\nRUN echo s3cret-registry-token\n"})

	without := b.request(commit, "chg_01KBZ00000000000000000000A")
	if err := b.builder.Build(b.ctx(), without); err == nil {
		t.Fatal("a private base image pulled with no login")
	}

	req := b.request(commit, "chg_01KBZ00000000000000000000B")
	req.Builder, req.Logins = resource.BuilderDockerfile, []registry.Auth{ghcrLogin}
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Fatal(err)
	}
	if got := b.engine.SentLogins(); !slices.Contains(got, privateBase+" as ci:s3cret-registry-token") {
		t.Errorf("the base image pulled with %q", got)
	}
	for _, l := range b.lines() {
		if strings.Contains(l, "s3cret-registry-token") {
			t.Fatalf("the build's log holds the login's password: %q", l)
		}
	}
	builder := b.made("dockwell-build")[1]
	if !slices.Contains(builder.Env, "DOCKER_CONFIG="+registriesMount) {
		t.Errorf("the builder doesn't read its logins from %s: %q", registriesMount, builder.Env)
	}
	for _, kv := range builder.Env {
		if strings.Contains(kv, "s3cret-registry-token") {
			t.Errorf("the login is in the builder's environment: %s", kv)
		}
	}
	if strings.Contains(strings.Join(builder.Cmd, " "), "s3cret-registry-token") {
		t.Error("the login is in the builder's command")
	}
	entries, err := os.ReadDir(b.builder.SecretsDir)
	if err != nil || len(entries) != 0 {
		t.Errorf("the build's logins are still on the server once it ended: %v %v", entries, err)
	}
}

// Railpack's builder and runtime images are Railpack's, not the app's: a build
// Railpack makes is given no login.
func TestARailpackBuildIsGivenNoLogin(t *testing.T) {
	b := withRailpack(newBench(t))
	commit := b.repo.Commit("main", "A Node app", map[string]string{"package.json": `{"scripts":{"start":"node index.js"}}`, "index.js": "console.log(1)"})
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Builder, req.Logins = resource.BuilderRailpack, []registry.Auth{ghcrLogin}
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Fatal(err)
	}
	if len(b.made("dockwell-build")) == 0 {
		t.Fatal("nothing built")
	}
	for _, c := range b.made("dockwell-build") {
		for _, kv := range c.Env {
			if strings.HasPrefix(kv, "DOCKER_CONFIG=") {
				t.Errorf("a Railpack build was given logins: %s", kv)
			}
		}
	}
}

// Logins need somewhere in memory, as secrets do.
func TestABuildWithLoginsNeedsSomewhereInMemoryForThem(t *testing.T) {
	b := newBench(t)
	b.builder.InMemory = func(string) bool { return false }
	commit := b.repo.Commit("main", "Ours", map[string]string{"Dockerfile": "FROM " + privateBase + "\n"})
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Builder, req.Logins = resource.BuilderDockerfile, []registry.Auth{ghcrLogin}
	err := b.builder.Build(b.ctx(), req)
	if plain, _ := resource.Explain(err); !strings.Contains(plain, "nowhere in memory") {
		t.Errorf("a server with nowhere in memory for logins says %q", plain)
	}
}

var hubLogin = registry.Auth{Name: "hub", Registry: registry.Hub, Username: "acme", Password: "hub-s3cret-token"}

// buildWith builds dockerfile with logins, and says what the builder was given in
// Docker's config.json, and how the build ended.
func buildWith(t *testing.T, b *bench, dockerfile string, args map[string]string, logins ...registry.Auth) ([]string, error) {
	t.Helper()
	commit := b.repo.Commit("main", "Ours", map[string]string{"Dockerfile": dockerfile})
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Builder, req.Logins, req.Args = resource.BuilderDockerfile, logins, args
	err := b.builder.Build(b.ctx(), req)
	return b.engine.GivenBuilders(), err
}

// A build hands BuildKit only the logins of registries its Dockerfile pulls from, by
// FROM, COPY --from or RUN --mount from=: a login it names for another registry stays
// out of the builder, where a step that copies from an image on it would read with it.
// (Security review of #591, H1.)
func TestABuildHandsBuildKitOnlyTheRegistriesItsDockerfilePulls(t *testing.T) {
	b := newBench(t)
	b.engine.Registry[privateBase] = dockertest.Image{Login: "ci:s3cret-registry-token"}
	given, err := buildWith(t, b, "FROM "+privateBase+"\nRUN echo ok\n", nil, ghcrLogin, hubLogin)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(given, []string{"ghcr.io as ci:s3cret-registry-token"}) {
		t.Errorf("a Dockerfile that pulls from ghcr.io alone gave the builder %q", given)
	}

	b = newBench(t)
	b.engine.Registry[privateBase] = dockertest.Image{Login: "ci:s3cret-registry-token"}
	given, err = buildWith(t, b, "FROM "+privateBase+"\nCOPY --from=acme/billing:1 / /srv\nRUN --mount=type=bind,from=ghcr.io/acme/tools:2,target=/t true\n", nil, ghcrLogin, hubLogin)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(given)
	if !slices.Equal(given, []string{"docker.io as acme:hub-s3cret-token", "ghcr.io as ci:s3cret-registry-token"}) {
		t.Errorf("a Dockerfile that copies from Docker Hub gave the builder %q", given)
	}
}

// A login whose registry is on the server's loopback never goes to the builder,
// whose steps could listen there. (Security review of #591, H2.)
func TestABuildNeverHandsOnALoopbackLogin(t *testing.T) {
	for _, host := range []string{"localhost:5000", "127.0.0.1:5000", "[::1]:5000"} {
		b := newBench(t)
		local := registry.Auth{Name: "local", Registry: host, Username: "ci", Password: "loop-s3cret"}
		given, err := buildWith(t, b, "FROM "+host+"/acme/base:1\nRUN echo ok\n", nil, local)
		if err != nil {
			t.Fatal(err)
		}
		if len(given) != 0 {
			t.Errorf("a build from %s gave the builder %q", host, given)
		}
	}
}

// A Dockerfile that picks its own frontend could ask for any image, so a build with
// logins refuses it, and BUILDKIT_SYNTAX too; Docker's own frontend builds. So does a
// Dockerfile whose image a build argument it isn't given names. (Security review of
// #591, M1.)
func TestABuildWithLoginsReadsItsDockerfileWithDockersOwnFrontend(t *testing.T) {
	b := newBench(t)
	b.engine.Registry[privateBase] = dockertest.Image{Login: "ci:s3cret-registry-token"}
	if _, err := buildWith(t, b, "# syntax=docker/dockerfile:1.7\nFROM "+privateBase+"\n", nil, ghcrLogin); err != nil {
		t.Errorf("Docker's own frontend = %v", err)
	}
	// And BuildKit's frontend reads it itself, whatever a directive says, which
	// --opt cmdline= makes it do.
	if cmd := strings.Join(b.made("dockwell-build")[0].Cmd, " "); !strings.Contains(cmd, "--opt cmdline=") {
		t.Errorf("a build with logins can hand itself to another frontend: %s", cmd)
	}
	b = newBench(t)
	if _, err := buildWith(t, b, "# syntax=docker/dockerfile:1.7\nFROM alpine:3\n", nil); err != nil {
		t.Fatal(err)
	}
	if cmd := strings.Join(b.made("dockwell-build")[0].Cmd, " "); strings.Contains(cmd, "cmdline=") {
		t.Errorf("a build without logins was kept to BuildKit's own frontend: %s", cmd)
	}
	for what, c := range map[string]struct {
		dockerfile string
		args       map[string]string
		says       string
	}{
		"its own frontend":       {"# syntax=frontend.example.com/fe:1\nFROM " + privateBase + "\n", nil, "picks its own frontend"},
		"Docker Hub's other one": {"#syntax = acme/dockerfile:1\nFROM " + privateBase + "\n", nil, "picks its own frontend"},
		"BUILDKIT_SYNTAX":        {"FROM " + privateBase + "\n", map[string]string{"BUILDKIT_SYNTAX": "frontend.example.com/fe:1"}, "BUILDKIT_SYNTAX"},
		"an image no one names":  {"ARG BASE\nFROM ${BASE}\n", nil, "build argument"},
	} {
		b := newBench(t)
		b.engine.Registry[privateBase] = dockertest.Image{Login: "ci:s3cret-registry-token"}
		given, err := buildWith(t, b, c.dockerfile, c.args, ghcrLogin)
		if plain, _ := resource.Explain(err); err == nil || !strings.Contains(plain, c.says) {
			t.Errorf("%s: %v", what, err)
		}
		if len(given) != 0 {
			t.Errorf("%s gave the builder %q", what, given)
		}
	}
}

// What a registry said of a login when the step started is asked again once the
// build leaves the queue, and a login it no longer takes stays out of the builder.
// (Security review of #591, L2.)
func TestABuildChecksItsLoginsAgainWhenItLeavesTheQueue(t *testing.T) {
	b := newBench(t)
	commit := b.repo.Commit("main", "Ours", map[string]string{"Dockerfile": "FROM " + privateBase + "\nCOPY --from=acme/billing:1 / /srv\n"})
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Builder, req.Logins = resource.BuilderDockerfile, []registry.Auth{ghcrLogin, hubLogin}
	asked := 0
	req.Recheck = func(_ context.Context, logins []registry.Auth) ([]registry.Auth, error) {
		asked++
		if len(b.builder.slot) != 1 {
			t.Error("the logins were asked again before the build took its slot")
		}
		return logins[1:], nil
	}
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Fatal(err)
	}
	if got := b.engine.GivenBuilders(); asked != 1 || !slices.Equal(got, []string{"docker.io as acme:hub-s3cret-token"}) {
		t.Errorf("asked %d times, the builder had %q", asked, got)
	}

	again := b.repo.Commit("main", "Ours, again", map[string]string{"Dockerfile": "FROM " + privateBase + "\nRUN true\n"})
	req = b.request(again, "chg_01KBZ00000000000000000000B")
	req.Builder, req.Logins = resource.BuilderDockerfile, []registry.Auth{ghcrLogin}
	req.Recheck = func(context.Context, []registry.Auth) ([]registry.Auth, error) {
		return nil, errors.New("ghcr.io moved its token service")
	}
	if err := b.builder.Build(b.ctx(), req); err == nil {
		t.Error("a registry that moved its token service while the build waited built anyway")
	}
}

// The log hides a login's password in each form a step could print it in: alone, as
// username:password, and as Docker's config.json holds it, in base64. (Security review
// of #591, L3.)
func TestABuildsLogHidesTheLoginInEveryForm(t *testing.T) {
	b := newBench(t)
	b.engine.Registry[privateBase] = dockertest.Image{Login: "ci:s3cret-registry-token"}
	pair := "ci:s3cret-registry-token"
	forms := []string{pair, base64.StdEncoding.EncodeToString([]byte(pair)), base64.URLEncoding.EncodeToString([]byte(pair))}
	var dockerfile strings.Builder
	dockerfile.WriteString("FROM " + privateBase + "\n")
	for _, f := range forms {
		dockerfile.WriteString("RUN echo " + f + "\n")
	}
	if _, err := buildWith(t, b, dockerfile.String(), nil, ghcrLogin); err != nil {
		t.Fatal(err)
	}
	for _, l := range b.lines() {
		for _, f := range forms {
			if strings.Contains(l, f) {
				t.Errorf("the log holds the login as %q: %q", f, l)
			}
		}
	}
}

// A test's builder reaches the test's own registry over plain HTTP, and names it in
// BuildKit's configuration only as a host:port can be written; no server sets it.
func TestATestsBuilderReachesItsOwnRegistry(t *testing.T) {
	for registry, want := range map[string]string{
		"":                           "",
		"192.0.2.10:5000":            "DW_PLAIN_REGISTRY=192.0.2.10:5000",
		"registry.example.com":       "DW_PLAIN_REGISTRY=registry.example.com",
		"registry.example.com\"]\nx": "",
	} {
		b := newBench(t)
		b.builder.PlainRegistry = registry
		if _, err := buildWith(t, b, "FROM alpine:3\n", nil); err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, kv := range b.made("dockwell-build")[0].Env {
			if strings.HasPrefix(kv, "DW_PLAIN_REGISTRY=") {
				got = kv
			}
		}
		if got != want {
			t.Errorf("with %q, the builder has %q", registry, got)
		}
	}
	if !strings.Contains(script, `[registry."%s"]\n  http = true\n' "$DW_PLAIN_REGISTRY"`) {
		t.Error("the builder's script doesn't name the test's registry in BuildKit's configuration")
	}
}

// cacheOf is the cache volume the builder of a build mounted.
func cacheOf(t *testing.T, c docker.CreateRequest) string {
	t.Helper()
	for _, m := range c.HostConfig.Mounts {
		if m.Target == cacheMount {
			return m.Source
		}
	}
	t.Fatalf("the builder mounts no cache: %+v", c.HostConfig.Mounts)
	return ""
}

// A build given registry logins keeps a cache of its own, for that service and those
// logins: BuildKit serves an image pinned by digest from what its cache holds, so a
// build with no login, or another service's, would read a private image a login
// pulled. (Re-check of #591.)
func TestABuildWithLoginsKeepsACacheOfItsOwn(t *testing.T) {
	b := newBench(t)
	b.engine.Registry[privateBase] = dockertest.Image{Login: "ci:s3cret-registry-token"}
	n := 0
	build := func(service string, logins ...registry.Auth) string {
		t.Helper()
		n++
		commit := b.repo.Commit("main", fmt.Sprintf("Build %d", n), map[string]string{"Dockerfile": "FROM " + privateBase + "\n", "n": strconv.Itoa(n)})
		req := b.request(commit, fmt.Sprintf("chg_01KBZ0000000000000000000%02d", n))
		req.Service.Name, req.Builder, req.Logins = service, resource.BuilderDockerfile, logins
		err := b.builder.Build(b.ctx(), req)
		if len(logins) > 0 && err != nil {
			t.Fatal(err)
		}
		made := b.made("dockwell-build")
		return cacheOf(t, made[len(made)-1])
	}
	shared := cacheVolume(b.request("", ""))
	api := build("api", ghcrLogin)
	if api == shared {
		t.Fatalf("a build with a login keeps its cache in its environment's, %s", shared)
	}
	if again := build("api", ghcrLogin); again != api {
		t.Errorf("the same service with the same login builds in %s, then %s", api, again)
	}
	for what, got := range map[string]string{
		"another service with no login":    build("docs"),
		"the service itself with no login": build("api"),
	} {
		if got != shared {
			t.Errorf("%s builds in %s, want the environment's %s", what, got, shared)
		}
	}
	for what, got := range map[string]string{
		"another service with the same login": build("docs", ghcrLogin),
		"the service with another login":      build("api", registry.Auth{Name: "ghcr-other", Registry: "ghcr.io", Username: "ci", Password: "s3cret-registry-token"}),
		"the service with one more login":     build("api", ghcrLogin, hubLogin),
	} {
		if got == api || got == shared {
			t.Errorf("%s builds in %s, which another build uses", what, got)
		}
	}
}

// A reading says a Dockerfile of up to 1 MiB whole, which in base64 is more than a
// container's output otherwise keeps, and a longer one refuses a build with logins.
// (Re-check of #591.)
func TestAReadingSaysALargeDockerfileWhole(t *testing.T) {
	big := func(size int) string {
		var d strings.Builder
		d.WriteString("FROM " + privateBase + "\n")
		line := "# " + strings.Repeat("x", 97) + "\n"
		for d.Len() < size {
			d.WriteString(line)
		}
		return d.String()
	}
	b := newBench(t)
	b.engine.Registry[privateBase] = dockertest.Image{Login: "ci:s3cret-registry-token"}
	if given, err := buildWith(t, b, big(900<<10), nil, ghcrLogin); err != nil || len(given) != 1 {
		t.Errorf("a 900 KiB Dockerfile built with %q: %v", given, err)
	}
	b = newBench(t)
	b.engine.Registry[privateBase] = dockertest.Image{Login: "ci:s3cret-registry-token"}
	_, err := buildWith(t, b, big(1100<<10), nil, ghcrLogin)
	if plain, _ := resource.Explain(err); err == nil || !strings.Contains(plain, "The Dockerfile is over") {
		t.Errorf("a 1.1 MiB Dockerfile with a login = %v", err)
	}
}

// The reading says a byte more of a Dockerfile than a build with logins reads, so a
// longer one shows as longer.
func TestTheReadingSaysAByteMoreThanABuildReads(t *testing.T) {
	if want := "head -c " + strconv.Itoa(maxDockerfileText+1) + " | base64"; !strings.Contains(detectScript, want) {
		t.Errorf("the reading's script doesn't read %q", want)
	}
}
