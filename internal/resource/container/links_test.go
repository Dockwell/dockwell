// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
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

// A link's name goes on a network of the linked container's own Compose project,
// then the linking container's main network, and only then the first by name
// (the narrow check of ADR 0097, N1).
func TestALinksNetworkIsTheProjectsOwn(t *testing.T) {
	e := dockertest.New(t)
	e.AddNetwork(docker.Network{ID: "panel", Name: "coolify", Driver: "bridge"})
	e.AddNetwork(docker.Network{ID: "own", Name: "shop_default", Driver: "bridge", Labels: map[string]string{docker.LabelProject: "shop"}})
	e.AddNetwork(docker.Network{ID: "main", Name: "shop_main", Driver: "bridge"})
	d := &Docker{Client: e.Client()}
	for name, c := range map[string]struct {
		shared        []string
		project, mode string
		want          string
	}{
		"the project's own":   {[]string{"coolify", "shop_default", "shop_main"}, "shop", "shop_main", "shop_default"},
		"the main network":    {[]string{"coolify", "shop_main"}, "shop", "shop_main", "shop_main"},
		"no project":          {[]string{"coolify", "shop_default"}, "", "shop_default", "shop_default"},
		"the first, at worst": {[]string{"coolify", "shop_main"}, "", "", "coolify"},
		"none":                {nil, "shop", "shop_main", ""},
	} {
		if got, err := d.linkNetwork(t.Context(), c.shared, c.project, c.mode); err != nil || got != c.want {
			t.Errorf("%s: %q, %v, want %q", name, got, err, c.want)
		}
	}
}
