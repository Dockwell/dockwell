// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// The inputs an owner's steps name are found as ${NAME}, $NAME and {{NAME}}, which a
// script a step runs fills from its environment, in steps, in a check that skips a
// step, and in done, so each is sent to the agent (the re-check's N5).
func TestOwnerInputsFindsEveryInputTheStepsName(t *testing.T) {
	t.Parallel()
	command := func(words ...string) *resourcev1.OwnerCommand { return &resourcev1.OwnerCommand{Words: words} }
	e := &catalogv1.Entry{
		Inputs: []*catalogv1.Input{{Name: "BLOG_TITLE"}, {Name: "ADMIN_PATH"}, {Name: "SITE_NAME"}, {Name: "UNUSED"}},
		Owner: &catalogv1.Owner{
			PasswordInput: "OWNER_PASSWORD",
			Steps: []*resourcev1.OwnerStep{{
				Step:   &resourcev1.OwnerStep_Command{Command: command("node", "-e", "script", `{"title":"{{BLOG_TITLE}}"}`)},
				Unless: &resourcev1.OwnerCheck{Request: &resourcev1.OwnerRequest{Path: "/${ADMIN_PATH}/users"}},
			}},
			Done: &resourcev1.OwnerCheck{Command: command("sh", "-c", `check "$SITE_NAME"`)},
		},
	}
	got := OwnerInputs(e)
	for _, name := range []string{"BLOG_TITLE", "ADMIN_PATH", "SITE_NAME", "OWNER_PASSWORD"} {
		if !got[name] {
			t.Errorf("OwnerInputs misses %s: %v", name, got)
		}
	}
	if got["UNUSED"] {
		t.Error("OwnerInputs takes an input no step names")
	}
}
