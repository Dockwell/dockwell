// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// fakeMongo stands in for mongosh on a MongoDB: it keeps the users each script makes,
// by name, with the database each owns, and the password each was last given.
type fakeMongo struct {
	mu        sync.Mutex
	users     map[string]string
	passwords map[string]string
	scripts   []string
	// open answers one who never signs in, as a server without access control does,
	// and away answers no one, as one not up yet.
	open, away bool
}

var (
	mongoName = regexp.MustCompile(`const name = ("[^"]*");`)
	mongoDB   = regexp.MustCompile(`const target = m\.getDB\(("[^"]*")\);`)
	mongoPwd  = regexp.MustCompile(`const pwd = ("(?:[^"\\]|\\.)*");`)
)

func (f *fakeMongo) exec(x dockertest.Exec) int {
	b, _ := io.ReadAll(x.Stdin)
	script := string(b)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, script)
	unquote := func(re *regexp.Regexp) string {
		var s string
		if m := re.FindStringSubmatch(script); m != nil {
			_ = json.Unmarshal([]byte(m[1]), &s)
		}
		return s
	}
	name, db := unquote(mongoName), unquote(mongoDB)
	switch {
	case strings.Contains(script, "listDatabases"):
		root := slices.ContainsFunc(x.Env, func(kv string) bool {
			return strings.HasPrefix(kv, "MONGO_INITDB_ROOT_USERNAME=") && len(kv) > len("MONGO_INITDB_ROOT_USERNAME=")
		})
		out, _ := json.Marshal(map[string]bool{"root": root, "answering": !f.away, "open": f.open})
		_, _ = io.WriteString(x.Stdout, string(out)+"\n")
	case strings.Contains(script, "createUser("):
		f.users[name], f.passwords[name] = db, unquote(mongoPwd)
		_, _ = io.WriteString(x.Stdout, `{"made":true}`+"\n")
	case strings.Contains(script, "const owns"):
		owner := ""
		if f.users[name] == db {
			owner = name
		}
		_, has := f.users[name]
		out, _ := json.Marshal(map[string]any{"user": has, "owner": owner})
		_, _ = io.WriteString(x.Stdout, "Warning: a line mongosh prints first\n"+string(out)+"\n")
	case strings.Contains(script, "dropDatabase()"):
		delete(f.users, name)
		_, _ = io.WriteString(x.Stdout, `{"dropped":true}`+"\n")
	}
	return 0
}

// On MongoDB, an app's user is made by the image's root user in the app's own
// database, owning that database alone, its password as JSON in the script, never
// an argument; it reads back as made, and goes with its database (ADR 0084).
func TestAMongoDBUserOwnsItsDatabaseAlone(t *testing.T) {
	mg := &fakeMongo{users: map[string]string{}, passwords: map[string]string{}}
	e := dockertest.New(t)
	e.AddLocalImage("mongo:8.0", dockertest.Image{Config: docker.Config{Cmd: []string{"mongod"}}, Exec: mg.exec})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_1-db-1", &docker.CreateRequest{Config: docker.Config{
		Image: "mongo:8.0", Labels: map[string]string{docker.LabelProject: "env_1", docker.LabelService: "db", docker.LabelNumber: "1"},
		Env: []string{"MONGO_INITDB_ROOT_USERNAME=root", "MONGO_INITDB_ROOT_PASSWORD=the-administrators"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	d := &UserDriver{Client: client, Dir: t.TempDir()}
	ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}
	desired := &resourcev1.DatabaseUser{Service: "db", Password: "SHOP_DATABASE_PASSWORD"}
	const password = `a "quoted" password\no-one-typed`
	ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: password})
	if err := d.Apply(ctx, resource.Step{Ref: ref, Action: resource.ActionCreate}, desired); err != nil {
		t.Fatal(err)
	}
	if mg.users["shop"] != "shop" || mg.passwords["shop"] != password {
		t.Errorf("made %v with %q", mg.users, mg.passwords["shop"])
	}
	made := mg.scripts[len(mg.scripts)-2]
	if !strings.Contains(made, `roles = [{ role: "dbOwner", db: target.getName() }]`) || strings.Contains(made, "root") && !strings.Contains(made, "MONGO_INITDB_ROOT_USERNAME") {
		t.Errorf("made with %s", made)
	}
	if err := d.Verify(t.Context(), ref, desired); err != nil {
		t.Errorf("checked as %v", err)
	}
	if got, err := d.Observe(t.Context(), ref); err != nil || got == nil {
		t.Errorf("read as %v, %v", got, err)
	}
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Action: resource.ActionDelete}, nil); err != nil {
		t.Fatal(err)
	}
	if _, has := mg.users["shop"]; has {
		t.Error("deleting it left the user")
	}
}

// A MongoDB without access control, or with no root user, gets no user of an app's
// own, which would keep no one out: planning and applying refuse it (ADR 0084).
func TestAMongoDBWithoutAccessControlGetsNoUser(t *testing.T) {
	for name, tc := range map[string]struct {
		open bool
		env  []string
	}{
		"no access control": {open: true, env: []string{"MONGO_INITDB_ROOT_USERNAME=root", "MONGO_INITDB_ROOT_PASSWORD=x"}},
		"no root user":      {},
	} {
		t.Run(name, func(t *testing.T) {
			mg := &fakeMongo{users: map[string]string{}, passwords: map[string]string{}, open: tc.open}
			e := dockertest.New(t)
			e.AddLocalImage("mongo:8.0", dockertest.Image{Config: docker.Config{Cmd: []string{"mongod"}}, Exec: mg.exec})
			client := e.Client()
			id, err := client.CreateContainer(t.Context(), "env_1-db-1", &docker.CreateRequest{Config: docker.Config{
				Image: "mongo:8.0", Labels: map[string]string{docker.LabelProject: "env_1", docker.LabelService: "db", docker.LabelNumber: "1"}, Env: tc.env,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := client.StartContainer(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			d := &UserDriver{Client: client, Dir: t.TempDir(), Wait: 50 * time.Millisecond, Every: 10 * time.Millisecond}
			ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}
			desired := &resourcev1.DatabaseUser{Service: "db", Password: "SHOP_DATABASE_PASSWORD"}
			if _, err := d.Judge(t.Context(), ref, nil, desired, nil); err == nil || !strings.Contains(err.Error(), "runs without access control") {
				t.Errorf("planning = %v", err)
			}
			ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password"})
			if err := d.Apply(ctx, resource.Step{Ref: ref, Action: resource.ActionCreate}, desired); err == nil || !strings.Contains(err.Error(), "runs without access control") {
				t.Errorf("applying = %v", err)
			}
			if len(mg.users) != 0 {
				t.Errorf("made %v", mg.users)
			}
		})
	}
}

// A MongoDB whose image makes its root user on a server it runs open at first, then
// starts again with access control, is waited for, not refused (ADR 0084).
func TestAMongoDBOpenWhileItStartsIsWaitedFor(t *testing.T) {
	mg := &fakeMongo{users: map[string]string{}, passwords: map[string]string{}, open: true}
	e := dockertest.New(t)
	e.AddLocalImage("mongo:8.0", dockertest.Image{Config: docker.Config{Cmd: []string{"mongod"}}, Exec: mg.exec})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_1-db-1", &docker.CreateRequest{Config: docker.Config{
		Image: "mongo:8.0", Labels: map[string]string{docker.LabelProject: "env_1", docker.LabelService: "db", docker.LabelNumber: "1"},
		Env: []string{"MONGO_INITDB_ROOT_USERNAME=root", "MONGO_INITDB_ROOT_PASSWORD=x"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		mg.mu.Lock()
		mg.open = false
		mg.mu.Unlock()
	}()
	d := &UserDriver{Client: client, Dir: t.TempDir(), Wait: 5 * time.Second, Every: 10 * time.Millisecond}
	ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}
	ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password"})
	if err := d.Apply(ctx, resource.Step{Ref: ref, Action: resource.ActionCreate}, &resourcev1.DatabaseUser{Service: "db", Password: "P"}); err != nil {
		t.Fatalf("making a user on a MongoDB that was starting = %v", err)
	}
	if mg.users["shop"] != "shop" {
		t.Errorf("made %v", mg.users)
	}
}

// A MongoDB that isn't answering yet, as while its image makes its root user, is
// waited for, not refused nor taken as closed (ADR 0084).
func TestAMongoDBNotAnsweringYetIsWaitedFor(t *testing.T) {
	mg := &fakeMongo{users: map[string]string{}, passwords: map[string]string{}, away: true}
	e := dockertest.New(t)
	e.AddLocalImage("mongo:8.0", dockertest.Image{Config: docker.Config{Cmd: []string{"mongod"}}, Exec: mg.exec})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_1-db-1", &docker.CreateRequest{Config: docker.Config{
		Image: "mongo:8.0", Labels: map[string]string{docker.LabelProject: "env_1", docker.LabelService: "db", docker.LabelNumber: "1"},
		Env: []string{"MONGO_INITDB_ROOT_USERNAME=root", "MONGO_INITDB_ROOT_PASSWORD=x"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		mg.mu.Lock()
		mg.away = false
		mg.mu.Unlock()
	}()
	d := &UserDriver{Client: client, Dir: t.TempDir(), Wait: 5 * time.Second, Every: 10 * time.Millisecond}
	ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password"})
	ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}
	if err := d.Apply(ctx, resource.Step{Ref: ref, Action: resource.ActionCreate}, &resourcev1.DatabaseUser{Service: "db", Password: "P"}); err != nil {
		t.Fatalf("making a user on a MongoDB not answering yet = %v", err)
	}
}
