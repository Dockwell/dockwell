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

// redisImages are the releases a user's rules are made for, each of which must take
// every rule Dockwell gives: Redis before 7 takes no rule for a subcommand, but for
// adding one back; 8 runs the query engine and JSON.
var redisImages = []string{
	"redis:6.2-alpine", "redis:7.2-alpine", "redis:7.4-alpine", "redis:8.2-alpine",
	"valkey/valkey:7.2-alpine", "valkey/valkey:8-alpine",
}

// On real Redis and Valkey releases, an app's ACL user signs in with its own password
// and works with keys, but administers nothing; on a server shared with others it runs
// nothing that reaches every key, and with a key prefix reaches no other app's keys,
// channels, key names, functions or scripts, nor the query engine's indexes, while
// clients that load their scripts still can; one with no prefix reaches the prefixed
// one's keys, which is why its server's apps read as reaching each other's data. A
// restart loses them, and the agent puts them back. The agent signs in with the
// password the server's command line requires, as no variable holds it (ADR 0084).
func TestAppsHaveRedisACLUsersOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	for _, image := range redisImages {
		t.Run(image, func(t *testing.T) { appsHaveACLUsers(t, client, image) })
	}
}

func appsHaveACLUsers(t *testing.T, client *docker.Client, image string) {
	seven := !strings.HasPrefix(image, "redis:6")
	modules := strings.HasPrefix(image, "redis:8")
	env := dockertest.Scope(t)
	project := docker.Project(env)
	dockertest.Cleanup(t, client, project)
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	server := "redis-server"
	if strings.HasPrefix(image, "valkey/") {
		server = "valkey-server"
	}
	id, err := client.CreateContainer(t.Context(), docker.ContainerName(project, "cache"), &docker.CreateRequest{Config: docker.Config{
		Image: image, Cmd: []string{server, "--requirepass", "the-administrators"},
		Labels: map[string]string{docker.LabelProject: project, docker.LabelService: "cache", docker.LabelNumber: "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	d := &UserDriver{Client: client, Dir: t.TempDir(), Wait: time.Minute, Every: time.Second}
	// Each user's rules load, or making it fails.
	for _, app := range []string{"shop", "blog", "news"} {
		u := map[string]*resourcev1.DatabaseUser{
			"shop": {Service: "cache", Password: "P"},
			"blog": {Service: "cache", Password: "P", Shared: true},
			"news": {Service: "cache", Password: "P", Shared: true, KeyPrefix: "news:"},
		}[app]
		step := resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: env, Name: app}, Action: resource.ActionCreate}
		ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-for-" + app})
		if err := d.Apply(ctx, step, u); err != nil {
			t.Fatalf("making %s's user: %v", app, err)
		}
	}
	cli := func(args ...string) string {
		var out, errs bytes.Buffer
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := append([]string{"sh", "-c", `c=redis-cli; command -v valkey-cli >/dev/null 2>&1 && c=valkey-cli; exec "$c" --no-auth-warning "$@"`, "cli"}, args...)
		if _, err := client.Exec(ctx, id, cmd, docker.Streams{Stdout: &out, Stderr: &errs}); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String() + errs.String())
	}
	as := func(user string, args ...string) string {
		return cli(append([]string{"--user", user, "--pass", "a-password-for-" + user}, args...)...)
	}
	admin := func(args ...string) string { return cli(append([]string{"--pass", "the-administrators"}, args...)...) }
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
	denied := [][]string{
		{"GET", "blog:hello"},
		{"SET", "blog:hello", "2"},
		{"PUBLISH", "blog:jobs", "x"},
		{"SCAN", "0"},
		{"RANDOMKEY"},
		{"PUBSUB", "CHANNELS"},
		{"PUBSUB", "NUMSUB", "blog:jobs"},
		{"SCRIPT", "FLUSH"},
	}
	if seven {
		denied = append(denied, []string{"FUNCTION", "LIST"}, []string{"FUNCTION", "FLUSH"},
			[]string{"FUNCTION", "LOAD", "#!lua name=newslib\nredis.register_function('f', function() return 1 end)"},
			[]string{"CLIENT", "TRACKING", "on"})
	}
	for _, args := range denied {
		if got := as("news", args...); !strings.Contains(got, "NOPERM") {
			t.Errorf("news runs %v: %s", args, got)
		}
	}
	// Clients that load their scripts still can, and ask whether they have.
	sha := as("news", "SCRIPT", "LOAD", "return 1")
	if strings.Contains(sha, "NOPERM") || len(sha) != 40 {
		t.Errorf("news can't load a script: %s", sha)
	}
	if got := as("news", "SCRIPT", "EXISTS", sha); got != "1" {
		t.Errorf("news can't ask whether it loaded its script: %s", got)
	}
	// One that keeps to no prefix still reaches news's keys: so a server with one on it
	// reads as its apps reading and changing each other's data, prefix or not.
	if got := as("blog", "GET", "news:hello"); got != "1" {
		t.Errorf("blog, with no prefix, reads %q of news's key", got)
	}
	// The query engine's indexes are no keys, so a shared user has none of it; its
	// JSON keeps to key patterns as any command does.
	if modules {
		if got := admin("FT.CREATE", "blogidx", "ON", "HASH", "PREFIX", "1", "blog:", "SCHEMA", "title", "TEXT"); got != "OK" {
			t.Fatalf("this Redis 8 has no query engine: %s", got)
		}
		admin("HSET", "blog:post", "title", "hello")
		for _, args := range [][]string{{"FT.SEARCH", "blogidx", "hello"}, {"FT._LIST"}, {"FT.DROPINDEX", "blogidx"}} {
			if got := as("news", args...); !strings.Contains(got, "NOPERM") {
				t.Errorf("news runs %v: %s", args, got)
			}
		}
		if got := admin("JSON.SET", "blog:doc", "$", `{"a":1}`); got != "OK" {
			t.Fatalf("this Redis 8 has no JSON: %s", got)
		}
		if got := as("news", "JSON.GET", "blog:doc"); !strings.Contains(got, "NOPERM") {
			t.Errorf("news reads blog's JSON: %s", got)
		}
		if got := as("news", "JSON.SET", "news:doc", "$", `{"a":1}`); got != "OK" {
			t.Errorf("news can't write its own JSON: %s", got)
		}
		// A time series is found by its labels past any key pattern.
		if got := admin("TS.CREATE", "blog:temp", "LABELS", "app", "blog"); got != "OK" {
			t.Fatalf("this Redis 8 has no time series: %s", got)
		}
		for _, args := range [][]string{{"TS.QUERYINDEX", "app=blog"}, {"TS.MGET", "FILTER", "app=blog"}, {"TS.MRANGE", "-", "+", "FILTER", "app=blog"}} {
			if got := as("news", args...); !strings.Contains(got, "NOPERM") {
				t.Errorf("news runs %v: %s", args, got)
			}
		}
	}

	// A user made by hand keeps apps from keeping apart, whatever its rules, as do shop
	// and blog, which keep to no prefix: making a user where the plan said they keep
	// apart is refused, naming them.
	if got := admin("ACL", "SETUSER", "jobs", "on", ">a-password-for-jobs", "~jobs:*", "resetchannels", "&jobs:*", "+@all"); got != "OK" {
		t.Fatalf("making jobs by hand: %s", got)
	}
	feed := resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: env, Name: "feed"}, Action: resource.ActionCreate}
	ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-for-feed"})
	err = d.Apply(ctx, feed, &resourcev1.DatabaseUser{Service: "cache", Password: "P", Shared: true, KeyPrefix: "feed:", KeepsApart: true})
	if err == nil || !strings.Contains(err.Error(), "jobs") || !strings.Contains(err.Error(), "are there now, which Dockwell didn't make with a prefix") {
		t.Errorf("making feed beside jobs, made by hand = %v", err)
	}
	if got := admin("ACL", "DELUSER", "jobs"); got != "1" {
		t.Fatalf("taking jobs away: %s", got)
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
		if as("shop", "PING") == "PONG" && as("blog", "PING") == "PONG" && as("news", "PING") == "PONG" {
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("after a restart, shop says %q, blog %q and news %q", as("shop", "PING"), as("blog", "PING"), as("news", "PING"))
}

// A real Redis that requires no password gets no user of an app's own: the agent
// refuses it as the plan is made (ADR 0084).
func TestARedisWithoutAPasswordGetsNoUserOnRealDocker(t *testing.T) {
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
