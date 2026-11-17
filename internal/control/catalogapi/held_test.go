// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// A store's converted app with web is held unless its entry says how it's owned:
// owner steps, an admin its environment makes, or none. A password everyone knows,
// which nothing changes, is no way. Dockwell's own entries are held only where their
// first visitor owns them (ADR 0105).
func TestAConvertedAppIsHeldUnlessItsEntrySaysHowItsOwned(t *testing.T) {
	t.Parallel()
	web := &catalogv1.Web{Service: "web"}
	steps := []*resourcev1.OwnerStep{{Step: &resourcev1.OwnerStep_Request{Request: &resourcev1.OwnerRequest{Path: "/setup", Port: 80}}}}
	for name, c := range map[string]struct {
		e         *catalogv1.Entry
		converted bool
		held      bool
	}{
		"converted, no owner way":           {e: &catalogv1.Entry{Web: web}, converted: true, held: true},
		"converted, no web":                 {e: &catalogv1.Entry{}, converted: true},
		"converted, an admin its env makes": {e: &catalogv1.Entry{Web: web, SignIn: &catalogv1.SignIn{User: "admin", PasswordInput: "APP_PASSWORD"}}, converted: true},
		"converted, owner steps":            {e: &catalogv1.Entry{Web: web, Owner: &catalogv1.Owner{Steps: steps}}, converted: true},
		"converted, none":                   {e: &catalogv1.Entry{Web: web, Owner: &catalogv1.Owner{None: true}}, converted: true},
		"converted, a known password":       {e: &catalogv1.Entry{Web: web, Owner: &catalogv1.Owner{KnownUser: "admin", KnownPassword: "changeme"}}, converted: true, held: true},
		"converted, its first visitor owns": {e: &catalogv1.Entry{Web: web, FirstVisitorOwns: true}, converted: true, held: true},
		"Dockwell's, no owner way":          {e: &catalogv1.Entry{Web: web}},
		"Dockwell's, its first visitor":     {e: &catalogv1.Entry{Web: web, FirstVisitorOwns: true}, held: true},
	} {
		if got := heldAtInstall(c.e, c.converted); got != c.held {
			t.Errorf("%s: held %v, want %v", name, got, c.held)
		}
	}
}
