// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

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
}

func (l *loggedRoutes) Apply(ctx context.Context, ref resource.Ref, spec *resourcev1.Route, effect *resource.IngressEffect) error {
	l.log.add(map[bool]string{true: "delete ", false: "route "}[spec == nil] + ref.Name)
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
			"coolify.managed": "true", "coolify.type": "service", "coolify.serviceUuid": "k4s9", "coolify.resourceName": "blog",
			docker.LabelWorkingDir:                                             "/data/coolify/services/k4s9",
			"traefik.http.routers.http-0-k4s9-ghost.rule":                      "Host(`blog.example.com`) && PathPrefix(`/`)",
			"traefik.http.services.http-0-k4s9-ghost.loadbalancer.server.port": "2368",
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
	if change.GetSummary() != "Hand over Coolify's domains on hz-1" || !noteSays(change, "panel_domains", "Stops coolify-proxy, which holds ports 80 and 443 for Coolify") {
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
	if _, err := domains(); reasonOf(err) != "already" {
		t.Errorf("handing over again = %v", err)
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
