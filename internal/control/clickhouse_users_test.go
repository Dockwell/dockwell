// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	rv "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestClickHouseDedicatedUserLinksAndRotates(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := storageControl(t, backend, nil, "")
		f.engine.Registry["busybox:1.37"] = dockertest.Image{}
		cat := catalogClient(f.person)
		entry, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&v1.GetCatalogEntryRequest{Id: "clickhouse"}))
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var commands []string
		users, databases := map[string]bool{}, map[string]bool{}
		for tag, digest := range catalog.PinnedDigests(entry.Msg.GetEntry().GetCompose()) {
			f.engine.Registry[tag+"@"+digest] = dockertest.Image{Exec: func(x dockertest.Exec) int {
				if slices.Contains(x.Cmd, "--config-file=/dev/stdin") {
					return clickHouseProbeResult(x, false)
				}
				if x.Stdin == nil || strings.Contains(strings.Join(x.Cmd, " "), "select_from_system_db_requires_grant") {
					return 0
				}
				body, _ := io.ReadAll(x.Stdin)
				sql := string(body)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case strings.Contains(sql, "SELECT version() AS version"):
					_, _ = io.WriteString(x.Stdout, "{\"version\":\"26.3.62.3\",\"user\":\"default\"}\n{\"name\":\"default\",\"engine\":\"Atomic\"}\n")
				case strings.HasPrefix(sql, "SELECT 'u'"):
					for _, name := range []string{"web_events", "worker_events"} {
						if strings.Contains(sql, "'"+name+"'") {
							if users[name] {
								_, _ = io.WriteString(x.Stdout, "u\t1\n")
							}
							if databases[name] {
								_, _ = io.WriteString(x.Stdout, "d\t1\n")
							}
						}
					}
				default:
					commands = append(commands, sql)
					for _, name := range []string{"web_events", "worker_events"} {
						if strings.Contains(sql, "CREATE DATABASE IF NOT EXISTS `"+name+"`") {
							databases[name], users[name] = true, true
						}
					}
				}
				return 0
			}}
		}
		var firstEnv string
		readURL := func(app, secret, env string) *url.URL {
			t.Helper()
			c := f.engine.Newest(docker.Project(env), app)
			if c == nil {
				t.Fatal("missing app container")
			}
			for _, value := range c.Config.Env {
				if raw, ok := strings.CutPrefix(value, secret+"="); ok {
					u, err := url.Parse(raw)
					if err != nil || u.User == nil {
						t.Fatal("connection URL is missing its credentials")
					}
					return u
				}
			}
			t.Fatal("the app did not receive its connection URL")
			return nil
		}
		for _, app := range []string{"web", "worker"} {
			secret := strings.ToUpper(app) + "_CLICKHOUSE_URL"
			initial := propose(t, f.changes, "Run an app that needs ClickHouse",
				&v1.Edit{
					Resource: &v1.ResourceRef{Kind: "secret", Name: secret},
					Spec:     &rv.Spec{Kind: &rv.Spec_Secret{Secret: &rv.Secret{Value: "http://example.invalid"}}},
				},
				&v1.Edit{
					Resource: &v1.ResourceRef{Kind: "service", Name: app},
					Spec: &rv.Spec{Kind: &rv.Spec_Service{Service: &rv.Service{
						Runs:    &rv.Service_App{App: &rv.Container{Image: "busybox:1.37", Command: []string{"sleep", "600"}}},
						Secrets: map[string]string{secret: secret},
					}}},
				})
			appEnv := storageScope(t, initial, "service", app).GetResource().GetScope()
			applyConfirmed(t, f.changes, initial)
			firstEnv = appEnv
		}
		installed := installApp(t, cat, &v1.InstallCatalogEntryRequest{Id: "clickhouse", Project: "clickhouse-fixture", Servers: []string{f.server.id.ServerID}})
		service := storageScope(t, installed, "service", "db")
		applyConfirmed(t, f.changes, installed)
		db := databaseClient(f.person, f.person.origin)
		for _, app := range []string{"web", "worker"} {
			secret := strings.ToUpper(app) + "_CLICKHOUSE_URL"
			appEnv := firstEnv
			request := &v1.ConnectDatabaseRequest{
				Id: service.GetId(), App: app, AppProject: "default", AppEnvironment: appEnv,
				Link: app + "-db", Database: app + "_events", DedicatedUser: true,
				Sets: map[string]rv.LinkPart{secret: rv.LinkPart_LINK_PART_URL},
			}
			linked, err := db.ConnectDatabase(t.Context(), connect.NewRequest(request))
			if err != nil {
				t.Fatal(err)
			}
			ch := linked.Msg.GetChange()
			storageScope(t, ch, "database.user", app+"_events")
			applyConfirmed(t, f.changes, ch)
			if u := readURL(app, secret, appEnv); u.User.Username() != app+"_events" || u.Path != "/"+app+"_events" {
				t.Fatal("the app did not receive its own account and database")
			}
			if app == "web" {
				firstEnv = appEnv
			}
		}
		const field = "WEB_CLICKHOUSE_URL"
		previous, _ := readURL("web", field, firstEnv).User.Password()
		rotated, err := db.ChangeDatabasePassword(t.Context(), connect.NewRequest(&v1.ChangeDatabasePasswordRequest{
			Id: service.GetId(), User: "web_events",
		}))
		if err != nil {
			t.Fatal(err)
		}
		applyConfirmed(t, f.changes, rotated.Msg.GetChange())
		next, _ := readURL("web", field, firstEnv).User.Password()
		if next == "" || previous == "" || next == previous {
			t.Fatal("the new password did not follow the app's link")
		}
		mu.Lock()
		defer mu.Unlock()
		got := strings.Join(commands, "\n")
		if strings.Contains(got, previous) || strings.Contains(got, next) ||
			!strings.Contains(got, "GRANT ALL ON `web_events`.* TO `web_events`") ||
			!strings.Contains(got, "GRANT ALL ON `worker_events`.* TO `worker_events`") ||
			!strings.Contains(got, "REVOKE BACKUP ON `web_events`.* FROM `web_events`") ||
			!strings.Contains(got, "max_threads=2 CONST, max_insert_threads=1 CONST, max_memory_usage=335544320 CONST") {
			t.Fatal("the plans did not set hashed, database-scoped users")
		}
	})
}
