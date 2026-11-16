// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"strings"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// A plan is refused where the file, as Compose reads it, publishes a port past the
// server that the entry doesn't declare public, whatever form put it there: an
// alias, a merge key, a whole port from a default. A port on 127.0.0.1, and one
// declared public on every address, pass (#752's review, H1).
func TestAPortPastTheServerItDoesntDeclareIsRefused(t *testing.T) {
	entry := func(file string, public bool) *catalogv1.Entry {
		return &catalogv1.Entry{Id: "e", Compose: file, Inputs: []*catalogv1.Input{
			{Name: "HTTP_PORT", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "18180", Public: public},
		}}
	}
	values := map[string]string{"HTTP_PORT": "18180"}
	for name, file := range map[string]string{
		"an alias for the list":         "x-ports: &ports\n  - \"18130:5432\"\nservices:\n  db:\n    image: postgres:17\n    ports: *ports\n",
		"a merge key that brings ports": "x-base: &base\n  ports:\n    - \"18131:5432\"\nservices:\n  db:\n    <<: *base\n    image: postgres:17\n",
		"a whole port from a default":   "services:\n  db:\n    image: postgres:17\n    ports:\n      - \"${SPEC:-18123:83}\"\n",
		"an input it doesn't declare":   "services:\n  db:\n    image: postgres:17\n    ports:\n      - \"${HTTP_PORT}:80\"\n",
	} {
		err := serverAlone(t.Context(), entry(file, false), values)
		if err == nil || !strings.Contains(err.Error(), "would answer past this server, which its catalog entry doesn't declare") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		file   string
		public bool
	}{
		"on the server alone": {"services:\n  db:\n    image: postgres:17\n    ports:\n      - \"127.0.0.1:${HTTP_PORT}:80\"\n", false},
		"declared public":     {"services:\n  db:\n    image: postgres:17\n    ports:\n      - \"${HTTP_PORT}:80\"\n", true},
		"nothing published":   {"services:\n  db:\n    image: postgres:17\n", false},
	} {
		if err := serverAlone(t.Context(), entry(tc.file, tc.public), values); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
