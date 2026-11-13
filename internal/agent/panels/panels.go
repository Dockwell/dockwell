// SPDX-License-Identifier: AGPL-3.0-only

// Package panels reads what Coolify or Dokploy runs on a server, for an import:
// from what Docker runs, the labels the panel gave it, and the files in its
// folders, read-only, never its database or its API. What it returns is what a
// plan shows: names, kinds, Compose projects, containers, images, domains and
// paths, never an environment, a label's value beyond a domain, or a file's
// contents. See docs/adr/0089-importing-from-coolify-and-dokploy.md.
package panels

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// Reader reads a panel on this server.
type Reader struct {
	Docker *docker.Client
	// Root is the server's root, "/", or in a test a folder laid out as one. Empty
	// reads no folder: what Docker says is read alone.
	Root string
}

// Read reads what the panel runs, or says why there's nothing to read.
func (r Reader) Read(ctx context.Context, kind dockwellv1.PanelKind) (*agentv1.ReadPanelResult, error) {
	if r.Docker == nil {
		return nil, errors.New("this agent reads no Docker, so it can't read what a panel runs")
	}
	list, err := r.Docker.ListContainers(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("can't list the containers Docker runs: %w", err)
	}
	switch kind {
	case dockwellv1.PanelKind_PANEL_KIND_COOLIFY:
		return r.coolify(list), nil
	case dockwellv1.PanelKind_PANEL_KIND_DOKPLOY:
		services, err := r.Docker.ListServices(ctx)
		if err != nil {
			return nil, fmt.Errorf("can't list the Swarm services Docker runs: %w", err)
		}
		return r.dokploy(list, services), nil
	default:
		return nil, fmt.Errorf("there's no panel %v to read: name Coolify or Dokploy", kind)
	}
}

// What each panel's own containers are called, as its installer names them.
var (
	coolifyOwn = []string{"coolify", "coolify-db", "coolify-redis", "coolify-realtime", "coolify-proxy", "coolify-sentinel"}
	dokployOwn = []string{"dokploy", "dokploy-postgres", "dokploy-redis", "dokploy-traefik"}
)

// The folders each panel keeps what it runs in.
const (
	coolifyFolder = "/data/coolify"
	dokployFolder = "/etc/dokploy"
)

// The versions this reader was written from (ADR 0089). Each pattern here is
// compiled on first use, as the agent starts with little.
var (
	coolifyKnown = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^v?4\.7(\.|$)`) })
	dokployKnown = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^v?0\.31(\.|$)`) })
)

// Labels Docker Compose and the panels give what they run.
const (
	labelProject    = "com.docker.compose.project"
	labelWorkingDir = "com.docker.compose.project.working_dir"
	labelStack      = "com.docker.stack.namespace"
)

func (r Reader) coolify(list []docker.ContainerSummary) *agentv1.ReadPanelResult {
	p := &dockwellv1.Panel{Kind: dockwellv1.PanelKind_PANEL_KIND_COOLIFY, Folder: coolifyFolder}
	type key struct{ kind, id string }
	apps := map[key]*dockwellv1.PanelApp{}
	for i := range list {
		c := &list[i]
		name := containerName(c)
		if slices.Contains(coolifyOwn, name) {
			p.Own = append(p.Own, &dockwellv1.PanelContainer{Name: name, Image: c.Image, State: c.State})
			if name == "coolify" {
				p.Version = tagOf(c.Image)
			}
			continue
		}
		l := c.Labels
		if l["coolify.managed"] != "true" {
			continue
		}
		kind := cmp.Or(l["coolify.type"], "application")
		id := cmp.Or(l["coolify."+kind+"Uuid"], l[labelProject], name)
		k := key{kind, id}
		a := apps[k]
		if a == nil {
			a = &dockwellv1.PanelApp{
				Name: cmp.Or(l["coolify.resourceName"], l["coolify.name"], id), Kind: kind,
				Project: l["coolify.projectName"], Environment: l["coolify.environmentName"], Id: id,
				ComposeProject: l[labelProject], Preview: preview(l["coolify.pullRequestId"]),
				Folder: r.folder(path.Join(coolifyFolder, kind+"s", id)),
			}
			if kind == "database" && !strings.HasPrefix(c.Image, "sha256:") {
				a.Image = c.Image
			}
			apps[k] = a
		}
		a.Containers = append(a.Containers, name)
		a.Domains = append(a.Domains, domainsOf(l)...)
	}
	p.Apps = sorted(apps)
	p.KnownVersion = coolifyKnown().MatchString(p.Version)
	if len(p.Own) == 0 && len(p.Apps) == 0 {
		return &agentv1.ReadPanelResult{NotHere: "Coolify doesn't run here: Docker runs none of its containers, and nothing it labelled"}
	}
	return &agentv1.ReadPanelResult{Found: p}
}

func (r Reader) dokploy(list []docker.ContainerSummary, services []docker.Service) *agentv1.ReadPanelResult {
	p := &dockwellv1.Panel{Kind: dockwellv1.PanelKind_PANEL_KIND_DOKPLOY, Folder: dokployFolder}
	apps := map[string]*dockwellv1.PanelApp{}
	for i := range services {
		s := &services[i].Spec
		image := s.TaskTemplate.ContainerSpec.Image
		if slices.Contains(dokployOwn, s.Name) {
			p.Own = append(p.Own, &dockwellv1.PanelContainer{Name: s.Name, Image: image, State: "service"})
			if s.Name == "dokploy" {
				p.Version = tagOf(image)
			}
			continue
		}
		l := s.Labels
		if stack := l[labelStack]; stack != "" {
			// A Compose app of the type stack: its services, as one.
			a := apps[stack]
			if a == nil {
				a = r.dokployApp(stack, "stack", l)
				a.Swarm = true
				apps[stack] = a
			}
			a.Containers = append(a.Containers, s.Name)
			a.Domains = append(a.Domains, domainsOf(l)...)
			continue
		}
		if l["dokploy.application"] == "" && l["dokploy.project"] == "" {
			continue // a service of someone else's
		}
		a := r.dokployApp(s.Name, engineOf(image), l)
		a.Swarm, a.Image = true, image
		a.Containers = []string{s.Name}
		a.Domains = append(domainsOf(l), r.dynamicDomains(s.Name)...)
		apps[s.Name] = a
	}
	for i := range list {
		c := &list[i]
		name := containerName(c)
		if slices.Contains(dokployOwn, name) {
			p.Own = append(p.Own, &dockwellv1.PanelContainer{Name: name, Image: c.Image, State: c.State})
			continue
		}
		l := c.Labels
		project := l[labelProject]
		if project == "" || !strings.HasPrefix(l[labelWorkingDir], path.Join(dokployFolder, "compose")+"/") || l["com.docker.swarm.service.name"] != "" {
			continue
		}
		a := apps[project]
		if a == nil {
			a = r.dokployApp(project, "compose", l)
			a.ComposeProject = project
			apps[project] = a
		}
		a.Containers = append(a.Containers, name)
		a.Domains = append(a.Domains, domainsOf(l)...)
	}
	p.Apps = sorted(apps)
	p.KnownVersion = dokployKnown().MatchString(p.Version)
	if len(p.Own) == 0 && len(p.Apps) == 0 {
		return &agentv1.ReadPanelResult{NotHere: "Dokploy doesn't run here: Docker runs none of its services or containers, and nothing it labelled"}
	}
	return &agentv1.ReadPanelResult{Found: p}
}

// dokployApp is what Dokploy's labels say of an app called id, its appName.
func (r Reader) dokployApp(id, kind string, l map[string]string) *dockwellv1.PanelApp {
	folder := path.Join(dokployFolder, "applications", id)
	if kind == "compose" || kind == "stack" {
		folder = path.Join(dokployFolder, "compose", id)
	}
	return &dockwellv1.PanelApp{
		Name: cmp.Or(l["dokploy.application"], id), Kind: kind, Project: l["dokploy.project"], Environment: l["dokploy.environment"],
		Id: id, Folder: r.folder(folder),
	}
}

// engines are the database images Dokploy runs, by their repository.
var engines = map[string]string{
	"postgres": "postgres", "mysql": "mysql", "mariadb": "mariadb", "mongo": "mongo", "redis": "redis",
	"ghcr.io/tursodatabase/libsql-server": "libsql",
}

// engineOf is the kind of a Swarm service Dokploy runs: a database's engine by its
// image, or an application.
func engineOf(image string) string {
	repo, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	repo = strings.TrimPrefix(strings.TrimPrefix(repo, "docker.io/"), "library/")
	if e, ok := engines[repo]; ok {
		return e
	}
	return "application"
}

// hostRule is a Host() of Traefik's rules, with each name it takes.
var hostRule = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`Host\(([^)]*)\)`) })

// quoted is a name in a Host() rule.
var quoted = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile("[`\"']([^`\"']+)[`\"']") })

// domainsOf are the domains a container's or service's proxy labels route to: the
// hosts of Traefik's rules, and Caddy's addresses. Nothing else of a label leaves
// the server.
func domainsOf(l map[string]string) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(l)) {
		v := l[k]
		switch {
		case strings.HasPrefix(k, "traefik.") && strings.HasSuffix(k, ".rule"):
			out = append(out, hostsIn(v)...)
		case caddyLabel().MatchString(k):
			for _, a := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
				if h := hostOf(a); h != "" {
					out = append(out, h)
				}
			}
		}
	}
	return out
}

// caddyLabel is a Caddy address label, as caddy-docker-proxy reads them.
var caddyLabel = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^caddy(_\d+)?$`) })

// hostsIn are the hosts of each Host() in a Traefik rule.
func hostsIn(rule string) []string {
	var out []string
	for _, m := range hostRule().FindAllStringSubmatch(rule, -1) {
		for _, q := range quoted().FindAllStringSubmatch(m[1], -1) {
			if h := hostOf(q[1]); h != "" {
				out = append(out, h)
			}
		}
	}
	return out
}

// domainName is a name a domain can have.
var domainName = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z0-9*]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
})

// hostOf is the host of an address, such as https://a.example.com/x, or "" for what
// isn't one.
func hostOf(a string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	if _, rest, ok := strings.Cut(a, "://"); ok {
		a = rest
	}
	a, _, _ = strings.Cut(a, "/")
	if h, port, ok := strings.Cut(a, ":"); ok && port != "" {
		a = h
	}
	if !domainName().MatchString(a) {
		return ""
	}
	return a
}

// maxDynamic is how much of a Traefik file of Dokploy's is read: a few routers.
const maxDynamic = 256 << 10

// dynamicDomains are the domains Dokploy routes to an application, from Traefik's
// file for it, traefik/dynamic/<appName>.yml.
func (r Reader) dynamicDomains(app string) []string {
	if r.Root == "" || strings.ContainsAny(app, `/\`) || app == "." || app == ".." {
		return nil
	}
	p := filepath.Join(r.Root, dokployFolder, "traefik", "dynamic", app+".yml")
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(p) //nolint:gosec // a fixed path under Dokploy's folder, refused where it's a link
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxDynamic))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "rule:") {
			out = append(out, hostsIn(line)...)
		}
	}
	return out
}

// folder is p where it's a folder on the server, or "".
func (r Reader) folder(p string) string {
	if r.Root == "" {
		return ""
	}
	if fi, err := os.Lstat(filepath.Join(r.Root, p)); err == nil && fi.IsDir() {
		return p
	}
	return ""
}

// containerName is a container's name, without Docker's leading slash.
func containerName(c *docker.ContainerSummary) string {
	if len(c.Names) == 0 {
		return c.ID
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// tagOf is an image's tag, without its digest, such as "4.7.5".
func tagOf(image string) string {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[i+1:]
	}
	return ""
}

// preview reports a pull request's ID other than 0, which Coolify gives a preview.
func preview(id string) bool {
	return id != "" && id != "0"
}

// sorted are the apps by name, then ID, each with its containers and domains
// sorted, and each domain once.
func sorted[K comparable](apps map[K]*dockwellv1.PanelApp) []*dockwellv1.PanelApp {
	out := slices.Collect(maps.Values(apps))
	for _, a := range out {
		slices.Sort(a.Containers)
		slices.Sort(a.Domains)
		a.Domains = slices.Compact(a.Domains)
	}
	slices.SortFunc(out, func(a, b *dockwellv1.PanelApp) int {
		return cmp.Or(strings.Compare(a.GetName(), b.GetName()), strings.Compare(a.GetId(), b.GetId()))
	})
	return out
}
