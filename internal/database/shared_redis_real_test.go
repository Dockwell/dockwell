// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// On a real Redis, an app's ACL user signs in with its own password and works with
// keys, but administers nothing, and on a server shared with others runs nothing that
// reaches every key, and with a key prefix reaches no other app's keys or channels; a
// restart loses it, and the agent puts it back. The agent signs in with the password
// the server's command line requires, as no variable holds it, and a Redis that
// requires none gets no user (ADR 0084).
func TestAppsHaveACLUsersOnARealRedis(t *testing.T) {
	client := dockertest.Real(t)
	const image = "redis:8.2-alpine"
	env := dockertest.Scope(t)
	project := docker.Project(env)
	dockertest.Cleanup(t, client, project)
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	id, err := client.CreateContainer(t.Context(), docker.ContainerName(project, "cache"), &docker.CreateRequest{Config: docker.Config{
		Image: image, Cmd: []string{"redis-server", "--requirepass", "the-administrators"},
		Labels: map[string]string{docker.LabelProject: project, docker.LabelService: "cache", docker.LabelNumber: "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	d := &UserDriver{Client: client, Dir: t.TempDir(), Wait: time.Minute, Every: time.Second}
	for app, u := range map[string]*resourcev1.DatabaseUser{
		"shop": {Service: "cache", Password: "P"},
		"blog": {Service: "cache", Password: "P", Shared: true},
		"news": {Service: "cache", Password: "P", Shared: true, KeyPrefix: "news:"},
	} {
		step := resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: env, Name: app}, Action: resource.ActionCreate}
		ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-for-" + app})
		if err := d.Apply(ctx, step, u); err != nil {
			t.Fatalf("making %s's user: %v", app, err)
		}
	}
	// redis-cli as an app, signed in with its own user and password.
	as := func(user string, args ...string) string {
		var out, errs bytes.Buffer
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := append([]string{"redis-cli", "--no-auth-warning", "--user", user, "--pass", "a-password-for-" + user}, args...)
		if _, err := client.Exec(ctx, id, cmd, docker.Streams{Stdout: &out, Stderr: &errs}); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String() + errs.String())
	}
	for _, app := range []string{"shop", "blog"} {
		if got := as(app, "SET", app+":hello", "1"); got != "OK" {
			t.Errorf("%s can't write a key: %s", app, got)
		}
		if got := as(app, "CONFIG", "GET", "requirepass"); !strings.Contains(got, "NOPERM") {
			t.Errorf("%s reads the server's settings: %s", app, got)
		}
	}
	if got := as("shop", "KEYS", "*"); strings.Contains(got, "NOPERM") {
		t.Errorf("shop can't list the keys of its own server: %s", got)
	}
	if got := as("blog", "KEYS", "*"); !strings.Contains(got, "NOPERM") {
		t.Errorf("blog lists every key of a server it shares: %s", got)
	}
	// With its prefix, news keeps to its own keys and channels, and reaches no key
	// name, channel, function or script of anyone else's.
	if got := as("news", "SET", "news:hello", "1"); got != "OK" {
		t.Errorf("news can't write its own key: %s", got)
	}
	for _, args := range [][]string{
		{"GET", "blog:hello"},
		{"SET", "blog:hello", "2"},
		{"PUBLISH", "blog:jobs", "x"},
		{"SCAN", "0"},
		{"RANDOMKEY"},
		{"PUBSUB", "CHANNELS"},
		{"PUBSUB", "NUMSUB", "blog:jobs"},
		{"FUNCTION", "LIST"},
		{"FUNCTION", "FLUSH"},
		{"FUNCTION", "LOAD", "#!lua name=newslib\nredis.register_function('f', function() return 1 end)"},
		{"SCRIPT", "FLUSH"},
	} {
		if got := as("news", args...); !strings.Contains(got, "NOPERM") {
			t.Errorf("news runs %v: %s", args, got)
		}
	}
	// One that keeps to no prefix still reaches news's keys: so a server with one on it
	// reads as its apps reading and changing each other's data, prefix or not.
	if got := as("blog", "GET", "news:hello"); got != "1" {
		t.Errorf("blog, with no prefix, reads %q of news's key", got)
	}
	// The query engine's indexes are no keys, so a shared user has none of it; its
	// JSON keeps to key patterns as any command does.
	admin := func(args ...string) string {
		var out, errs bytes.Buffer
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := append([]string{"redis-cli", "--no-auth-warning", "--pass", "the-administrators"}, args...)
		if _, err := client.Exec(ctx, id, cmd, docker.Streams{Stdout: &out, Stderr: &errs}); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String() + errs.String())
	}
	if got := admin("FT.CREATE", "blogidx", "ON", "HASH", "PREFIX", "1", "blog:", "SCHEMA", "title", "TEXT"); got == "OK" {
		admin("HSET", "blog:post", "title", "hello")
		if got := as("news", "FT.SEARCH", "blogidx", "hello"); !strings.Contains(got, "NOPERM") {
			t.Errorf("news searches blog's index: %s", got)
		}
		if got := as("news", "FT._LIST"); !strings.Contains(got, "NOPERM") {
			t.Errorf("news lists the indexes: %s", got)
		}
	} else {
		t.Logf("this Redis has no query engine: %s", got)
	}
	if got := admin("JSON.SET", "blog:doc", "$", `{"a":1}`); got == "OK" {
		if got := as("news", "JSON.GET", "blog:doc"); !strings.Contains(got, "NOPERM") {
			t.Errorf("news reads blog's JSON: %s", got)
		}
	}

	// A restart loses them; the agent gives them back.
	if err := client.StopContainer(t.Context(), id, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	started := map[string]time.Time{}
	for range 30 {
		d.putRedisUsersBack(t.Context(), started)
		if as("shop", "PING") == "PONG" && as("blog", "PING") == "PONG" {
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("after a restart, shop says %q and blog %q", as("shop", "PING"), as("blog", "PING"))
}

// A real Redis that requires no password gets no user of an app's own: the agent
// refuses it as the plan is made (ADR 0084).
func TestARealRedisWithoutAPasswordGetsNoUser(t *testing.T) {
	client := dockertest.Real(t)
	const image = "redis:8.2-alpine"
	env := dockertest.Scope(t)
	project := docker.Project(env)
	dockertest.Cleanup(t, client, project)
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	id, err := client.CreateContainer(t.Context(), docker.ContainerName(project, "cache"), &docker.CreateRequest{Config: docker.Config{
		Image: image, Cmd: []string{"redis-server"},
		Labels: map[string]string{docker.LabelProject: project, docker.LabelService: "cache", docker.LabelNumber: "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	d := &UserDriver{Client: client, Dir: t.TempDir(), Wait: time.Minute, Every: time.Second}
	ref := resource.Ref{Kind: UserKind, Scope: env, Name: "shop"}
	var err2 error
	for range 30 {
		if _, err2 = d.Judge(t.Context(), ref, nil, &resourcev1.DatabaseUser{Service: "cache", Password: "P"}, nil); err2 == nil || !errors.Is(err2, ErrStarting) {
			break
		}
		time.Sleep(time.Second)
	}
	if err2 == nil || !strings.Contains(err2.Error(), "asks no one for a password") {
		t.Errorf("planning a user on a Redis that requires no password = %v", err2)
	}
}
