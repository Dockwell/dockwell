// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// Each database an entry runs says its engine, whichever engine Dockwell knows its
// image as, MongoDB and Valkey too, and an app's own part says none (#599).
func TestAPartSaysItsEngine(t *testing.T) {
	e := &catalogv1.Entry{Id: "stack", Compose: "services:\n  web:\n    image: example/web:1.0\n  docs:\n    image: mongo:8.0\n  cache:\n    image: valkey/valkey:8.1\n  db:\n    image: postgres:17.11\n"}
	parts, err := partsOf(t.Context(), e, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]catalogv1.Engine{}
	for _, p := range parts {
		got[p.GetService()] = p.GetEngine()
		if p.GetDatabase() != (p.GetService() != "web") {
			t.Errorf("%s: database %t", p.GetService(), p.GetDatabase())
		}
	}
	want := map[string]catalogv1.Engine{
		"web":   catalogv1.Engine_ENGINE_UNSPECIFIED,
		"docs":  catalogv1.Engine_ENGINE_MONGODB,
		"cache": catalogv1.Engine_ENGINE_VALKEY,
		"db":    catalogv1.Engine_ENGINE_POSTGRESQL,
	}
	for service, engine := range want {
		if got[service] != engine {
			t.Errorf("%s says %v, want %v", service, got[service], engine)
		}
	}
}
