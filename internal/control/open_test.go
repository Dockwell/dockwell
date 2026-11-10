// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// An app that answers only on its server opens through the panel (ADR 0061): the
// panel gets a one-time address on an origin of the app's own, a browser that opens
// it gets a cookie of that origin, and what it asks for reaches the app through the
// server's agent, which dials the port the service publishes. It goes on the audit
// log, and a panel on its domain is told it opens apps on localhost for now.
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
	res, err := services.OpenService(t.Context(), connect.NewRequest(&dockwellv1.OpenServiceRequest{Id: "phpmyadmin"}))
	if err != nil {
		t.Fatal(err)
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
	}
	if err := json.Unmarshal([]byte(told), &detail); err != nil || detail.Service != "phpmyadmin" || detail.Port != 80 || detail.Origin != origin {
		t.Errorf("the audit log says %q", told)
	}

	_, err = services.OpenService(t.Context(), connect.NewRequest(&dockwellv1.OpenServiceRequest{Id: "phpmyadmin", Port: 443}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "publishes no port for 443") {
		t.Errorf("a port it doesn't publish: %v", err)
	}
}
