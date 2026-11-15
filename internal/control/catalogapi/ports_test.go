// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"strings"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// A server that lends a range of its own, set narrower than Linux's, keeps a default
// in it out, says so with that range, and leaves one outside it alone (#688).
func TestADefaultPortStaysOutOfTheServersOwnLentRange(t *testing.T) {
	lent := lentPorts{{49152, 65535}}
	e := &catalogv1.Entry{Inputs: []*catalogv1.Input{
		{Name: "WEB", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "50000"},
		{Name: "API", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "40108"},
	}}
	got := catalog.Suggest(e, nil, lent.has)
	if got["WEB"] != "8080" || got["API"] != "40108" {
		t.Errorf("Suggest = %v", got)
	}
	notes := lentNotes(e, got, lent)
	if len(notes) != 1 || !strings.Contains(notes[0].Message, "on 8080 in place of its default 50000") || !strings.Contains(notes[0].Message, "(49152 to 65535)") {
		t.Errorf("notes %v", notes)
	}
	// A port a person typed in the range is theirs: no default was replaced.
	if notes := lentNotes(e, map[string]string{"WEB": "50000", "API": "40108"}, lent); len(notes) != 0 {
		t.Errorf("a port given says %v", notes)
	}
}
