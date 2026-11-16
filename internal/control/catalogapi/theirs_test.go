// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// A secret is the person's by the template the install's pin says Dockwell wrote it
// from; a pin with no word for it, as one from before pins kept them, reads a
// template as Dockwell's, and a value or a reference as the person's, as before
// (#570's check 2, H1).
func TestASecretIsThePersonsByThePinsRecord(t *testing.T) {
	dockwells := &resourcev1.ValueTemplate{Text: "postgres://u:${P}@db/x", Names: []*resourcev1.TemplateName{{Name: "P", Secret: true}}}
	other := &resourcev1.ValueTemplate{Text: "postgres://me:${P}@elsewhere/x", Names: []*resourcev1.TemplateName{{Name: "P", Secret: true}}}
	f := found{
		held: map[string]bool{"SAME": true, "OTHER": true, "PLAIN": true, "OLD": true},
		from: map[string]*resourcev1.ValueTemplate{"SAME": dockwells, "OTHER": other, "OLD": other},
	}
	pin := &resourcev1.CatalogPin{Written: map[string]*resourcev1.ValueTemplate{"SAME": dockwells, "OTHER": dockwells, "PLAIN": dockwells}}
	for name, want := range map[string]bool{"SAME": false, "OTHER": true, "PLAIN": true, "OLD": false, "GONE": false} {
		if got := f.theirs(name, pin); got != want {
			t.Errorf("%s reads as the person's: %v, want %v", name, got, want)
		}
	}
	if f.theirs("OTHER", &resourcev1.CatalogPin{}) {
		t.Error("with no record, a template reads as the person's")
	}
}
