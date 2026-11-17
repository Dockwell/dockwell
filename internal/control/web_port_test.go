// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// ListServices says which port serves each app's pages, as Add a domain starts
// from it (#791): Gitea, which lists SSH's 22 first, is on 3000, its health
// check's port; an app whose health check names a port it doesn't publish is on
// that port; and once a route sends an app to a port, that's the one.
func TestListServicesSaysWhichPortServesAnAppsPages(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
		f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
		c := changeClient(b, b.origin)
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())

		gitea := serviceEdit("gitea", "ghcr.io/acme/shop:1.4", nil)
		gitea.GetSpec().GetService().GetApp().Ports = []*resourcev1.PortMapping{
			{HostPort: 2222, ContainerPort: 22, HostIp: "127.0.0.1"},
			{HostPort: 3000, ContainerPort: 3000, HostIp: "127.0.0.1"},
		}
		gitea.GetSpec().GetService().GetApp().Healthcheck = &resourcev1.Healthcheck{
			Test: []string{"CMD", "curl", "-fsS", "--max-time", "4", "http://127.0.0.1:3000/api/healthz"},
		}
		checked := serviceEdit("checked", "ghcr.io/acme/shop:1.4", nil)
		checked.GetSpec().GetService().GetApp().Ports = []*resourcev1.PortMapping{{HostPort: 8080, ContainerPort: 8080, HostIp: "127.0.0.1"}}
		checked.GetSpec().GetService().GetApp().Healthcheck = &resourcev1.Healthcheck{
			Test: []string{"CMD-SHELL", "wget -qO- http://localhost:9090/health || exit 1"},
		}
		routed := serviceEdit("routed", "ghcr.io/acme/worker:1.0", nil)
		routed.GetSpec().GetService().GetApp().Ports = []*resourcev1.PortMapping{
			{HostPort: 8000, ContainerPort: 8000, HostIp: "127.0.0.1"},
			{HostPort: 8001, ContainerPort: 8001, HostIp: "127.0.0.1"},
		}
		applied(t, c, propose(t, c, "Three apps", gitea, checked, routed))

		ports := func() map[string]uint32 {
			t.Helper()
			res, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]uint32{}
			for _, info := range res.Msg.GetServices() {
				out[info.GetResource().GetName()] = info.GetWebPort()
			}
			return out
		}
		got := ports()
		if got["gitea"] != 3000 {
			t.Errorf("Gitea's web port is %d, want 3000, its health check's, never SSH's 22", got["gitea"])
		}
		if got["checked"] != 9090 {
			t.Errorf("the checked app's web port is %d, want 9090, the port its health check asks", got["checked"])
		}
		if got["routed"] != 8000 {
			t.Errorf("before its route, the routed app's web port is %d, want 8000, its first", got["routed"])
		}

		res, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		env := res.Msg.GetServices()[0].GetResource().GetScope()
		applied(t, c, propose(t, c, "A domain", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: "admin"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "admin.example.com", Service: "routed", Port: 8001}}},
		}))
		if got := ports()["routed"]; got != 8001 {
			t.Errorf("once a route sends it to 8001, its web port is %d, want 8001", got)
		}
	})
}
