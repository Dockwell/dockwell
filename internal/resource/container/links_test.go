// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
)

// A link reads from HostConfig, as "/target:/self/alias", and from a network's
// endpoint, as "target:alias", each once, and a name Compose links by is one the
// linked container answers to.
func TestWhatALinkSays(t *testing.T) {
	o := &docker.Options{HostConfig: map[string]json.RawMessage{"Links": json.RawMessage(`["/shop-db-1:/shop-web-1/database"]`)}}
	o.NetworkSettings.Networks = map[string]map[string]json.RawMessage{
		"shop_default": {"Links": json.RawMessage(`["shop-db-1:database", "shop-db-1:db-1", "cache"]`)},
	}
	got := linksOf(o)
	want := []link{{"shop-db-1", "database"}, {"shop-db-1", "db-1"}, {"cache", "cache"}}
	if !slices.Equal(got, want) {
		t.Errorf("linksOf = %v, want %v", got, want)
	}
	db := &docker.Container{Name: "/shop-db-1"}
	db.Config.Labels = map[string]string{docker.LabelService: "db", docker.LabelProject: "shop", docker.LabelLinkedAs: "storage"}
	db.NetworkSettings.Networks = map[string]*docker.EndpointSettings{"shop_default": {Aliases: []string{"db", "primary"}}}
	for name, want := range map[string]bool{
		"shop-db-1": true, "db": true, "db-1": true, "shop-db-2": true, "primary": true, "storage": true,
		"database": false, "web-1": false,
	} {
		if got := linkedAs(db, "shop_default", name); got != want {
			t.Errorf("linkedAs %s = %v", name, got)
		}
	}
}
