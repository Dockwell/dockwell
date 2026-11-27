// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestClickHouseInstallAndLink(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		f := storageControl(t, b, nil, "")
		testClickHouseInstallAndLink(t, f)
	})
}

func TestClickHouseLifecycleOnRealDocker(t *testing.T) {
	f := storageControl(t, store.SQLite, dockertest.Real(t), "")
	testClickHouseInstallAndLink(t, f)
}

func testClickHouseInstallAndLink(t *testing.T, f *storageFixture) {
	t.Helper()
	const kind = v1.DatabaseEngine_DATABASE_ENGINE_CLICKHOUSE
	const dataProject = "clickhouse-fixture"
	cat := catalogClient(f.person)
	listed, err := cat.ListCatalogEntries(t.Context(), connect.NewRequest(&v1.ListCatalogEntriesRequest{
		Category: "databases", WithEngines: true,
	}))
	if err != nil || int32(listed.Msg.GetEngines()["clickhouse"]) != int32(kind) {
		t.Fatalf("ClickHouse engine tile: %v, %v", listed, err)
	}
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&v1.GetCatalogEntryRequest{Id: "clickhouse"}))
	if err != nil {
		t.Fatal(err)
	}
	entry := got.Msg.GetEntry()
	var image string
	for tag, hash := range catalog.PinnedDigests(entry.GetCompose()) {
		image = tag + "@" + hash
	}
	if image == "" || !slices.Contains(entry.GetArchitectures(), "amd64") || !slices.Contains(entry.GetArchitectures(), "arm64") {
		t.Fatal("the entry must pin an image for both architectures")
	}
	const consumerImage = "busybox:1.37"
	if f.engine != nil {
		f.engine.Registry[image] = dockertest.Image{
			Volumes: []string{"/var/lib/clickhouse"},
			Exec: func(x dockertest.Exec) int {
				_, _ = io.WriteString(x.Stdout, "{\"version\":\"25.8.33.6\",\"user\":\"default\"}\n{\"name\":\"default\",\"engine\":\"Atomic\"}\n")
				return 0
			},
		}
		f.engine.Registry[consumerImage] = dockertest.Image{}
	}
	dockerClient := f.server.client
	const oldURL = "http://example.com/default"
	consumer := propose(t, f.changes, "Run the app with its variables",
		&v1.Edit{
			Resource: &v1.ResourceRef{Kind: "secret", Name: "CLICKHOUSE_URL"},
			Spec:     &spec.Spec{Kind: &spec.Spec_Secret{Secret: &spec.Secret{Value: oldURL}}},
		},
		&v1.Edit{
			Resource: &v1.ResourceRef{Kind: "variable", Name: "CLICKHOUSE_NATIVE_PORT"},
			Spec:     &spec.Spec{Kind: &spec.Spec_Variable{Variable: &spec.Variable{Value: "1"}}},
		},
		&v1.Edit{
			Resource: &v1.ResourceRef{Kind: "service", Name: "web"},
			Spec: &spec.Spec{Kind: &spec.Spec_Service{Service: &spec.Service{
				Runs:      &spec.Service_App{App: &spec.Container{Image: consumerImage, Command: []string{"sleep", "600"}}},
				Variables: map[string]string{"CLICKHOUSE_NATIVE_PORT": "CLICKHOUSE_NATIVE_PORT"},
				Secrets:   map[string]string{"CLICKHOUSE_URL": "CLICKHOUSE_URL"},
			}}},
		},
	)
	web := storageScope(t, consumer, "service", "web")
	appEnv := web.GetResource().GetScope()
	if f.engine == nil {
		removeStorageFixture(t, dockerClient, appEnv, []string{"default", "clickhouse-link"}, nil)
	}
	applyConfirmed(t, f.changes, consumer)
	install := installApp(t, cat, &v1.InstallCatalogEntryRequest{
		Id: "clickhouse", Project: dataProject, Servers: []string{f.server.id.ServerID},
	})
	data := storageScope(t, install, "service", "db")
	secret := storageScope(t, install, "secret", "CLICKHOUSE_PASSWORD")
	if secret.GetId() == "" || slices.ContainsFunc(install.GetPlan().GetResources(), func(r *v1.PlannedResource) bool {
		return r.GetResource().GetKind() == "backup.policy"
	}) {
		t.Fatal("the plan should make a password secret and leave the backup schedule unset")
	}
	env := data.GetResource().GetScope()
	if f.engine == nil {
		removeStorageFixture(t, dockerClient, env, []string{"default"}, []string{"data"})
	}
	applyConfirmed(t, f.changes, install)

	inspect := func() *docker.Container {
		t.Helper()
		c, err := dockerClient.ContainerOf(t.Context(), docker.Project(env), "db", 1)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	db := inspect()
	if db.Config.Image != image || db.HostConfig.Memory != 512<<20 || len(db.HostConfig.PortBindings) != 0 {
		t.Fatal("the reviewed image, 512 MiB limit or private networking was not applied")
	}
	if !slices.ContainsFunc(db.Config.Env, func(v string) bool {
		value, ok := strings.CutPrefix(v, "CLICKHOUSE_PASSWORD=")
		return ok && len(value) == 32
	}) {
		t.Fatal("the service did not receive its generated password")
	}
	if f.engine == nil && (db.State.Health == nil || db.State.Health.Status != "healthy") {
		t.Fatal("the ClickHouse health check did not pass")
	}
	readDB := func() *v1.Database {
		t.Helper()
		out, err := databaseClient(f.person, f.person.origin).GetDatabase(t.Context(), connect.NewRequest(&v1.GetDatabaseRequest{Id: data.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		return out.Msg.GetDatabase()
	}
	current := readDB()
	if current.GetEngine() != kind || current.GetPort() != 9000 || current.GetHttpPort() != 8123 ||
		current.GetVolumeId() == "" || current.GetPasswordSecret() != "CLICKHOUSE_PASSWORD" || len(current.GetReadings()) != 1 {
		t.Fatalf("ClickHouse inventory: %v", current)
	}
	reading := current.GetReadings()[0]
	if reading.GetUnread() != "" || reading.GetVersion() != entry.GetVersion() || !slices.ContainsFunc(reading.GetDatabases(), func(d *v1.InnerDatabase) bool {
		return d.GetName() == "default" && d.GetEngine() == "Atomic"
	}) {
		t.Fatalf("ClickHouse inventory reading: %v", reading)
	}

	// The link writes its credential-bearing URL through an existing secret.
	connected, err := databaseClient(f.person, f.person.origin).ConnectDatabase(t.Context(), connect.NewRequest(&v1.ConnectDatabaseRequest{
		Id: data.GetId(), App: "web", AppProject: "default", AppEnvironment: appEnv, Link: "clickhouse",
		Sets: map[string]spec.LinkPart{
			"CLICKHOUSE_URL":         spec.LinkPart_LINK_PART_URL,
			"CLICKHOUSE_NATIVE_PORT": spec.LinkPart_LINK_PART_NATIVE_PORT,
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	link := connected.Msg.GetChange()
	storageScope(t, link, "link", "clickhouse")
	storageScope(t, link, "network", "clickhouse-link")
	if slices.ContainsFunc(link.GetPlan().GetResources(), func(r *v1.PlannedResource) bool {
		return r.GetResource().GetKind() == "database.user"
	}) {
		t.Fatal("the link should use the existing ClickHouse user")
	}
	applyConfirmed(t, f.changes, link)
	app := f.workload(t, appEnv)
	if !slices.Contains(app.Config.Env, "CLICKHOUSE_NATIVE_PORT=9000") || !slices.ContainsFunc(app.Config.Env, func(v string) bool {
		return strings.HasPrefix(v, "CLICKHOUSE_URL=http://default:") && strings.HasSuffix(v, "@db:8123/default")
	}) {
		t.Fatal("the app did not receive both ClickHouse endpoints")
	}
	query := func(sql string) string {
		t.Helper()
		var result bytes.Buffer
		code, err := dockerClient.Exec(t.Context(), app.ID, []string{
			"sh", "-ec",
			`wget -qO- --post-data "$1" "${CLICKHOUSE_URL%/*}/?database=${CLICKHOUSE_URL##*/}"`, "sh", sql,
		},
			docker.Streams{Stdout: &result})
		if err != nil || code != 0 {
			t.Fatalf("query %q through the app link: exit %d, %v", sql, code, err)
		}
		return strings.TrimSpace(result.String())
	}
	if f.engine == nil {
		if got := query("SELECT value FROM system.server_settings WHERE name = 'max_server_memory_usage'"); got != "335544320" {
			t.Fatalf("server memory cap in bytes: %q", got)
		}
		query("CREATE TABLE notes (id UInt64) ENGINE = MergeTree ORDER BY id")
		query("INSERT INTO notes VALUES (42)")
		if got := query("SELECT id FROM notes"); got != "42" {
			t.Fatalf("inserted row: %q", got)
		}
	}
	restart, err := f.changes.RestartService(t.Context(), connect.NewRequest(&v1.RestartServiceRequest{Id: data.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, f.changes, restart.Msg.GetChange())
	if f.engine == nil {
		if got := query("SELECT id FROM notes"); got != "42" {
			t.Fatalf("row after restart: %q", got)
		}
	}
	unlink, err := f.changes.UndoChange(t.Context(), connect.NewRequest(&v1.UndoChangeRequest{Id: link.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, f.changes, unlink.Msg.GetChange())
	applyConfirmed(t, f.changes, propose(t, f.changes, "Remove ClickHouse", &v1.Edit{Resource: data.GetResource(), Delete: true}))
	if _, err := dockerClient.ContainerOf(t.Context(), docker.Project(env), "db", 1); !docker.IsNotFound(err) {
		t.Fatalf("the ClickHouse container should be gone: %v", err)
	}
}
