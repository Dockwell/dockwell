// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
)

// A link reads from HostConfig, as "/target:/self/alias", and from a network's
// endpoint, as "target:alias", each once. A link by a name the linked container
// answers to on the link's network needs nothing; a numbered name Compose links by,
// as db-1, isn't one, as nothing answers to it after a replace (the review of ADR
// 0097, L5), and a name others linked by holds only on its own network (L4).
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
	db.Config.Labels = map[string]string{docker.LabelService: "db", docker.LabelProject: "shop", docker.LabelLinkedAs: "shop_default=storage,shop_back=archive"}
	db.NetworkSettings.Networks = map[string]*docker.EndpointSettings{"shop_default": {Aliases: []string{"db", "primary"}}}
	for name, want := range map[string]bool{
		"shop-db-1": true, "db": true, "primary": true, "storage": true,
		"db-1": false, "shop-db-2": false, "archive": false, "database": false, "web-1": false,
	} {
		if got := linkedAs(db, "shop_default", name); got != want {
			t.Errorf("linkedAs %s = %v", name, got)
		}
	}
}
