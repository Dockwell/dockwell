// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A service's ports are read on its server, by its agent: what its image exposes
// and what it publishes. Below an admin, only a developer of the service's project
// may ask, as the server checks: a developer of another project finds no such
// service, and a viewer of its own is refused. A server it doesn't run on is
// refused too, so no one has an agent ask on a server the service isn't on (#791).
func TestAServicesPortsAreReadOnItsServerForItsDevelopers(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		e.Registry["ghcr.io/acme/gitea:1.24"] = dockertest.Image{Config: docker.Config{ExposedPorts: map[string]struct{}{"3000/tcp": {}}}}
		c := changeClient(b, b.origin)
		edit := serviceEdit("gitea", "ghcr.io/acme/gitea:1.24", nil)
		edit.GetSpec().GetService().GetApp().Ports = []*resourcev1.PortMapping{{HostPort: 2222, ContainerPort: 22}}
		applied(t, c, propose(t, c, "Run gitea", edit, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Name: "data"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Data"}}},
		}))
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		gitea := serviceNamed(t, s, "gitea")
		ask := func(server string) (*dockwellv1.CheckServicePortsResponse, error) {
			t.Helper()
			res, err := s.CheckServicePorts(t.Context(), connect.NewRequest(&dockwellv1.CheckServicePortsRequest{Id: gitea.GetId(), ServerId: server}))
			if err != nil {
				return nil, err
			}
			return res.Msg, nil
		}

		got, err := ask("")
		if err != nil {
			t.Fatal(err)
		}
		ports := map[uint32]*dockwellv1.ServicePortCheck{}
		for _, p := range got.GetPorts() {
			ports[p.GetPort()] = p
		}
		if !ports[3000].GetExposed() || ports[22].GetServerPort() != 2222 {
			t.Errorf("CheckServicePorts = %v, want 3000 exposed and 22 on 2222", got)
		}
		if _, err := ask("srv_01M7MCWT7CPR404W2SNZY9AE0Y"); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("a server gitea doesn't run on: %v", err)
		}

		var own, data string
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			tree, err := scope.Load(t.Context(), tx)
			for _, p := range tree.Projects {
				if p.Name == "data" {
					data = p.ID
				}
				for _, env := range p.Environments {
					if env.ID == gitea.GetResource().GetScope() {
						own = p.ID
					}
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		hold := aMember(t, f)
		hold(store.Grant{ProjectID: data, Role: store.RoleDeveloper})
		if _, err := ask(""); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("a developer of another project: %v", err)
		}
		hold(store.Grant{ProjectID: own, Role: store.RoleViewer})
		if _, err := ask(""); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("a viewer of its project: %v", err)
		}
		hold(store.Grant{ProjectID: own, Role: store.RoleDeveloper})
		if _, err := ask(""); err != nil {
			t.Errorf("a developer of its project: %v", err)
		}
	})
}

// serviceNamed is the service of that name, as ListServices has it.
func serviceNamed(t *testing.T, s dockwellv1connect.ServiceServiceClient, name string) *dockwellv1.ServiceInfo {
	t.Helper()
	res, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range res.Msg.GetServices() {
		if info.GetResource().GetName() == name {
			return info
		}
	}
	t.Fatalf("no service %s", name)
	return nil
}
