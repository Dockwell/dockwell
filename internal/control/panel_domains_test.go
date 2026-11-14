// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
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
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/panels"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
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

// handoverStand is a server Coolify runs on: its proxy, Traefik's official image
// as Coolify's installer runs it, with its own folder mounted; the panel's own
// route to its dashboard in that folder; and an app, blog, routed to
// blog.example.com. The proxy's answers come from a TLS server of the test's,
// which says for each domain what the proxy answers.
type handoverStand struct {
	f      *fullServer
	e      *dockertest.Engine
	id     string
	log    *handoverLog
	ing    *loggedRoutes
	ic     dockwellv1connect.ImportServiceClient
	c      dockwellv1connect.ChangeServiceClient
	ghost  string
	mu     sync.Mutex
	answer map[string]http.HandlerFunc
}

// answers sets what the proxy answers for a domain, 200 where none is set.
func (s *handoverStand) answers(domain string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer[domain] = h
}

func coolifyHandover(t *testing.T) *handoverStand {
	t.Helper()
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
	s := &handoverStand{f: f, log: &handoverLog{}, answer: map[string]http.HandlerFunc{}}
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		h := s.answer[r.TLS.ServerName]
		s.mu.Unlock()
		if h == nil || r.Host != r.TLS.ServerName {
			w.WriteHeader(http.StatusOK)
			return
		}
		h(w, r)
	}))
	proxy.StartTLS()
	t.Cleanup(proxy.Close)
	f.panelsRoot, f.panelsProbe = root, proxy.Listener.Addr().String()
	stopper := &loggedStopper{log: s.log}
	s.ing = &loggedRoutes{routesInMemory: &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}, log: s.log}
	s.e = f.addServerWith(t, func() []resource.Type {
		return []resource.Type{&host.Driver{Panels: stopper}, &route.Driver{Ingress: s.ing}}
	})
	stopper.s = &panels.Stopper{Docker: s.e.Client(), Check: f.panelReader(s.e.Client()).Check}
	waitForHost(t, f, f.lastDocker)
	s.id = f.lastDocker.id.ServerID
	runByHand(t, s.e, "coolify", func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
	s.e.AddLocalImage("traefik:v3.1", dockertest.Image{Config: docker.Config{Cmd: []string{"traefik"}}})
	runByHand(t, s.e, "coolify-proxy", func(r *docker.CreateRequest) {
		// As Coolify's installer runs it.
		r.Image = "traefik:v3.1"
		r.Cmd = []string{
			"--ping=true", "--api.dashboard=true", "--entrypoints.http.address=:80", "--entrypoints.https.address=:443",
			"--providers.file.directory=/traefik/dynamic/", "--providers.file.watch=true", "--providers.docker=true", "--providers.docker.exposedbydefault=false",
		}
		r.HostConfig.PortBindings, r.HostConfig.Binds = nil, []string{"/data/coolify/proxy:/traefik", "/var/run/docker.sock:/var/run/docker.sock:ro"}
	})
	s.e.AddNetwork(docker.Network{ID: "k4s9net", Name: "k4s9_default", Driver: "bridge"})
	s.ghost = composeContainer(t, s.e, "k4s9", "ghost", "ghost-k4s9", func(r *docker.CreateRequest) {
		maps.Copy(r.Labels, map[string]string{
			"coolify.managed": "true", "coolify.type": "service", "traefik.enable": "true", "coolify.serviceUuid": "k4s9", "coolify.resourceName": "blog",
			docker.LabelWorkingDir:                                             "/data/coolify/services/k4s9",
			"traefik.http.routers.http-0-k4s9-ghost.rule":                      "Host(`blog.example.com`) && PathPrefix(`/`)",
			"traefik.http.services.http-0-k4s9-ghost.loadbalancer.server.port": "2368",
		})
	})
	s.ic, s.c = importClient(b), changeClient(b, b.origin)
	return s
}

// check is what CheckPanelDomains says of each domain, by name.
func (s *handoverStand) check(t *testing.T) map[string]*dockwellv1.PanelDomainCheck {
	t.Helper()
	res, err := s.ic.CheckPanelDomains(t.Context(), connect.NewRequest(&dockwellv1.CheckPanelDomainsRequest{ServerId: s.id, Panel: dockwellv1.PanelKind_PANEL_KIND_COOLIFY}))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*dockwellv1.PanelDomainCheck{}
	for _, d := range res.Msg.GetDomains() {
		out[d.GetDomain()] = d
	}
	return out
}

func (s *handoverStand) plan(t *testing.T, domains ...string) (*dockwellv1.Change, error) {
	t.Helper()
	res, err := s.ic.PlanPanelDomains(t.Context(), connect.NewRequest(&dockwellv1.PlanPanelDomainsRequest{ServerId: s.id, Panel: dockwellv1.PanelKind_PANEL_KIND_COOLIFY, Domains: domains}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetChange(), nil
}

func (s *handoverStand) takeOn(t *testing.T) {
	t.Helper()
	took, err := s.ic.ImportCoolify(t.Context(), connect.NewRequest(&dockwellv1.ImportCoolifyRequest{ServerId: s.id}))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, s.c, took.Msg.GetChange())
}

// Handing over a panel's domains is evidence and a choice (ADR 0089): each domain
// is checked, one nothing was found for is what a person can choose, and the
// change stops the proxy first, then makes a route for each chosen, as the proxy's
// labels send it. Its undo deletes the routes before it starts the proxy again.
func TestHandingOverAPanelsDomains(t *testing.T) {
	s := coolifyHandover(t)
	// One behind a login stays behind: a plain route would let anyone in.
	s.e.AddNetwork(docker.Network{ID: "p2q7net", Name: "p2q7_default", Driver: "bridge"})
	composeContainer(t, s.e, "p2q7", "wiki", "wiki-p2q7", func(r *docker.CreateRequest) {
		maps.Copy(r.Labels, map[string]string{
			"coolify.managed": "true", "coolify.type": "service", "traefik.enable": "true", "coolify.serviceUuid": "p2q7", "coolify.resourceName": "wiki",
			docker.LabelWorkingDir:                                            "/data/coolify/services/p2q7",
			"traefik.http.routers.http-0-p2q7-wiki.rule":                      "Host(`wiki.example.com`)",
			"traefik.http.routers.http-0-p2q7-wiki.middlewares":               "sso@file",
			"traefik.http.services.http-0-p2q7-wiki.loadbalancer.server.port": "3000",
		})
	})
	domainStep := func() *dockwellv1.ImportStep {
		res, err := s.ic.ImportCoolify(t.Context(), connect.NewRequest(&dockwellv1.ImportCoolifyRequest{ServerId: s.id, SkipPlan: true}))
		if err != nil {
			t.Fatal(err)
		}
		return panelSteps(res.Msg)["IMPORT_KIND_DOMAIN blog.example.com"]
	}

	// Nothing taken on yet: the domain stays, and says why.
	if blog := s.check(t)["blog.example.com"]; blog.GetHeld() != "blog isn't taken on yet" {
		t.Errorf("before a take-on, blog reads %v", blog)
	}
	s.takeOn(t)
	if st := domainStep(); st.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_MAKE || st.GetBecomes() != "a route to ghost on port 2368" {
		t.Errorf("before the handover, the domain reads %v", st)
	}
	checks := s.check(t)
	blog := checks["blog.example.com"]
	if blog.GetHeld() != "" || len(blog.GetMentions()) != 0 || len(blog.GetProbes()) != 1 || !blog.GetProbes()[0].GetOpen() || blog.GetProbes()[0].GetAnswer() != "200" ||
		!strings.Contains(blog.GetScanned(), "and 1 files in the proxy's mounts") {
		t.Errorf("blog reads %v", blog)
	}
	if !strings.Contains(checks["wiki.example.com"].GetHeld(), "sso@file") || checks["coolify.example.com"].GetHeld() == "" {
		t.Errorf("the others read %v and %v", checks["wiki.example.com"], checks["coolify.example.com"])
	}

	// A handover names its domains, each one nothing was found for.
	if _, err := s.plan(t); reasonOf(err) != "no_domains" {
		t.Errorf("a handover of nothing = %v", err)
	}
	if _, err := s.plan(t, "blog.example.com", "wiki.example.com"); reasonOf(err) != "domain_held" || !strings.Contains(err.Error(), "wiki.example.com stays behind") {
		t.Errorf("a handover of a guarded domain = %v", err)
	}
	change, err := s.plan(t, "Blog.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if change.GetSummary() != "Hand over blog.example.com from Coolify on hz-1" || !noteSays(change, "panel_domains", "Stops coolify-proxy, which holds ports 80 and 443 for Coolify") ||
		!noteSays(change, "panel_domains", "the agent checks each again") ||
		!noteSays(change, "panel_domains", "These stop answering, as nothing in Dockwell serves them: coolify.example.com and wiki.example.com") ||
		!noteSays(change, "panel_domains_unguarded", "No login or address limit found on the old proxy: blog.example.com") {
		t.Errorf("the handover reads %q, %v", change.GetSummary(), change.GetPlan().GetNotes())
	}
	var kinds []string
	for _, st := range change.GetPlan().GetServers()[0].GetSteps() {
		kinds = append(kinds, st.GetResource().GetKind())
		if p := st.GetPanel(); p != nil && (!slices.Equal(p.GetDomains(), []string{"blog.example.com"}) || len(p.GetEvidence()) != 32) {
			t.Errorf("the proxy's step reads %v", p)
		}
	}
	if !slices.Equal(kinds, []string{"server.host", "route"}) {
		t.Errorf("the handover's steps are %v", kinds)
	}
	if done := actedOn(t, s.c, change, ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the handover ended %v: %s", done.GetState(), done.GetError())
	}
	if got := s.log.read(); !slices.Equal(got, []string{"stop coolify-proxy", "route blog-example-com"}) {
		t.Errorf("the handover did %v", got)
	}
	var made *resourcev1.Route
	for _, r := range s.ing.routes {
		made = r
	}
	if made.GetDomain() != "blog.example.com" || made.GetService() != "ghost" || made.GetPort() != 2368 {
		t.Errorf("the route is %v", made)
	}
	if p := s.e.Container("coolify-proxy"); p.State.Running || p.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("the proxy is left %v, %q", p.State.Running, p.HostConfig.RestartPolicy.Name)
	}
	if !s.e.Container(s.ghost).State.Running {
		t.Error("the app was stopped")
	}
	if st := domainStep(); st.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_DONE {
		t.Errorf("after the handover, the domain reads %v", st)
	}
	if _, ok := s.check(t)["blog.example.com"]; ok {
		t.Error("a domain Dockwell routes is offered again")
	}
	// The proxy can't start again while Dockwell's ingress holds its ports.
	if _, err := s.ic.PlanStartPanel(t.Context(), connect.NewRequest(&dockwellv1.PlanStartPanelRequest{ServerId: s.id, Panel: dockwellv1.PanelKind_PANEL_KIND_COOLIFY, Proxy: true})); reasonOf(err) != "ports_held" ||
		!strings.Contains(err.Error(), "blog-example-com") {
		t.Errorf("starting the proxy with a route on the server = %v", err)
	}

	undo, err := s.c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: change.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if done := actedOn(t, s.c, undo.Msg.GetChange(), ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the undo ended %v: %s", done.GetState(), done.GetError())
	}
	if got := s.log.read()[2:]; !slices.Equal(got, []string{"delete blog-example-com", "start coolify-proxy"}) {
		t.Errorf("the undo did %v", got)
	}
	if p := s.e.Container("coolify-proxy"); !p.State.Running || p.HostConfig.RestartPolicy.Name != "always" {
		t.Errorf("after the undo the proxy is %v, %q", p.State.Running, p.HostConfig.RestartPolicy.Name)
	}
	if len(s.ing.routes) != 0 || !strings.Contains(domainStep().GetBecomes(), "a route to ghost") {
		t.Errorf("after the undo, routes %v and the domain reads %v", s.ing.routes, domainStep())
	}
}

// What the probe hears holds a domain back: a login's 401, a redirect to a
// sign-in, another host, or a TLS handshake the proxy refuses.
func TestTheProbeHoldsWhatALoginAnswers(t *testing.T) {
	s := coolifyHandover(t)
	s.takeOn(t)
	for _, c := range []struct {
		answer http.HandlerFunc
		held   string
	}{
		{func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, "a request for / through the proxy answered 401"},
		{func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://blog.example.com/auth/sign-in?rd=/", http.StatusFound)
		}, "answered 302 to a sign-in, /auth/sign-in"},
		{func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://sso.example.com/", http.StatusFound)
		}, "answered 302 to another host, sso.example.com"},
		{func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/home", http.StatusFound) }, ""},
		{func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }, ""},
	} {
		s.answers("blog.example.com", c.answer)
		if got := s.check(t)["blog.example.com"].GetHeld(); !strings.Contains(got, c.held) || (c.held == "") != (got == "") {
			t.Errorf("held %q, want %q", got, c.held)
		}
	}
}

// Another rule anywhere on the server that names a domain holds it back: a label
// on a container the panel didn't make, a HostRegexp that matches it, a file in
// the proxy's mounts. What can't be read in full holds every domain back.
func TestTheScanHoldsWhatElseRoutesADomain(t *testing.T) {
	s := coolifyHandover(t)
	s.takeOn(t)
	blog := func() string { return s.check(t)["blog.example.com"].GetHeld() }
	if got := blog(); got != "" {
		t.Fatalf("blog is held at first: %s", got)
	}
	runByHand(t, s.e, "sso", func(r *docker.CreateRequest) {
		r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
		r.Labels = map[string]string{"traefik.http.routers.admin.rule": "Host(`BLOG.example.com`) && PathPrefix(`/admin`)"}
	})
	if got := blog(); got != "another rule mentions it: the label traefik.http.routers.admin.rule on sso" {
		t.Errorf("with another container's rule, blog reads %q", got)
	}
	s.e.Remove("sso")
	runByHand(t, s.e, "catchall", func(r *docker.CreateRequest) {
		r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
		r.Labels = map[string]string{"traefik.http.routers.all.rule": "HostRegexp(`^.+\\.example\\.com$`)"}
	})
	if got := blog(); !strings.Contains(got, "the label traefik.http.routers.all.rule on catchall") {
		t.Errorf("with a HostRegexp, blog reads %q", got)
	}
	s.e.Remove("catchall")
	// A file anywhere in the proxy's mounts, not only those it loads.
	extra := filepath.Join(s.f.panelsRoot, "data", "coolify", "proxy", "kept", "rules.toml")
	if err := os.MkdirAll(filepath.Dir(extra), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extra, []byte("[http.routers.x]\n  rule = \"Host(`blog.example.com`) && Path(`/metrics`)\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := blog(); got != "another rule mentions it: the file /data/coolify/proxy/kept/rules.toml" {
		t.Errorf("with a file of the proxy's, blog reads %q", got)
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(s.f.panelsRoot, "data", "coolify", "proxy", "linked.yml")); err != nil {
		t.Fatal(err)
	}
	if got := blog(); got != "the proxy's mount holds a link, /data/coolify/proxy/linked.yml, which Dockwell doesn't follow" {
		t.Errorf("with a link in the proxy's mount, blog reads %q", got)
	}
}

// The agent checks the chosen domains again just before it stops the proxy, and
// leaves it running where anything changed since the plan: a rule that names the
// domain now, or what the proxy answers.
func TestAHandoverChecksAgainBeforeTheProxyStops(t *testing.T) {
	s := coolifyHandover(t)
	s.takeOn(t)
	refused := func(change *dockwellv1.Change, says string) {
		t.Helper()
		done := actedOn(t, s.c, change, "")
		if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(done.GetError(), says) {
			t.Errorf("the handover ended %v: %s", done.GetState(), done.GetError())
		}
		if p := s.e.Container("coolify-proxy"); !p.State.Running || len(s.ing.routes) != 0 {
			t.Errorf("after the refusal the proxy runs %v, routes %v", p.State.Running, s.ing.routes)
		}
	}
	change, err := s.plan(t, "blog.example.com")
	if err != nil {
		t.Fatal(err)
	}
	runByHand(t, s.e, "sso", func(r *docker.CreateRequest) {
		r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
		r.Labels = map[string]string{"traefik.http.routers.admin.rule": "Host(`blog.example.com`)"}
	})
	refused(change, "blog.example.com stays behind now: another rule mentions it")
	s.e.Remove("sso")
	// The refused change's route rolls back after it reads failed; until then
	// the domain reads as Dockwell's, so it isn't offered.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if c, ok := s.check(t)["blog.example.com"]; ok && c.GetHeld() == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the refusal blog reads %v", s.check(t)["blog.example.com"])
		}
	}

	change, err = s.plan(t, "blog.example.com")
	if err != nil {
		t.Fatal(err)
	}
	s.answers("blog.example.com", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	refused(change, "a request for / through the proxy answered 403")
}

// A handover whose route fails after the proxy stopped rolls back, and the rollback
// starts the proxy again as it stood, so the server's domains aren't left down.
func TestAFailedHandoverStartsTheProxyAgain(t *testing.T) {
	s := coolifyHandover(t)
	s.takeOn(t)
	change, err := s.plan(t, "blog.example.com")
	if err != nil {
		t.Fatal(err)
	}
	s.ing.fails = true
	if done := actedOn(t, s.c, change, ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED {
		t.Fatalf("the handover ended %v", done.GetState())
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		p := s.e.Container("coolify-proxy")
		if p.State.Running && p.HostConfig.RestartPolicy.Name == "always" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the failure the proxy is %v, %q; the agent did %v", p.State.Running, p.HostConfig.RestartPolicy.Name, s.log.read())
		}
	}
	if got := s.log.read(); len(got) < 3 || got[0] != "stop coolify-proxy" || got[len(got)-1] != "start coolify-proxy" {
		t.Errorf("the agent did %v", got)
	}
}

// A domain whose /admin a sibling router guards with forwardauth stays behind
// whole: a route for its root would serve /admin with no login (N1). An agent that
// doesn't check domains hands over none.
func TestAGuardedPathKeepsItsDomainBehind(t *testing.T) {
	s := coolifyHandover(t)
	s.e.AddNetwork(docker.Network{ID: "m3n8net", Name: "m3n8_default", Driver: "bridge"})
	composeContainer(t, s.e, "m3n8", "app", "app-m3n8", func(r *docker.CreateRequest) {
		maps.Copy(r.Labels, map[string]string{
			"coolify.managed": "true", "coolify.type": "service", "coolify.serviceUuid": "m3n8", "coolify.resourceName": "app", "traefik.enable": "true",
			docker.LabelWorkingDir:                               "/data/coolify/services/m3n8",
			"traefik.http.routers.app.rule":                      "Host(`app.example.com`)",
			"traefik.http.services.app.loadbalancer.server.port": "3000",
			"traefik.http.routers.admin.rule":                    "Host(`app.example.com`) && PathPrefix(`/admin`)",
			"traefik.http.routers.admin.service":                 "app",
			"traefik.http.routers.admin.middlewares":             "sso",
			"traefik.http.middlewares.sso.forwardauth.address":   "http://sso.example.com/verify",
		})
	})
	s.takeOn(t)
	if got := s.check(t)["app.example.com"].GetHeld(); got == "" {
		t.Error("app.example.com can be chosen")
	}
	if _, err := s.plan(t, "app.example.com"); reasonOf(err) != "domain_held" {
		t.Errorf("handing over a domain with a guarded path = %v", err)
	}
	if len(s.ing.routes) != 0 {
		t.Errorf("routes were made: %v", s.ing.routes)
	}
	old := s.f.addOnePanelServer(t, t.TempDir(), func(h *agentv1.Hello) {
		h.ReadsPanels, h.StopsPanels, h.ChecksPanelDomains, h.Forwards = true, true, false, true
	}).id.ServerID
	if _, err := s.ic.CheckPanelDomains(t.Context(), connect.NewRequest(&dockwellv1.CheckPanelDomainsRequest{ServerId: old, Panel: dockwellv1.PanelKind_PANEL_KIND_COOLIFY})); reasonOf(err) != "agent_outdated" {
		t.Errorf("a check through an agent that doesn't check domains = %v", err)
	}
}
