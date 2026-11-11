// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"io"
	"slices"
	"strings"
	"testing"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// mongoReading is what mongoRead printed on mongo:8.0, its version aside.
const mongoReading = `{"version":"8.0.34","user":"root","uptime":3538,"connections":3,"max_connections":209715,"requested":2308,"read":30,` +
	`"size":356352,"fcv":"7.0","replica_set":"","auth":true,"database_count":4,"user_count":2,` +
	`"databases":[{"name":"admin","size":151552},{"name":"config","size":49152},{"name":"local","size":73728},{"name":"shop","size":81920}],` +
	`"users":[{"name":"root","db":"admin","roles":[{"role":"root","db":"admin"}]},{"name":"shopper","db":"shop","roles":[{"role":"dbOwner","db":"shop"}]}]}`

func TestMongoIsItsOwnImage(t *testing.T) {
	for image, want := range map[string]bool{
		"mongo:8.0": true, "docker.io/library/mongo:7.0.12": true, "mongo": true, "mongo@sha256:" + strings.Repeat("a", 64): true,
		"bitnami/mongodb:8.0": false, "mongo-express:1": false, "ghcr.io/acme/mongo:8": false,
	} {
		if got := Recognise(image); (got != nil && got.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB) != want {
			t.Errorf("%s: recognised as %v", image, got)
		}
	}
}

// Its data is /data/db, or where --dbpath says, and its port 27017, or --port.
func TestMongoFromSpec(t *testing.T) {
	mongo := func(cmd []string, target string) *resourcev1.Service {
		return &resourcev1.Service{
			Runs: &resourcev1.Service_App{App: &resourcev1.Container{
				Image: "mongo:8.0", Command: cmd,
				Mounts: []*resourcev1.Mount{{Target: target, Source: &resourcev1.Mount_Volume{Volume: "data"}}},
			}},
			Secrets: map[string]string{"MONGO_INITDB_ROOT_PASSWORD": "mongo-root"},
		}
	}
	for _, c := range []struct {
		cmd    []string
		target string
		volume string
		port   uint32
	}{
		{nil, "/data/db", "data", 27017},
		{[]string{"mongod", "--port", "27018"}, "/data/db", "data", 27018},
		{[]string{"--dbpath", "/srv/mongo"}, "/srv/mongo", "data", 27017},
		{[]string{"mongod", "--dbpath=/srv/mongo"}, "/srv/mongo", "data", 27017},
		{[]string{"--dbpath", "/srv/mongo"}, "/data/db", "", 27017},
		{[]string{"sh", "-c", "exec mongod --dbpath /srv/mongo --port 27019"}, "/srv/mongo", "data", 0},
	} {
		f, ok := FromSpec(mongo(c.cmd, c.target))
		if !ok || f.Engine.Kind != dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB || f.Volume != c.volume || c.port != 0 && f.Port != c.port || f.PasswordSecret != "mongo-root" {
			t.Errorf("%q at %s: %+v, %v", c.cmd, c.target, f, ok)
		}
	}
	if _, ok := FromSpec(mongo([]string{"mongodump", "--archive"}, "/data/db")); ok {
		t.Error("a service of the image that runs mongodump reads as a database")
	}
}

// The reading signs in from the container's own environment: its password is on no
// command line, and its script asks nothing that holds a credential.
func TestMongoReadingHoldsNoPassword(t *testing.T) {
	cmd, input := Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB).reading(27018)
	line := strings.Join(cmd, " ")
	if !strings.Contains(line, "DW_PORT=27018") || !strings.Contains(line, "mongosh") || strings.Contains(line, "--password") || strings.Contains(line, "-p ") {
		t.Errorf("the reading runs %q", line)
	}
	if !strings.Contains(input, "process.env") || strings.Contains(input, "showCredentials") || strings.Contains(input, "credentials") {
		t.Errorf("the script is %q", input)
	}
}

func TestParseMongo(t *testing.T) {
	// mongosh may warn first: the reading is its last line.
	r, err := parseMongo([]byte("Warning: something about the shell\n" + mongoReading + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.GetVersion() != "8.0.34" || r.GetUser() != "root" || r.GetConnections() != 3 || r.GetMaxConnections() != 209715 || r.GetUptimeSeconds() != 3538 ||
		r.GetSizeBytes() != 356352 || r.GetFeatureCompatibility() != "7.0" || r.GetReplicaSet() != "" || r.GetChecksNoOne() {
		t.Errorf("the reading is %v", r)
	}
	if hit := r.GetCacheHit(); hit < 0.98 || hit > 0.99 {
		t.Errorf("the cache answered %v of reads, want 2278 of 2308", hit)
	}
	var system []string
	for _, d := range r.GetDatabases() {
		if d.GetSystem() {
			system = append(system, d.GetName())
		}
	}
	if !slices.Equal(system, []string{"admin", "config", "local"}) || len(r.GetDatabases()) != 4 {
		t.Errorf("the databases are %v", r.GetDatabases())
	}
	users := r.GetUsers()
	if len(users) != 2 || !users[0].GetSuperuser() || users[0].GetAuthDatabase() != "admin" ||
		users[1].GetSuperuser() || users[1].GetAuthDatabase() != "shop" || !slices.Equal(users[1].GetDatabases(), []string{"shop"}) {
		t.Errorf("the users are %v", users)
	}

	open, err := parseMongo([]byte(strings.Replace(mongoReading, `"auth":true`, `"auth":false`, 1)))
	if err != nil || !open.GetChecksNoOne() {
		t.Errorf("one with --auth off reads %v, %v", open, err)
	}
	if _, err := parseMongo([]byte(`{"user":""}`)); err == nil {
		t.Error("a reading with no version is taken")
	}
}

// One on its own image is read with mongosh in its container; one too old for that
// says so, and one that refuses the sign-in says which variables Dockwell used.
func TestMongoIsReadInItsContainer(t *testing.T) {
	e := dockertest.New(t)
	var asked string
	e.AddLocalImage("mongo:8.0", dockertest.Image{Config: docker.Config{Cmd: []string{"mongod"}}, Exec: func(x dockertest.Exec) int {
		b, _ := io.ReadAll(x.Stdin)
		asked = strings.Join(x.Cmd, " ") + "\n" + string(b)
		_, _ = io.WriteString(x.Stdout, mongoReading+"\n")
		return 0
	}})
	e.AddLocalImage("mongo:7.0", dockertest.Image{Config: docker.Config{Cmd: []string{"mongod"}}, Exec: func(x dockertest.Exec) int {
		_, _ = io.ReadAll(x.Stdin)
		_, _ = io.WriteString(x.Stderr, "MongoServerError: Authentication failed.\n")
		return 1
	}})
	e.AddLocalImage("mongo:5.0", dockertest.Image{Config: docker.Config{Cmd: []string{"mongod"}}, Exec: func(dockertest.Exec) int { return 127 }})
	client := e.Client()
	var asks []*agentv1.DatabaseAsked
	for _, s := range []struct{ name, image string }{{"now", "mongo:8.0"}, {"refuses", "mongo:7.0"}, {"old", "mongo:5.0"}} {
		id, err := client.CreateContainer(t.Context(), "env_1-"+s.name+"-1", &docker.CreateRequest{Config: docker.Config{
			Image: s.image, Labels: map[string]string{docker.LabelProject: "env_1", docker.LabelService: s.name, docker.LabelNumber: "1"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.StartContainer(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		asks = append(asks, &agentv1.DatabaseAsked{Service: &dockwellv1.ResourceRef{Kind: "service", Scope: "env_1", Name: s.name}, Engine: dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB})
	}
	got, err := Reader{Client: client}.Read(t.Context(), &agentv1.DatabaseCall{Databases: asks})
	if err != nil {
		t.Fatal(err)
	}
	if r := got.GetReadings()[0]; r.GetUnread() != "" || r.GetVersion() != "8.0.34" || r.GetFeatureCompatibility() != "7.0" {
		t.Errorf("mongo:8.0 reads %v", r)
	}
	if !strings.Contains(asked, "mongosh") || !strings.Contains(asked, "appName=dockwell-reading") {
		t.Errorf("it asked %q", asked)
	}
	if why := got.GetReadings()[1].GetUnread(); why != "MongoDB refused Dockwell's sign-in with what its environment holds. "+
		"Dockwell signs in as its image does, with MONGO_INITDB_ROOT_USERNAME and MONGO_INITDB_ROOT_PASSWORD" {
		t.Errorf("one that refuses reads %q", why)
	}
	if why := got.GetReadings()[2].GetUnread(); !strings.Contains(why, "MongoDB 5.0 is older than Dockwell reads") || !strings.Contains(why, "Upgrade it to 6.0 or later") {
		t.Errorf("mongo:5.0 reads %q", why)
	}
}
