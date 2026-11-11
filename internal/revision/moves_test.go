// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

func mongoAt(image string, external bool) *resourcev1.Service {
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image:  image,
		Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data/db", External: external}},
	}}}
}

func moving(from, to string, external bool) []item {
	return []item{{
		id: "svc_1", ref: resource.Ref{Kind: "service", Scope: "env_1", Name: "docs"},
		current: mongoAt(from, external), spec: mongoAt(to, external),
	}}
}

func reasons(notes []Note) []string {
	out := make([]string, len(notes))
	for i := range notes {
		out[i] = notes[i].Reason
	}
	return out
}

// A MongoDB moving to a release it can't come back from backs up its volume first,
// unless it's asked not to or its data isn't in a volume Dockwell made, and the plan
// says which. Moving back, the plan says what its last reading says of its data.
func TestADatabaseMoveBacksUpWhatItCantUndo(t *testing.T) {
	notes, backUp, err := databaseMoves(moving("mongo:7.0", "mongo:8.0", false), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []resource.Ref{{Kind: volumeKind, Scope: "env_1", Name: "data"}}; !slices.Equal(backUp, want) {
		t.Errorf("it backs up %v, want %v", backUp, want)
	}
	if !slices.Equal(reasons(notes), []string{"raises_features", "backup_first"}) || !strings.Contains(notes[1].Message, "backs up docs's volume data before it changes anything") {
		t.Errorf("notes = %+v", notes)
	}

	notes, backUp, _ = databaseMoves(moving("mongo:7.0", "mongo:8.0", false), true, nil)
	if len(backUp) != 0 || !slices.Equal(reasons(notes), []string{"raises_features", "no_backup"}) || !strings.Contains(notes[1].Message, "as you asked") {
		t.Errorf("asked for no backup: %v, %+v", backUp, notes)
	}

	notes, backUp, _ = databaseMoves(moving("mongo:7.0", "mongo:8.0", true), false, nil)
	if len(backUp) != 0 || !slices.Equal(reasons(notes), []string{"raises_features", "unbacked_move"}) || !strings.Contains(notes[1].Message, "Back it up yourself") {
		t.Errorf("an external volume: %v, %+v", backUp, notes)
	}

	// Back: with no reading, the plan says the agent asks; with one at newer
	// features, it says the move stops.
	read := map[string]string{}
	features := func(id string) string { return read[id] }
	notes, backUp, _ = databaseMoves(moving("mongo:8.0", "mongo:7.0", false), false, features)
	if len(backUp) != 0 || len(notes) != 1 || notes[0].Reason != "lowers_release" || !slices.Equal(notes[0].Names, []string{"8.0", "7.0"}) ||
		!strings.Contains(notes[0].Message, "only while its data uses no newer features") {
		t.Errorf("back with no reading: %v, %+v", backUp, notes)
	}
	read["svc_1"] = "7.0"
	if notes, _, _ = databaseMoves(moving("mongo:8.0", "mongo:7.0", false), false, features); len(notes[0].Names) != 2 {
		t.Errorf("back with data at 7.0: %+v", notes)
	}
	read["svc_1"] = "8.0"
	notes, _, _ = databaseMoves(moving("mongo:8.0", "mongo:7.0", false), false, features)
	if !slices.Equal(notes[0].Names, []string{"8.0", "7.0", "8.0"}) || !strings.Contains(notes[0].Message, "last reading says its data uses MongoDB 8.0's features, which 7.0 can't run") {
		t.Errorf("back with data at 8.0: %+v", notes)
	}
}
