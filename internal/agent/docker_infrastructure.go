// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"slices"
	"strings"

	"github.com/dockwell/dockwell/internal/build"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/ingress/caddy"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// Keep both current pins and the infrastructure repositories. A previous
// version's BuildKit or Caddy image may still be needed after a downgrade.
var infrastructureRepositories = []string{
	caddy.Repository, "moby/buildkit", "ghcr.io/railwayapp/railpack-frontend",
	"cloudflare/cloudflared", "ghcr.io/cloudflare/cloudflared",
	docker.ProbeRepository, "dockwell-volume-tool",
}

var infrastructurePins = func() []string {
	pins := []string{
		build.Image, build.RailpackFrontend, caddy.Image,
		docker.WebsitePHP85Image, docker.WebsitePHP84Image, docker.WebsitePHP83Image, docker.WebsitePHP82Image,
	}
	for _, runtime := range service.Runtimes() {
		pins = append(pins, runtime.Image)
	}
	return pins
}()

func (c *calls) ownedImage(image docker.DiskUsageImage) bool {
	for label := range image.Labels {
		if strings.HasPrefix(label, docker.OwnPrefix) {
			return true
		}
	}
	if containsProtectedImage(image, infrastructurePins) ||
		(c.cfg.DockerIngressImage != "" && containsProtectedImage(image, []string{c.cfg.DockerIngressImage})) {
		return true
	}
	return slices.ContainsFunc(image.RepoTags, infrastructureRepository) || slices.ContainsFunc(image.RepoDigests, infrastructureRepository)
}

func infrastructureRepository(ref string) bool {
	repo := docker.Repository(ref)
	return repo == "dockwell" || strings.HasPrefix(repo, "dockwell/") ||
		strings.HasPrefix(repo, "dockwell-") || strings.HasPrefix(repo, "ghcr.io/dockwell/") ||
		slices.Contains(infrastructureRepositories, repo)
}

func (c *calls) infrastructureImageAlias(ref string) bool {
	return !strings.Contains(ref, "@") && (infrastructureRepository(ref) ||
		resource.DockerAliasProtected(ref, infrastructurePins) ||
		resource.DockerAliasProtected(ref, []string{c.cfg.DockerIngressImage}))
}
