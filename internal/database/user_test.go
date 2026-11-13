// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"cmp"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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

// fakePostgres keeps roles and databases as psql's \gexec would make them, and can
// be told to take SQL and make nothing, or to fail what makes a database.
type fakePostgres struct {
	mu        sync.Mutex
	roles     map[string]bool
	databases map[string]string // a database's owner
	makeNone  bool
	failDB    bool
	said      []string
	// starting is how many more times it answers as a server that's starting.
	starting int
	// dump is what its pg_dump writes, by database, and loaded what its psql was
	// given to load, by database; loads are the environment and the arguments of each
	// load, and loadFails fails each after it reads what it's given.
	dump      map[string]string
	loaded    map[string]string
	loads     []fakeLoad
	loadFails bool
	// psql is what its psql -V says: 17.6 unless set. trusts lets anyone in with no
	// password from any host, as POSTGRES_HOST_AUTH_METHOD=trust does; from loopback
	// it always does, as the official image's pg_hba.conf has it. superuser and shell
	// say a load became the administrator, or ran a shell, as a \connect or a \! it
	// was given would have it; onLoad runs as each load starts.
	psql     string
	dumpedAs []string
	// probeSays is what a sign-in with no password says, where it says something
	// other than that a password is needed.
	probeSays string
	trusts    bool
	superuser bool
	shell     bool
	onLoad    func()
}

// fakeLoad is how a load ran: its environment and the arguments after its script.
type fakeLoad struct {
	env  []string
	args []string
}

var (
	fakeCreateRole = regexp.MustCompile(`SELECT 'CREATE ROLE "([a-z0-9_]+)" LOGIN'`)
	fakeCreateDB   = regexp.MustCompile(`SELECT 'CREATE DATABASE "([a-z0-9_]+)" OWNER "([a-z0-9_]+)"'`)
	fakeAsk        = regexp.MustCompile(`'user', EXISTS \(SELECT 1 FROM pg_roles WHERE rolname = '([a-z0-9_]+)'\), 'owner', coalesce\(\(SELECT pg_get_userbyid\(datdba\) FROM pg_database WHERE datname = '([a-z0-9_]+)'`)
	fakeDropDB     = regexp.MustCompile(`DROP DATABASE IF EXISTS "([a-z0-9_]+)"`)
	fakeDropRole   = regexp.MustCompile(`DROP ROLE IF EXISTS "([a-z0-9_]+)"`)
	fakeOwner      = regexp.MustCompile(`json_build_object\('owner', r.rolname\) FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE d.datname = '([a-z0-9_]+)'`)
)

func (f *fakePostgres) exec(x dockertest.Exec) int {
	script := strings.Join(x.Cmd, " ")
	switch {
	case strings.Contains(script, "pg_dump"):
		f.mu.Lock()
		defer f.mu.Unlock()
		// pg_dump's arguments are the database, then who it runs as.
		f.dumpedAs = append(f.dumpedAs, x.Cmd[len(x.Cmd)-1])
		_, _ = io.WriteString(x.Stdout, f.dump[x.Cmd[len(x.Cmd)-2]])
		return 0
	case slices.Equal(x.Cmd, []string{"psql", "-V"}):
		f.mu.Lock()
		defer f.mu.Unlock()
		_, _ = io.WriteString(x.Stdout, cmp.Or(f.psql, "psql (PostgreSQL) 17.6 (Debian 17.6-1.pgdg120+1)")+"\n")
		return 0
	case strings.Contains(script, "psql") && strings.Contains(script, " -w ") && strings.Contains(script, "SELECT 1"):
		f.mu.Lock()
		defer f.mu.Unlock()
		// Its arguments end with the address it dials, then the database.
		if f.probeSays != "" {
			_, _ = io.WriteString(x.Stderr, f.probeSays+"\n")
			return 2
		}
		if f.trusts || x.Cmd[len(x.Cmd)-2] == "127.0.0.1" {
			return 0
		}
		_, _ = io.WriteString(x.Stderr, "psql: error: fe_sendauth: no password supplied\n")
		return 2
	case strings.Contains(script, "psql") && strings.Contains(script, `-d "$d"`):
		if f.onLoad != nil {
			f.onLoad()
		}
		b, _ := io.ReadAll(x.Stdin)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.loaded == nil {
			f.loaded = map[string]string{}
		}
		args := x.Cmd[4:]
		f.loads = append(f.loads, fakeLoad{env: x.Env, args: args})
		// What psql does with the client commands it's given.
		restricted, kept := "", []string{}
		for i, line := range strings.SplitAfter(string(b), "\n") {
			cmd := strings.TrimSpace(line)
			switch {
			case i == 0 && strings.HasPrefix(cmd, `\restrict `):
				restricted = strings.TrimPrefix(cmd, `\restrict `)
				continue
			case !strings.HasPrefix(cmd, `\`):
				kept = append(kept, line)
				continue
			case restricted != "" && cmd != `\unrestrict `+restricted:
				_, _ = io.WriteString(x.Stderr, "psql:<stdin>:"+strconv.Itoa(i+1)+": error: backslash commands are restricted; only \\unrestrict is allowed\n")
				return 3
			case strings.HasPrefix(cmd, `\connect - postgres`):
				if f.trusts || args[3] == "127.0.0.1" {
					f.superuser = true
					continue
				}
				_, _ = io.WriteString(x.Stderr, "psql:<stdin>:"+strconv.Itoa(i+1)+": error: connection to server failed: FATAL:  password authentication failed for user \"postgres\"\n")
				return 2
			case strings.HasPrefix(cmd, `\!`):
				f.shell = true
			}
		}
		b = []byte(strings.Join(kept, ""))
		if f.loadFails {
			_, _ = io.WriteString(x.Stderr, "psql:<stdin>:2: ERROR:  relation \"posts\" already exists\n")
			return 3
		}
		f.loaded[args[1]] = string(b)
		return 0
	}
	b, _ := io.ReadAll(x.Stdin)
	sql := string(b)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.said = append(f.said, sql)
	if f.starting > 0 {
		f.starting--
		_, _ = io.WriteString(x.Stderr, "psql: error: connection to server on socket \"/var/run/postgresql/.s.PGSQL.5432\" failed: FATAL:  the database system is shutting down\n")
		return 2
	}
	if m := fakeOwner.FindStringSubmatch(sql); m != nil {
		_, _ = io.WriteString(x.Stdout, `{"owner":"`+f.databases[m[1]]+`"}`+"\n")
		return 0
	}
	if m := fakeAsk.FindStringSubmatch(sql); m != nil {
		_, _ = io.WriteString(x.Stdout, `{"user":`+map[bool]string{true: "true", false: "false"}[f.roles[m[1]]]+`,"owner":"`+f.databases[m[2]]+`"}`+"\n")
		return 0
	}
	if f.makeNone {
		return 0
	}
	for _, m := range fakeCreateRole.FindAllStringSubmatch(sql, -1) {
		f.roles[m[1]] = true
	}
	for _, m := range fakeCreateDB.FindAllStringSubmatch(sql, -1) {
		if f.failDB {
			_, _ = io.WriteString(x.Stderr, "psql:<stdin>:4: ERROR:  could not write to file \"base/16384/PG_VERSION\": No space left on device\n")
			return 3
		}
		if _, has := f.databases[m[1]]; !has {
			f.databases[m[1]] = m[2]
		}
	}
	for _, m := range fakeDropDB.FindAllStringSubmatch(sql, -1) {
		delete(f.databases, m[1])
	}
	for _, m := range fakeDropRole.FindAllStringSubmatch(sql, -1) {
		delete(f.roles, m[1])
	}
	return 0
}

// userStand is a PostgreSQL service's container on a fake Docker Engine, and a
// driver that makes users in it.
func userStand(t *testing.T) (*UserDriver, *fakePostgres, *dockertest.Engine) {
	t.Helper()
	pg := &fakePostgres{roles: map[string]bool{"postgres": true}, databases: map[string]string{"postgres": "postgres"}}
	e := dockertest.New(t)
	e.AddLocalImage("postgres:17", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: pg.exec})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_1-db-1", &docker.CreateRequest{Config: docker.Config{
		Image: "postgres:17", Labels: map[string]string{docker.LabelProject: "env_1", docker.LabelService: "db", docker.LabelNumber: "1"},
		Env: []string{"POSTGRES_PASSWORD=the-administrators"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return &UserDriver{Client: client, Dir: t.TempDir()}, pg, e
}

func makeUser(ctx context.Context, d *UserDriver, name string) error {
	step := resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: "env_1", Name: name}, Action: resource.ActionCreate}
	ctx = resource.WithValues(ctx, map[string]string{passwordUse: "a-password-no-one-typed"})
	return d.Apply(ctx, step, &resourcev1.DatabaseUser{Service: "db", Password: "SHOP_DATABASE_PASSWORD"})
}

// A server that takes the SQL and makes nothing fails the step, and says so, rather
// than reading as applied.
func TestAStepThatMakesNoUserFails(t *testing.T) {
	d, pg, _ := userStand(t)
	pg.makeNone = true
	err := makeUser(t.Context(), d, "shop")
	if err == nil || !strings.Contains(err.Error(), "took the SQL that makes the user shop without an error, and has no such user") {
		t.Fatalf("a step that made no user: %v", err)
	}
	if got, _ := d.Observe(t.Context(), resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}); got != nil {
		t.Errorf("a user that wasn't made reads as %v", got)
	}
}

// A step that stops halfway leaves a user Dockwell knows it began: applying again
// finishes it, rather than refusing a user it says Dockwell didn't make.
func TestAHalfMadeUserFinishesNextTime(t *testing.T) {
	d, pg, _ := userStand(t)
	ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}
	pg.failDB = true
	err := makeUser(t.Context(), d, "shop")
	if err == nil || !strings.Contains(err.Error(), "PostgreSQL refused it: could not write to file") {
		t.Fatalf("a step whose database failed: %v", err)
	}
	if !pg.roles["shop"] {
		t.Fatal("the fake made no role before the database failed")
	}
	if got, err := d.Observe(t.Context(), ref); got != nil || err != nil {
		t.Errorf("a user without its database reads as %v, %v", got, err)
	}
	desired := &resourcev1.DatabaseUser{Service: "db", Password: "SHOP_DATABASE_PASSWORD"}
	if _, err := d.Judge(t.Context(), ref, nil, desired, nil); err != nil {
		t.Fatalf("planning it again: %v", err)
	}
	pg.failDB = false
	if err := makeUser(t.Context(), d, "shop"); err != nil {
		t.Fatalf("applying it again: %v", err)
	}
	if pg.databases["shop"] != "shop" {
		t.Errorf("the database shop is %q's", pg.databases["shop"])
	}

	// One Dockwell never began is still refused.
	pg.roles["someone"] = true
	if _, err := d.Judge(t.Context(), resource.Ref{Kind: UserKind, Scope: "env_1", Name: "someone"}, nil, desired, nil); err == nil ||
		!strings.Contains(err.Error(), "which Dockwell didn't make") {
		t.Errorf("a user made by hand: %v", err)
	}
}

// A user whose server the same change makes is planned with nothing to ask, and made
// once that server takes connections, as PostgreSQL's image takes a moment to on an
// empty volume, starting it twice (ADR 0084). One whose server is nowhere is refused.
func TestAUserIsMadeWithItsServer(t *testing.T) {
	d, _, e := userStand(t)
	d.Every = time.Millisecond
	ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}
	desired := &resourcev1.DatabaseUser{Service: "db", Password: "SHOP_DATABASE_PASSWORD"}
	e.Remove("env_1-db-1")
	if _, err := d.Judge(t.Context(), ref, nil, desired, nil); err == nil || !strings.Contains(err.Error(), "isn't on this server") {
		t.Errorf("a user whose server is nowhere: %v", err)
	}
	ctx := resource.WithDesired(t.Context(), []resource.Desired{
		{Ref: resource.Ref{Kind: "service", Scope: "env_1", Name: "db"}, Spec: &resourcev1.Service{}},
		{Ref: ref, Spec: desired},
	})
	dec, err := d.Judge(ctx, ref, nil, desired, nil)
	if err != nil || dec.Action != resource.ActionCreate {
		t.Fatalf("a user whose server this change makes: %v, %v", dec, err)
	}

	// The change makes the server, which starts, stops and starts again.
	d2, pg2, _ := userStand(t)
	d2.Every = time.Millisecond
	pg2.starting = 3
	if err := makeUser(t.Context(), d2, "shop"); err != nil {
		t.Fatalf("making a user while its server starts: %v", err)
	}
	if !pg2.roles["shop"] || pg2.databases["shop"] != "shop" {
		t.Errorf("roles %v, databases %v", pg2.roles, pg2.databases)
	}

	// One that's still starting when the wait runs out says so.
	d2.Wait = 5 * time.Millisecond
	pg2.starting = 1 << 20
	if err := makeUser(t.Context(), d2, "blog"); err == nil || !strings.Contains(err.Error(), "isn't taking connections yet") {
		t.Errorf("a server that never takes connections: %v", err)
	}
}

// A user made with a database to copy gets that database's dump from the other
// server's container, loaded into theirs as them, so what it makes is theirs; one whose
// source is another engine, or gone, fails the step and takes the user away again (ADR
// 0084).
func TestAUserIsMadeWithACopyOfAnotherDatabase(t *testing.T) {
	d, pg, e := userStand(t)
	old := &fakePostgres{
		roles: map[string]bool{"postgres": true}, databases: map[string]string{"postgres": "postgres", "blog": "blog"},
		dump: map[string]string{"blog": "CREATE TABLE posts (id int);\nINSERT INTO posts VALUES (1);\n"},
	}
	e.AddLocalImage("postgres:16", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: old.exec})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_2-db-1", &docker.CreateRequest{Config: docker.Config{
		Image: "postgres:16", Labels: map[string]string{docker.LabelProject: "env_2", docker.LabelService: "db", docker.LabelNumber: "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	copying := func(name, env string) error {
		step := resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: "env_1", Name: name}, Action: resource.ActionCreate}
		ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-no-one-typed"})
		return d.Apply(ctx, step, &resourcev1.DatabaseUser{Service: "db", Password: "BLOG_DATABASE_PASSWORD", CopyFrom: &resourcev1.DatabaseCopy{Environment: env, Service: "db", Database: "blog"}})
	}
	// As it loads, it's on an internal network of its own with the new server alone,
	// whose address there it dials.
	server := e.Container("env_1-db-1").ID
	var onNet []string
	var serverAddr string
	internal := false
	pg.onLoad = func() {
		nets, err := client.ListNetworks(t.Context())
		if err != nil {
			t.Error(err)
			return
		}
		for _, n := range nets {
			if _, ok := n.Labels[LabelCopy]; !ok {
				continue
			}
			internal = n.Internal && n.Options["com.docker.network.bridge.inhibit_ipv4"] == "true"
			for _, c := range n.Containers {
				onNet = append(onNet, c.Name)
			}
			if c, err := client.InspectContainer(t.Context(), server); err == nil && c.NetworkSettings.Networks[n.Name] != nil {
				serverAddr = c.NetworkSettings.Networks[n.Name].IPAddress
			}
		}
	}
	if err := copying("blog", "env_2"); err != nil {
		t.Fatal(err)
	}
	pg.onLoad = nil
	slices.Sort(onNet)
	if !internal || len(onNet) != 2 || !strings.HasPrefix(onNet[0], "dockwell-copy-blog-") || onNet[1] != "env_1-db-1" || serverAddr == "" || serverAddr == "127.0.0.1" {
		t.Errorf("the load's network is internal with no address of the server's own %v, with %v, the server at %q", internal, onNet, serverAddr)
	}
	if got := pg.loaded["blog"]; got != "CREATE TABLE posts (id int);\nINSERT INTO posts VALUES (1);\n" {
		t.Errorf("the new server loaded %q", got)
	}
	// It loaded as the new user, with their password, in a container of its own that
	// has none of the server's environment, its administrator's password among it.
	if len(pg.loads) != 1 {
		t.Fatalf("loads %v", pg.loads)
	}
	load := pg.loads[0]
	if !slices.Equal(load.args, []string{"blog", "blog", "5432", serverAddr}) || !slices.Contains(load.env, "PGPASSWORD=a-password-no-one-typed") {
		t.Errorf("the load ran with %v, %v", load.args, load.env)
	}
	if slices.ContainsFunc(load.env, func(kv string) bool { return strings.HasPrefix(kv, "POSTGRES_PASSWORD=") }) {
		t.Errorf("the load ran with the server's environment: %v", load.env)
	}
	list, err := client.ListContainers(t.Context(), map[string][]string{"label": {LabelCopy}})
	if err != nil || len(list) != 0 {
		t.Errorf("the container that loaded the copy is still there: %v, %v", list, err)
	}
	if nets, _ := client.ListNetworks(t.Context()); slices.ContainsFunc(nets, func(n docker.Network) bool { _, ok := n.Labels[LabelCopy]; return ok }) {
		t.Errorf("the copy's network is still there: %v", nets)
	}
	if c, _ := client.InspectContainer(t.Context(), server); len(c.NetworkSettings.Networks) != 1 {
		t.Errorf("after the copy, the server is on %v", c.NetworkSettings.Networks)
	}
	g, _, _ := d.recorded(resource.Ref{Kind: UserKind, Scope: "env_1", Name: "blog"})
	if g.Copying || !slices.Equal(g.CopiedFrom, []string{"env_2", "db", "blog"}) {
		t.Errorf("a whole copy is kept as %+v", g)
	}
	// It dumped as the database's owner, never the old server's administrator (#595).
	if !slices.Equal(old.dumpedAs, []string{"blog"}) {
		t.Errorf("the dump ran as %v", old.dumpedAs)
	}
	if err := copying("wiki", "env_9"); err == nil || !strings.Contains(err.Error(), "has no container on this server") {
		t.Errorf("a copy from nowhere: %v", err)
	}
	if pg.roles["wiki"] {
		t.Error("a copy that failed left its user behind")
	}
}

// A client command in what's loaded never signs in as the server's administrator
// nor runs a shell: psql from 17.6 on refuses every one after the load's own
// \restrict, an older one dials the server off loopback, where a password is asked
// for, and a server that lets anyone in with no password isn't copied into at all.
// The \restrict and \unrestrict a newer pg_dump writes don't get in the way (ADR
// 0084, #590's review).
func TestACopyRefusesClientCommands(t *testing.T) {
	for _, c := range []struct {
		name, psql, dump, says string
		trusts                 bool
	}{
		{"connect, restricted", "", "CREATE TABLE posts (id int);\n\\connect - postgres\nALTER ROLE blog SUPERUSER;\n", "backslash commands are restricted", false},
		{"connect, an older psql", "psql (PostgreSQL) 16.2", "CREATE TABLE posts (id int);\n\\connect - postgres\nALTER ROLE blog SUPERUSER;\n", "password authentication failed", false},
		{"shell, restricted", "", "\\! id > /tmp/owned\n", "backslash commands are restricted", false},
		{"a server that trusts its networks", "", "CREATE TABLE posts (id int);\n", "lets anyone on its networks sign in as its administrator with no password", true},
		{"a newer pg_dump's own restrict", "", "--\n\\restrict abc123\n\nCREATE TABLE posts (id int);\n\\unrestrict abc123\n", "", false},
		// A check that says nothing of a password is no answer, and nothing is copied.
		{"a check that can't tell", "", "CREATE TABLE posts (id int);\n", "can't tell whether db asks for a password on the copy's network", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, pg, e := userStand(t)
			pg.psql, pg.trusts = c.psql, c.trusts
			if c.name == "a check that can't tell" {
				pg.probeSays = "psql: error: connection to server at \"172.18.0.4\", port 5432 failed: Connection refused"
			}
			old := &fakePostgres{roles: map[string]bool{"postgres": true}, databases: map[string]string{"postgres": "postgres", "blog": "blog"}, dump: map[string]string{"blog": c.dump}}
			e.AddLocalImage("postgres:16", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: old.exec})
			client := e.Client()
			id, err := client.CreateContainer(t.Context(), "env_2-db-1", &docker.CreateRequest{Config: docker.Config{
				Image: "postgres:16", Labels: map[string]string{docker.LabelProject: "env_2", docker.LabelService: "db", docker.LabelNumber: "1"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := client.StartContainer(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			step := resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: "env_1", Name: "blog"}, Action: resource.ActionCreate}
			ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-no-one-typed"})
			err = d.Apply(ctx, step, &resourcev1.DatabaseUser{Service: "db", Password: "BLOG_DATABASE_PASSWORD", CopyFrom: &resourcev1.DatabaseCopy{Environment: "env_2", Service: "db", Database: "blog"}})
			if c.says == "" {
				if err != nil || pg.loaded["blog"] != "--\n\nCREATE TABLE posts (id int);\n" {
					t.Fatalf("copying: %v, loaded %q", err, pg.loaded["blog"])
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Errorf("copying: %v", err)
			}
			if pg.superuser || pg.shell {
				t.Errorf("the load became the administrator %v, ran a shell %v", pg.superuser, pg.shell)
			}
			if (c.trusts || pg.probeSays != "") && len(pg.loads) != 0 {
				t.Errorf("a server that may trust its networks was loaded into: %v", pg.loads)
			}
			if pg.roles["blog"] {
				t.Error("a copy that was refused left its user behind")
			}
		})
	}
}

// What a copy an agent's crash cut short left goes as the agent starts again: its
// throwaway containers, and its network, with the server taken off it first (#595).
func TestWhatACopyLeftIsSweptAway(t *testing.T) {
	d, _, e := userStand(t)
	client := e.Client()
	server := e.Container("env_1-db-1").ID
	if _, err := client.CreateNetwork(t.Context(), &docker.NetworkCreate{Name: docker.CopyPrefix + "blog-1", Internal: true, Labels: map[string]string{LabelCopy: "blog"}}); err != nil {
		t.Fatal(err)
	}
	if err := client.ConnectNetwork(t.Context(), docker.CopyPrefix+"blog-1", server, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateContainer(t.Context(), docker.CopyPrefix+"blog-2", &docker.CreateRequest{
		Config:     docker.Config{Image: "postgres:17", Labels: map[string]string{docker.LabelOneoff: "True", LabelCopy: "blog"}},
		HostConfig: docker.HostConfig{NetworkMode: docker.CopyPrefix + "blog-1"},
	}); err != nil {
		t.Fatal(err)
	}
	// Someone's own network stays.
	if _, err := client.CreateNetwork(t.Context(), &docker.NetworkCreate{Name: "dockwell-copy-of-mine"}); err != nil {
		t.Fatal(err)
	}
	d.Clean(t.Context())
	if left, err := client.ListContainers(t.Context(), map[string][]string{"label": {LabelCopy}}); err != nil || len(left) != 0 {
		t.Errorf("containers left: %v, %v", left, err)
	}
	nets, err := client.ListNetworks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, n := range nets {
		names = append(names, n.Name)
	}
	if slices.Contains(names, docker.CopyPrefix+"blog-1") || !slices.Contains(names, "dockwell-copy-of-mine") {
		t.Errorf("networks left: %v", names)
	}
	if c, err := client.InspectContainer(t.Context(), server); err != nil || c.NetworkSettings.Networks[docker.CopyPrefix+"blog-1"] != nil || !c.State.Running {
		t.Errorf("the server after the sweep: %v, %v", c, err)
	}
}

// A copy an agent's restart cut short reads as no user, not as one that's whole, and
// applying again empties the database of the part it holds and copies it whole (ADR
// 0084).
func TestACopyCutShortStartsOver(t *testing.T) {
	d, pg, e := userStand(t)
	old := &fakePostgres{
		roles: map[string]bool{"postgres": true}, databases: map[string]string{"postgres": "postgres", "blog": "blog"},
		dump: map[string]string{"blog": "CREATE TABLE posts (id int);\n"},
	}
	e.AddLocalImage("postgres:16", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: old.exec})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_2-db-1", &docker.CreateRequest{Config: docker.Config{
		Image: "postgres:16", Labels: map[string]string{docker.LabelProject: "env_2", docker.LabelService: "db", docker.LabelNumber: "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "blog"}
	desired := &resourcev1.DatabaseUser{Service: "db", Password: "BLOG_DATABASE_PASSWORD", CopyFrom: &resourcev1.DatabaseCopy{Environment: "env_2", Service: "db", Database: "blog"}}
	// What a run that stopped mid-copy leaves: the user, their database, part of it.
	pg.roles["blog"], pg.databases["blog"] = true, "blog"
	if err := d.record(ref, &given{Service: "db", Uses: "BLOG_DATABASE_PASSWORD", Copying: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Observe(t.Context(), ref); err != nil || got != nil {
		t.Fatalf("a copy cut short reads as %v, %v", got, err)
	}
	step := resource.Step{Ref: ref, Action: resource.ActionCreate}
	ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-no-one-typed"})
	if err := d.Apply(ctx, step, desired); err != nil {
		t.Fatal(err)
	}
	dropped := slices.IndexFunc(pg.said, func(sql string) bool { return strings.Contains(sql, `DROP DATABASE IF EXISTS "blog"`) })
	made := slices.IndexFunc(pg.said, func(sql string) bool { return strings.Contains(sql, `CREATE DATABASE "blog"`) })
	if dropped < 0 || made < dropped {
		t.Errorf("it didn't empty the database before copying again: %q", pg.said)
	}
	got, err := d.Observe(t.Context(), ref)
	if err != nil || got.(*resourcev1.DatabaseUser).GetCopyFrom().GetDatabase() != "blog" {
		t.Errorf("a whole copy reads as %v, %v", got, err)
	}
	// One that fails to load takes the user away again, and says the copy is begun no more.
	pg.loadFails = true
	ref.Name, desired.DatabaseName = "wiki", "wiki"
	step.Ref = ref
	if err := d.Apply(ctx, step, desired); err == nil || !strings.Contains(err.Error(), "couldn't load the copy of blog as wiki") {
		t.Errorf("a load that fails: %v", err)
	}
	if _, ok, _ := d.recorded(ref); ok || pg.roles["wiki"] {
		t.Error("a copy that failed left its user behind")
	}
}

// A copy never reads a database its server keeps for itself, whatever the control
// plane took.
func TestASystemDatabaseIsNeverCopied(t *testing.T) {
	for _, name := range []string{"postgres", "template1", "mysql", "information_schema"} {
		u := &resourcev1.DatabaseUser{Service: "db", Password: "P", CopyFrom: &resourcev1.DatabaseCopy{Environment: "env_2", Service: "db", Database: name}}
		if err := (&UserDriver{}).Validate(u); err == nil || !strings.Contains(err.Error(), "keeps for itself") {
			t.Errorf("copying %s: %v", name, err)
		}
	}
	d, _, _ := userStand(t)
	err := d.copyInto(t.Context(), &docker.Container{}, &Engine{Kind: dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL}, "blog", "blog", "p", &resourcev1.DatabaseCopy{Environment: "env_2", Service: "db", Database: "postgres"})
	if err == nil || !strings.Contains(err.Error(), "keeps for itself") {
		t.Errorf("the agent copied postgres: %v", err)
	}
}

// A MySQL or MariaDB dump loads with no DEFINER of the old server's, so what it makes
// is the loading user's; data that reads like one stays as it is.
func TestACopyTakesOutDefiners(t *testing.T) {
	dump := "/*!50013 DEFINER=`wordpress`@`%` SQL SECURITY DEFINER */\n" +
		"CREATE DEFINER=`root`@`localhost` PROCEDURE `p`()\n" +
		"/*!50003 CREATE*/ /*!50017 DEFINER='shop'@'%'*/ /*!50003 TRIGGER t BEFORE INSERT ON x FOR EACH ROW SET @a = 1 */;;\n" +
		"INSERT INTO `notes` VALUES ('CREATE DEFINER=`root`@`%` stays in data');\n" +
		"/*!50106 CREATE*/ /*!50117 DEFINER=root@localhost*/ /*!50106 EVENT `e` ON SCHEDULE EVERY 1 DAY DO SELECT 1 */"
	b, err := io.ReadAll(withoutDefiners(strings.NewReader(dump)))
	if err != nil {
		t.Fatal(err)
	}
	want := "/*!50013 SQL SECURITY DEFINER */\n" +
		"CREATE PROCEDURE `p`()\n" +
		"/*!50003 CREATE*/ /*!50017 */ /*!50003 TRIGGER t BEFORE INSERT ON x FOR EACH ROW SET @a = 1 */;;\n" +
		"INSERT INTO `notes` VALUES ('CREATE DEFINER=`root`@`%` stays in data');\n" +
		"/*!50106 CREATE*/ /*!50117 */ /*!50106 EVENT `e` ON SCHEDULE EVERY 1 DAY DO SELECT 1 */"
	if string(b) != want {
		t.Errorf("got\n%s\nwant\n%s", b, want)
	}
	// Any case and spacing, a role with no host, a clause late on its line, and one
	// in a comment of its own, as genuine dumps have them.
	for in, want := range map[string]string{
		"create definer=root@localhost procedure p()\n":                                          "create procedure p()\n",
		"CREATE DEFINER=`app_admin` VIEW v AS SELECT 1;\n":                                       "CREATE VIEW v AS SELECT 1;\n",
		"/*!50001 CREATE ALGORITHM=UNDEFINED DEFINER = `x`@`%` SQL SECURITY DEFINER VIEW v */\n": "/*!50001 CREATE ALGORITHM=UNDEFINED SQL SECURITY DEFINER VIEW v */\n",
		"  /*!50017 definer='a'@'b'*/\n":                                                         "  /*!50017 */\n",
		"ALTER DEFINER=CURRENT_USER() EVENT e ENABLE;\n":                                         "ALTER EVENT e ENABLE;\n",
	} {
		b, err := io.ReadAll(withoutDefiners(strings.NewReader(in)))
		if err != nil || string(b) != want {
			t.Errorf("%q became %q, %v", in, b, err)
		}
	}
	// A line longer than what's read at once goes through whole.
	long := strings.Repeat("x", 200<<10) + "\n" + "CREATE DEFINER=`a`@`%` VIEW v\n"
	b, err = io.ReadAll(withoutDefiners(strings.NewReader(long)))
	if err != nil || string(b) != strings.Repeat("x", 200<<10)+"\nCREATE VIEW v\n" {
		t.Errorf("a long line: %d bytes, %v", len(b), err)
	}
}

// Deleting a user whose server's container is gone has nothing to drop it from, so it
// succeeds, and Dockwell forgets it.
func TestDeletingAUserWithItsContainerGoneForgetsIt(t *testing.T) {
	d, _, e := userStand(t)
	ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}
	if err := makeUser(t.Context(), d, "shop"); err != nil {
		t.Fatal(err)
	}
	e.Remove("env_1-db-1")
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Action: resource.ActionDelete}, nil); err != nil {
		t.Fatalf("deleting it: %v", err)
	}
	if _, ok, _ := d.recorded(ref); ok {
		t.Error("Dockwell still keeps the user")
	}
	b, err := os.ReadFile(filepath.Join(d.Dir, "database-users.json"))
	if err != nil || strings.Contains(string(b), "shop") {
		t.Errorf("the agent keeps %s, %v", b, err)
	}
}

// The agent takes no name into SQL that it wouldn't take from the control plane.
func TestTheAgentChecksANameBeforeItsSQL(t *testing.T) {
	d, pg, _ := userStand(t)
	if err := makeUser(t.Context(), d, `shop"; DROP DATABASE postgres; --`); err == nil || !strings.Contains(err.Error(), "isn't a valid user name") {
		t.Errorf("a name that isn't one: %v", err)
	}
	if len(pg.said) != 0 {
		t.Errorf("PostgreSQL was given %q", pg.said)
	}
}
