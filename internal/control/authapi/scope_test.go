// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
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

// Every list and stream of a project's objects holds what the filter can place,
// so a project role's narrowed list leaves out only what isn't theirs, never all
// of it, and a new list can't skip the filter by holding nothing it reads.
func TestEveryListOfAProjectsObjectsCanBeNarrowed(t *testing.T) {
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
