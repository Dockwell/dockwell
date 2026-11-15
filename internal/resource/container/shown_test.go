// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"slices"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// A value whose name says nothing of a secret, that's a whole part of a name a plan
// shows anyway, its aliases, networks, image or hostname, shows nothing more there.
// A secret with the same value still counts, as does a value that's only part of a
// word, and any value in any other field (the gaps survey of #604).
func TestAValueThatIsPartOfANameShowsNothingMore(t *testing.T) {
	for name, c := range map[string]struct {
		app   *resourcev1.Container
		shows []string
	}{
		"a database named as its network": {
			app: &resourcev1.Container{ExternalNetworks: []string{"activepieces-k2m4ab_default"}, Env: map[string]string{"POSTGRES_DB": "activepieces"}},
		},
		"a user named as its image": {
			app: &resourcev1.Container{Image: "ghcr.io/activepieces/activepieces:0.70", Env: map[string]string{"AP_DB_USER": "activepieces"}},
		},
		"a database named as its alias": {
			app: &resourcev1.Container{Networks: []string{"default"}, Aliases: []string{"castopod"}, Env: map[string]string{"MYSQL_DATABASE": "castopod"}},
		},
		"each bound": {
			app: &resourcev1.Container{
				Image: "registry.example.com:5000/team/shopapp@sha256:abc", Hostname: "shopapp.internal",
				ExternalNetworks: []string{"net.shopapp", "shopapp"}, Aliases: []string{"x-shopapp-y"},
				Env: map[string]string{"APP_NAME": "shopapp"},
			},
		},
		"a secret with that value": {
			app:   &resourcev1.Container{ExternalNetworks: []string{"forgejo-cdiehf_default"}, Image: "forgejo:11", Env: map[string]string{"POSTGRES_PASSWORD": "forgejo"}},
			shows: []string{"the value of POSTGRES_PASSWORD in its external networks", "the value of POSTGRES_PASSWORD in its image"},
		},
		"only part of a word": {
			app:   &resourcev1.Container{ExternalNetworks: []string{"activepieces_default"}, Env: map[string]string{"MODE": "active"}},
			shows: []string{"the value of MODE in its external networks"},
		},
		"another field": {
			app:   &resourcev1.Container{Image: "shopapp:1", Command: []string{"serve", "--db", "shopapp"}, Env: map[string]string{"DB_NAME": "shopapp"}},
			shows: []string{"the value of DB_NAME in its command"},
		},
		"a whole part in a label": {
			app:   &resourcev1.Container{Labels: map[string]string{"app.role": "shopapp-web"}, Env: map[string]string{"APP_NAME": "shopapp"}},
			shows: []string{"the value of APP_NAME in its label app.role"},
		},
	} {
		if got := valuesShown(c.app); !slices.Equal(got, c.shows) {
			t.Errorf("%s shows %q, want %q", name, got, c.shows)
		}
	}
	for word, want := range map[string]bool{
		"shop": true, "shop_default": true, "my-shop": true, "a.shop.b": true, "img/shop:1": true, "shop@sha256": true,
		"shopping": false, "workshop": false, "shop2": false, "shopshop": false, "x-shopping-shop": true,
	} {
		if got := wholePart(word, "shop"); got != want {
			t.Errorf("wholePart(%q, shop) = %v", word, got)
		}
	}
}
