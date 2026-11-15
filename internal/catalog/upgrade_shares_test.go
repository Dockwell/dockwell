// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// A server another app's install keeps in its own file names the upgrade that lets it
// be shared only where the catalog has one: a newer release of that app whose need
// that service meets, of the engine it runs. Otherwise it reads as before (ADR 0084).
func TestAServerAnOlderReleaseKeepsNamesTheUpgrade(t *testing.T) {
	need := &catalogv1.Need{Name: "db", Accepts: []*catalogv1.NeedAccept{{Engine: catalogv1.Engine_ENGINE_MYSQL, Versions: ">=8.0"}}}
	ghost := func(release uint32, service string, engine catalogv1.Engine) Lookup {
		return func(id string) (*catalogv1.Entry, string, bool) {
			if id != "ghost" {
				return nil, "", false
			}
			return &catalogv1.Entry{Id: "ghost", Name: "Ghost", Release: release, Needs: []*catalogv1.Need{{
				Name: "db", Service: service, Accepts: []*catalogv1.NeedAccept{{Engine: engine}},
			}}}, "dockwell", true
		}
	}
	server := Candidate{Service: "db", Engine: catalogv1.Engine_ENGINE_MYSQL, Version: "8.4.11", Servers: []string{"srv_1"}, JoinsInPlace: true, PartOf: "ghost", PartOfRelease: 4}
	for _, tc := range []struct {
		name    string
		lookup  Lookup
		why     string
		release uint32
	}{
		{"a newer release whose need its service meets", ghost(5, "db", catalogv1.Engine_ENGINE_MYSQL), "Ghost's release 4 keeps it in its own install", 5},
		{"no newer release", ghost(4, "db", catalogv1.Engine_ENGINE_MYSQL), "Part of ghost's install, whose upgrades would change it", 0},
		{"a newer release met by another service", ghost(5, "mysql", catalogv1.Engine_ENGINE_MYSQL), "Part of ghost's install, whose upgrades would change it", 0},
		{"a newer release of another engine", ghost(5, "db", catalogv1.Engine_ENGINE_POSTGRESQL), "Part of ghost's install, whose upgrades would change it", 0},
		{"no catalog to look in", nil, "Part of ghost's install, whose upgrades would change it", 0},
	} {
		got := existing(need, "WordPress", []Candidate{server}, "", tc.lookup)
		if len(got) != 1 || got[0].Why != tc.why || got[0].UpgradeRelease != tc.release {
			t.Errorf("with %s, the server reads %q, upgrading to %d", tc.name, got[0].Why, got[0].UpgradeRelease)
		}
	}
}
