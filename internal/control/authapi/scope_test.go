// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/store"
)

// placeable reports an element the filter can place: by its change, its own ID,
// its environment, its resource's scope, what it's about, or its scope, as
// elementIDs reads them.
func placeable(md protoreflect.MessageDescriptor) bool {
	f := md.Fields()
	str := func(d protoreflect.MessageDescriptor, name string) bool {
		fd := d.Fields().ByName(protoreflect.Name(name))
		return fd != nil && fd.Kind() == protoreflect.StringKind && !fd.IsList()
	}
	msg := func(name, inner string) bool {
		fd := f.ByName(protoreflect.Name(name))
		return fd != nil && fd.Kind() == protoreflect.MessageKind && !fd.IsList() && str(fd.Message(), inner)
	}
	return msg("change", "id") || str(md, "id") || str(md, "environment_id") || msg("resource", "scope") || msg("subject", "id") || str(md, "scope")
}

// A call of a project's whose request takes a project beside its id takes a name
// in that id too, which the scope check reads as one only for a call idNames
// lists: one it doesn't would name no place by name, and pass with nothing
// checked, as OpenService once did.
func TestEveryIDThatTakesANameIsReadAsOne(t *testing.T) {
	checked := 0
	protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				procedure := "/" + string(sd.FullName()) + "/" + string(md.Name())
				if roleRules[procedure].reach != dockwellv1.Reach_REACH_PROJECT {
					continue
				}
				in := md.Input().Fields()
				id, project := in.ByName("id"), in.ByName("project")
				if id == nil || project == nil || id.Kind() != protoreflect.StringKind || id.IsList() {
					continue
				}
				checked++
				if idNames[procedure] == "" {
					t.Errorf("%s takes a project beside its id, so its id may be a name, but idNames doesn't say what it names. Add it there", procedure)
				}
			}
		}
		return true
	})
	if checked < 5 {
		t.Errorf("checked %d calls, which says the walk missed some", checked)
	}
}

// A call on a catalog install names its project by name or ID in its id, so the
// scope check reads each as a project's name: one it didn't would pass a name
// unchecked, and what it answers would say what another project holds, as
// OpenCatalogInstall's not_held once could.
func TestEveryCatalogInstallCallReadsItsIDAsAProject(t *testing.T) {
	checked := 0
	sd, err := protoregistry.GlobalFiles.FindDescriptorByName("dockwell.v1.CatalogService")
	if err != nil {
		t.Fatal(err)
	}
	methods := sd.(protoreflect.ServiceDescriptor).Methods()
	for j := range methods.Len() {
		md := methods.Get(j)
		procedure := "/dockwell.v1.CatalogService/" + string(md.Name())
		if roleRules[procedure].reach != dockwellv1.Reach_REACH_PROJECT || md.Input().Fields().ByName("id") == nil {
			continue
		}
		checked++
		if idNames[procedure] != "project" {
			t.Errorf("%s names an install's project in its id, and idNames reads it as %q. Add it there as a project", procedure, idNames[procedure])
		}
	}
	if checked < 4 {
		t.Errorf("checked %d calls, which says the walk missed some", checked)
	}
}

// Every list and stream of a project's objects holds what the filter can place,
// so a project role's narrowed list leaves out only what isn't theirs, never all
// of it, and a new list can't skip the filter by holding nothing it reads. This
// checks the shape; TestEveryProjectsListReadsWhole in internal/control checks
// what the lists hold, as a viewer on the install reads them whole.
func TestEveryListOfAProjectsObjectsCanBeNarrowed(t *testing.T) {
	checked := 0
	protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				procedure := "/" + string(sd.FullName()) + "/" + string(md.Name())
				if roleRules[procedure].reach != dockwellv1.Reach_REACH_PROJECT || requestBound[procedure] {
					continue // not narrowed: the response is about what the request named
				}
				out := md.Output()
				for k := range out.Fields().Len() {
					f := out.Fields().Get(k)
					if f.Kind() != protoreflect.MessageKind || f.IsMap() {
						continue
					}
					if !f.IsList() && !strings.HasPrefix(string(md.Name()), "Watch") {
						continue
					}
					checked++
					if !placeable(f.Message()) && !aboutTheRequest[f.Message().FullName()] {
						t.Errorf("%s: %s.%s holds %s, which the filter can't place. Give it an id, an environment_id, a resource, a subject or a change",
							procedure, out.Name(), f.Name(), f.Message().FullName())
					}
				}
			}
		}
		return true
	})
	if checked < 10 {
		t.Errorf("checked %d lists, which says the walk missed some", checked)
	}
}

// A domain with routes in several environments keeps, for a project role, its
// routes in their places alone, under the environment of the first; with none
// left, it's left out.
func TestADomainKeepsTheRoutesInReach(t *testing.T) {
	envs := map[string]string{"rte_01JD6Z3R8M9QF2K7T4XW5BNCAA": "env_01JD6Z3R8M9QF2K7T4XW5BNCA1", "rte_01JD6Z3R8M9QF2K7T4XW5BNCBB": "env_01JD6Z3R8M9QF2K7T4XW5BNCB1"}
	placeOf := func(objectID string) ([]place.Place, error) {
		return []place.Place{{Project: "prj_x", Environment: envs[objectID]}}, nil
	}
	only := func(id string) func(string) (bool, error) {
		return func(objectID string) (bool, error) { return objectID == id, nil }
	}
	domain := func() *dockwellv1.Domain {
		return &dockwellv1.Domain{Name: "shop.example.com", EnvironmentId: envs["rte_01JD6Z3R8M9QF2K7T4XW5BNCAA"], Routes: []*dockwellv1.DomainRoute{
			{Id: "rte_01JD6Z3R8M9QF2K7T4XW5BNCAA", Service: "web"}, {Id: "rte_01JD6Z3R8M9QF2K7T4XW5BNCBB", Service: "api"},
		}}
	}
	d := domain()
	kept, err := narrowDomain(d.ProtoReflect(), only("rte_01JD6Z3R8M9QF2K7T4XW5BNCBB"), placeOf)
	if err != nil || !kept || len(d.GetRoutes()) != 1 || d.GetRoutes()[0].GetService() != "api" || d.GetEnvironmentId() != envs["rte_01JD6Z3R8M9QF2K7T4XW5BNCBB"] {
		t.Errorf("narrowed to api's route: kept %v, %v, %v", kept, d, err)
	}
	d = domain()
	if kept, err := narrowDomain(d.ProtoReflect(), only("rte_none"), placeOf); kept || err != nil {
		t.Errorf("with no route in reach the domain is kept: %v, %v", d, err)
	}
}

// Without the caller's grants, nothing a project holds is in reach: the check
// refuses, and lists narrow to nothing.
func TestScopeFailsClosedWithoutGrants(t *testing.T) {
	i, ada, _ := gated(t, store.RoleViewer)
	ctx := context.WithValue(t.Context(), principalKey{}, ada)
	err := i.checkScope(ctx, dockwellv1connect.ServiceServiceListServicesProcedure, &dockwellv1.ListServicesRequest{})
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("checking with no grants read: %v, want unavailable", err)
	}
	res := &dockwellv1.ListServicesResponse{Services: []*dockwellv1.ServiceInfo{{Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: "env_01JD6Z3R8M9QF2K7T4XW5BNCA1", Name: "web"}}}}
	if _, err := i.filter(ctx, dockwellv1connect.ServiceServiceListServicesProcedure, res); err != nil || len(res.GetServices()) != 0 {
		t.Errorf("narrowing with no grants read kept %v, %v", res.GetServices(), err)
	}
}

// A question's field is set by its path, as handlers name it.
func TestSetPath(t *testing.T) {
	req := &dockwellv1.ProposeChangeRequest{Edits: []*dockwellv1.Edit{{Resource: &dockwellv1.ResourceRef{}}, {Resource: &dockwellv1.ResourceRef{}}}}
	if !setPath(req.ProtoReflect(), "edits[1].resource.scope", "prj_01JD6Z3R8M9QF2K7T4XW5BNCA1") || req.GetEdits()[1].GetResource().GetScope() != "prj_01JD6Z3R8M9QF2K7T4XW5BNCA1" {
		t.Errorf("edits[1].resource.scope: %v", req)
	}
	for _, path := range []string{"edits[2].resource.scope", "edits.resource", "nothing", "edits[0]", "summary.x"} {
		if setPath(req.ProtoReflect(), path, "x") {
			t.Errorf("%s was set", path)
		}
	}
}

// A database's password changes by its service's name too, which the scope check
// reads as a name, so it looks for it in the caller's projects alone.
func TestADatabaseNamedForANewPasswordIsReadAsAName(t *testing.T) {
	m := (&dockwellv1.ChangeDatabasePasswordRequest{Id: "db"}).ProtoReflect()
	ref, _ := requested(dockwellv1connect.DatabaseServiceChangeDatabasePasswordProcedure, m)
	if ref.named != "db" || len(ref.ids) != 0 {
		t.Errorf("ChangeDatabasePassword{id: db} names %+v, want the service named db", ref)
	}
}
