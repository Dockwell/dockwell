// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/database"
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
				if slices.Contains(x.Cmd, "--config-file=/dev/stdin") {
					return clickHouseProbeResult(x, false)
				}
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
		c, err := dockerClient.ServiceContainer(t.Context(), docker.Project(env), "db")
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
	request := &v1.ConnectDatabaseRequest{
		Id: data.GetId(), App: "web", AppProject: "default", AppEnvironment: appEnv, Link: "clickhouse",
		Sets: map[string]spec.LinkPart{
			"CLICKHOUSE_URL":         spec.LinkPart_LINK_PART_URL,
			"CLICKHOUSE_NATIVE_PORT": spec.LinkPart_LINK_PART_NATIVE_PORT,
		},
	}
	client := databaseClient(f.person, f.person.origin)
	if f.engine != nil {
		developer, _ := narrowedToken(t, f.person, &v1.CreateTokenRequest{Role: v1.TokenRole_TOKEN_ROLE_DEVELOPER})
		developerDB := databaseClient(bearer{token: developer, rt: f.person.client.Transport}, f.person.origin)
		if _, err := developerDB.ConnectDatabase(t.Context(), connect.NewRequest(request)); connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "needs_role" {
			t.Fatalf("sharing the administrator password as a developer: %v", err)
		}
		partsOnly := proto.CloneOf(request)
		delete(partsOnly.Sets, "CLICKHOUSE_URL")
		if _, err := client.ConnectDatabase(t.Context(), connect.NewRequest(partsOnly)); reasonOf(err) != "invalid_sets" {
			t.Fatalf("link without credentials: %v", err)
		}
	}
	connected, err := client.ConnectDatabase(t.Context(), connect.NewRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	link := connected.Msg.GetChange()
	if noteOf(link, "clickhouse_admin_link").GetReason() == "" {
		t.Fatal("the plan did not disclose administrator access")
	}
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
	checkVolume := func() {
		t.Helper()
		code, err := dockerClient.Exec(t.Context(), inspect().ID, []string{
			"sh", "-ec",
			`grep -q password_sha256_hex /var/lib/clickhouse/preprocessed_configs/users.xml; code=0; printf '%s\n' "$CLICKHOUSE_PASSWORD" | grep -rFq -f - /var/lib/clickhouse || code=$?; test "$code" -eq 1`,
		}, docker.Streams{})
		if err != nil || code != 0 {
			t.Fatalf("checking the hashed password and scanning the volume: exit %d, %v", code, err)
		}
	}
	if f.engine == nil {
		checkVolume()
		if got := query("SELECT count() FROM system.tables WHERE database = 'system' AND endsWith(name, '_log')"); got != "0" {
			t.Fatalf("unbounded system log tables: %q", got)
		}
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
		checkVolume()
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
	if _, err := dockerClient.ServiceContainer(t.Context(), docker.Project(env), "db"); !docker.IsNotFound(err) {
		t.Fatalf("the ClickHouse container should be gone: %v", err)
	}
	if _, err := dockerClient.InspectVolume(t.Context(), docker.ScopedName(docker.Project(env), "data")); err != nil {
		t.Fatalf("removing the ClickHouse service should keep its data volume: %v", err)
	}
}

func TestClickHouseTakeOnAndSetPassword(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		f := storageControl(t, b, nil, "")
		const image = "clickhouse/clickhouse-server:24.12.6.70-alpine"
		f.engine.Registry[image] = dockertest.Image{
			Exec: func(x dockertest.Exec) int {
				_, protected := x.Volumes[database.ClickHouseAuthTarget]
				if slices.Contains(x.Cmd, "--config-file=/dev/stdin") {
					return clickHouseProbeResult(x, !protected)
				}
				if protected {
					_, _ = io.WriteString(x.Stdout, `{"version":"25.8.33.6","user":"default","plaintext_password":false,"unbounded_logs":true}`+"\n")
				} else {
					// The pinned binary's inventory row on its stock users.xml.
					_, _ = io.WriteString(x.Stdout, `{"version":"25.8.33.6","user":"default","uptime":2,"size_bytes":526528,"connections":1,"max_connections":4096,"hits":0,"misses":0,"plaintext_password":true,"unbounded_logs":true,"unlisted":0}`+"\n")
				}
				_, _ = io.WriteString(x.Stdout, `{"name":"analytics","engine":"Atomic"}`+"\n")
				return 0
			},
		}
		f.engine.Registry["busybox:1.37"] = dockertest.Image{}
		taken := propose(t, f.changes, "Take on an analytics stack",
			&v1.Edit{
				Resource: &v1.ResourceRef{Kind: "service", Name: "events"},
				Spec: &spec.Spec{Kind: &spec.Spec_Service{Service: &spec.Service{
					Runs: &spec.Service_App{App: &spec.Container{
						Image: image, Command: []string{"sh", "-c", "printf '%s' ready >/tmp/ready && exec /entrypoint.sh"},
						Env: map[string]string{"CLICKHOUSE_SKIP_USER_SETUP": "1"},
					}},
				}}},
			},
			&v1.Edit{
				Resource: &v1.ResourceRef{Kind: "secret", Name: "EVENTS_URL"},
				Spec:     &spec.Spec{Kind: &spec.Spec_Secret{Secret: &spec.Secret{Value: "http://example.com/analytics"}}},
			},
			&v1.Edit{
				Resource: &v1.ResourceRef{Kind: "secret", Name: "SECOND_URL"},
				Spec:     &spec.Spec{Kind: &spec.Spec_Secret{Secret: &spec.Secret{Value: "http://example.com/analytics"}}},
			},
			&v1.Edit{
				Resource: &v1.ResourceRef{Kind: "service", Name: "web"},
				Spec: &spec.Spec{Kind: &spec.Spec_Service{Service: &spec.Service{
					Runs: &spec.Service_App{
						App: &spec.Container{
							Image: "busybox:1.37",
							Env:   map[string]string{"CLICKHOUSE_DATABASE_URL": "http://events:8123/analytics"},
						},
					},
					Secrets: map[string]string{"LINK_URL": "EVENTS_URL"},
				}}},
			},
			&v1.Edit{
				Resource: &v1.ResourceRef{Kind: "service", Name: "worker"},
				Spec: &spec.Spec{Kind: &spec.Spec_Service{Service: &spec.Service{
					Runs:    &spec.Service_App{App: &spec.Container{Image: "busybox:1.37"}},
					Secrets: map[string]string{"LINK_URL": "SECOND_URL"},
				}}},
			},
		)
		applyConfirmed(t, f.changes, taken)
		server := storageScope(t, taken, "service", "events")
		appEnv := server.GetResource().GetScope()
		dc := databaseClient(f.person, f.person.origin)
		get := func() *v1.Database {
			t.Helper()
			out, err := dc.GetDatabase(t.Context(), connect.NewRequest(&v1.GetDatabaseRequest{Id: server.GetId()}))
			if err != nil {
				t.Fatal(err)
			}
			return out.Msg.GetDatabase()
		}
		if db := get(); db.GetEngine() != v1.DatabaseEngine_DATABASE_ENGINE_CLICKHOUSE || !db.GetReadings()[0].GetChecksNoOne() ||
			db.GetReadings()[0].GetClickhousePlaintextPassword() || db.GetReadings()[0].GetClickhousePasswordVerified() ||
			!db.GetReadings()[0].GetClickhouseUnboundedLogs() {
			t.Fatal("the catalog-shaped passwordless server was not recognized")
		}
		if _, err := dc.SetDatabaseExternalAccess(t.Context(), connect.NewRequest(&v1.SetDatabaseExternalAccessRequest{
			Id: server.GetId(), AllowFrom: []string{"203.0.113.42"},
		})); reasonOf(err) != "password_required" {
			t.Fatalf("exposing passwordless ClickHouse: %v", err)
		}
		request := &v1.ConnectDatabaseRequest{
			Id: server.GetId(), App: "web", Link: "events", Database: "analytics",
			Sets: map[string]spec.LinkPart{"EVENTS_URL": spec.LinkPart_LINK_PART_URL},
		}
		other := propose(t, f.changes, "Make a separate project", &v1.Edit{
			Resource: &v1.ResourceRef{Kind: "project", Name: "clickhouse-consumer"},
			Spec:     &spec.Spec{Kind: &spec.Spec_Project{Project: &spec.Project{}}},
		})
		applyConfirmed(t, f.changes, other)
		project := storageScope(t, other, "project", "clickhouse-consumer").GetId()
		env := propose(t, f.changes, "Make a separate environment", &v1.Edit{
			Resource: &v1.ResourceRef{Kind: "environment", Scope: project, Name: "staging"},
			Spec:     &spec.Spec{Kind: &spec.Spec_Environment{Environment: &spec.Environment{}}},
		})
		applyConfirmed(t, f.changes, env)
		otherEnv := storageScope(t, env, "environment", "staging").GetId()
		applyConfirmed(t, f.changes, propose(t, f.changes, "Run the separate app",
			&v1.Edit{
				Resource: &v1.ResourceRef{Kind: "secret", Scope: otherEnv, Name: "EVENTS_URL"},
				Spec:     &spec.Spec{Kind: &spec.Spec_Secret{Secret: &spec.Secret{Value: "http://example.com/analytics"}}},
			},
			&v1.Edit{
				Resource: &v1.ResourceRef{Kind: "service", Scope: otherEnv, Name: "web"},
				Spec: &spec.Spec{Kind: &spec.Spec_Service{Service: &spec.Service{
					Runs:    &spec.Service_App{App: &spec.Container{Image: "busybox:1.37"}},
					Secrets: map[string]string{"LINK_URL": "EVENTS_URL"},
				}}},
			},
		))
		across := proto.CloneOf(request)
		across.AppProject, across.AppEnvironment = project, otherEnv
		if _, err := dc.ConnectDatabase(t.Context(), connect.NewRequest(across)); reasonOf(err) != "administered" {
			t.Fatalf("linking passwordless ClickHouse across environments: %v", err)
		}
		openLink, err := dc.ConnectDatabase(t.Context(), connect.NewRequest(request))
		if err != nil {
			t.Fatal(err)
		}
		second := proto.CloneOf(request)
		second.App, second.Link = "worker", "another"
		second.Sets = map[string]spec.LinkPart{"SECOND_URL": spec.LinkPart_LINK_PART_URL}
		if _, err := dc.ConnectDatabase(t.Context(), connect.NewRequest(second)); reasonOf(err) != "administered" {
			t.Fatalf("another app linked before the companion: %v", err)
		}
		otherPlan := propose(t, f.changes, "Plan a competing link", &v1.Edit{
			Resource: &v1.ResourceRef{Kind: "link", Scope: appEnv, Name: "another"},
			Spec: &spec.Spec{Kind: &spec.Spec_Link{Link: &spec.Link{
				Label: "Database", Service: "events", Engine: spec.LinkEngine_LINK_ENGINE_CLICKHOUSE, User: "default",
				Accepts: []*spec.LinkAccept{{Engine: spec.LinkEngine_LINK_ENGINE_CLICKHOUSE}},
				Sets:    []*spec.LinkSet{{Name: "SECOND_URL", Part: spec.LinkPart_LINK_PART_URL, Template: "http://{host}:{port}/{database}"}},
			}}},
		})
		applyConfirmed(t, f.changes, openLink.Msg.GetChange())
		if _, err := f.changes.ApplyChange(t.Context(), connect.NewRequest(&v1.ApplyChangeRequest{
			Id: otherPlan.GetId(), Fingerprint: otherPlan.GetPlan().GetFingerprint(),
		})); err == nil || !strings.Contains(err.Error(), "already has an app link") {
			t.Fatalf("applying a second administrator link planned before the first applied: %v", err)
		}
		checkURL := func(prefix string) {
			t.Helper()
			if !slices.ContainsFunc(f.workload(t, appEnv).Config.Env, func(v string) bool {
				return strings.HasPrefix(v, "LINK_URL="+prefix) && strings.HasSuffix(v, "events:8123/analytics")
			}) {
				t.Fatal("the ClickHouse URL did not reach the app")
			}
		}
		checkURL("http://")

		// Both the original catalog URL and the managed link follow the new secret.
		plan, err := dc.ChangeDatabasePassword(t.Context(), connect.NewRequest(&v1.ChangeDatabasePasswordRequest{Id: server.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		change := plan.Msg.GetChange()
		storageScope(t, change, "secret", "EVENTS_CLICKHOUSE_PASSWORD")
		storageScope(t, change, "service", "web")
		applyConfirmed(t, f.changes, change)
		if db := get(); db.GetPasswordSecret() == "" || db.GetReadings()[0].GetChecksNoOne() || !db.GetReadings()[0].GetClickhousePasswordVerified() {
			t.Fatalf("the ClickHouse password was not set: %v", db.GetReadings())
		}
		if !slices.ContainsFunc(f.workload(t, appEnv).Config.Env, func(v string) bool {
			return strings.HasPrefix(v, "CLICKHOUSE_DATABASE_URL=http://default:") && strings.HasSuffix(v, "@events:8123/analytics")
		}) {
			t.Fatal("setting the password did not update the existing app")
		}
		checkURL("http://default:")

		openResource := storageScope(t, openLink.Msg.GetChange(), "link", "events").GetResource()
		applyConfirmed(t, f.changes, propose(t, f.changes, "Remove the original link", &v1.Edit{Resource: openResource, Delete: true}))
		if _, err := dc.ConnectDatabase(t.Context(), connect.NewRequest(second)); reasonOf(err) != "administered" {
			t.Fatalf("another app linked despite the companion's password-backed URL: %v", err)
		}
		request.Link = "protected"
		link, err := dc.ConnectDatabase(t.Context(), connect.NewRequest(request))
		if err != nil {
			t.Fatal(err)
		}
		applyConfirmed(t, f.changes, link.Msg.GetChange())
		checkURL("http://default:")

		logs, err := dc.BoundClickHouseLogs(t.Context(), connect.NewRequest(&v1.BoundClickHouseLogsRequest{Id: server.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		applyConfirmed(t, f.changes, logs.Msg.GetChange())
		if reading := get().GetReadings()[0]; reading.GetClickhouseUnboundedLogs() || !reading.GetClickhousePasswordVerified() {
			t.Fatalf("limiting logs did not preserve the protected database: %v", reading)
		}

		if _, err := f.changes.ProposeChange(t.Context(), connect.NewRequest(&v1.ProposeChangeRequest{
			Summary: "Clear the password", Edits: []*v1.Edit{
				{
					Resource: &v1.ResourceRef{Kind: "variable", Scope: appEnv, Name: "EMPTY_SETTING"},
					Spec:     &spec.Spec{Kind: &spec.Spec_Variable{Variable: &spec.Variable{}}},
				},
				{
					Resource: &v1.ResourceRef{Kind: "secret", Scope: appEnv, Name: "EVENTS_CLICKHOUSE_PASSWORD"},
					Spec: &spec.Spec{Kind: &spec.Spec_Secret{Secret: &spec.Secret{From: &spec.ValueFrom{
						Source: &spec.ValueFrom_Template{Template: &spec.ValueTemplate{Text: "${EMPTY_SETTING}", Names: []*spec.TemplateName{{Name: "EMPTY_SETTING"}}}},
					}}}},
				},
			},
		})); err == nil || !strings.Contains(err.Error(), database.EmptyClickHouseCredential) {
			t.Fatalf("planning an empty password: %v", err)
		}
	})
}

func clickHouseProbeResult(x dockertest.Exec, open bool) int {
	config, _ := io.ReadAll(x.Stdin)
	if string(config) != "<clickhouse/>" || !slices.Contains(x.Env, "CLICKHOUSE_PASSWORD=") {
		return 1
	}
	if open {
		_, _ = io.WriteString(x.Stdout, "1\n")
		return 0
	}
	_, _ = io.WriteString(x.Stderr, "Code: 516. DB::Exception: Authentication failed. (AUTHENTICATION_FAILED)\n")
	return 1
}

func TestClickHouseRotationKeepsImageUserSettings(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		f := storageControl(t, b, nil, "")
		const image = "clickhouse/clickhouse-server:25.8.33.6-alpine"
		f.engine.Registry[image] = dockertest.Image{
			Exec: func(x dockertest.Exec) int {
				if slices.Contains(x.Cmd, "--config-file=/dev/stdin") {
					return clickHouseProbeResult(x, false)
				}
				_, _ = io.WriteString(x.Stdout, "{\"version\":\"25.8.33.6\",\"user\":\"analytics\",\"plaintext_password\":true}\n")
				return 0
			},
		}
		start := propose(t, f.changes, "Take on a database with a custom administrator", &v1.Edit{
			Resource: &v1.ResourceRef{Kind: "service", Name: "events"},
			Spec: &spec.Spec{Kind: &spec.Spec_Service{Service: &spec.Service{
				Runs: &spec.Service_App{App: &spec.Container{Image: image, Env: map[string]string{
					"CLICKHOUSE_USER": "analytics", "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT": "1", "CLICKHOUSE_PASSWORD": "fixture-value",
				}}},
			}}},
		})
		applyConfirmed(t, f.changes, start)
		server := storageScope(t, start, "service", "events")
		dc := databaseClient(f.person, f.person.origin)
		plan, err := dc.ChangeDatabasePassword(t.Context(), connect.NewRequest(&v1.ChangeDatabasePasswordRequest{Id: server.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		applyConfirmed(t, f.changes, plan.Msg.GetChange())
		app := f.engine.Newest(docker.Project(server.GetResource().GetScope()), "events")
		if app == nil || !slices.Contains(app.Config.Env, "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1") {
			t.Fatal("rotation lost the administrator's access-management setting")
		}
		for _, m := range app.Mounts {
			if m.Destination != database.ClickHouseAuthTarget || m.RW {
				continue
			}
			body, err := os.ReadFile(m.Source)
			if err != nil {
				t.Fatal(err)
			}
			for _, fragment := range []string{
				"<analytics>", "<profile>default</profile>", "<quota>default</quota>", "<ip>::/0</ip>",
				`<access_management from_env="CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT"/>`,
			} {
				if !bytes.Contains(body, []byte(fragment)) {
					t.Errorf("users config is missing %s", fragment)
				}
			}
			return
		}
		t.Fatal("rotation did not mount a read-only users config")
	})
}

func TestClickHouseCatalogUpgradeKeepsSetPassword(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		const events = "clickhouse/clickhouse-server:25.8.33.6-alpine@sha256:" + digest1
		entry := func(release int, version, web, command string) string {
			compose := testEntry(release, version, web, "    events:\n      image: "+events+"\n"+
				"      environment:\n        CLICKHOUSE_SKIP_USER_SETUP: '1'\n"+command)
			return strings.Replace(compose, "        PASSWORD: ${PASSWORD}\n",
				"        PASSWORD: ${PASSWORD}\n        CLICKHOUSE_DATABASE_URL: http://events:8123/analytics\n", 1)
		}
		u := startUpgrading(t, b, entry(1, "1.0", firstRelease, "      command: [\"sh\", \"-c\", \"printf ready && exec /entrypoint.sh\"]\n"))
		dc := databaseClient(u.b, u.b.origin)
		plan, err := dc.ChangeDatabasePassword(t.Context(), connect.NewRequest(&v1.ChangeDatabasePasswordRequest{
			Id: "events", Project: "thing",
		}))
		if err != nil {
			t.Fatal(err)
		}
		u.applied(plan.Msg.GetChange())
		before := u.e.Newest(docker.Project(u.env), "events")
		credentials := func(c *docker.Container, key string) string {
			for _, v := range c.Config.Env {
				if value, ok := strings.CutPrefix(v, key+"="); ok {
					return value
				}
			}
			return ""
		}
		password := credentials(before, "CLICKHOUSE_PASSWORD")
		if password == "" || !database.ClickHouseAuthWrapped(before.Config.Cmd) {
			t.Fatal("the password plan did not prepare the catalog database")
		}
		u.release(entry(2, "2.0", secondRelease, ""))
		u.applied(u.upgrade())
		after := u.e.Newest(docker.Project(u.env), "events")
		if !database.ClickHouseAuthWrapped(after.Config.Cmd) || credentials(after, "CLICKHOUSE_PASSWORD") != password ||
			credentials(after, "CLICKHOUSE_SKIP_USER_SETUP") != "1" ||
			!slices.ContainsFunc(after.Mounts, func(m docker.MountPoint) bool {
				return m.Destination == database.ClickHouseAuthTarget && !m.RW
			}) {
			t.Fatal("the upgrade dropped the local database authentication")
		}
		if got := credentials(u.web(), "CLICKHOUSE_DATABASE_URL"); !strings.HasPrefix(got, "http://default:"+password+"@events:8123/") {
			t.Fatal("the upgraded companion lost its password-backed URL")
		}
	})
}
