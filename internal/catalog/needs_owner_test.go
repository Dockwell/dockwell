// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// An entry with a need keeps the inputs its owner names when the need is met, though
// its Compose file names none of them, so an install gives them and the steps read
// them, as Ghost's database need did not (ADR 0105; catalog#34's Proof).
func TestMeetingANeedKeepsTheOwnersInputs(t *testing.T) {
	e := proto.CloneOf(wordpressNeeds(t))
	e.FirstVisitorOwns = false
	e.Inputs = append(e.Inputs,
		&catalogv1.Input{Name: "OWNER_EMAIL", Label: "Owner's email", Type: catalogv1.InputType_INPUT_TYPE_EMAIL},
		&catalogv1.Input{Name: "OWNER_PASSWORD", Label: "Owner's password", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 24, GenerateAs: catalogv1.GenerateAs_GENERATE_AS_PASSWORD},
		&catalogv1.Input{Name: "SITE_TITLE", Label: "Title", Type: catalogv1.InputType_INPUT_TYPE_TEXT, DefaultValue: "Blog"},
	)
	e.Owner = &catalogv1.Owner{
		Service: "wordpress", EmailInput: "OWNER_EMAIL", PasswordInput: "OWNER_PASSWORD",
		Steps: []*resourcev1.OwnerStep{{Step: &resourcev1.OwnerStep_Request{Request: &resourcev1.OwnerRequest{
			Method: "POST", Path: "/setup", Body: `{"title":"${SITE_TITLE}"}`, Port: 80,
		}}}},
		Done: &resourcev1.OwnerCheck{Request: &resourcev1.OwnerRequest{Path: "/me", Port: 80}},
	}
	met, err := catalog.Meet(e, catalog.Ways(e, lookupBuiltIn(t)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := catalog.Need(e, met)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, in := range needed.Entry.GetInputs() {
		names = append(names, in.GetName())
	}
	for _, want := range []string{"OWNER_EMAIL", "OWNER_PASSWORD", "SITE_TITLE"} {
		if !slices.Contains(names, want) {
			t.Errorf("met, the entry takes %v, without %s", names, want)
		}
	}
	if _, err := catalog.Fill(needed.Entry, map[string]string{"OWNER_EMAIL": "owner@example.com"}, map[string]string{"PORT": "8080", "SITE_TITLE": "Blog"}); err != nil {
		t.Errorf("an install giving the owner's email: %v", err)
	}
}
