// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/opener"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// An app that answers only on its server opens through the panel (ADR 0061): with a
// passkey just now, the panel gets a one-time address on an origin of the app's own,
// a browser that opens it gets a cookie of that origin, and what it asks for reaches
// the app through the server's agent, which dials the port the service publishes. It
// goes on the audit log, with where it was asked from, and signing out of the panel
// ends it at once.
func TestAnAppOpensThroughThePanel(t *testing.T) { storetest.Each(t, testAnAppOpensThroughThePanel) }

func testAnAppOpensThroughThePanel(t *testing.T, backend store.Backend) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("dockwell_session"); err == nil {
			http.Error(w, "the panel's cookie reached the app", http.StatusTeapot)
			return
		}
		_, _ = io.WriteString(w, "phpMyAdmin at "+r.Host+r.URL.Path)
	}))
	defer app.Close()
	_, appPort, _ := net.SplitHostPort(strings.TrimPrefix(app.URL, "http://"))
	port, _ := strconv.Atoi(appPort)

	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.Registry["phpmyadmin:5"] = dockertest.Image{Config: docker.Config{Cmd: []string{"apache2-foreground"}}}
	ch := changeClient(b, b.origin)
	apply(t, ch, propose(t, ch, "Run phpMyAdmin", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "phpmyadmin"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs: &resourcev1.Service_App{App: &resourcev1.Container{
				Image: "phpmyadmin:5",
				Ports: []*resourcev1.PortMapping{{HostIp: "127.0.0.1", HostPort: uint32(port), ContainerPort: 80}},
			}},
		}}},
	}))

	services := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	_, err := services.OpenService(t.Context(), connect.NewRequest(&dockwellv1.OpenServiceRequest{Id: "phpmyadmin"}))
	if reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatalf("opening it with no passkey: %v, want needs_step_up", err)
	}
	listed, err := services.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
	if err != nil || len(listed.Msg.GetServices()) != 1 {
		t.Fatalf("ListServices: %v, %v", listed, err)
	}
	serviceID := listed.Msg.GetServices()[0].GetId()
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_OPEN_SERVICE, serviceID)
	res, err := services.OpenService(t.Context(), connect.NewRequest(&dockwellv1.OpenServiceRequest{Id: "phpmyadmin", Proof: proof}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.OpenService(t.Context(), connect.NewRequest(&dockwellv1.OpenServiceRequest{Id: "phpmyadmin", Proof: proof})); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("a proof opened it a second time: %v", err)
	}
	origin := res.Msg.GetOrigin()
	if !strings.HasSuffix(origin, ".localhost:"+strings.TrimPrefix(b.origin[strings.LastIndex(b.origin, ":"):], ":")) {
		t.Fatalf("it opens at %s, from a panel at %s", origin, b.origin)
	}
	got, err := b.client.Get(res.Msg.GetUrl())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(got.Body)
	_ = got.Body.Close()
	host := strings.TrimPrefix(origin, "http://")
	if got.StatusCode != http.StatusOK || string(body) != "phpMyAdmin at "+host+"/" {
		t.Fatalf("opening it answered %d: %s", got.StatusCode, body)
	}
	opened := origin + "/index.php"
	again, err := b.client.Get(res.Msg.GetUrl())
	if err != nil {
		t.Fatal(err)
	}
	_ = again.Body.Close()
	if again.StatusCode != http.StatusForbidden {
		t.Errorf("its address opened a second time: %d", again.StatusCode)
	}

	var told string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		events, err := tx.ListEvents(t.Context(), 50)
		for i := range events {
			if events[i].Action == "service.open" {
				told = events[i].Detail
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var detail struct {
		Service string `json:"service"`
		Port    uint32 `json:"port"`
		Origin  string `json:"origin"`
		From    string `json:"from"`
	}
	if err := json.Unmarshal([]byte(told), &detail); err != nil || detail.Service != "phpmyadmin" || detail.Port != 80 || detail.Origin != origin || detail.From != "127.0.0.1" {
		t.Errorf("the audit log says %q", told)
	}

	// Refused before a passkey is asked for, so nobody confirms what can't happen.
	_, err = services.OpenService(t.Context(), connect.NewRequest(&dockwellv1.OpenServiceRequest{Id: "phpmyadmin", Port: 443}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "publishes no port for 443") {
		t.Errorf("a port it doesn't publish: %v", err)
	}

	// Signing out of the panel ends what it opened at once, not at a recheck.
	if again, err := b.client.Get(opened); err != nil || again.StatusCode != http.StatusOK {
		t.Fatalf("the open app answered %v, %v", again, err)
	} else {
		_ = again.Body.Close()
	}
	if _, err := b.authClient().SignOut(t.Context(), connect.NewRequest(&dockwellv1.SignOutRequest{})); err != nil {
		t.Fatal(err)
	}
	after, err := b.client.Get(opened)
	if err != nil {
		t.Fatal(err)
	}
	_ = after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Errorf("the app stayed open after signing out of the panel: %d", after.StatusCode)
	}
}

// An agent carries 16 forwards to apps opened through the panel at once, and what an
// open session no longer uses goes back (ADR 0061): opening an app again ends the
// session before, and a session keeps one idle connection, so a person who opens one
// app four times, or four apps a page each, loads every page whole. Past them, the
// agent's refusal reaches the browser as a full server, in words that say whose
// apps fill it.
func TestOpenedAppsGiveTheirForwardsBack(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.Registry["app:1"] = dockertest.Image{Config: docker.Config{Cmd: []string{"app"}}}
	ch := changeClient(b, b.origin)
	services := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	slow := make(chan time.Duration, 1)
	slow <- 100 * time.Millisecond
	var ids []string
	for _, name := range []string{"one", "two", "three", "four"} {
		app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			d := <-slow
			slow <- d
			time.Sleep(d)
			_, _ = io.WriteString(w, "ok")
		}))
		t.Cleanup(app.Close)
		_, appPort, _ := net.SplitHostPort(strings.TrimPrefix(app.URL, "http://"))
		port, _ := strconv.Atoi(appPort)
		ids = append(ids, resourceIn(t, apply(t, ch, propose(t, ch, "Run "+name, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Name: name},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs: &resourcev1.Service_App{App: &resourcev1.Container{
					Image: "app:1",
					Ports: []*resourcev1.PortMapping{{HostIp: "127.0.0.1", HostPort: uint32(port), ContainerPort: 80}},
				}},
			}}},
		})), "service"))
	}
	type tab struct {
		host   string
		cookie *http.Cookie
	}
	open := func(id string) tab {
		t.Helper()
		proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_OPEN_SERVICE, id)
		res, err := services.OpenService(t.Context(), connect.NewRequest(&dockwellv1.OpenServiceRequest{Id: id, Proof: proof}))
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.client.Get(res.Msg.GetUrl())
		if err != nil {
			t.Fatal(err)
		}
		_ = got.Body.Close()
		u, _ := url.Parse(res.Msg.GetOrigin())
		for _, c := range b.client.Jar.Cookies(u) {
			if c.Name == opener.CookieName {
				return tab{host: u.Host, cookie: c}
			}
		}
		t.Fatal("opening it set no cookie")
		return tab{}
	}
	// A page is six requests at once, as many as a browser sends to a host.
	type page struct {
		ok      int
		refused []string
	}
	load := func(tabs ...tab) page {
		var mu sync.Mutex
		var p page
		var wg sync.WaitGroup
		for _, tb := range tabs {
			var d net.Dialer
			browser := &http.Client{Transport: &http.Transport{
				MaxConnsPerHost: 6,
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return d.DialContext(ctx, network, f.api)
				},
			}}
			t.Cleanup(browser.CloseIdleConnections)
			for i := range 6 {
				wg.Go(func() {
					req, _ := http.NewRequest(http.MethodGet, "http://"+tb.host+"/asset/"+strconv.Itoa(i), http.NoBody)
					req.AddCookie(tb.cookie)
					res, err := browser.Do(req)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						p.refused = append(p.refused, err.Error())
						return
					}
					body, _ := io.ReadAll(res.Body)
					_ = res.Body.Close()
					if res.StatusCode == http.StatusOK {
						p.ok++
					} else {
						p.refused = append(p.refused, strconv.Itoa(res.StatusCode)+" "+strings.TrimSpace(string(body)))
					}
				})
			}
		}
		wg.Wait()
		return p
	}

	// The re-check's case: one app opened four times, a page each, where four
	// sessions kept four idle connections each.
	for i := range 4 {
		if p := load(open(ids[0])); p.ok != 6 {
			t.Fatalf("opening the app a %d. time, its page loaded %d of 6: %v", i+1, p.ok, p.refused)
		}
	}
	// Four apps, a page each: each keeps one idle connection, not four.
	for i, id := range ids {
		if p := load(open(id)); p.ok != 6 {
			t.Fatalf("app %d of four loaded %d of 6: %v", i+1, p.ok, p.refused)
		}
	}
	// Three apps opened again and busy at once ask for 18, past the 16 it carries.
	// Opening them again gave back their sessions' idle connections, so the agent
	// carries 15 at least, the first app's idle one aside, and refuses the rest, and
	// the browser hears that the server is full of the person's own tabs.
	<-slow
	slow <- time.Second
	p := load(open(ids[1]), open(ids[2]), open(ids[3]))
	if p.ok < 15 || len(p.refused) == 0 {
		t.Fatalf("three busy apps loaded %d of 18, with %d refused", p.ok, len(p.refused))
	}
	for _, why := range p.refused {
		if !strings.HasPrefix(why, "503 The server ") || !strings.Contains(why, "to other tabs of apps you opened through the panel too") {
			t.Errorf("a connection past the agent's 16 was refused with %q", why)
		}
	}
}
