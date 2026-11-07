// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	agentlogs "github.com/dockwell/dockwell/internal/agent/logs"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// addDockerServer enrolls hz-1, whose agent runs the Docker drivers on a
// fake Docker Engine.
func (f *fullServer) addDockerServer(t *testing.T) *dockertest.Engine {
	t.Helper()
	return f.addServerWith(t, nil)
}

// addServerWith is addDockerServer with more drivers on the server's agent.
func (f *fullServer) addServerWith(t *testing.T, more func() []resource.Type) *dockertest.Engine {
	t.Helper()
	return f.addServerNamed(t, "hz-1", more, nil)
}

// addServerNamed adds a server by name. prepare runs before its agent connects, for
// what is on its disk before it joins.
func (f *fullServer) addServerNamed(t *testing.T, name string, more func() []resource.Type, prepare func(*dockertest.Engine, *agentlogs.Archive)) *dockertest.Engine {
	t.Helper()
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.EnrollLocal(t.Context(), csr, name)
	if err != nil {
		t.Fatal(err)
	}
	e := dockertest.New(t)
	e.Registry["ghcr.io/acme/shop:1.4"] = dockertest.Image{Size: 30 << 20, Config: docker.Config{Cmd: []string{"shop"}, Env: []string{"PATH=/bin"}}}
	e.Registry["ghcr.io/acme/shop:1.5"] = dockertest.Image{Size: 30 << 20, Config: docker.Config{Cmd: []string{"shop"}, Env: []string{"PATH=/bin"}}}
	e.Registry["postgres:17"] = dockertest.Image{Volumes: []string{"/var/lib/postgresql/data"}, Config: docker.Config{Cmd: []string{"postgres"}}}
	e.Registry["ghcr.io/acme/worker:1.0"] = dockertest.Image{Config: docker.Config{Cmd: []string{"work"}}}
	e.Registry["alpine:latest"] = dockertest.Image{Config: docker.Config{Cmd: []string{"sh"}}}
	client := e.Client()
	client.Adopted = &docker.Adopted{}
	archive := &agentlogs.Archive{Dir: t.TempDir(), Client: client, Logger: slog.New(slog.DiscardHandler), Free: func(string) (uint64, error) { return 1 << 40, nil }}
	containers := container.Driver{Runtime: &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Logs: archive, Paths: &container.Paths{Client: client, DataDir: t.TempDir()}}}
	helper := filepath.Join(t.TempDir(), "dockwell")
	if err := os.WriteFile(helper, []byte("#!agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backups := &backup.Runner{Client: client, Root: t.TempDir(), Binary: helper, Work: 1000}
	volumes := &volume.Driver{Runtime: &volume.Docker{Client: client, Kept: &volume.Kept{}}, Restores: backups, BacksUp: backups}
	var volumeType resource.Type = volumes
	if f.volumes != nil {
		volumeType = f.volumes(volumes)
	}
	drivers := []resource.Type{&containers, &service.Driver{Containers: containers}, volumeType, &network.Driver{Client: client}}
	if more != nil {
		drivers = append(drivers, more()...)
	}
	if prepare != nil {
		prepare(e, archive)
	}
	s := &testServer{
		id:      enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		drivers: resource.NewRegistry(drivers...),
		logs:    &agentlogs.Reader{Client: client, Archive: archive, Logger: slog.New(slog.DiscardHandler)},
		client:  client,
		backups: backups,
		mounts:  e.Mountinfo,
	}
	s.connect(t, f)
	f.lastDocker = s
	return e
}

const shopStack = `
services:
  web:
    image: ghcr.io/acme/shop:${TAG:-1.4}
    environment:
      DATABASE_URL: postgres://shop:${DB_PASSWORD}@db/shop
    ports: ["8080:8080"]
    depends_on: [db]
    restart: unless-stopped
  db:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
    volumes: [db-data:/var/lib/postgresql/data]
  worker:
    image: ghcr.io/acme/worker:1.0
volumes:
  db-data: {}
`

func importCompose(t *testing.T, c dockwellv1connect.ComposeServiceClient, file string, vars map[string]string, orphans bool) *dockwellv1.Change {
	t.Helper()
	res, err := c.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: scope.DefaultProject, Compose: file, Variables: vars, RemoveOrphans: orphans}))
	if err != nil {
		t.Fatalf("ImportCompose: %v", err)
	}
	return res.Msg.GetChange()
}

// A Compose file becomes services, volumes and networks with a plan, runs
// on Docker as docker compose would run it, exports back as a file
// docker compose reads the same, and changes and undoes like anything
// else.
func TestComposeThroughTheAPI(t *testing.T) { storetest.Each(t, testComposeThroughTheAPI) }

func testComposeThroughTheAPI(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	vars := map[string]string{"DB_PASSWORD": "s3cret-pw"}

	// A variable with no value and no default is refused, by name.
	_, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: shopStack}))
	if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "the Compose file uses DB_PASSWORD, with no value") {
		t.Fatalf("importing without DB_PASSWORD = %v", err)
	}

	// What a container may never mount is refused as the file imports, as
	// a bind or a volume's bind device; another server path is host access.
	for file, want := range map[string]string{
		"services:\n  sh:\n    image: alpine\n    volumes: [\"/:/host\"]\n":                                                                                       "the server path / mounted at /host is the server's whole file system",
		"services:\n  sh:\n    image: alpine\n    volumes: [\"keys:/k\"]\nvolumes:\n  keys:\n    driver_opts: {type: none, o: bind, device: /var/lib/dockwell}\n": "the server path /var/lib/dockwell that the volume binds is in Dockwell's data directory",
	} {
		_, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: file}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("importing %q = %v, want it refused, saying %s", file, err, want)
		}
	}
	logs := importCompose(t, cc, "services:\n  sh:\n    image: alpine\n    volumes: [\"/srv/logs:/logs:ro\"]\n", nil, false)
	if !logs.GetPlan().GetHostAccess() {
		t.Errorf("importing a bind of /srv/logs plans no host access: %v", logs.GetPlan())
	}
	if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: logs.GetId()})); err != nil {
		t.Fatal(err)
	}

	planned := importCompose(t, cc, shopStack, vars, false)
	p := planned.GetPlan()
	var order []string
	for _, r := range p.GetResources() {
		order = append(order, r.GetResource().GetKind()+" "+r.GetResource().GetName())
	}
	if want := []string{"network default", "volume db-data", "service db", "service web", "service worker"}; !slices.Equal(order, want) {
		t.Errorf("planned %v, want %v: what services need first, and db before web, which depends on it", order, want)
	}
	if p.GetSummary().GetCreate() != 5 || p.GetDestructive() {
		t.Errorf("plan summary %v, destructive %t", p.GetSummary(), p.GetDestructive())
	}
	if strings.Contains(planned.String(), "s3cret-pw") {
		t.Error("the plan holds a secret")
	}

	w := watch(t, c, planned.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: planned.GetId()})); err != nil {
		t.Fatal(err)
	}
	watched := waitWatch(t, w)
	if got := watched.last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the import ended %v: %s", got.GetState(), got.GetError())
	}
	pulled := false
	for _, m := range watched.msgs {
		if s := m.GetStep(); s != nil && strings.HasPrefix(s.GetStep().GetActivity(), "Pulling postgres:17") {
			pulled = true
		}
	}
	if !pulled {
		t.Error("the watch never said what hz-1 was pulling")
	}

	envID := planned.GetPlan().GetResources()[0].GetResource().GetScope()
	project := docker.Project(envID)
	web := e.Container(project + "-web-1")
	if web == nil || web.Config.Labels[docker.LabelProject] != project || web.Config.Labels[docker.LabelService] != "web" {
		t.Fatalf("Docker has %v; want web as Compose's project %s runs it", e.Containers(), project)
	}
	if _, on := web.NetworkSettings.Networks[project+"_default"]; !on {
		t.Errorf("web is on %v, want the project's default network", web.NetworkSettings.Networks)
	}
	if e.Volume(project+"_db-data") == nil {
		t.Error("the db-data volume isn't on Docker")
	}
	drift, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil || len(drift.Msg.GetDrift()) != 0 {
		t.Fatalf("drift right after the import = %v, %v", drift.Msg.GetDrift(), err)
	}

	// The environment exports as a file docker compose reads as Dockwell
	// runs it, without the secrets.
	ex, err := cc.ExportCompose(t.Context(), connect.NewRequest(&dockwellv1.ExportComposeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	file := ex.Msg.GetCompose()
	if strings.Contains(file, "s3cret-pw") || !strings.Contains(file, "name: "+project) {
		t.Errorf("export:\n%s", file)
	}
	if want := []string{"db.POSTGRES_PASSWORD", "web.DATABASE_URL"}; !slices.Equal(ex.Msg.GetOmitted(), want) {
		t.Errorf("omitted %v, want %v", ex.Msg.GetOmitted(), want)
	}
	back, err := compose.Parse(t.Context(), []byte(file), map[string]string{"POSTGRES_PASSWORD": "x", "DATABASE_URL": "y"})
	if err != nil {
		t.Fatalf("the export doesn't read back: %v\n%s", err, file)
	}
	if len(back.Services) != 3 || len(back.Volumes) != 1 || len(back.Networks) != 1 {
		t.Errorf("the export reads back as %d services, %d volumes and %d networks", len(back.Services), len(back.Volumes), len(back.Networks))
	}
	one, err := cc.ExportCompose(t.Context(), connect.NewRequest(&dockwellv1.ExportComposeRequest{Service: "db"}))
	if err != nil {
		t.Fatal(err)
	}
	if f := one.Msg.GetCompose(); !strings.Contains(f, "db-data") || strings.Contains(f, "worker") {
		t.Errorf("exporting db:\n%s", f)
	}
	if _, err := cc.ExportCompose(t.Context(), connect.NewRequest(&dockwellv1.ExportComposeRequest{Service: "api"})); reasonOf(err) != "not_found" ||
		!strings.Contains(err.Error(), "has no service api. Its services are db, web, worker") {
		t.Errorf("exporting a service that isn't there = %v", err)
	}

	// A new tag recreates web, saying for how long, and undo puts it back.
	bump := importCompose(t, cc, shopStack, map[string]string{"DB_PASSWORD": "s3cret-pw", "TAG": "1.5"}, false)
	steps := bump.GetPlan().GetServers()[0].GetSteps()
	if len(steps) != 1 || steps[0].GetResource().GetName() != "web" || steps[0].GetAction() != dockwellv1.PlanAction_PLAN_ACTION_REPLACE ||
		steps[0].GetDowntime().AsDuration() != container.DefaultRecreateDowntime {
		t.Fatalf("a new tag plans %v, want web replaced, with %s of downtime", steps, container.DefaultRecreateDowntime)
	}
	applied := apply(t, c, bump)
	if got := e.Container(project + "-web-1"); got.Config.Image != "ghcr.io/acme/shop:1.5" {
		t.Errorf("web runs %s after the bump", got.Config.Image)
	}
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: applied.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, undo.Msg.GetChange())
	if got := e.Container(project + "-web-1"); got.Config.Image != "ghcr.io/acme/shop:1.4" {
		t.Errorf("web runs %s after the undo", got.Config.Image)
	}

	// A container changed by hand, as with docker compose, is drift.
	e.Edit(project+"-worker-1", func(c *docker.Container) { c.Config.Env = append(c.Config.Env, "DEBUG=1") })
	drift, err = c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if d := drift.Msg.GetDrift(); len(d) != 1 || d[0].GetResource().GetName() != "worker" || d[0].GetChanges()[0].GetPath() != "app.env[DEBUG]" {
		t.Errorf("drift after an edit by hand = %v", d)
	}

	// Without worker, and with remove_orphans, worker goes, and the volume
	// stays, as docker compose leaves volumes.
	without := strings.Replace(shopStack, "  worker:\n    image: ghcr.io/acme/worker:1.0\n", "", 1)
	orphans := importCompose(t, cc, without, vars, true)
	if s := orphans.GetPlan().GetSummary(); s.GetDestroy() != 1 || s.GetCreate() != 0 {
		t.Errorf("removing orphans plans %v, want worker destroyed", s)
	}
	apply(t, c, orphans)
	if e.Container(project+"-worker-1") != nil {
		t.Error("worker still runs")
	}

	// Deleting the volume destroys its data, so the plan says so, and only
	// a person who names the plan may apply it. Docker keeps the volume.
	del := propose(t, c, "Drop the database",
		&dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: envID, Name: "db"}, Delete: true},
		&dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "volume", Scope: envID, Name: "db-data"}, Delete: true},
	)
	if !del.GetPlan().GetDestructive() {
		t.Fatalf("deleting a volume isn't destructive: %v", del.GetPlan())
	}
	var destroys []string
	for _, s := range del.GetPlan().GetServers()[0].GetSteps() {
		if s.GetDestroysData() {
			destroys = append(destroys, s.GetResource().GetName())
		}
	}
	if !slices.Equal(destroys, []string{"db-data"}) {
		t.Errorf("steps that destroy data: %v, want the volume's alone", destroys)
	}
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: del.GetId()})); reasonOf(err) != "needs_confirmation" {
		t.Errorf("applying without the fingerprint = %v, want needs_confirmation", err)
	}
	w = watch(t, c, del.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: del.GetId(), Fingerprint: del.GetPlan().GetFingerprint()})); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("deleting the database ended %v: %s", got.GetState(), got.GetError())
	}
	if e.Volume(project+"_db-data") == nil {
		t.Error("the deleted volume is gone from Docker; it should be kept for 7 days")
	}
}

// Docker's error for a mount that fails can hold the volume's password,
// and a change's error is there for anyone who can read changes.
func TestAFailedMountDoesNotShowItsPassword(t *testing.T) {
	storetest.Each(t, testAFailedMountDoesNotShowItsPassword)
}

func testAFailedMountDoesNotShowItsPassword(t *testing.T, backend store.Backend) {
	const file = `services:
  app:
    image: ghcr.io/acme/shop:1.4
    volumes: ["share:/x"]
volumes:
  share:
    driver_opts: {type: cifs, device: //nas/share, o: "username=svc,password=hunter2"}
`
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	engine := f.addDockerServer(t)
	changes := changeClient(b, b.origin)
	imports := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	reader := changeClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY), rt: b.client.Transport}, b.origin)

	engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start") {
			return errors.New("error while mounting volume '/var/lib/docker/volumes/x/_data': failed to mount local volume: " +
				"mount //nas/share:/var/lib/docker/volumes/x/_data, data: username=svc,password=hunter2: permission denied")
		}
		return nil
	})
	change := importCompose(t, imports, file, nil, false)
	w := watch(t, changes, change.GetId())
	if _, err := changes.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{
		Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(),
	})); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, w)

	got, err := reader.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: change.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	text := got.Msg.String()
	if strings.Contains(text, "hunter2") {
		t.Errorf("a read-only token reads the volume's password in the change: %s", text)
	}
	if !strings.Contains(text, "permission denied") {
		t.Errorf("the change doesn't say why the mount failed: %s", text)
	}
}

// valuesStack runs web on the one server, using a variable and a secret
// from its environment, with what its own environment holds.
func valuesStack(image string, env map[string]string) []*dockwellv1.Edit {
	return []*dockwellv1.Edit{
		variableSpec("LOG_LEVEL", "debug"),
		secretSpec("DB_PASSWORD", "s3cret-pw-1"),
		{
			Resource: &dockwellv1.ResourceRef{Name: "web"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs:      &resourcev1.Service_App{App: &resourcev1.Container{Image: image, Env: env}},
				Variables: map[string]string{"LOG_LEVEL": ""},
				Secrets:   map[string]string{"DATABASE_PASSWORD": "DB_PASSWORD"},
			}}},
		},
	}
}

// An environment with variables and secrets exports with no secret in the
// file, as what it uses, and importing that file plans no change: nothing
// restarts, nothing is lost, and the service still uses the environment's
// values, so a later rotation still reaches it. What it can't keep it
// refuses, by name, and never by comparing a secret's value.
func TestAnExportImportsAgainKeepingVariablesAndSecrets(t *testing.T) {
	storetest.Each(t, testAnExportImportsAgainKeepingVariablesAndSecrets)
}

func testAnExportImportsAgainKeepingVariablesAndSecrets(t *testing.T, backend store.Backend) {
	logs := &syncBuffer{}
	f := startFullLogging(t, backend, nil, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())

	made := apply(t, c, propose(t, c, "Run web", valuesStack("ghcr.io/acme/shop:1.4", nil)...))
	project := docker.Project(listedEnv(t, b))
	web := e.Container(project + "-web-1")
	if web == nil {
		t.Fatalf("Docker has %v, want web", e.Containers())
	}
	if !slices.Contains(web.Config.Env, "DATABASE_PASSWORD=s3cret-pw-1") || !slices.Contains(web.Config.Env, "LOG_LEVEL=debug") {
		t.Fatalf("web runs with %v; the test didn't run what it meant to", web.Config.Env)
	}
	if got := web.Config.Labels[binding.LabelKey]; got != "DATABASE_PASSWORD=secret:DB_PASSWORD@1,LOG_LEVEL=variable:LOG_LEVEL@1" {
		t.Errorf("web's label says it uses %q", got)
	}
	for k, v := range web.Config.Labels {
		if strings.Contains(v, "s3cret") {
			t.Errorf("web's label %s holds the secret: %s", k, v)
		}
	}
	drift, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil || len(drift.Msg.GetDrift()) != 0 {
		t.Fatalf("drift right after the change = %v, %v", drift.Msg.GetDrift(), err)
	}

	export := func() *dockwellv1.ExportComposeResponse {
		t.Helper()
		ex, err := cc.ExportCompose(t.Context(), connect.NewRequest(&dockwellv1.ExportComposeRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return ex.Msg
	}
	first := export()
	file := first.GetCompose()
	if strings.Contains(file, "s3cret") {
		t.Errorf("the export holds the secret:\n%s", file)
	}
	for _, want := range []string{"DATABASE_PASSWORD: null", "LOG_LEVEL: debug", binding.LabelKey + ": DATABASE_PASSWORD=secret:DB_PASSWORD@1,LOG_LEVEL=variable:LOG_LEVEL@1"} {
		if !strings.Contains(file, want) {
			t.Errorf("the export doesn't say %q:\n%s", want, file)
		}
	}
	if want := []string{"web.DATABASE_PASSWORD"}; !slices.Equal(first.GetOmitted(), want) {
		t.Errorf("omitted %v, want %v", first.GetOmitted(), want)
	}

	// Importing it changes nothing: it plans nothing, and removing orphans
	// leaves the environment's variables and secrets alone.
	again := importCompose(t, cc, file, nil, true)
	if !plansNothing(again.GetPlan()) {
		t.Errorf("importing the export plans %v:\n%v", again.GetPlan().GetSummary(), again.GetPlan().GetResources())
	}
	apply(t, c, again)
	if got := e.Container(project + "-web-1"); got.ID != web.ID {
		t.Error("importing the export recreated web")
	}
	if got := export().GetCompose(); got != file {
		t.Errorf("after importing it, the export is:\n%s\nwant:\n%s", got, file)
	}

	// An edit to the file changes what it edits alone, and web goes on
	// using the values, with the secret delivered again.
	bumped := strings.ReplaceAll(file, "ghcr.io/acme/shop:1.4", "ghcr.io/acme/shop:1.5")
	bump := importCompose(t, cc, bumped, nil, false)
	if rs := bump.GetPlan().GetResources(); len(rs) != 1 || len(rs[0].GetChanges()) != 1 || rs[0].GetChanges()[0].GetPath() != "app.image" {
		t.Errorf("a new image plans %v, want the image alone", rs)
	}
	apply(t, c, bump)
	web = e.Newest(project, "web")
	if web.Config.Image != "ghcr.io/acme/shop:1.5" || !slices.Contains(web.Config.Env, "DATABASE_PASSWORD=s3cret-pw-1") {
		t.Errorf("after the bump, web runs %s with %v", web.Config.Image, web.Config.Env)
	}

	// So a rotation still reaches it, and the next export says so.
	rotated := apply(t, c, propose(t, c, "Rotate the password", secretSpec("DB_PASSWORD", "s3cret-pw-2")))
	if web = e.Newest(project, "web"); !slices.Contains(web.Config.Env, "DATABASE_PASSWORD=s3cret-pw-2") {
		t.Errorf("after the rotation, web runs with %v", web.Config.Env)
	}
	file = export().GetCompose()
	if !strings.Contains(file, "DATABASE_PASSWORD=secret:DB_PASSWORD@2") || strings.Contains(file, "s3cret") {
		t.Errorf("the export after the rotation:\n%s", file)
	}
	if got := importCompose(t, cc, file, nil, false).GetPlan(); !plansNothing(got) {
		t.Errorf("importing it again plans %v", got.GetResources())
	}

	refused := []struct {
		name, want string
		edit       func(string) string
		variables  map[string]string
	}{
		{
			name: "a secret given a value in the file", want: "DATABASE_PASSWORD is set in the app's environment and from a secret too",
			edit: func(s string) string {
				return strings.Replace(s, "DATABASE_PASSWORD: null", "DATABASE_PASSWORD: s3cret-pw-2", 1)
			},
		},
		{
			name: "a secret given another value in the file", want: "DATABASE_PASSWORD is set in the app's environment and from a secret too",
			edit: func(s string) string {
				return strings.Replace(s, "DATABASE_PASSWORD: null", "DATABASE_PASSWORD: a-guess", 1)
			},
		},
		{
			name: "a secret given a value in the request", want: "DATABASE_PASSWORD is set in the app's environment and from a secret too",
			edit: func(s string) string { return s }, variables: map[string]string{"DATABASE_PASSWORD": "a-guess"},
		},
		{
			name: "a variable given another value in the file", want: "LOG_LEVEL is set in the app's environment and from a variable too",
			edit: func(s string) string { return strings.Replace(s, "LOG_LEVEL: debug", "LOG_LEVEL: info", 1) },
		},
		{
			name: "a name with no value, and no label to say what it uses", want: "the service web's variable DATABASE_PASSWORD has no value",
			edit: func(s string) string {
				var kept []string
				for line := range strings.Lines(s) {
					if !strings.Contains(line, binding.LabelKey+":") {
						kept = append(kept, line)
					}
				}
				return strings.Join(kept, "")
			},
		},
		{
			name: "a label Dockwell didn't write", want: "has a label it can't use",
			edit: func(s string) string {
				return strings.ReplaceAll(s, "DATABASE_PASSWORD=secret:DB_PASSWORD@2,", "DATABASE_PASSWORD=volume:DB_PASSWORD@2,")
			},
		},
		{
			name: "a secret that isn't in the environment", want: "ANOTHER",
			edit: func(s string) string {
				return strings.ReplaceAll(s, "DATABASE_PASSWORD=secret:DB_PASSWORD@2,", "DATABASE_PASSWORD=secret:ANOTHER@2,")
			},
		},
	}
	for _, r := range refused {
		_, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: r.edit(file), Variables: r.variables}))
		if err == nil {
			t.Errorf("importing %s planned a change", r.name)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "a-guess") {
			t.Errorf("importing %s says a value: %v", r.name, err)
		}
		if !strings.Contains(err.Error(), r.want) {
			t.Errorf("importing %s = %v, want it to say %q", r.name, err, r.want)
		}
	}

	// Taking a name out of the file stops using it, which is an edit, and
	// the plan says so.
	stopped := strings.Replace(file, "      LOG_LEVEL: debug\n", "", 1)
	drop := importCompose(t, cc, stopped, nil, false)
	if rs := drop.GetPlan().GetResources(); len(rs) != 1 {
		t.Errorf("taking LOG_LEVEL out plans %v", rs)
	} else if !slices.ContainsFunc(rs[0].GetChanges(), func(ch *dockwellv1.FieldChange) bool { return ch.GetPath() == "variables[LOG_LEVEL]" }) {
		t.Errorf("taking LOG_LEVEL out plans %v, want it to stop using the variable", rs[0].GetChanges())
	}

	// Nothing above put a value of the secret anywhere anyone reads: not
	// the changes, the timeline's logs, or the store.
	for _, id := range []string{made.GetId(), rotated.GetId(), again.GetId(), bump.GetId(), drop.GetId()} {
		got, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got.Msg.String(), "s3cret") {
			t.Errorf("change %s holds the secret: %s", id, got.Msg)
		}
	}
	if strings.Contains(logs.String(), "s3cret") {
		t.Error("the logs hold the secret")
	}
	for table, rows := range storetest.Dump(t, f.db) {
		for _, row := range rows {
			if strings.Contains(row, "s3cret") {
				t.Errorf("%s holds the secret: %q", table, row)
			}
		}
	}
}

// A volume's options are sensitive, so its spec is sealed like a secret's:
// a CIFS password is in no table, in any encoding, on either store, and a
// plan says the options changed and never what they hold. Reading it back
// to apply it still gives Docker the password.
func TestAVolumesPasswordIsSealed(t *testing.T) { storetest.Each(t, testAVolumesPasswordIsSealed) }

func testAVolumesPasswordIsSealed(t *testing.T, backend store.Backend) {
	const password = "cifs-pw-7Hq2mXv9Lr4Kd8Zs"
	file := "services:\n  app:\n    image: ghcr.io/acme/shop:1.4\n    volumes: [\"share:/x\"]\nvolumes:\n  share:\n" +
		"    driver_opts: {type: cifs, device: //nas/share, o: \"addr=10.0.0.2,username=svc,password=" + password + "\"}\n"
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())

	planned := importCompose(t, cc, file, nil, false)
	if strings.Contains(planned.String(), password) {
		t.Errorf("the plan holds the password: %s", planned)
	}
	assertNoPassword := func(when string) {
		t.Helper()
		dump := storetest.Dump(t, f.db)
		for _, table := range []string{"change_items", "data_keys"} {
			if len(dump[table]) == 0 {
				t.Errorf("the dump has no %s; the test reads the wrong database", table)
			}
		}
		for table, rows := range dump {
			for _, row := range rows {
				for _, enc := range encodings(password) {
					if strings.Contains(row, enc) {
						t.Errorf("%s, %s holds the password, as %q: %q", when, table, enc, row)
					}
				}
			}
		}
	}
	assertNoPassword("once it is planned")
	applyConfirmed(t, c, planned)
	assertNoPassword("once it is applied")

	project := docker.Project(listedEnv(t, b))
	v := e.Volume(project + "_share")
	if v == nil || !strings.Contains(v.Options["o"], password) {
		t.Fatalf("Docker has the volume %+v; the test didn't run what it meant to", v)
	}

	// An export leaves the options out, and what they hold stays out of
	// each change that reads the volume, undo's included.
	ex, err := cc.ExportCompose(t.Context(), connect.NewRequest(&dockwellv1.ExportComposeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ex.Msg.GetCompose(), password) || !slices.Contains(ex.Msg.GetOmitted(), "volumes.share.driver_opts") {
		t.Errorf("the export omits %v:\n%s", ex.Msg.GetOmitted(), ex.Msg.GetCompose())
	}
	// Docker can't change a volume, so importing the file without its
	// options keeps the ones it was made with, plans nothing for it and says so.
	back, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: ex.Msg.GetCompose()}))
	if err != nil {
		t.Fatalf("importing the export without the options = %v, want it planned", err)
	}
	if strings.Contains(back.Msg.String(), password) {
		t.Errorf("the plan of the export holds the password: %s", back.Msg)
	}
	if !plansNothing(back.Msg.GetChange().GetPlan()) {
		t.Errorf("importing the export plans %v, want nothing", back.Msg.GetChange().GetPlan())
	}
	if !slices.ContainsFunc(back.Msg.GetChange().GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool {
		return n.GetReason() == "kept_options" && n.GetResource().GetName() == "share"
	}) {
		t.Errorf("the plan doesn't say share keeps its options: %v", back.Msg.GetChange().GetPlan().GetNotes())
	}
	if got := e.Volume(project + "_share"); got == nil || !strings.Contains(got.Options["o"], password) {
		t.Errorf("after importing the export, Docker has %+v", got)
	}
	got, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: planned.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Msg.String(), password) {
		t.Errorf("the change holds the password: %s", got.Msg)
	}
	assertNoPassword("at the end")
}

// A read-write token can import a file with a guess at a volume's CIFS password,
// and the plan can't tell it whether the guess is right: a volume keeps the
// password it was made with, whatever an edit says, and a plan never compares one
// with it. The right password and a wrong one plan alike, changing nothing, and
// each says the volume keeps its password. Anything else in the options is refused,
// as Docker can't change it. The last check of #70 found the right one planned
// nothing and a wrong one was refused, as the options couldn't change.
func TestATokenCantConfirmAGuessAtAVolumesPassword(t *testing.T) {
	storetest.Each(t, testATokenCantConfirmAGuessAtAVolumesPassword)
}

func testATokenCantConfirmAGuessAtAVolumesPassword(t *testing.T, backend store.Backend) {
	const password = "cifs-pw-7Hq2mXv9Lr4Kd8Zs"
	file := func(o string) string {
		return "services:\n  app:\n    image: ghcr.io/acme/shop:1.4\n    volumes: [\"share:/x\"]\nvolumes:\n  share:\n" +
			"    driver_opts: {type: cifs, device: //nas/share, o: \"" + o + "\"}\n"
	}
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	applyConfirmed(t, c, importCompose(t, dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()), file("addr=10.0.0.2,username=svc,password="+password), nil, false))

	script := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}
	tc := dockwellv1connect.NewComposeServiceClient(script, b.origin, connect.WithProtoJSON())
	guess := func(o string) (*dockwellv1.Plan, error) {
		t.Helper()
		res, err := tc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: scope.DefaultProject, Compose: file(o)}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange().GetPlan(), nil
	}
	right, err := guess("addr=10.0.0.2,username=svc,password=" + password)
	if err != nil {
		t.Fatalf("the right password = %v, want it planned", err)
	}
	wrong, err := guess("addr=10.0.0.2,username=svc,password=letmein")
	if err != nil {
		t.Fatalf("a wrong password = %v, want it planned as the right one is", err)
	}
	for _, p := range []*dockwellv1.Plan{right, wrong} {
		if !plansNothing(p) {
			t.Errorf("a guess plans %v, want nothing", p.GetSummary())
		}
		if !slices.ContainsFunc(p.GetNotes(), func(n *dockwellv1.PlanNote) bool {
			return n.GetReason() == "kept_password" && n.GetResource().GetName() == "share"
		}) {
			t.Errorf("a guess's plan doesn't say share keeps its password: %v", p.GetNotes())
		}
		p.Fingerprint = ""
		for _, r := range p.GetResources() {
			r.Id = ""
		}
	}
	if !proto.Equal(right, wrong) {
		t.Errorf("the right password plans\n%v\nand a wrong one\n%v", right, wrong)
	}
	if strings.Contains(right.String(), password) {
		t.Errorf("the plan holds the password: %v", right)
	}
	// The rest of o, and the other options, still can't change.
	for what, o := range map[string]string{
		"another address":          "addr=10.0.0.3,username=svc,password=letmein",
		"a password added to o":    "addr=10.0.0.2,username=svc,password=letmein,pass=x",
		"the same, in other order": "username=svc,addr=10.0.0.2,password=" + password,
	} {
		if _, err := guess(o); err == nil || !strings.Contains(err.Error(), "can't change") || strings.Contains(err.Error(), password) {
			t.Errorf("%s = %v, want it refused, with no password", what, err)
		}
	}
	project := docker.Project(listedEnv(t, b))
	if got := e.Volume(project + "_share"); got == nil || !strings.Contains(got.Options["o"], password) {
		t.Errorf("after the guesses, Docker has %+v", got)
	}
}

// planHeard is a server's volume driver, noting the options of each volume a plan
// call brings its agent, so a test can say what reached that server. A plan sends a
// spec to every server it would run on, before anyone applies it.
type planHeard struct {
	*volume.Driver
	mu    sync.Mutex
	heard []string
}

func (p *planHeard) Judge(ctx context.Context, ref resource.Ref, actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	if v, ok := desired.(*resourcev1.Volume); ok {
		p.mu.Lock()
		p.heard = append(p.heard, v.GetOptions()["o"])
		p.mu.Unlock()
	}
	return p.Driver.Judge(ctx, ref, actual, desired, changes)
}

// options is what plan calls have brought, in order.
func (p *planHeard) options() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.heard)
}

// A volume's password goes only where it went. An edit that gives one and changes
// anything else of the volume is refused, on any server, in the same words whatever
// the password is. Planning it would take the password the volume keeps to the
// address the edit names, as a move makes a volume anew on another server, and the
// paths the plan says changed would say whether a guess was right.
//
// And what a volume keeps goes only to the servers that have it. A plan sends a spec
// to every server it would run on, on a proposal alone, so an edit that keeps
// anything can't add a server, in the same words whatever it gave. The review of
// #140 found a token could have the real options sent to another server's agent by
// proposing the volume there with none, or with a wrong guess.
func TestAVolumesPasswordGoesNowhereElse(t *testing.T) {
	storetest.Each(t, testAVolumesPasswordGoesNowhereElse)
}

func testAVolumesPasswordGoesNowhereElse(t *testing.T, backend store.Backend) {
	const password = "cifs-pw-3Ws8nBt6Yc1Jf5Rd"
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.addServerNamed(t, "hz-1", nil, nil)
	one := f.lastDocker.id.ServerID
	heard := &planHeard{}
	f.volumes = func(d *volume.Driver) resource.Type {
		heard.Driver = d
		return heard
	}
	hz2 := f.addServerNamed(t, "hz-2", nil, nil)
	f.volumes = nil
	two := f.lastDocker.id.ServerID
	edit := func(options map[string]string, servers ...string) *dockwellv1.ProposeChangeRequest {
		return &dockwellv1.ProposeChangeRequest{Summary: "Share", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "volume", Name: "share"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: &resourcev1.Volume{Options: options}}},
			Servers:  servers,
		}}}
	}
	cifs := func(device, o string) map[string]string {
		return map[string]string{"type": "cifs", "device": device, "o": o}
	}
	const nas, there = "//nas/share", "addr=10.0.0.2,username=svc,password="
	c := changeClient(b, b.origin)
	made, err := c.ProposeChange(t.Context(), connect.NewRequest(edit(cifs(nas, there+password), one)))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, c, made.Msg.GetChange())

	script := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}
	tc := dockwellv1connect.NewChangeServiceClient(script, b.origin, connect.WithProtoJSON())
	tcc := dockwellv1connect.NewComposeServiceClient(script, b.origin, connect.WithProtoJSON())
	for what, options := range map[string]func(guess string) map[string]string{
		"another address": func(guess string) map[string]string { return cifs(nas, "addr=10.0.0.9,username=svc,password="+guess) },
		"another device":  func(guess string) map[string]string { return cifs("//10.0.0.9/share", there+guess) },
		"another type":    func(guess string) map[string]string { return map[string]string{"type": "tmpfs", "o": there + guess} },
	} {
		_, right := tc.ProposeChange(t.Context(), connect.NewRequest(edit(options(password), two)))
		_, wrong := tc.ProposeChange(t.Context(), connect.NewRequest(edit(options("letmein"), two)))
		if right == nil || wrong == nil || !strings.Contains(right.Error(), "can't change") {
			t.Errorf("%s: the right password = %v, a wrong one = %v, want both refused", what, right, wrong)
			continue
		}
		if right.Error() != wrong.Error() {
			t.Errorf("%s: the right password is refused with\n%v\nand a wrong one with\n%v", what, right, wrong)
		}
		if strings.Contains(right.Error(), password) || strings.Contains(wrong.Error(), "letmein") {
			t.Errorf("%s: a refusal holds the password: %v, %v", what, right, wrong)
		}
	}

	// Nothing an edit can say sends what's kept to a server that hasn't got it, and
	// nothing it says changes the words, so a refusal tells no guess.
	file := func(o string) string {
		return "services:\n  app:\n    image: ghcr.io/acme/shop:1.4\n    volumes: [\"share:/x\"]\nvolumes:\n  share:\n" +
			"    driver_opts: {type: cifs, device: " + nas + ", o: \"" + o + "\"}\n"
	}
	var words string
	for what, propose := range map[string]func() error{
		"no options": func() error {
			_, err := tc.ProposeChange(t.Context(), connect.NewRequest(edit(nil, two)))
			return err
		},
		"a wrong guess": func() error {
			_, err := tc.ProposeChange(t.Context(), connect.NewRequest(edit(cifs(nas, there+"letmein"), two)))
			return err
		},
		"the right password": func() error {
			_, err := tc.ProposeChange(t.Context(), connect.NewRequest(edit(cifs(nas, there+password), two)))
			return err
		},
		"a server beside the one it's on": func() error {
			_, err := tc.ProposeChange(t.Context(), connect.NewRequest(edit(nil, one, two)))
			return err
		},
		"a wrong guess in a file, with --servers": func() error {
			_, err := tcc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: scope.DefaultProject, Compose: file(there + "letmein"), Servers: []string{two}}))
			return err
		},
		"no password in a file that gives it none, with --servers": func() error {
			_, err := tcc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{
				Project: scope.DefaultProject,
				Compose: "services:\n  app:\n    image: ghcr.io/acme/shop:1.4\n    volumes: [\"share:/x\"]\nvolumes:\n  share: {}\n", Servers: []string{two},
			}))
			return err
		},
	} {
		before := len(heard.options())
		err := propose()
		if brought := heard.options()[before:]; slices.ContainsFunc(brought, func(o string) bool { return strings.Contains(o, password) }) {
			t.Errorf("%s: a plan call brought hz-2 the password: %q", what, brought)
		}
		if err == nil || !strings.Contains(err.Error(), "can't go on hz-2") || !strings.Contains(err.Error(), "make a volume of another name") {
			t.Errorf("%s = %v, want it refused, saying what to do", what, err)
			continue
		}
		if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "letmein") {
			t.Errorf("%s: the refusal holds the password: %v", what, err)
		}
		_, says, _ := strings.Cut(err.Error(), "can't go on")
		if words != "" && says != words {
			t.Errorf("%s says\n%s\nand another says\n%s", what, says, words)
		}
		words = says
	}
	if got := hz2.Volume(docker.Project(listedEnv(t, b)) + "_share"); got != nil {
		t.Errorf("hz-2 has %+v", got)
	}

	// Where it is, it goes on planning, as it says nothing new to any server.
	if _, err := tc.ProposeChange(t.Context(), connect.NewRequest(edit(cifs(nas, there+"letmein"), one))); err != nil {
		t.Errorf("a guess where the volume is = %v, want it planned", err)
	}

	// An edit that gives no password keeps nothing, so a move can change what it
	// gives: what goes is the edit's own, and nothing stored goes with it.
	const own = "addr=10.0.0.9,username=svc"
	moved, err := tc.ProposeChange(t.Context(), connect.NewRequest(edit(cifs("//10.0.0.9/share", own), two)))
	if err != nil {
		t.Fatalf("moving the volume with options of its own = %v, want it planned", err)
	}
	if got := heard.options(); !slices.Contains(got, own) || slices.ContainsFunc(got, func(o string) bool { return strings.Contains(o, password) }) {
		t.Errorf("plan calls brought hz-2 %q, want the edit's options and no password", got)
	}
	applyConfirmed(t, c, moved.Msg.GetChange())
	if got := hz2.Volume(docker.Project(listedEnv(t, b)) + "_share"); got == nil || got.Options["o"] != own {
		t.Errorf("hz-2 has %+v, want the volume as the edit gave it", got)
	}
}

// An undo puts back the revision a change replaced, on the servers that revision ran
// on, so what it holds goes nowhere it hasn't been, and nothing a caller said is in
// it. The check of #140 found Keep refused both of these: a move, as the volume there
// now has options of its own, and a server taken off, as adding one back looked like
// what TestAVolumesPasswordGoesNowhereElse stops, though it had the volume all along.
func TestUndoPutsAVolumeBackWhereItRan(t *testing.T) {
	storetest.Each(t, testUndoPutsAVolumeBackWhereItRan)
}

func testUndoPutsAVolumeBackWhereItRan(t *testing.T, backend store.Backend) {
	const password, there = "cifs-pw-7Hq2mXv9Lc4Tz1Sd", "addr=10.0.0.2,username=svc,password="
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	heard := map[string]*planHeard{"hz-1": {}, "hz-2": {}, "hz-3": {}}
	engines, ids := map[string]*dockertest.Engine{}, map[string]string{}
	for _, name := range []string{"hz-1", "hz-2", "hz-3"} {
		f.volumes = func(d *volume.Driver) resource.Type {
			heard[name].Driver = d
			return heard[name]
		}
		engines[name] = f.addServerNamed(t, name, nil, nil)
		ids[name] = f.lastDocker.id.ServerID
	}
	f.volumes = nil
	c := changeClient(b, b.origin)
	edit := func(volumeName string, options map[string]string, servers ...string) *dockwellv1.ProposeChangeRequest {
		named := make([]string, len(servers))
		for i, s := range servers {
			named[i] = ids[s]
		}
		return &dockwellv1.ProposeChangeRequest{Summary: "Share", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "volume", Name: volumeName},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: &resourcev1.Volume{Options: options}}},
			Servers:  named,
		}}}
	}
	cifs := func(device, o string) map[string]string {
		return map[string]string{"type": "cifs", "device": device, "o": o}
	}
	propose := func(t *testing.T, req *dockwellv1.ProposeChangeRequest) *dockwellv1.Change {
		t.Helper()
		res, err := c.ProposeChange(t.Context(), connect.NewRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetChange()
	}
	undo := func(t *testing.T, ch *dockwellv1.Change) *dockwellv1.Change {
		t.Helper()
		res, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: ch.GetId()}))
		if err != nil {
			t.Fatalf("undoing it = %v, want it planned", err)
		}
		return res.Msg.GetChange()
	}
	name := func(v string) string { return docker.Project(listedEnv(t, b)) + "_" + v }
	holds := func(server, v string) bool {
		got := engines[server].Volume(name(v))
		return got != nil && strings.Contains(got.Options["o"], password)
	}
	stillOffAt := func(t *testing.T, server string, before int) {
		t.Helper()
		if got := heard[server].options()[before:]; slices.ContainsFunc(got, func(o string) bool { return strings.Contains(o, password) }) {
			t.Errorf("a plan call brought %s the password: %q", server, got)
		}
	}

	t.Run("a move, with options of its own", func(t *testing.T) {
		made := propose(t, edit("moved", cifs("//nas/share", there+password), "hz-1"))
		applyConfirmed(t, c, made)
		moved := propose(t, edit("moved", cifs("//10.0.0.9/share", "addr=10.0.0.9,username=svc"), "hz-2"))
		applyConfirmed(t, c, moved)
		two, three := len(heard["hz-2"].options()), len(heard["hz-3"].options())
		back := undo(t, moved)
		stillOffAt(t, "hz-2", two)
		stillOffAt(t, "hz-3", three)
		if !back.GetPlan().GetDestructive() {
			t.Error("undoing a move takes the volume off hz-2, and its plan doesn't say that destroys data")
		}
		restores := false
		for _, s := range back.GetPlan().GetServers() {
			for _, step := range s.GetSteps() {
				restores = restores || step.GetRestores()
			}
		}
		if !restores {
			t.Errorf("the plan doesn't say hz-1 brings back the data it kept: %v", back.GetPlan().GetServers())
		}
		applyConfirmed(t, c, back)
		if !holds("hz-1", "moved") {
			t.Errorf("hz-1 has %+v after the undo", engines["hz-1"].Volume(name("moved")))
		}
	})

	t.Run("a server taken off", func(t *testing.T) {
		applyConfirmed(t, c, propose(t, edit("both", cifs("//nas/share", there+password), "hz-1", "hz-2")))
		off := propose(t, edit("both", nil, "hz-1"))
		applyConfirmed(t, c, off)
		three := len(heard["hz-3"].options())
		back := undo(t, off)
		stillOffAt(t, "hz-3", three)
		applyConfirmed(t, c, back)
		for _, server := range []string{"hz-1", "hz-2"} {
			if !holds(server, "both") {
				t.Errorf("%s has %+v after the undo", server, engines[server].Volume(name("both")))
			}
		}
	})

	t.Run("a volume with no options", func(t *testing.T) {
		applyConfirmed(t, c, propose(t, edit("plain", nil, "hz-1")))
		moved := propose(t, edit("plain", cifs("//10.0.0.9/share", "addr=10.0.0.9,username=svc"), "hz-2"))
		applyConfirmed(t, c, moved)
		applyConfirmed(t, c, undo(t, moved))
		if got := engines["hz-1"].Volume(name("plain")); got == nil || len(got.Options) != 0 {
			t.Errorf("hz-1 has %+v after the undo, want the volume as it was", got)
		}
	})

	t.Run("a move that fails", func(t *testing.T) {
		applyConfirmed(t, c, propose(t, edit("failed", cifs("//nas/share", there+password), "hz-1")))
		// Builds run before deletes, so hz-2 fails while hz-1 still has it, and the rollback
		// has only its record to put back.
		engines["hz-2"].Intercept(func(r *http.Request) error {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/volumes/create") {
				return errors.New("no space left on device")
			}
			return nil
		})
		defer engines["hz-2"].Intercept(nil)
		moved := propose(t, edit("failed", cifs("//10.0.0.9/share", "addr=10.0.0.9,username=svc"), "hz-2"))
		w := watch(t, c, moved.GetId())
		if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: moved.GetId(), Fingerprint: moved.GetPlan().GetFingerprint()})); err != nil {
			t.Fatal(err)
		}
		if last := waitWatch(t, w).last(); last.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED {
			t.Fatalf("the move ended %v, so this proves nothing", last.GetState())
		}
		var rolled *dockwellv1.RollbackStatus
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			res, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: moved.GetId()}))
			if err != nil {
				t.Fatal(err)
			}
			if rolled = res.Msg.GetChange().GetRollbackStatus(); rolled.GetState() == dockwellv1.RollbackState_ROLLBACK_STATE_APPLIED {
				break
			}
		}
		if rolled.GetState() != dockwellv1.RollbackState_ROLLBACK_STATE_APPLIED {
			t.Fatalf("the failed move's rollback reads %v, want it applied", rolled)
		}
		if !holds("hz-1", "failed") {
			t.Errorf("hz-1 has %+v after the rollback", engines["hz-1"].Volume(name("failed")))
		}
	})
}

// An undo can send a volume's password to a server it left because that server keeps
// the volume for 7 days, as long as an undo is open (ADR 0022). The window was read
// only as an undo was proposed, so one left planned could be applied weeks on, when
// hz-2 had pruned its copy. Applying plans again first, so hz-2 heard a password it no
// longer held, and only then was it refused. The review of #150 found this.
func TestAPlannedUndoCantOutliveItsWindow(t *testing.T) {
	storetest.Each(t, testAPlannedUndoCantOutliveItsWindow)
}

func testAPlannedUndoCantOutliveItsWindow(t *testing.T, backend store.Backend) {
	const password, there = "cifs-pw-2Lk9sDf4Gh7Jm3Np", "addr=10.0.0.2,username=svc,password="
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.addServerNamed(t, "hz-1", nil, nil)
	one := f.lastDocker.id.ServerID
	heard := &planHeard{}
	f.volumes = func(d *volume.Driver) resource.Type {
		heard.Driver = d
		return heard
	}
	hz2 := f.addServerNamed(t, "hz-2", nil, nil)
	f.volumes = nil
	two := f.lastDocker.id.ServerID
	c := changeClient(b, b.origin)
	edit := func(options map[string]string, servers ...string) *dockwellv1.ProposeChangeRequest {
		return &dockwellv1.ProposeChangeRequest{Summary: "Share", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "volume", Name: "share"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: &resourcev1.Volume{Options: options}}},
			Servers:  servers,
		}}}
	}
	made, err := c.ProposeChange(t.Context(), connect.NewRequest(edit(map[string]string{"type": "cifs", "device": "//nas/share", "o": there + password}, one, two)))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, c, made.Msg.GetChange())
	off, err := c.ProposeChange(t.Context(), connect.NewRequest(edit(nil, one)))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, c, off.Msg.GetChange())

	script := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}
	tc := dockwellv1connect.NewChangeServiceClient(script, b.origin, connect.WithProtoJSON())
	planned, err := tc.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: off.Msg.GetChange().GetId()}))
	if err != nil {
		t.Fatalf("a token's undo, within the window = %v, want it planned", err)
	}
	back := planned.Msg.GetChange()

	// Its 7 days go by, and hz-2 prunes what it kept.
	storetest.Exec(t, f.db, fmt.Sprintf("UPDATE changes SET applied_at = applied_at - %d WHERE id = '%s'",
		(revision.UndoWindow+time.Hour).Milliseconds(), off.Msg.GetChange().GetId()))
	name := docker.Project(listedEnv(t, b)) + "_share"
	if err := hz2.Client().RemoveVolume(t.Context(), name); err != nil {
		t.Fatal(err)
	}
	// What it reaches on hz-2 is a person's to approve, so a token gets no further.
	before := len(heard.options())
	if _, err := tc.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: back.GetId(), Fingerprint: back.GetPlan().GetFingerprint()})); reasonOf(err) != "needs_session" {
		t.Errorf("a token applying it = %v, want a person to", err)
	}
	_, err = c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: back.GetId(), Fingerprint: back.GetPlan().GetFingerprint()}))
	if brought := heard.options()[before:]; slices.ContainsFunc(brought, func(o string) bool { return strings.Contains(o, password) }) {
		t.Errorf("hz-2 no longer holds the password, and a plan call brought it: %q", brought)
	}
	if reasonOf(err) != "cant_undo" || !strings.Contains(err.Error(), "changes can be undone for 7 days") {
		t.Errorf("applying the undo past its window = %v, want it refused, saying so", err)
	}
	if got := hz2.Volume(name); got != nil {
		t.Errorf("hz-2 has %+v", got)
	}
}

// Someone remakes a volume by hand with a new password, and drift shows it. Adopting
// that takes what the server has, which no caller said, so nothing is kept of what's
// stored: Docker has what's adopted, so nothing changes there. The review of #140
// found the stored options went in its place, the agent refused them as a change, and
// that drift couldn't be adopted.
func TestAVolumeRemadeByHandCanBeAdopted(t *testing.T) {
	storetest.Each(t, testAVolumeRemadeByHandCanBeAdopted)
}

func testAVolumeRemadeByHandCanBeAdopted(t *testing.T, backend store.Backend) {
	file := func(o string) string {
		return "services:\n  app:\n    image: ghcr.io/acme/shop:1.4\n    volumes: [\"share:/x\"]\nvolumes:\n  share:\n" +
			"    driver_opts: {type: cifs, device: //nas/share, o: \"" + o + "\"}\n"
	}
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	applyConfirmed(t, c, importCompose(t, dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()), file("addr=10.0.0.2,username=svc,password=first-pw-5Tn2"), nil, false))

	name := docker.Project(listedEnv(t, b)) + "_share"
	v := e.Volume(name)
	if v == nil {
		t.Fatal("Docker has no volume")
	}
	remade := *v
	remade.Options = maps.Clone(v.Options)
	remade.Options["o"] = "addr=10.0.0.2,username=svc,password=second-pw-8Kw3"
	e.AddVolume(remade)

	drift := func() []*dockwellv1.Drift {
		t.Helper()
		res, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetDrift()
	}
	found := drift()
	if len(found) != 1 || found[0].GetResource().GetName() != "share" {
		t.Fatalf("drift = %v, want the volume", found)
	}
	adopted, err := c.AdoptDrift(t.Context(), connect.NewRequest(&dockwellv1.AdoptDriftRequest{ServerId: found[0].GetServerId(), ResourceId: found[0].GetResourceId()}))
	if err != nil {
		t.Fatalf("adopting a volume remade by hand = %v, want it planned", err)
	}
	applyConfirmed(t, c, adopted.Msg.GetChange())
	if left := drift(); len(left) != 0 {
		t.Errorf("after adopting it, drift = %v", left)
	}
	if got := e.Volume(name); got == nil || got.Options["o"] != remade.Options["o"] {
		t.Errorf("Docker has %+v after adopting what it had", got)
	}

	// Undoing that asks Docker for what it can't do, and says so. Keeping what's
	// there would plan nothing, and read as undone with nothing changed.
	_, err = c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: adopted.Msg.GetChange().GetId()}))
	if err == nil || !strings.Contains(err.Error(), "can't change a volume") {
		t.Errorf("undoing the adopt = %v, want it refused, as Docker can't change a volume", err)
	}
	if err != nil && (strings.Contains(err.Error(), "first-pw-5Tn2") || strings.Contains(err.Error(), "second-pw-8Kw3")) {
		t.Errorf("the refusal holds a password: %v", err)
	}
}

// A volume on two servers, remade by hand on one with a password of its own. Adopting
// that plans the same spec for both, and a plan call goes to each server before anyone
// applies anything, so the other's agent heard a password it doesn't hold, only to
// refuse it, as Docker can't change a volume. So each server is asked what it has
// first, which sends it nothing. Both remade alike is one thing to keep. The check of
// #140 found this.
func TestAdoptingAVolumeAsksItsOtherServersFirst(t *testing.T) {
	storetest.Each(t, testAdoptingAVolumeAsksItsOtherServersFirst)
}

func testAdoptingAVolumeAsksItsOtherServersFirst(t *testing.T, backend store.Backend) {
	const first, made = "addr=10.0.0.2,username=svc,password=first-pw-5Tn2", "by-hand-pw-9Rw4Lk2Xs7"
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	one := f.addServerNamed(t, "hz-1", nil, nil)
	oneID := f.lastDocker.id.ServerID
	heard := &planHeard{}
	f.volumes = func(d *volume.Driver) resource.Type {
		heard.Driver = d
		return heard
	}
	two := f.addServerNamed(t, "hz-2", nil, nil)
	twoID := f.lastDocker.id.ServerID
	f.volumes = nil
	c := changeClient(b, b.origin)
	res, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Share", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "volume", Name: "share"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: &resourcev1.Volume{Options: map[string]string{"type": "cifs", "device": "//nas/share", "o": first}}}},
		Servers:  []string{oneID, twoID},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, c, res.Msg.GetChange())

	name := docker.Project(listedEnv(t, b)) + "_share"
	remake := func(e *dockertest.Engine) {
		v := *e.Volume(name)
		v.Options = maps.Clone(v.Options)
		v.Options["o"] = "addr=10.0.0.2,username=svc,password=" + made
		e.AddVolume(v)
	}
	drift := func(server string) *dockwellv1.Drift {
		t.Helper()
		res, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range res.Msg.GetDrift() {
			if d.GetServerId() == server {
				return d
			}
		}
		t.Fatalf("drift on %s = %v, want the volume", server, res.Msg.GetDrift())
		return nil
	}
	remake(one)
	found := drift(oneID)
	before := len(heard.options())
	_, err = c.AdoptDrift(t.Context(), connect.NewRequest(&dockwellv1.AdoptDriftRequest{ServerId: found.GetServerId(), ResourceId: found.GetResourceId()}))
	if brought := heard.options()[before:]; slices.ContainsFunc(brought, func(o string) bool { return strings.Contains(o, made) }) {
		t.Errorf("a plan call brought hz-2 the password made by hand: %q", brought)
	}
	if err == nil || !strings.Contains(err.Error(), "hz-2") || !strings.Contains(err.Error(), "Make them the same") {
		t.Errorf("adopting what one of two servers has = %v, want it refused, saying what to do", err)
	}
	if err != nil && strings.Contains(err.Error(), made) {
		t.Errorf("the refusal holds the password: %v", err)
	}

	// Both remade alike, what's kept is what each has.
	remake(two)
	found = drift(oneID)
	adopted, err := c.AdoptDrift(t.Context(), connect.NewRequest(&dockwellv1.AdoptDriftRequest{ServerId: found.GetServerId(), ResourceId: found.GetResourceId()}))
	if err != nil {
		t.Fatalf("adopting what both servers have = %v, want it planned", err)
	}
	applyConfirmed(t, c, adopted.Msg.GetChange())
	left, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := left.Msg.GetDrift(); len(got) != 0 {
		t.Errorf("after adopting it, drift = %v", got)
	}
}

// A server that's away can't say what it has, so an adopt waits for it. One removed
// from the fleet will never say, and nothing runs on it any more, so it has no part in
// this. Its placement outlives it, so it read as away for good, and the volume's
// drift could never be adopted. The review of #150 found this.
func TestAdoptingAVolumeLeavesARemovedServerOut(t *testing.T) {
	storetest.Each(t, testAdoptingAVolumeLeavesARemovedServerOut)
}

func testAdoptingAVolumeLeavesARemovedServerOut(t *testing.T, backend store.Backend) {
	const first, second = "addr=10.0.0.2,username=svc,password=first-pw-5Tn2", "addr=10.0.0.2,username=svc,password=rotated-pw-2Vb8"
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	one := f.addServerNamed(t, "hz-1", nil, nil)
	oneID := f.lastDocker.id.ServerID
	f.addServerNamed(t, "hz-2", nil, nil)
	hz2 := f.lastDocker
	twoID := hz2.id.ServerID
	c := changeClient(b, b.origin)
	res, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Share", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "volume", Name: "share"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: &resourcev1.Volume{Options: map[string]string{"type": "cifs", "device": "//nas/share", "o": first}}}},
		Servers:  []string{oneID, twoID},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, c, res.Msg.GetChange())

	name := docker.Project(listedEnv(t, b)) + "_share"
	v := *one.Volume(name)
	v.Options = maps.Clone(v.Options)
	v.Options["o"] = second
	one.AddVolume(v)
	drift := func() []*dockwellv1.Drift {
		t.Helper()
		res, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetDrift()
	}
	adopt := func() (*dockwellv1.Change, error) {
		t.Helper()
		found := drift()
		if len(found) != 1 || found[0].GetServerId() != oneID {
			t.Fatalf("drift = %v, want the volume on hz-1", found)
		}
		res, err := c.AdoptDrift(t.Context(), connect.NewRequest(&dockwellv1.AdoptDriftRequest{ServerId: oneID, ResourceId: found[0].GetResourceId()}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange(), nil
	}

	hz2.stop()
	waitConnected(t, f, twoID, false)
	if _, err := adopt(); err == nil || !strings.Contains(err.Error(), "hz-2 isn't connected") {
		t.Errorf("adopting while hz-2 is away = %v, want it refused, as hz-2 can't say what it has", err)
	}
	if err := f.ident.Revoke(t.Context(), twoID, "cli:test"); err != nil {
		t.Fatal(err)
	}
	adopted, err := adopt()
	if err != nil {
		t.Fatalf("adopting with hz-2 removed = %v, want it planned", err)
	}
	applyConfirmed(t, c, adopted)
	if left := drift(); len(left) != 0 {
		t.Errorf("after adopting it, drift = %v", left)
	}
}

// A network Dockwell didn't make can be a macvlan on the server's LAN, past its
// firewall, and only the server knows. So its agent says which driver one uses, and
// the plan says that where it says what joins it. The last check of #92 found it read
// the driver, for a health check, and dropped it. A bridge says nothing more.
func TestAPlanSaysWhichDriverANetworkItDidntMakeUses(t *testing.T) {
	storetest.Each(t, testAPlanSaysWhichDriverANetworkItDidntMakeUses)
}

func testAPlanSaysWhichDriverANetworkItDidntMakeUses(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.AddNetwork(docker.Network{Name: "lan", Driver: "macvlan", Options: map[string]string{"parent": "eth0"}})
	e.AddNetwork(docker.Network{Name: "edge", Driver: "bridge"})
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	plan := importCompose(t, cc, "services:\n  app:\n    image: ghcr.io/acme/shop:1.4\n    networks: [lan, edge]\n"+
		"networks:\n  lan:\n    external: true\n  edge:\n    external: true\n", nil, false).GetPlan()
	got := map[string]string{}
	for _, s := range plan.GetServers() {
		for _, step := range s.GetSteps() {
			for _, a := range step.GetHostAccess() {
				if a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_EXTERNAL_NETWORK {
					got[a.GetPath()] = a.GetLeadsTo()
				}
			}
		}
	}
	if want := map[string]string{"lan": "macvlan", "edge": ""}; !maps.Equal(got, want) {
		t.Errorf("the plan says the networks use %v, want %v", got, want)
	}
}

// plansNothing reports a plan that changes no resource and runs no step.
func plansNothing(p *dockwellv1.Plan) bool {
	s := p.GetSummary()
	if s.GetCreate()+s.GetChange()+s.GetReplace()+s.GetDestroy() != 0 {
		return false
	}
	for _, r := range p.GetResources() {
		if r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE || len(r.GetChanges()) != 0 {
			return false
		}
	}
	for _, server := range p.GetServers() {
		for _, step := range server.GetSteps() {
			if step.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
				return false
			}
		}
	}
	return true
}

// applyConfirmed applies a change that takes a person who names its plan,
// and waits for it to apply.
func applyConfirmed(t *testing.T, c dockwellv1connect.ChangeServiceClient, ch *dockwellv1.Change) {
	t.Helper()
	w := watch(t, c, ch.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the change ended %v: %s", got.GetState(), got.GetError())
	}
}
