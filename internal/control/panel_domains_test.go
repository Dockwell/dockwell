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
	"google.golang.org/protobuf/proto"

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
	t.Parallel()
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
	t.Parallel()
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
		{func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/oidc/start?next=/", http.StatusFound)
		}, "answered 302 to a sign-in, /oidc/start"},
		{func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/home", http.StatusFound) }, ""},
		// An app that's down could be one behind a login that's down.
		{func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }, "answered 502"},
		{func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }, "answered 418"},
		{func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }, ""},
	} {
		s.answers("blog.example.com", c.answer)
		got := s.check(t)["blog.example.com"]
		if held := got.GetHeld(); !strings.Contains(held, c.held) || (c.held == "") != (held == "") || got.GetUnsure() != "" {
			t.Errorf("held %q, unsure %q, want %q", held, got.GetUnsure(), c.held)
		}
	}
	// A 404 says nothing either way: blog can be chosen, and the plan says so.
	s.answers("blog.example.com", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	got := s.check(t)["blog.example.com"]
	if got.GetHeld() != "" || got.GetUnsure() != "a request for / answered 404, which says nothing of a login" {
		t.Errorf("after a 404, held %q, unsure %q", got.GetHeld(), got.GetUnsure())
	}
	if got.GetAskedFrom() == "" {
		t.Error("the check doesn't say what it asked from")
	}
	change, err := s.plan(t, "blog.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !noteSays(change, "panel_domains_unsure", "No clear answer for blog.example.com") ||
		!noteSays(change, "panel_domains_asked_from", "Asked from "+got.GetAskedFrom()+", so an address limit that lets in this server or private networks can't be seen") {
		t.Errorf("the plan's notes are %v", change.GetPlan().GetNotes())
	}
}

// The app's own rules pass the scan only where the panel's read made a route of
// that very router. One it couldn't read holds the domain back, as one with a
// pattern, a TCP router's, or a matcher written with a space would: each could
// guard a path with a login no route would keep.
func TestTheAppsOwnRulesPassOnlyWhereARouteCameFromThem(t *testing.T) {
	t.Parallel()
	s := coolifyHandover(t)
	for _, c := range []struct{ id, rule, router string }{
		{"rx", "HostRegexp(`^rx\\.example\\.com$`) && PathPrefix(`/admin`)", "traefik.http.routers.admin"},
		{"sni", "HostSNI(`sni.example.com`)", "traefik.tcp.routers.admin"},
		{"sp", "Host (`sp.example.com`) && PathPrefix(`/admin`)", "traefik.http.routers.admin"},
	} {
		s.e.AddNetwork(docker.Network{ID: c.id + "net", Name: c.id + "_default", Driver: "bridge"})
		composeContainer(t, s.e, c.id, "app", "app-"+c.id, func(r *docker.CreateRequest) {
			maps.Copy(r.Labels, map[string]string{
				"coolify.managed": "true", "coolify.type": "service", "coolify.serviceUuid": c.id, "coolify.resourceName": c.id, "traefik.enable": "true",
				docker.LabelWorkingDir:                               "/data/coolify/services/" + c.id,
				"traefik.http.routers.app.rule":                      "Host(`" + c.id + ".example.com`)",
				"traefik.http.services.app.loadbalancer.server.port": "3000",
				c.router + ".rule":                                   c.rule,
				c.router + ".middlewares":                            "sso",
				"traefik.http.middlewares.sso.forwardauth.address":   "http://sso.example.com/verify",
			})
		})
	}
	s.takeOn(t)
	checks := s.check(t)
	for _, id := range []string{"rx", "sni", "sp"} {
		d := checks[id+".example.com"]
		if !strings.Contains(d.GetHeld(), "on app-"+id) || len(d.GetProbes()) != 0 {
			t.Errorf("%s reads held %q, probed %v", id, d.GetHeld(), d.GetProbes())
		}
	}
	if got := checks["blog.example.com"]; got.GetHeld() != "" {
		t.Errorf("blog, whose one router made its route, reads %q", got.GetHeld())
	}
}

// Another rule anywhere on the server that names a domain holds it back: a label
// on a container the panel didn't make, a HostRegexp that matches it, a file in
// the proxy's mounts. What can't be read in full holds every domain back.
func TestTheScanHoldsWhatElseRoutesADomain(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// A domain Coolify redirects to its other half, www to the bare domain here, is
// handed over as a route that answers with that redirect (ADR 0070): the check
// expects the proxy to answer with it, and holds it back where it doesn't; and it
// comes only with the domain it lands on, which would stop answering otherwise.
func TestAWWWRedirectIsHandedOverAsOne(t *testing.T) {
	t.Parallel()
	s := coolifyHandover(t)
	s.e.AddNetwork(docker.Network{ID: "m3n8net", Name: "m3n8_default", Driver: "bridge"})
	labels := map[string]string{
		"coolify.managed": "true", "coolify.type": "service", "traefik.enable": "true", "coolify.serviceUuid": "m3n8", "coolify.resourceName": "shop",
		docker.LabelWorkingDir: "/data/coolify/services/m3n8",
		"traefik.http.middlewares.redirect-to-non-www.redirectregex.regex":       `^(http|https)://www\.(.+)`,
		"traefik.http.middlewares.redirect-to-non-www.redirectregex.replacement": "${1}://${2}",
		"traefik.http.middlewares.redirect-to-non-www.redirectregex.permanent":   "false",
		"traefik.http.services.http-0-m3n8-shop.loadbalancer.server.port":        "8080",
	}
	for i, host := range []string{"shop.example.com", "www.shop.example.com"} {
		router := "traefik.http.routers.https-" + string(rune('0'+i)) + "-m3n8-shop."
		maps.Copy(labels, map[string]string{
			router + "rule": "Host(`" + host + "`) && PathPrefix(`/`)", router + "entrypoints": "https",
			router + "middlewares": "redirect-to-non-www", router + "service": "http-0-m3n8-shop",
		})
	}
	composeContainer(t, s.e, "m3n8", "shop", "shop-m3n8", func(r *docker.CreateRequest) { maps.Copy(r.Labels, labels) })
	s.answers("www.shop.example.com", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://shop.example.com"+r.URL.Path, http.StatusFound)
	})
	s.takeOn(t)
	res, err := s.ic.ImportCoolify(t.Context(), connect.NewRequest(&dockwellv1.ImportCoolifyRequest{ServerId: s.id, SkipPlan: true}))
	if err != nil {
		t.Fatal(err)
	}
	if st := panelSteps(res.Msg)["IMPORT_KIND_DOMAIN www.shop.example.com"]; st.GetBecomes() != "a route that redirects to https://shop.example.com, for a while (302), as it does now" {
		t.Errorf("www's step reads %v", st)
	}
	checks := s.check(t)
	www := checks["www.shop.example.com"]
	if www.GetHeld() != "" || www.GetRedirectsTo() != "https://shop.example.com" || len(www.GetProbes()) != 1 || !www.GetProbes()[0].GetOpen() ||
		www.GetProbes()[0].GetAnswer() != "302 to shop.example.com, as its redirect does" {
		t.Errorf("www reads %v", www)
	}
	if shop := checks["shop.example.com"]; shop.GetHeld() != "" || shop.GetRedirectsTo() != "" {
		t.Errorf("the domain it lands on reads %v", shop)
	}

	// Alone, it would land on a domain that stops answering with the proxy.
	if _, err := s.plan(t, "www.shop.example.com"); reasonOf(err) != "redirect_left" ||
		!strings.Contains(err.Error(), "www.shop.example.com redirects to shop.example.com, which stops answering once coolify-proxy stops, as it isn't chosen. Choose shop.example.com too") {
		t.Errorf("a handover of the redirect alone = %v", err)
	}
	change, err := s.plan(t, "shop.example.com", "www.shop.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if done := actedOn(t, s.c, change, ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the handover ended %v: %s", done.GetState(), done.GetError())
	}
	made := map[string]*resourcev1.Route{}
	for _, r := range s.ing.routes {
		made[r.GetDomain()] = r
	}
	if r := made["www.shop.example.com"]; r.GetService() != "" || r.GetPort() != 0 ||
		!proto.Equal(r.GetRedirect(), &resourcev1.RouteRedirect{To: "https://shop.example.com", Temporary: true}) {
		t.Errorf("www's route is %v", r)
	}
	if r := made["shop.example.com"]; r.GetService() != "shop" || r.GetPort() != 8080 || r.GetRedirect() != nil {
		t.Errorf("the domain's route is %v", r)
	}
}

// A domain whose route would redirect is held back where the proxy answers it any
// other way, and so is one that redirects to a domain that stays behind.
func TestAWWWRedirectTheProxyDoesntAnswerStaysBehind(t *testing.T) {
	t.Parallel()
	s := coolifyHandover(t)
	s.e.AddNetwork(docker.Network{ID: "m3n8net", Name: "m3n8_default", Driver: "bridge"})
	labels := map[string]string{
		"coolify.managed": "true", "coolify.type": "service", "traefik.enable": "true", "coolify.serviceUuid": "m3n8", "coolify.resourceName": "shop",
		docker.LabelWorkingDir:                                            "/data/coolify/services/m3n8",
		"traefik.http.middlewares.to-www.redirectregex.regex":             `^(http|https)://(?:www\.)?(.+)`,
		"traefik.http.middlewares.to-www.redirectregex.replacement":       "${1}://www.${2}",
		"traefik.http.services.http-0-m3n8-shop.loadbalancer.server.port": "8080",
		"traefik.http.routers.https-0-m3n8-shop.rule":                     "Host(`shop.example.com`)",
		"traefik.http.routers.https-0-m3n8-shop.middlewares":              "to-www",
		"traefik.http.routers.https-1-m3n8-shop.rule":                     "Host(`www.shop.example.com`)",
		"traefik.http.routers.https-1-m3n8-shop.middlewares":              "to-www",
		"traefik.http.routers.https-1-m3n8-shop.service":                  "http-0-m3n8-shop",
	}
	composeContainer(t, s.e, "m3n8", "shop", "shop-m3n8", func(r *docker.CreateRequest) { maps.Copy(r.Labels, labels) })
	s.takeOn(t)

	// The proxy serves a page where the route would redirect.
	shop := s.check(t)["shop.example.com"]
	if shop.GetRedirectsTo() != "https://www.shop.example.com" || shop.GetHeld() != "a request for / through the proxy answered 200, where its redirect would send it to www.shop.example.com" {
		t.Errorf("a redirect the proxy doesn't answer with reads %v", shop)
	}
	// It does now, but where it lands stays behind, behind a login.
	s.answers("shop.example.com", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://www.shop.example.com"+r.URL.Path, http.StatusFound)
	})
	s.answers("www.shop.example.com", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	checks := s.check(t)
	if got := checks["shop.example.com"].GetHeld(); got != "it redirects to www.shop.example.com, which stays behind" {
		t.Errorf("a redirect to a domain held back reads %q, and that domain %v", got, checks["www.shop.example.com"])
	}
}

// A redirect whose domain the control plane holds, as one its proxy's labels name no
// port of, stays behind too: chosen, it would land on nothing, and the plan would
// refuse it with a fix the sheet can't offer (slice 4's review, M2).
func TestAWWWRedirectToADomainHeldHereStaysBehind(t *testing.T) {
	t.Parallel()
	s := coolifyHandover(t)
	s.e.AddNetwork(docker.Network{ID: "m3n8net", Name: "m3n8_default", Driver: "bridge"})
	labels := map[string]string{
		"coolify.managed": "true", "coolify.type": "service", "traefik.enable": "true", "coolify.serviceUuid": "m3n8", "coolify.resourceName": "shop",
		docker.LabelWorkingDir: "/data/coolify/services/m3n8",
		"traefik.http.middlewares.to-non-www.redirectregex.regex":         `^(http|https)://www\.(.+)`,
		"traefik.http.middlewares.to-non-www.redirectregex.replacement":   "${1}://${2}",
		"traefik.http.services.http-0-m3n8-shop.loadbalancer.server.port": "8080",
		// The bare domain's router names a service with no port, which the control
		// plane can't route; www's redirect needs none.
		"traefik.http.routers.https-0-m3n8-shop.rule":        "Host(`shop.example.com`)",
		"traefik.http.routers.https-0-m3n8-shop.middlewares": "to-non-www",
		"traefik.http.routers.https-0-m3n8-shop.service":     "elsewhere",
		"traefik.http.routers.https-1-m3n8-shop.rule":        "Host(`www.shop.example.com`)",
		"traefik.http.routers.https-1-m3n8-shop.middlewares": "to-non-www",
		"traefik.http.routers.https-1-m3n8-shop.service":     "http-0-m3n8-shop",
	}
	composeContainer(t, s.e, "m3n8", "shop", "shop-m3n8", func(r *docker.CreateRequest) { maps.Copy(r.Labels, labels) })
	s.answers("www.shop.example.com", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://shop.example.com"+r.URL.Path, http.StatusFound)
	})
	s.takeOn(t)
	checks := s.check(t)
	if !strings.Contains(checks["shop.example.com"].GetHeld(), "which port") {
		t.Fatalf("the domain it lands on reads %v", checks["shop.example.com"])
	}
	if got := checks["www.shop.example.com"].GetHeld(); got != "it redirects to shop.example.com, which stays behind" {
		t.Errorf("a redirect to a domain held here reads %q", got)
	}
	if _, err := s.plan(t, "www.shop.example.com"); reasonOf(err) != "domain_held" {
		t.Errorf("a handover of it = %v", err)
	}
}
