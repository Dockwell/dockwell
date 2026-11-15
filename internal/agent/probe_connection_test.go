// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"slices"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// A check runs on the networks an app is on, but never on the ingress network,
// whose addresses are the ingress's to give, nor on a database copy's: an address
// Docker picked there could be one the ingress holds for another project's
// stopped service (ADR 0024). An app on the host's network has none.
func TestACheckLeavesOutTheIngressAndCopyNetworks(t *testing.T) {
	t.Parallel()
	e := dockertest.New(t)
	project := docker.Project("env_shop")
	app := map[string]string{docker.LabelProject: project, docker.LabelService: "shop"}
	on := func(id string, labels map[string]string, networks map[string]*docker.EndpointSettings) {
		c := docker.Container{ID: id, Name: "/" + id, Config: docker.Config{Labels: labels}}
		c.NetworkSettings.Networks = networks
		e.AddContainer(c)
	}
	on("shop-1", app, map[string]*docker.EndpointSettings{
		project + "_default":            {IPAddress: "172.20.0.2"},
		docker.IngressNetwork:           {IPAddress: "10.200.0.7"},
		docker.CopyPrefix + "shop-db-1": {IPAddress: "172.21.0.3"},
	})
	on("shop-2", app, map[string]*docker.EndpointSettings{
		project + "_default": {IPAddress: "172.20.0.3"},
		"links_media":        {IPAddress: "172.22.0.2"},
	})
	on("edge-1", map[string]string{docker.LabelProject: project, docker.LabelService: "edge"}, map[string]*docker.EndpointSettings{"host": {}})
	c := &calls{cfg: Config{Docker: e.Client()}}

	got, err := c.appNetworks(t.Context(), "env_shop", "shop")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{project + "_default", "links_media"}; !slices.Equal(got, want) {
		t.Errorf("a check of shop runs on %v, want %v", got, want)
	}
	if got, err := c.appNetworks(t.Context(), "env_shop", "edge"); err != nil || got != nil {
		t.Errorf("a check of an app on the host's network runs on %v, %v", got, err)
	}
}
