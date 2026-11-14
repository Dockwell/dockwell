// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/panels"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
)

// handoverLog is what the server's agent did, in order: the proxy stopped or
// started, and each route made or deleted.
type handoverLog struct {
	mu   sync.Mutex
	done []string
}

func (l *handoverLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.done = append(l.done, s)
}

func (l *handoverLog) read() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.done)
}

type loggedStopper struct {
	laterStopper
	log *handoverLog
}

func (l *loggedStopper) ApplyPanel(ctx context.Context, p resource.Panel, say func(string)) error {
	l.log.add(map[bool]string{true: "start ", false: "stop "}[p.Start] + p.Parts[0].Name)
	return l.laterStopper.ApplyPanel(ctx, p, say)
}

type loggedRoutes struct {
	*routesInMemory
	log *handoverLog
	// fails makes each route it's asked to make fail, as a pull or a port taken would.
	fails bool
}

func (l *loggedRoutes) Apply(ctx context.Context, ref resource.Ref, spec *resourcev1.Route, effect *resource.IngressEffect) error {
	l.log.add(map[bool]string{true: "delete ", false: "route "}[spec == nil] + ref.Name)
	if l.fails && spec != nil {
		return errors.New("can't pull Caddy: the registry didn't answer")
	}
	return l.routesInMemory.Apply(ctx, ref, spec, effect)
}

// Handing over a panel's domains is one change (ADR 0089): it stops the panel's
// proxy first, then makes a route to each taken-on app's service for each of its
// domains, as the proxy's labels send it; the import reads them as in Dockwell
// then; and its undo deletes the routes before it starts the proxy again. What
// isn't taken on yet has nothing to hand over.
func TestHandingOverAPanelsDomains(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	root := t.TempDir()
	dynamic := filepath.Join(root, "data", "coolify", "proxy", "dynamic")
	if err := os.MkdirAll(dynamic, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dynamic, "coolify.yaml"), []byte("http:\n  routers:\n    coolify-http:\n      rule: Host(`coolify.example.com`)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.panelsRoot = root
	log := &handoverLog{}
	stopper := &loggedStopper{log: log}
	ing := &loggedRoutes{routesInMemory: &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}, log: log}
	e := f.addServerWith(t, func() []resource.Type {
		return []resource.Type{&host.Driver{Panels: stopper}, &route.Driver{Ingress: ing}}
	})
	stopper.s = &panels.Stopper{Docker: e.Client()}
	waitForHost(t, f, f.lastDocker)
	id := f.lastDocker.id.ServerID
	for _, name := range []string{"coolify", "coolify-proxy"} {
		runByHand(t, e, name, func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
	}
	e.AddNetwork(docker.Network{ID: "k4s9net", Name: "k4s9_default", Driver: "bridge"})
	ghost := composeContainer(t, e, "k4s9", "ghost", "ghost-k4s9", func(r *docker.CreateRequest) {
		maps.Copy(r.Labels, map[string]string{
			"coolify.managed": "true", "coolify.type": "service", "traefik.enable": "true", "coolify.serviceUuid": "k4s9", "coolify.resourceName": "blog",
			docker.LabelWorkingDir:                                             "/data/coolify/services/k4s9",
			"traefik.http.routers.http-0-k4s9-ghost.rule":                      "Host(`blog.example.com`) && PathPrefix(`/`)",
			"traefik.http.services.http-0-k4s9-ghost.loadbalancer.server.port": "2368",
		})
	})
	// One behind a login doesn't come: a plain route would let anyone in.
	e.AddNetwork(docker.Network{ID: "p2q7net", Name: "p2q7_default", Driver: "bridge"})
	composeContainer(t, e, "p2q7", "wiki", "wiki-p2q7", func(r *docker.CreateRequest) {
		maps.Copy(r.Labels, map[string]string{
			"coolify.managed": "true", "coolify.type": "service", "traefik.enable": "true", "coolify.serviceUuid": "p2q7", "coolify.resourceName": "wiki",
			docker.LabelWorkingDir:                                            "/data/coolify/services/p2q7",
			"traefik.http.routers.http-0-p2q7-wiki.rule":                      "Host(`wiki.example.com`)",
			"traefik.http.routers.http-0-p2q7-wiki.middlewares":               "sso@file",
			"traefik.http.services.http-0-p2q7-wiki.loadbalancer.server.port": "3000",
		})
	})
	ic, c := importClient(b), changeClient(b, b.origin)
	domains := func() (*dockwellv1.Change, error) {
		res, err := ic.PlanPanelDomains(t.Context(), connect.NewRequest(&dockwellv1.PlanPanelDomainsRequest{ServerId: id, Panel: dockwellv1.PanelKind_PANEL_KIND_COOLIFY}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange(), nil
	}
	domainStep := func() *dockwellv1.ImportStep {
		res, err := ic.ImportCoolify(t.Context(), connect.NewRequest(&dockwellv1.ImportCoolifyRequest{ServerId: id, SkipPlan: true}))
		if err != nil {
			t.Fatal(err)
		}
		return panelSteps(res.Msg)["IMPORT_KIND_DOMAIN blog.example.com"]
	}

	// Nothing taken on yet, nothing comes over.
	if _, err := domains(); reasonOf(err) != "not_taken_on" {
		t.Errorf("handing over before a take-on = %v", err)
	}
	took, err := ic.ImportCoolify(t.Context(), connect.NewRequest(&dockwellv1.ImportCoolifyRequest{ServerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, c, took.Msg.GetChange())
	if s := domainStep(); s.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_MAKE || s.GetWaitsFor() != dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_PORTS ||
		s.GetBecomes() != "a route to ghost on port 2368" {
		t.Errorf("before the handover, the domain reads %v", s)
	}

	change, err := domains()
	if err != nil {
		t.Fatal(err)
	}
	if change.GetSummary() != "Hand over Coolify's domains on hz-1" || !noteSays(change, "panel_domains", "Stops coolify-proxy, which holds ports 80 and 443 for Coolify") ||
		!noteSays(change, "panel_domains", "These stop answering, as nothing in Dockwell serves them: wiki.example.com and coolify.example.com") ||
		!noteSays(change, "panel_domains_unguarded", "No login or address limit found on the old proxy: blog.example.com") {
		t.Errorf("the handover reads %q, %v", change.GetSummary(), change.GetPlan().GetNotes())
	}
	var kinds []string
	for _, st := range change.GetPlan().GetServers()[0].GetSteps() {
		kinds = append(kinds, st.GetResource().GetKind())
	}
	if !slices.Equal(kinds, []string{"server.host", "route"}) {
		t.Errorf("the handover's steps are %v", kinds)
	}
	if done := actedOn(t, c, change, ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the handover ended %v: %s", done.GetState(), done.GetError())
	}
	if got := log.read(); !slices.Equal(got, []string{"stop coolify-proxy", "route blog-example-com"}) {
		t.Errorf("the handover did %v", got)
	}
	var made *resourcev1.Route
	for _, r := range ing.routes {
		made = r
	}
	if made.GetDomain() != "blog.example.com" || made.GetService() != "ghost" || made.GetPort() != 2368 {
		t.Errorf("the route is %v", made)
	}
	if p := e.Container("coolify-proxy"); p.State.Running || p.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("the proxy is left %v, %q", p.State.Running, p.HostConfig.RestartPolicy.Name)
	}
	if !e.Container(ghost).State.Running {
		t.Error("the app was stopped")
	}
	if s := domainStep(); s.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_DONE {
		t.Errorf("after the handover, the domain reads %v", s)
	}
	if _, err := domains(); reasonOf(err) != "nothing_to_route" {
		t.Errorf("handing over again = %v", err)
	}
	// The proxy can't start again while Dockwell's ingress holds its ports.
	if _, err := ic.PlanStartPanel(t.Context(), connect.NewRequest(&dockwellv1.PlanStartPanelRequest{ServerId: id, Panel: dockwellv1.PanelKind_PANEL_KIND_COOLIFY, Proxy: true})); reasonOf(err) != "ports_held" ||
		!strings.Contains(err.Error(), "blog-example-com") {
		t.Errorf("starting the proxy with a route on the server = %v", err)
	}

	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: change.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if done := actedOn(t, c, undo.Msg.GetChange(), ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the undo ended %v: %s", done.GetState(), done.GetError())
	}
	if got := log.read()[2:]; !slices.Equal(got, []string{"delete blog-example-com", "start coolify-proxy"}) {
		t.Errorf("the undo did %v", got)
	}
	if p := e.Container("coolify-proxy"); !p.State.Running || p.HostConfig.RestartPolicy.Name != "always" {
		t.Errorf("after the undo the proxy is %v, %q", p.State.Running, p.HostConfig.RestartPolicy.Name)
	}
	if len(ing.routes) != 0 || !strings.Contains(domainStep().GetBecomes(), "a route to ghost") {
		t.Errorf("after the undo, routes %v and the domain reads %v", ing.routes, domainStep())
	}
}

// A handover whose route fails after the proxy stopped rolls back, and the rollback
// starts the proxy again as it stood, so the server's domains aren't left down.
func TestAFailedHandoverStartsTheProxyAgain(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	log := &handoverLog{}
	stopper := &loggedStopper{log: log}
	ing := &loggedRoutes{routesInMemory: &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}, log: log}
	e := f.addServerWith(t, func() []resource.Type {
		return []resource.Type{&host.Driver{Panels: stopper}, &route.Driver{Ingress: ing}}
	})
	stopper.s = &panels.Stopper{Docker: e.Client()}
	waitForHost(t, f, f.lastDocker)
	id := f.lastDocker.id.ServerID
	for _, name := range []string{"coolify", "coolify-proxy"} {
		runByHand(t, e, name, func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
	}
	e.AddNetwork(docker.Network{ID: "k4s9net", Name: "k4s9_default", Driver: "bridge"})
	composeContainer(t, e, "k4s9", "ghost", "ghost-k4s9", func(r *docker.CreateRequest) {
		maps.Copy(r.Labels, map[string]string{
			"coolify.managed": "true", "coolify.type": "service", "traefik.enable": "true", "coolify.serviceUuid": "k4s9", "coolify.resourceName": "blog",
			docker.LabelWorkingDir:                                             "/data/coolify/services/k4s9",
			"traefik.http.routers.http-0-k4s9-ghost.rule":                      "Host(`blog.example.com`)",
			"traefik.http.services.http-0-k4s9-ghost.loadbalancer.server.port": "2368",
		})
	})
	ic, c := importClient(b), changeClient(b, b.origin)
	took, err := ic.ImportCoolify(t.Context(), connect.NewRequest(&dockwellv1.ImportCoolifyRequest{ServerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, c, took.Msg.GetChange())
	res, err := ic.PlanPanelDomains(t.Context(), connect.NewRequest(&dockwellv1.PlanPanelDomainsRequest{ServerId: id, Panel: dockwellv1.PanelKind_PANEL_KIND_COOLIFY}))
	if err != nil {
		t.Fatal(err)
	}
	ing.fails = true
	if done := actedOn(t, c, res.Msg.GetChange(), ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED {
		t.Fatalf("the handover ended %v", done.GetState())
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		p := e.Container("coolify-proxy")
		if p.State.Running && p.HostConfig.RestartPolicy.Name == "always" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the failure the proxy is %v, %q; the agent did %v", p.State.Running, p.HostConfig.RestartPolicy.Name, log.read())
		}
	}
	if got := log.read(); len(got) < 3 || got[0] != "stop coolify-proxy" || got[len(got)-1] != "start coolify-proxy" {
		t.Errorf("the agent did %v", got)
	}
}

// A domain whose /admin a sibling router guards with forwardauth doesn't come over
// at all: a route for its root would serve /admin with no login (N1).
func TestAGuardedPathKeepsItsDomainBehind(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	stopper := &loggedStopper{log: &handoverLog{}}
	ing := &loggedRoutes{routesInMemory: &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}, log: stopper.log}
	e := f.addServerWith(t, func() []resource.Type {
		return []resource.Type{&host.Driver{Panels: stopper}, &route.Driver{Ingress: ing}}
	})
	stopper.s = &panels.Stopper{Docker: e.Client()}
	waitForHost(t, f, f.lastDocker)
	id := f.lastDocker.id.ServerID
	for _, name := range []string{"coolify", "coolify-proxy"} {
		runByHand(t, e, name, func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
	}
	e.AddNetwork(docker.Network{ID: "k4s9net", Name: "k4s9_default", Driver: "bridge"})
	composeContainer(t, e, "k4s9", "app", "app-k4s9", func(r *docker.CreateRequest) {
		maps.Copy(r.Labels, map[string]string{
			"coolify.managed": "true", "coolify.type": "service", "coolify.serviceUuid": "k4s9", "coolify.resourceName": "app", "traefik.enable": "true",
			docker.LabelWorkingDir:                               "/data/coolify/services/k4s9",
			"traefik.http.routers.app.rule":                      "Host(`app.example.com`)",
			"traefik.http.services.app.loadbalancer.server.port": "3000",
			"traefik.http.routers.admin.rule":                    "Host(`app.example.com`) && PathPrefix(`/admin`)",
			"traefik.http.routers.admin.service":                 "app",
			"traefik.http.routers.admin.middlewares":             "sso",
			"traefik.http.middlewares.sso.forwardauth.address":   "http://sso.example.com/verify",
		})
	})
	ic, c := importClient(b), changeClient(b, b.origin)
	took, err := ic.ImportCoolify(t.Context(), connect.NewRequest(&dockwellv1.ImportCoolifyRequest{ServerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, c, took.Msg.GetChange())
	_, err = ic.PlanPanelDomains(t.Context(), connect.NewRequest(&dockwellv1.PlanPanelDomainsRequest{ServerId: id, Panel: dockwellv1.PanelKind_PANEL_KIND_COOLIFY}))
	if reasonOf(err) != "nothing_to_route" || !strings.Contains(err.Error(), "app.example.com") {
		t.Errorf("handing over a domain with a guarded path = %v", err)
	}
	if len(ing.routes) != 0 {
		t.Errorf("routes were made: %v", ing.routes)
	}
	// The Move in page says the same as the plan: neither path comes.
	read, err := ic.ImportCoolify(t.Context(), connect.NewRequest(&dockwellv1.ImportCoolifyRequest{ServerId: id, SkipPlan: true}))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"IMPORT_KIND_DOMAIN app.example.com", "IMPORT_KIND_DOMAIN app.example.com/admin"} {
		if s := panelSteps(read.Msg)[name]; s.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_LEAVE {
			t.Errorf("the report has %s as %v", name, s)
		}
	}

	// An agent from before the strict reader hands over no domain.
	old := f.addOnePanelServer(t, t.TempDir(), func(h *agentv1.Hello) {
		h.ReadsPanels, h.StopsPanels, h.ReadsPanelsStrictly, h.Forwards = true, true, false, true
	}).id.ServerID
	if _, err := ic.PlanPanelDomains(t.Context(), connect.NewRequest(&dockwellv1.PlanPanelDomainsRequest{ServerId: old, Panel: dockwellv1.PanelKind_PANEL_KIND_COOLIFY})); reasonOf(err) != "agent_outdated" {
		t.Errorf("a handover through an agent from before the strict reader = %v", err)
	}
}
