// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// The engine an entry makes a database of, as New database groups them in one
// request (#723): a database alone, or with a companion tool, makes its engine; a tool,
// an app that brings a database of its own, and a file that can't be read make none.
func TestTheEngineAnEntryMakes(t *testing.T) {
	for _, c := range []struct {
		name  string
		entry *catalogv1.Entry
		want  catalogv1.Engine
	}{
		{"a database", &catalogv1.Entry{Id: "postgres", Compose: "services:\n  db:\n    image: postgres:17.11\n"}, catalogv1.Engine_ENGINE_POSTGRESQL},
		{"a database with its version an input", &catalogv1.Entry{
			Id: "mysql", Compose: "services:\n  db:\n    image: mysql:${VERSION}\n",
			Inputs: []*catalogv1.Input{{Name: "VERSION", DefaultValue: "8.4.11"}},
		}, catalogv1.Engine_ENGINE_MYSQL},
		{"a database and its companion tool", &catalogv1.Entry{
			Id: "postgres-admin", Compose: "services:\n  db:\n    image: postgres:17.11\n  adminer:\n    image: adminer:5\n",
			Companions: []*catalogv1.Companion{{Service: "adminer"}},
		}, catalogv1.Engine_ENGINE_POSTGRESQL},
		{"a tool", &catalogv1.Entry{Id: "adminer", Compose: "services:\n  adminer:\n    image: adminer:5\n"}, catalogv1.Engine_ENGINE_UNSPECIFIED},
		{"an app with a database of its own", &catalogv1.Entry{
			Id: "metabase", Compose: "services:\n  app:\n    image: metabase/metabase:0.56\n  db:\n    image: postgres:17\n",
		}, catalogv1.Engine_ENGINE_UNSPECIFIED},
		{"a file that can't be read", &catalogv1.Entry{Id: "broken", Compose: "services: ["}, catalogv1.Engine_ENGINE_UNSPECIFIED},
	} {
		if got := engineOf(t.Context(), c.entry); got != c.want {
			t.Errorf("%s makes %v, want %v", c.name, got, c.want)
		}
	}
}
