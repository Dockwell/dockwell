// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"cmp"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// fakeRedis stands in for redis-cli on a Redis: it keeps the ACL users each command
// sets, by name, with their rules, as the server holds them until it restarts, and
// what it saved to its ACL file where it keeps one.
type fakeRedis struct {
	mu      sync.Mutex
	users   map[string]string
	aclfile string
	saved   int
	old     bool
	said    []string
	// version is what INFO says it runs, 8.2.0 unless set; nopass has its default
	// user ask no one for a password; noauth refuses everything, as a server whose
	// password changed does.
	version        string
	nopass, noauth bool
	// other is a user of its ACL file that asks no password; search, that it runs
	// the query engine.
	other, search bool
	// wide is a user made by hand that reaches every key.
	wide bool
}

func (f *fakeRedis) exec(x dockertest.Exec) int {
	b, _ := io.ReadAll(x.Stdin)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.said = append(f.said, string(b))
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Fields(line)
		switch {
		case f.noauth:
			_, _ = io.WriteString(x.Stdout, "NOAUTH Authentication required.\n")
		case len(fields) > 0 && fields[0] == "AUTH":
			_, _ = io.WriteString(x.Stdout, "OK\n")
		case line == "INFO server":
			_, _ = io.WriteString(x.Stdout, "# Server\nredis_version:"+cmp.Or(f.version, "8.2.0")+"\n")
		case len(fields) >= 2 && fields[0] == "ACL" && f.old:
			_, _ = io.WriteString(x.Stdout, "ERR unknown command 'ACL', with args beginning with: '"+fields[1]+"'\n")
		case len(fields) >= 3 && fields[0] == "ACL" && fields[1] == "SETUSER":
			f.users[fields[2]] = strings.Join(fields[3:], " ")
			_, _ = io.WriteString(x.Stdout, "OK\n")
		case line == "ACL LIST" && !f.old:
			def := "user default on sanitize-payload #0123 ~* &* +@all"
			if f.nopass {
				def = "user default on nopass sanitize-payload ~* &* +@all"
			}
			_, _ = io.WriteString(x.Stdout, def+"\n")
			if f.other {
				_, _ = io.WriteString(x.Stdout, "user reports on nopass ~reports:* +@read\n")
			}
			if f.wide {
				_, _ = io.WriteString(x.Stdout, "user ops on #0456 ~* &* +@all\n")
			}
		case line == "MODULE LIST":
			if f.search {
				_, _ = io.WriteString(x.Stdout, "name\nsearch\nver\n80200\n")
			}
		case len(fields) == 3 && fields[0] == "ACL" && fields[1] == "GETUSER":
			if rules, ok := f.users[fields[2]]; ok {
				_, _ = io.WriteString(x.Stdout, "flags\non\npasswords\n"+rules+"\n")
			} else {
				_, _ = io.WriteString(x.Stdout, "\n")
			}
		case len(fields) == 3 && fields[0] == "ACL" && fields[1] == "DELUSER":
			delete(f.users, fields[2])
			_, _ = io.WriteString(x.Stdout, "1\n")
		case len(fields) == 2 && fields[0] == "ACL" && fields[1] == "SAVE":
			f.saved++
			_, _ = io.WriteString(x.Stdout, "OK\n")
		case line == "CONFIG GET aclfile":
			_, _ = io.WriteString(x.Stdout, "aclfile\n"+f.aclfile+"\n")
		}
	}
	return 0
}

func redisStand(t *testing.T) (*UserDriver, *fakeRedis, *docker.Client, string) {
	t.Helper()
	r := &fakeRedis{users: map[string]string{}}
	e := dockertest.New(t)
	e.AddLocalImage("redis:8.2", dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}, Exec: r.exec})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_1-cache-1", &docker.CreateRequest{Config: docker.Config{
		Image: "redis:8.2", Labels: map[string]string{docker.LabelProject: docker.Project("env_1"), docker.LabelService: "cache", docker.LabelNumber: "1"},
		Env: []string{"REDIS_PASSWORD=the-administrators"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return &UserDriver{Client: client, Dir: t.TempDir()}, r, client, id
}

// On Redis 6 and later, an app's own user is an ACL user signed in with its own
// password, given as its hash, that can't administer the server, and on a server
// other apps share, can't reach past its keys either; it's saved where the server
// keeps an ACL file, goes when deleted, and the agent puts it back on a server that
// started again without it. A Redis before 6 has no users to give (ADR 0084).
func TestARedisUserIsAnACLUserOfItsOwn(t *testing.T) {
	d, r, client, id := redisStand(t)
	ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}
	desired := &resourcev1.DatabaseUser{Service: "cache", Password: "SHOP_CACHE_PASSWORD"}
	ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-no-one-typed"})
	apply := func(a resource.Action, u *resourcev1.DatabaseUser) {
		t.Helper()
		if err := d.Apply(ctx, resource.Step{Ref: ref, Action: a}, u); err != nil {
			t.Fatal(err)
		}
	}
	apply(resource.ActionCreate, desired)
	hash := redisHash("a-password-no-one-typed")
	if got := r.users["shop"]; got != "reset on #"+hash+" ~* &* +@all -@admin" {
		t.Errorf("shop's rules on its own Redis are %q", got)
	}
	if all := strings.Join(r.said, ""); strings.Contains(all, "a-password-no-one-typed") {
		t.Errorf("the password went to the server as itself: %q", all)
	}
	if r.saved != 0 {
		t.Error("it saved to an ACL file the server doesn't keep")
	}
	if err := d.Verify(t.Context(), ref, desired); err != nil {
		t.Errorf("checked as %v", err)
	}

	// Shared, it may do only what an app does with its data, and nothing past its
	// keys, and it's saved where the server keeps a file.
	r.aclfile = "/data/users.acl"
	shared := &resourcev1.DatabaseUser{Service: "cache", Password: "SHOP_CACHE_PASSWORD", Shared: true}
	if got, err := d.Observe(t.Context(), ref); err != nil || got.(*resourcev1.DatabaseUser).GetShared() {
		t.Fatalf("read as %v, %v", got, err)
	}
	apply(resource.ActionUpdate, shared)
	if got := r.users["shop"]; got != "reset on #"+hash+" ~* &* "+redisShared+" -function -script|flush -script|kill -pubsub|channels -pubsub|numsub -pubsub|shardchannels -pubsub|shardnumsub -client|tracking" || r.saved != 1 {
		t.Errorf("shared, shop's rules are %q, saved %d times", got, r.saved)
	}
	// With a key prefix, it keeps to its keys and channels.
	prefixed := &resourcev1.DatabaseUser{Service: "cache", Password: "SHOP_CACHE_PASSWORD", Shared: true, KeyPrefix: "shop:"}
	apply(resource.ActionUpdate, prefixed)
	if got := r.users["shop"]; !strings.Contains(got, " ~shop:* &shop:* -@all ") {
		t.Errorf("with a prefix, shop's rules are %q", got)
	}
	if got, err := d.Observe(t.Context(), ref); err != nil || got.(*resourcev1.DatabaseUser).GetKeyPrefix() != "shop:" {
		t.Errorf("read as %v, %v", got, err)
	}
	apply(resource.ActionUpdate, shared)

	// A restart loses it; the agent gives it back, once, as it was.
	if err := client.StopContainer(t.Context(), id, time.Second); err != nil {
		t.Fatal(err)
	}
	r.users = map[string]string{}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	started := map[string]time.Time{}
	d.putRedisUsersBack(t.Context(), started)
	if got := r.users["shop"]; !strings.Contains(got, " -@admin -@dangerous -scan") || !strings.Contains(got, "#"+hash) {
		t.Errorf("after a restart, shop's rules are %q", got)
	}
	if len(started) != 1 {
		t.Errorf("it keeps %d servers' starts", len(started))
	}
	delete(r.users, "shop")
	d.putRedisUsersBack(t.Context(), started)
	if _, back := r.users["shop"]; back {
		t.Error("it looked again at a server that hadn't started again")
	}

	// Taking it away deletes no key, which the plan says.
	dec, err := d.Judge(t.Context(), ref, shared, nil, nil)
	if err != nil || dec.Destroys || dec.KeysLeft != "*" {
		t.Errorf("deleting it plans %+v, %v", dec, err)
	}
	if err := d.Apply(ctx, resource.Step{Ref: ref, Action: resource.ActionDelete}, nil); err != nil {
		t.Fatal(err)
	}
	if _, has := r.users["shop"]; has {
		t.Error("deleting it left the user")
	}
	// What a step dropped isn't put back, whatever the loop read before.
	if err := d.putRedisUserBack(t.Context(), mustContainer(t, client, id), Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS), ref.String(), redisServer{Subcommands: true}); err != nil {
		t.Fatal(err)
	}
	if _, has := r.users["shop"]; has {
		t.Error("a dropped user was put back")
	}

	r.old = true
	if err := d.Apply(ctx, resource.Step{Ref: ref, Action: resource.ActionCreate}, desired); err == nil || !strings.Contains(err.Error(), "keeps one password before 6") {
		t.Errorf("a Redis before 6 = %v", err)
	}
	r.old = false

	// A Redis that asks no one for a password, or one before 6.2, gets no user, at
	// plan time or after.
	for _, tc := range []struct {
		version       string
		nopass, other bool
		want          string
	}{
		{nopass: true, want: "asks no one for a password"},
		{other: true, want: "has a user reports that asks no password"},
		{version: "6.0.20", want: "Upgrade it to 6.2 or later"},
	} {
		r.version, r.nopass, r.other = tc.version, tc.nopass, tc.other
		if _, err := d.Judge(t.Context(), ref, nil, desired, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("planning on %+v = %v", tc, err)
		}
		if err := d.Apply(ctx, resource.Step{Ref: ref, Action: resource.ActionCreate}, desired); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("applying on %+v = %v", tc, err)
		}
		if _, has := r.users["shop"]; has {
			t.Errorf("%+v got a user", tc)
		}
	}
}

// A shared user's rules follow its server: on 6.2, which takes no rule for a
// subcommand, SCRIPT and PUBSUB go whole; where the query engine runs, so does it,
// as its indexes are no keys a pattern keeps an app out of.
func TestASharedRedisUsersRulesFollowItsServer(t *testing.T) {
	if got := redisRules("h", true, "shop:", redisServer{}); !strings.HasSuffix(got, " -scan -randomkey -script +script|load +script|exists -pubsub") || !strings.Contains(got, " ~shop:* &shop:* ") {
		t.Errorf("on 6.2: %q", got)
	}
	got := redisRules("h", true, "shop:", redisServer{Subcommands: true, Search: true, Timeseries: true})
	for _, want := range []string{" -function ", " -script|flush ", " -pubsub|channels ", " -client|tracking ", " -scan ", " -randomkey ", " -@search ", " -ts.queryindex "} {
		if !strings.Contains(got+" ", want) {
			t.Errorf("on 8 with search, %q lacks %q", got, want)
		}
	}
	if got := redisRules("h", false, "", redisServer{Search: true}); got != "reset on #h ~* &* +@all -@admin" {
		t.Errorf("its own server's: %q", got)
	}
}

func mustContainer(t *testing.T, client *docker.Client, id string) *docker.Container {
	t.Helper()
	c, err := client.InspectContainer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A server that refuses the agent, as one whose password changed does, is looked at
// again on the next round, not once and forgotten.
func TestARedisThatRefusesIsTriedAgain(t *testing.T) {
	d, r, _, _ := redisStand(t)
	ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}
	ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-no-one-typed"})
	if err := d.Apply(ctx, resource.Step{Ref: ref, Action: resource.ActionCreate}, &resourcev1.DatabaseUser{Service: "cache", Password: "P"}); err != nil {
		t.Fatal(err)
	}
	r.users, r.noauth = map[string]string{}, true
	started := map[string]time.Time{}
	d.putRedisUsersBack(t.Context(), started)
	if len(started) != 0 {
		t.Error("a server that refused counts as looked at")
	}
	r.noauth = false
	d.putRedisUsersBack(t.Context(), started)
	if _, back := r.users["shop"]; !back {
		t.Error("the user wasn't put back once the server let the agent in")
	}
}

// A Redis whose command line alone requires its password is signed in to with it,
// sent on stdin, never as an argument.
func TestARedisCommandLinePasswordGoesOnStdin(t *testing.T) {
	r := &fakeRedis{users: map[string]string{}}
	var cmds [][]string
	e := dockertest.New(t)
	e.AddLocalImage("redis:8.2", dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}, Exec: func(x dockertest.Exec) int {
		cmds = append(cmds, x.Cmd)
		return r.exec(x)
	}})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_1-cache-1", &docker.CreateRequest{Config: docker.Config{
		Image: "redis:8.2", Cmd: []string{"redis-server", "--requirepass", `the "administrator's"`},
		Labels: map[string]string{docker.LabelProject: docker.Project("env_1"), docker.LabelService: "cache", docker.LabelNumber: "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	d := &UserDriver{Client: client, Dir: t.TempDir()}
	ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-no-one-typed"})
	if err := d.Apply(ctx, resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}, Action: resource.ActionCreate}, &resourcev1.DatabaseUser{Service: "cache", Password: "P"}); err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(r.said[0], "\n"); first != `AUTH "the \"administrator's\""` {
		t.Errorf("signed in with %q", first)
	}
	for _, c := range cmds {
		if strings.Contains(strings.Join(c, " "), "administrator") {
			t.Errorf("the password was an argument: %q", c)
		}
	}
	if _, ok := r.users["shop"]; !ok {
		t.Error("no user was made")
	}
}

// Which ACL users reach past one prefix of their own, by what ACL LIST says of them.
func TestWhichRedisUsersReachPastAPrefix(t *testing.T) {
	for rules, want := range map[string]bool{
		"on #h ~shop:* &shop:* -@all +@read":   false,
		"on #h ~shop:* resetchannels +@read":   false,
		"on #h ~* &* +@all":                    true,
		"on #h allkeys allchannels +@all":      true,
		"on #h ~shop:* ~blog:* &shop:* +@read": true,
		"on #h ~shop:* &* +@read":              true,
		"on #h %R~shop:* &shop:* +@read":       true,
		"on #h ~sh?p:* &shop:* +@read":         true,
		"off resetkeys resetchannels -@all":    false,
	} {
		if got := RedisReachesPast(strings.Fields(rules)); got != want {
			t.Errorf("%q reaches past a prefix: %t, want %t", rules, got, want)
		}
	}
}

// A user's rules are made again for what its server says now: once it runs the query
// engine, a shared user is held from it, on the agent's next look, without a restart.
// Where the plan said apps on the server keep to their own keys, making the user, or
// giving it its password anew, refuses a server where a user made by hand reaches
// every key by then, and one where a user asks no password (ADR 0084).
func TestARedisUsersRulesFollowItsServer(t *testing.T) {
	d, r, _, _ := redisStand(t)
	ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}
	ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-no-one-typed"})
	shared := &resourcev1.DatabaseUser{Service: "cache", Password: "P", Shared: true, KeyPrefix: "shop:", KeepsApart: true}
	if err := d.Apply(ctx, resource.Step{Ref: ref, Action: resource.ActionCreate}, shared); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.users["shop"], "@search") {
		t.Fatalf("made without the query engine, shop's rules are %q", r.users["shop"])
	}
	started := map[string]time.Time{}
	d.putRedisUsersBack(t.Context(), started)
	r.search = true
	d.putRedisUsersBack(t.Context(), started)
	if !strings.HasSuffix(r.users["shop"], " -@search") {
		t.Errorf("once the server runs the query engine, shop's rules are %q", r.users["shop"])
	}

	r.wide = true
	if err := d.Apply(ctx, resource.Step{Ref: ref, Action: resource.ActionUpdate}, shared); err == nil || !strings.Contains(err.Error(), "ops now reaches past a prefix of its own") {
		t.Errorf("giving shop its password anew beside a user that reaches every key = %v", err)
	}
	plain := &resourcev1.DatabaseUser{Service: "cache", Password: "P", Shared: true}
	if err := d.Apply(ctx, resource.Step{Ref: ref, Action: resource.ActionUpdate}, plain); err != nil {
		t.Errorf("where the plan said nothing of keeping apart = %v", err)
	}
	r.wide, r.nopass = false, true
	if err := d.Apply(ctx, resource.Step{Ref: ref, Action: resource.ActionUpdate}, plain); err == nil || !strings.Contains(err.Error(), "asks no one for a password") {
		t.Errorf("giving shop its password anew on a server that asks none = %v", err)
	}
}
