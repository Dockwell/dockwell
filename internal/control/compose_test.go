// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// addDockerServer enrolls hz-1, whose agent runs the Docker drivers on a
// fake Docker Engine.
func (f *fullServer) addDockerServer(t *testing.T) (*testServer, *dockertest.Engine) {
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
	client := e.Client()
	containers := container.Driver{Runtime: &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond}}
	s := &testServer{
		id:      enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		drivers: resource.NewRegistry(&containers, &service.Driver{Containers: containers}, &volume.Driver{Runtime: &volume.Docker{Client: client, Kept: &volume.Kept{}}}, &network.Driver{Client: client}),
	}
	s.connect(t, f)
	return s, e
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
	res, err := c.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: file, Variables: vars, RemoveOrphans: orphans}))
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
	hz1, e := f.addDockerServer(t)
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
		"services:\n  sh:\n    image: alpine\n    volumes: [\"/:/host\"]\n": "the server path / mounted at /host is the server's whole file system",
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
		if s := m.GetStep(); s != nil && strings.HasPrefix(s.GetStep().GetMessage(), "Creating service db on hz-1: pulling postgres:17") {
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
	_ = hz1
}
