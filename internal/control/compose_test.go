// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	agentlogs "github.com/dockwell/dockwell/internal/agent/logs"
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
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.EnrollLocal(t.Context(), csr, "hz-1")
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
	archive := &agentlogs.Archive{Dir: t.TempDir(), Client: client, Logger: slog.New(slog.DiscardHandler), Free: func(string) (uint64, error) { return 1 << 40, nil }}
	containers := container.Driver{Runtime: &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Logs: archive, Paths: &container.Paths{Client: client, DataDir: t.TempDir()}}}
	drivers := []resource.Type{&containers, &service.Driver{Containers: containers}, &volume.Driver{Runtime: &volume.Docker{Client: client, Kept: &volume.Kept{}}}, &network.Driver{Client: client}}
	if more != nil {
		drivers = append(drivers, more()...)
	}
	s := &testServer{
		id:      enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		drivers: resource.NewRegistry(drivers...),
		logs:    &agentlogs.Reader{Client: client, Archive: archive, Logger: slog.New(slog.DiscardHandler)},
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
	web = e.Container(project + "-web-1")
	if web.Config.Image != "ghcr.io/acme/shop:1.5" || !slices.Contains(web.Config.Env, "DATABASE_PASSWORD=s3cret-pw-1") {
		t.Errorf("after the bump, web runs %s with %v", web.Config.Image, web.Config.Env)
	}

	// So a rotation still reaches it, and the next export says so.
	rotated := apply(t, c, propose(t, c, "Rotate the password", secretSpec("DB_PASSWORD", "s3cret-pw-2")))
	if web = e.Container(project + "-web-1"); !slices.Contains(web.Config.Env, "DATABASE_PASSWORD=s3cret-pw-2") {
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
	// options is refused, and takes nothing away.
	_, err = cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: ex.Msg.GetCompose()}))
	if err == nil || strings.Contains(err.Error(), password) || !strings.Contains(err.Error(), "share") {
		t.Errorf("importing the export without the options = %v, want it refused, naming the volume and no password", err)
	}
	if got := e.Volume(project + "_share"); got == nil || !strings.Contains(got.Options["o"], password) {
		t.Errorf("after refusing the import, Docker has %+v", got)
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
