// SPDX-License-Identifier: AGPL-3.0-only

package composeapi

import (
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/compose"
)

// What a plan says of a value in free text says where it is, in a person's words, and
// how to keep it hidden, which depends on whether what is there can name a value.
func TestExposedMessage(t *testing.T) {
	const anyone = ", so anyone who can read this change or export this environment can read it"
	const words = "Its value has a space, a quote or another character Compose reads as part of the line, so one name can't stand for it. To keep it hidden, write the "
	const whole = "Another value on that line is what one name can't stand for, and Dockwell couldn't keep the rest of the line apart from it, so the whole line is text. To keep it hidden, write the "
	const notThere = "It can't be hidden there, so if it is a secret, set it in environment for the app to read instead"
	for _, c := range []struct {
		e    compose.Exposed
		want string
	}{
		{
			compose.Exposed{Variable: "V", Kind: "service", Owner: "web", Field: "command"},
			"V from your .env is in web's command" + anyone + ". " + words + "command as a list, with each word its own variable",
		},
		{
			compose.Exposed{Variable: "V", Kind: "service", Owner: "web", Field: "entrypoint"},
			"V from your .env is in web's entrypoint" + anyone + ". " + words + "entrypoint as a list, with each word its own variable",
		},
		{
			compose.Exposed{Variable: "V", Kind: "service", Owner: "web", Field: "command", Whole: true},
			"V from your .env is in web's command" + anyone + ". " + whole + "command as a list, with each word its own variable",
		},
		{
			compose.Exposed{Variable: "V", Kind: "service", Owner: "web", Field: "labels"},
			"V from your .env is in web's labels" + anyone + ". " + notThere,
		},
		{
			compose.Exposed{Variable: "V", Kind: "service", Owner: "web", Field: "environment"},
			"V from your .env is in web's environment variable names" + anyone + ", as only a variable's value is hidden. Don't write a secret in a variable's name",
		},
		{
			compose.Exposed{Variable: "V", Kind: "service", Owner: "web", Field: "configs"},
			"V from your .env is in web's config files" + anyone + ". " + notThere,
		},
		{
			compose.Exposed{Variable: "V", Kind: "network", Owner: "back", Field: "labels"},
			"V from your .env is in the network back's labels" + anyone + ". " + notThere,
		},
		{
			compose.Exposed{Variable: "V", Kind: "network", Owner: "back", Field: "driver_opts"},
			"V from your .env is in the network back's driver options" + anyone + ". " + notThere,
		},
		{
			compose.Exposed{Variable: "V", Kind: "volume", Owner: "data", Field: "labels"},
			"V from your .env is in the volume data's labels" + anyone + ". " + notThere,
		},
		{
			compose.Exposed{Variable: "GIVEN", Kind: "service", Owner: "web", Field: "labels"},
			"GIVEN from the request's variables is in web's labels" + anyone + ". " + notThere,
		},
	} {
		if got := exposedMessage(c.e, map[string]string{"GIVEN": "x"}); got != c.want {
			t.Errorf("%+v:\n got %s\nwant %s", c.e, got, c.want)
		}
	}
	notes := exposedNotes("env_1", nil, true, nil)
	if len(notes) != 1 || notes[0].Reason != "exposed_more" || !strings.Contains(notes[0].Message, "more variables") {
		t.Errorf("more = %+v", notes)
	}
	if notes := exposedNotes("env_1", nil, false, nil); len(notes) != 0 {
		t.Errorf("nothing = %+v", notes)
	}
}
