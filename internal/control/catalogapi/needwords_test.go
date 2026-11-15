// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// A need a release adds says what meets it: the entry's own server, which other apps
// can then share where its image makes the app a user of its own, or a new one beside
// the app, which runs beside it.
func TestANewNeedSaysWhatMeetsIt(t *testing.T) {
	e := &catalogv1.Entry{Name: "Blog", Version: "6.72.0", Needs: []*catalogv1.Need{{Name: "db", Label: "Database", Service: "db"}}}
	n := e.GetNeeds()[0]
	for want, w := range map[string]catalog.Way{
		"Blog 6.72.0 takes a database as a need from this release, met by its own MySQL, which you can then share with other apps, each with a user of its own": {Need: n, Engine: catalogv1.Engine_ENGINE_MYSQL},
		"Blog 6.72.0 takes a database as a need from this release, met by its own PostgreSQL":                                                                   {Need: n, Engine: catalogv1.Engine_ENGINE_POSTGRESQL},
		"Blog 6.72.0 needs a database, which it didn't before, so a new MySQL 8.4 runs beside it":                                                               {Need: n, Engine: catalogv1.Engine_ENGINE_MYSQL, Version: "8.4", From: &catalogv1.Entry{Id: "mysql"}},
	} {
		if got := newNeedWords(e, "db", w); got != want {
			t.Errorf("got %q\nwant %q", got, want)
		}
	}
}
