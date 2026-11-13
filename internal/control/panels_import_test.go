// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// stepper is an import's answer, which has steps.
type stepper interface {
	GetSteps() []*dockwellv1.ImportStep
}

// panelSteps are an import's steps by kind and name.
func panelSteps(res stepper) map[string]*dockwellv1.ImportStep {
	out := map[string]*dockwellv1.ImportStep{}
	for _, s := range res.GetSteps() {
		out[s.GetKind().String()+" "+s.GetName()] = s
	}
	return out
}

// adoptedIn are the containers a plan takes on, by ID.
func adoptedIn(c *dockwellv1.Change) map[string]bool {
	out := map[string]bool{}
	for _, server := range c.GetPlan().GetServers() {
		for _, step := range server.GetSteps() {
			if step.GetAdopts() != "" {
				out[step.GetAdopts()] = true
			}
		}
	}
	return out
}

// noteSays reports a plan note of reason that says words.
func noteSays(c *dockwellv1.Change, reason, words string) bool {
	return slices.ContainsFunc(c.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool {
		return n.GetReason() == reason && strings.Contains(n.GetMessage(), words)
	})
}

// An import of a server running Coolify takes on each of its applications,
// services and databases as it runs, in a project named for it, with nothing
// asked of Docker; leaves a pull request's preview and Coolify itself, saying why;
// lists each app's domains for later; and reads again as done.
func TestImportingFromCoolifyTakesOnWhatRuns(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.AddLocalImage("ghcr.io/coollabsio/coolify:4.7.5", dockertest.Image{Config: docker.Config{Cmd: []string{"php"}}})
	for _, name := range []string{"coolify", "coolify-db", "coolify-proxy"} {
		runByHand(t, e, name, func(r *docker.CreateRequest) {
			r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
			if name == "coolify" {
				r.Image = "ghcr.io/coollabsio/coolify:4.7.5"
			}
		})
	}
	coolify := func(uuid, kind, name, service, container string, more map[string]string) string {
		return composeContainer(t, e, uuid, service, container, func(r *docker.CreateRequest) {
			maps.Copy(r.Labels, map[string]string{
				"coolify.managed": "true", "coolify.type": kind, "coolify." + kind + "Uuid": uuid, "coolify.resourceName": name,
				"coolify.projectName": "personal", "coolify.environmentName": "production", "coolify.pullRequestId": "0",
				docker.LabelWorkingDir: "/data/coolify/" + kind + "s/" + uuid,
				// A label's value beyond a domain isn't part of what the read sends.
				"traefik.http.middlewares.auth.basicauth.users": "admin:$apr1$hash-7f3k",
			})
			maps.Copy(r.Labels, more)
		})
	}
	for _, uuid := range []string{"k4s9", "p2q7", "w8x1"} {
		e.AddNetwork(docker.Network{ID: uuid + "net", Name: uuid + "_default", Driver: "bridge"})
	}
	ghost := coolify("k4s9", "service", "blog", "ghost", "ghost-k4s9", map[string]string{"traefik.http.routers.http-0-k4s9-ghost.rule": "Host(`blog.example.com`) && PathPrefix(`/`)"})
	mysql := coolify("k4s9", "service", "blog", "mysql", "mysql-k4s9", nil)
	pg := coolify("p2q7", "database", "reports", "p2q7", "p2q7", map[string]string{"coolify.database.subType": "standalone-postgresql"})
	coolify("w8x1", "application", "site", "w8x1", "w8x1-pr-3", map[string]string{"coolify.pullRequestId": "3"})
	id := f.lastDocker.id.ServerID
	seen := len(e.Seen())

	res, err := importClient(b).ImportCoolify(t.Context(), connect.NewRequest(&dockwellv1.ImportCoolifyRequest{ServerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	if f := res.Msg.GetFound(); f.GetVersion() != "4.7.5" || !f.GetKnownVersion() || f.GetKind() != dockwellv1.PanelKind_PANEL_KIND_COOLIFY {
		t.Errorf("found %v", f)
	}
	steps := panelSteps(res.Msg)
	take, leave := dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON, dockwellv1.ImportAction_IMPORT_ACTION_LEAVE
	for key, want := range map[string]struct {
		action dockwellv1.ImportAction
		says   []string
	}{
		"IMPORT_KIND_APP blog":                {take, []string{"the project blog, taken on as it runs", "its volumes where they are", "blog.example.com", "coolify-proxy"}},
		"IMPORT_KIND_DATABASE_SERVER reports": {take, []string{"the project reports, taken on as it runs"}},
		"IMPORT_KIND_APP site":                {leave, []string{"a pull request's preview"}},
		"IMPORT_KIND_PANEL Coolify":           {leave, []string{"4.7.5", "coolify-db", "neither its database nor its API"}},
	} {
		s := steps[key]
		text := s.GetSummary() + " | " + s.GetBecomes() + " | " + s.GetReason()
		for _, w := range want.says {
			if s.GetAction() != want.action || !strings.Contains(text, w) {
				t.Errorf("%s = %v %q, want %v saying %q", key, s.GetAction(), text, want.action, w)
			}
		}
	}
	if s := res.Msg.GetFound().String(); strings.Contains(s, "hash-7f3k") || strings.Contains(s, secretValue) {
		t.Fatalf("the read sends a value it shouldn't: %s", s)
	}
	planned := res.Msg.GetChange()
	if planned.GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
		t.Fatalf("change = %v", planned)
	}
	if took := adoptedIn(planned); !took[ghost] || !took[mysql] || !took[pg] || len(took) != 3 {
		t.Fatalf("the plan takes on %v, want blog's two containers and reports'", took)
	}
	if !noteSays(planned, "imports_coolify", "Their volumes stay where they are") || !noteSays(planned, "managed_elsewhere", "Don't delete it in Coolify") {
		t.Errorf("the plan's notes are %v", planned.GetPlan().GetNotes())
	}
	if strings.Contains(planned.String(), secretValue) {
		t.Fatal("the plan shows a value of an environment")
	}
	applyConfirmed(t, changeClient(b, b.origin), planned)
	if got := mutations(e, seen); len(got) != 0 {
		t.Errorf("the import asked Docker to %v", got)
	}

	// Read again, what was taken on says as what, and nothing new is planned.
	again, err := importClient(b).ImportCoolify(t.Context(), connect.NewRequest(&dockwellv1.ImportCoolifyRequest{ServerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	if again.Msg.GetChange() != nil {
		t.Errorf("a second import planned %v", again.Msg.GetChange())
	}
	steps = panelSteps(again.Msg)
	for key, want := range map[string]string{"IMPORT_KIND_APP blog": "the project blog, taken on already", "IMPORT_KIND_DATABASE_SERVER reports": "the project reports, taken on already"} {
		if s := steps[key]; s.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_DONE || !strings.Contains(s.GetBecomes(), want) {
			t.Errorf("read again, %s = %v %q, want %q", key, s.GetAction(), s.GetBecomes(), want)
		}
	}

	// A token gets what was found, and no plan, as taking on is a person's.
	rw := map[string]string{"Authorization": "Bearer " + createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)}
	if status, _, body := callREST(t, b, http.MethodPost, "/v1/servers/"+id+"/imports:coolify", rw, `{}`); status != http.StatusOK || strings.Contains(body, `"change"`) {
		t.Errorf("a token's import = %d %s", status, body)
	}
}

// An import of a server running Dokploy takes on its Compose apps as they run,
// with their domains from their labels, and leaves what it runs as Swarm services,
// with the domains Dokploy's Traefik files give an application, saying why.
func TestImportingFromDokployTakesOnItsComposeApps(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	root := t.TempDir()
	dynamic := filepath.Join(root, "etc", "dokploy", "traefik", "dynamic")
	if err := os.MkdirAll(dynamic, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dynamic, "shop-k2m4.yml"), []byte("http:\n  routers:\n    shop-k2m4-router-1:\n      rule: Host(`shop.example.com`)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.panelsRoot = root
	e := f.addDockerServer(t)
	labels := func(app string) map[string]string {
		return map[string]string{"dokploy.project": "store", "dokploy.environment": "production", "dokploy.application": app}
	}
	e.Services = []docker.Service{
		{ID: "s1", Spec: swarmService("dokploy", "dokploy/dokploy:v0.31.2", nil)},
		{ID: "s2", Spec: swarmService("dokploy-postgres", "postgres:16", nil)},
		{ID: "s3", Spec: swarmService("shop-k2m4", "shop-k2m4:latest", labels("shop"))},
		{ID: "s4", Spec: swarmService("shopdb-v7n1", "postgres:16@sha256:00", labels("shop-db"))},
	}
	runByHand(t, e, "dokploy-traefik", func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
	e.AddNetwork(docker.Network{ID: "wikinet", Name: "wiki-ab12_default", Driver: "bridge"})
	wiki := composeContainer(t, e, "wiki-ab12", "wiki", "wiki-ab12-wiki-1", func(r *docker.CreateRequest) {
		maps.Copy(r.Labels, labels("wiki"))
		r.Labels[docker.LabelWorkingDir] = "/etc/dokploy/compose/wiki-ab12/code"
		r.Labels["traefik.http.routers.wiki-ab12-1-web.rule"] = "Host(`wiki.example.com`)"
	})
	id := f.lastDocker.id.ServerID

	res, err := importClient(b).ImportDokploy(t.Context(), connect.NewRequest(&dockwellv1.ImportDokployRequest{ServerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	found := res.Msg.GetFound()
	if found.GetVersion() != "v0.31.2" || !found.GetKnownVersion() || len(found.GetOwn()) != 3 {
		t.Errorf("found %v", found)
	}
	for _, a := range found.GetApps() {
		if a.GetName() == "shop" && !slices.Equal(a.GetDomains(), []string{"shop.example.com"}) {
			t.Errorf("shop's domains are %v, from Dokploy's Traefik file", a.GetDomains())
		}
	}
	steps := panelSteps(res.Msg)
	take, leave := dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON, dockwellv1.ImportAction_IMPORT_ACTION_LEAVE
	for key, want := range map[string]struct {
		action dockwellv1.ImportAction
		says   string
	}{
		"IMPORT_KIND_APP wiki":                {take, "the project wiki, taken on as it runs"},
		"IMPORT_KIND_APP shop":                {leave, "runs it as a Swarm service"},
		"IMPORT_KIND_DATABASE_SERVER shop-db": {leave, "runs it as a Swarm service"},
		"IMPORT_KIND_PANEL Dokploy":           {leave, "dokploy-traefik"},
	} {
		s := steps[key]
		text := s.GetSummary() + " | " + s.GetBecomes() + " | " + s.GetReason()
		if s.GetAction() != want.action || !strings.Contains(text, want.says) {
			t.Errorf("%s = %v %q, want %v saying %q", key, s.GetAction(), text, want.action, want.says)
		}
	}
	if !strings.Contains(steps["IMPORT_KIND_APP wiki"].GetReason(), "wiki.example.com") {
		t.Errorf("wiki's step says nothing of its domain: %q", steps["IMPORT_KIND_APP wiki"].GetReason())
	}
	planned := res.Msg.GetChange()
	if took := adoptedIn(planned); !took[wiki] || len(took) != 1 {
		t.Fatalf("the plan takes on %v, want wiki's container alone", took)
	}
	if !noteSays(planned, "managed_elsewhere", "Don't delete it in Dokploy") {
		t.Errorf("the plan's notes are %v", planned.GetPlan().GetNotes())
	}
}

// swarmService is a Swarm service's spec, as Dokploy makes one.
func swarmService(name, image string, labels map[string]string) docker.ServiceSpec {
	var s docker.ServiceSpec
	s.Name, s.Labels, s.TaskTemplate.ContainerSpec.Image = name, labels, image
	return s
}

// A server where the panel doesn't run says so, and an agent from before the import
// says to update it.
func TestNoPanelOrAnOldAgentSaysSo(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	id := f.lastDocker.id.ServerID
	_, err := importClient(b).ImportCoolify(t.Context(), connect.NewRequest(&dockwellv1.ImportCoolifyRequest{ServerId: id}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "panel_not_found" || !strings.Contains(err.Error(), "doesn't run Coolify: Docker runs none of its containers") {
		t.Errorf("an import where Coolify doesn't run = %v", err)
	}
	old := f.addOnePanelServer(t, t.TempDir(), nil).id.ServerID
	_, err = importClient(b).ImportDokploy(t.Context(), connect.NewRequest(&dockwellv1.ImportDokployRequest{ServerId: old}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "agent_outdated" {
		t.Errorf("an import through an agent from before = %v", err)
	}
}
