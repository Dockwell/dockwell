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
	"strconv"
	"strings"
	"sync"
	"syscall"

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
	// labelSwarmService names the Swarm service a task's container runs for.
	labelSwarmService = "com.docker.swarm.service.name"
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
				Folder: r.folder(coolifyFolder, kind+"s", id),
			}
			if kind == "database" && !strings.HasPrefix(c.Image, "sha256:") {
				a.Image = c.Image
			}
			apps[k] = a
		}
		a.Containers = append(a.Containers, name)
		a.Domains = append(a.Domains, domainsOf(l)...)
		a.Routes = append(a.Routes, routesOf(l, name)...)
		a.FromGit = a.GetFromGit() || kind == "application" && (builtFromGit(c.Image, id) || len(a.GetContainers()) > 1)
	}
	p.Apps = sorted(apps)
	p.Domains = r.ownDomains(p.GetKind())
	p.KnownVersion = coolifyKnown().MatchString(p.Version)
	if len(p.Own) == 0 && len(p.Apps) == 0 {
		return &agentv1.ReadPanelResult{NotHere: "Coolify doesn't run here: Docker runs none of its containers, and nothing it labelled"}
	}
	return &agentv1.ReadPanelResult{Found: p}
}

func (r Reader) dokploy(list []docker.ContainerSummary, services []docker.Service) *agentv1.ReadPanelResult {
	p := &dockwellv1.Panel{Kind: dockwellv1.PanelKind_PANEL_KIND_DOKPLOY, Folder: dokployFolder}
	apps := map[string]*dockwellv1.PanelApp{}
	// Each Swarm service's running task, which Swarm names <service>.<slot>.<task>.
	tasks := map[string]string{}
	for i := range list {
		if svc := list[i].Labels[labelSwarmService]; svc != "" && list[i].State == "running" {
			tasks[svc] = containerName(&list[i])
		}
	}
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
			a.Tasks = append(a.Tasks, tasks[s.Name])
			a.Domains = append(a.Domains, domainsOf(l)...)
			continue
		}
		if l["dokploy.application"] == "" && l["dokploy.project"] == "" {
			continue // a service of someone else's
		}
		a := r.dokployApp(s.Name, engineOf(image), l)
		a.Swarm, a.Image = true, image
		a.Containers, a.Tasks = []string{s.Name}, []string{tasks[s.Name]}
		routes := r.dynamicRoutes(s.Name, tasks[s.Name])
		a.Routes = routes
		a.Domains = domainsOf(l)
		for _, rt := range routes {
			a.Domains = append(a.Domains, rt.GetDomain())
		}
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
		a.Routes = append(a.Routes, routesOf(l, name)...)
	}
	p.Apps = sorted(apps)
	p.Domains = r.ownDomains(p.GetKind())
	p.KnownVersion = dokployKnown().MatchString(p.Version)
	if len(p.Own) == 0 && len(p.Apps) == 0 {
		return &agentv1.ReadPanelResult{NotHere: "Dokploy doesn't run here: Docker runs none of its services or containers, and nothing it labelled"}
	}
	return &agentv1.ReadPanelResult{Found: p}
}

// dokployApp is what Dokploy's labels say of an app called id, its appName. Its
// code folder is a Git checkout where Dokploy deploys it from a repository: it
// clones there, and writes a Compose file of its own there for one it doesn't.
func (r Reader) dokployApp(id, kind string, l map[string]string) *dockwellv1.PanelApp {
	in := "applications"
	if kind == "compose" || kind == "stack" {
		in = "compose"
	}
	return &dockwellv1.PanelApp{
		Name: cmp.Or(l["dokploy.application"], id), Kind: kind, Project: l["dokploy.project"], Environment: l["dokploy.environment"],
		Id: id, Folder: r.folder(dokployFolder, in, id), FromGit: r.folder(dokployFolder, in, id, "code", ".git") != "",
	}
}

// commit is a Git commit, as Coolify tags an image it built from one.
var commit = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9a-f]{7,40}$`) })

// builtFromGit reports an image Coolify built from an application's repository:
// tagged with the commit, as <uuid>:<commit> or the registry name it was given, or
// built by docker compose under its project, <uuid>-<service>. One it pulled keeps
// its own name, and one built from a Dockerfile written in Coolify is <uuid>:latest.
func builtFromGit(image, id string) bool {
	repo, _, _ := strings.Cut(image, "@")
	tag := tagOf(repo)
	repo = strings.TrimSuffix(repo, ":"+tag)
	return commit().MatchString(tag) || strings.HasPrefix(repo, id+"-") || strings.HasPrefix(repo, id+"_")
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
		case strings.HasPrefix(k, "traefik.") && strings.HasSuffix(k, ".rule") && l["traefik.enable"] != "false":
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

// routesOf says where the proxy's labels on a container send each domain: a
// Traefik router's hosts, path and the port of the service it names, or the one
// service the labels have; a Caddy address, its path and the port its
// reverse_proxy names. A domain that can't come as a plain route says why (Left):
// a middleware or a Caddy directive Dockwell can't tell is harmless, such as an
// SSO check, an allowlist or a stripped path, which a plain route would drop, so
// what was behind a login would answer anyone; a rule that says more than a host
// and a path prefix; or an entrypoint other than the web's. Nothing else of a
// label leaves the server. A domain the labels give no port for has 0.
func routesOf(l map[string]string, container string) []*dockwellv1.PanelRoute {
	service := l[labelService]
	var out []*dockwellv1.PanelRoute
	add := func(host, path string, port uint32, left string) {
		if path == "/" {
			path = ""
		}
		for _, r := range out {
			if r.GetDomain() == host && r.GetPath() == path {
				if r.GetPort() == 0 {
					r.Port = port
				}
				// What one router of it can't bring, the domain can't: an HTTPS
				// router behind a login and an HTTP one that redirects are one site.
				r.Left = cmp.Or(r.GetLeft(), left)
				return
			}
		}
		out = append(out, &dockwellv1.PanelRoute{Domain: host, Path: path, Container: container, Service: service, Port: port, Left: left})
	}
	if l["traefik.enable"] != "false" {
		for _, r := range traefikRoutes(l) {
			add(r.host, r.path, r.port, r.left)
		}
	}
	for _, r := range caddyRoutes(l) {
		add(r.host, r.path, r.port, r.left)
	}
	return out
}

// labelRoute is a domain a proxy's labels route, before it's merged with the
// container's others.
type labelRoute struct {
	host, path, left string
	port             uint32
}

// harmless are the middlewares a plain route can do without: a redirect to HTTPS,
// which Dockwell's ingress does too, compression, headers and a redirect of www.
var harmless = []string{"redirectscheme", "compress", "headers", "redirectregex"}

// webEntrypoints are the entrypoints that answer on ports 80 and 443, as Coolify
// and Dokploy name them.
var webEntrypoints = []string{"http", "https", "web", "websecure"}

func traefikRoutes(l map[string]string) []labelRoute {
	var services []string
	for k := range l {
		if name, ok := strings.CutPrefix(k, "traefik.http.services."); ok && strings.HasSuffix(name, ".loadbalancer.server.port") {
			services = append(services, strings.TrimSuffix(name, ".loadbalancer.server.port"))
		}
	}
	var out []labelRoute
	for _, k := range slices.Sorted(maps.Keys(l)) {
		router, ok := strings.CutPrefix(k, "traefik.http.routers.")
		if !ok || !strings.HasSuffix(router, ".rule") {
			continue
		}
		router = strings.TrimSuffix(router, ".rule")
		rule := l[k]
		at := "traefik.http.routers." + router + "."
		svc := l[at+"service"]
		if svc == "" && len(services) == 1 {
			svc = services[0]
		}
		port := portOf(l["traefik.http.services."+cmp.Or(svc, router)+".loadbalancer.server.port"])
		prefix := ""
		if m := pathRule().FindStringSubmatch(rule); len(m) > 1 {
			prefix = m[1]
		}
		left := ""
		if entry := l[at+"entrypoints"]; entry != "" {
			for _, e := range strings.Split(entry, ",") {
				if !slices.Contains(webEntrypoints, strings.TrimSpace(e)) {
					left = fmt.Sprintf("Traefik serves it on the entrypoint %s, not ports 80 and 443", strings.TrimSpace(e))
				}
			}
		}
		if left == "" && complexRule(rule) {
			// What it matches can't be told, so the whole domain stays with it.
			left, prefix = "its rule says more than a host and a path prefix", ""
		}
		if left == "" {
			left = middlewaresLeft(l, l[at+"middlewares"])
		}
		for _, h := range hostsIn(rule) {
			out = append(out, labelRoute{host: h, path: prefix, port: port, left: left})
		}
	}
	return out
}

// middlewaresLeft says why a router's middlewares keep its domain from coming as a
// plain route, or "" where each is one a route can do without. A middleware
// defined elsewhere, as in a file of the proxy's, can't be read, so it can't be
// told harmless either.
func middlewaresLeft(l map[string]string, list string) string {
	for _, m := range strings.Split(list, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(m), "@")
		if name == "" {
			continue
		}
		kind := ""
		for k := range l {
			if rest, ok := strings.CutPrefix(k, "traefik.http.middlewares."+name+"."); ok {
				kind, _, _ = strings.Cut(rest, ".")
			}
		}
		switch {
		case kind == "":
			return fmt.Sprintf("it passes through the middleware %s, which Dockwell can't read, so it can't tell what a plain route would drop", name)
		case strings.HasPrefix(kind, "stripprefix"):
			return "Traefik takes its path off before the app sees it, which a route doesn't"
		case !slices.Contains(harmless, kind):
			return fmt.Sprintf("it passes through the middleware %s (%s), which a plain route would drop, so what it guards would answer anyone", name, kind)
		}
	}
	return ""
}

// caddyHarmless are the directives of caddy-docker-proxy's labels a plain route
// can do without, as Coolify writes them.
var caddyHarmless = []string{"header", "try_files", "encode", "redir", "log", "reverse_proxy", "handle", "handle_path"}

func caddyRoutes(l map[string]string) []labelRoute {
	var out []labelRoute
	for _, k := range slices.Sorted(maps.Keys(l)) {
		if !caddyLabel().MatchString(k) {
			continue
		}
		port, prefix, left := uint32(0), "", ""
		for sub, v := range l {
			rest, ok := strings.CutPrefix(sub, k+".")
			if !ok {
				continue
			}
			directive, inner, _ := strings.Cut(rest, ".")
			if strings.HasSuffix(rest, "reverse_proxy") || strings.HasSuffix(inner, "_reverse_proxy") {
				if m := upstreams().FindStringSubmatch(v); m != nil {
					port = portOf(m[1])
				}
			}
			switch {
			case directive == "handle_path" && inner == "":
				if p := strings.TrimSuffix(strings.TrimSpace(v), "*"); p != "" && p != "/" {
					prefix, left = strings.TrimSuffix(p, "/"), cmp.Or(left, "Caddy takes its path off before the app sees it, which a route doesn't")
				}
			case directive == "handle" && inner == "":
				if p := strings.TrimSuffix(strings.TrimSpace(v), "*"); p != "/" {
					prefix = strings.TrimSuffix(p, "/")
				}
			case strings.HasPrefix(directive, "@") || !slices.Contains(caddyHarmless, directive):
				left = cmp.Or(left, fmt.Sprintf("its Caddy labels say %s, which a plain route would drop", directive))
			}
		}
		for _, a := range strings.FieldsFunc(l[k], func(r rune) bool { return r == ',' || r == ' ' }) {
			if h := hostOf(a); h != "" {
				out = append(out, labelRoute{host: h, path: prefix, port: port, left: left})
			}
		}
	}
	return out
}

// labelService is the Compose service a container runs as.
const labelService = "com.docker.compose.service"

// pathRule is a PathPrefix() of a Traefik rule, with the path it takes.
var pathRule = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile("PathPrefix\\(\\s*[`\"']([^`\"']+)[`\"']") })

// upstreams is the port caddy-docker-proxy's {{upstreams PORT}} names.
var upstreams = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`upstreams\s+(?:https?\s+)?(\d+)`) })

// portOf is a port a label names, or 0 for what isn't one.
func portOf(s string) uint32 {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil || n == 0 {
		return 0
	}
	return uint32(n)
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

// dynamicRoutes are where Dokploy routes an application's domains, from Traefik's
// file for it, traefik/dynamic/<appName>.yml: each router's hosts and path, and the
// port of the server URL its service names, http://<appName>:<port>, as the task
// container runs it. A router behind a middleware other than Dokploy's redirect
// to HTTPS, such as the basic auth its Security tab adds, or with a rule a route
// can't say, keeps its domain behind, saying why.
func (r Reader) dynamicRoutes(app, task string) []*dockwellv1.PanelRoute {
	if r.Root == "" || strings.ContainsAny(app, `/\`) || app == "." || app == ".." {
		return nil
	}
	b := r.panelFile(filepath.Join(dokployFolder, "traefik", "dynamic", app+".yml"))
	type router struct {
		rule, service string
		middlewares   []string
	}
	routers, ports := map[string]*router{}, map[string]uint32{}
	var order []string
	section, current, list := "", "", ""
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		key, value, _ := strings.Cut(trimmed, ":")
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch {
		case indent == 2 && (key == "routers" || key == "services"):
			section, current, list = key, "", ""
		case indent < 2:
			section, current, list = "", "", ""
		case indent == 4 && value == "" && !strings.HasPrefix(trimmed, "-"):
			current, list = key, ""
			if section == "routers" && routers[key] == nil {
				routers[key] = &router{}
				order = append(order, key)
			}
		case section == "routers" && current != "" && list == "middlewares" && strings.HasPrefix(trimmed, "- "):
			routers[current].middlewares = append(routers[current].middlewares, strings.Trim(strings.TrimSpace(trimmed[2:]), `"'`))
		case section == "routers" && current != "" && key == "middlewares":
			list = key
			for _, m := range strings.Split(strings.Trim(value, "[]"), ",") {
				if m = strings.Trim(strings.TrimSpace(m), `"'`); m != "" {
					routers[current].middlewares = append(routers[current].middlewares, m)
				}
			}
		case section == "routers" && current != "" && key == "rule":
			routers[current].rule, list = trimmed[len("rule:"):], ""
		case section == "routers" && current != "" && key == "service":
			routers[current].service, list = value, ""
		case section == "services" && current != "" && (strings.HasPrefix(trimmed, "- url:") || key == "url"):
			if m := serverURL().FindStringSubmatch(trimmed); m != nil {
				ports[current] = portOf(m[1])
			}
		}
	}
	var out []*dockwellv1.PanelRoute
	for _, name := range order {
		rt := routers[name]
		prefix, left := "", ""
		if m := pathRule().FindStringSubmatch(rt.rule); len(m) > 1 && m[1] != "/" {
			prefix = m[1]
		}
		if complexRule(rt.rule) {
			prefix, left = "", "its rule says more than a host and a path prefix"
		}
		for _, m := range rt.middlewares {
			if n, _, _ := strings.Cut(m, "@"); n != "redirect-to-https" && left == "" {
				left = fmt.Sprintf("it passes through the middleware %s, which a plain route would drop, so what it guards would answer anyone", n)
			}
		}
		for _, h := range hostsIn(rt.rule) {
			if i := slices.IndexFunc(out, func(o *dockwellv1.PanelRoute) bool { return o.GetDomain() == h && o.GetPath() == prefix }); i >= 0 {
				out[i].Left = cmp.Or(out[i].GetLeft(), left)
				continue
			}
			out = append(out, &dockwellv1.PanelRoute{Domain: h, Path: prefix, Container: task, Service: app, Port: ports[rt.service], Left: left})
		}
	}
	return out
}

// complexRule reports a Traefik rule that says more than a host and a path prefix,
// which a route can't say.
func complexRule(rule string) bool {
	for _, s := range []string{"||", "!", "Path(", "Regexp(", "Header", "Query("} {
		if strings.Contains(rule, s) {
			return true
		}
	}
	return false
}

// panelFile is a file under a panel's folder, read only where it's a regular
// file: a link, a pipe or a device there is refused as it opens, never followed
// or waited on. Nil where it can't be read.
func (r Reader) panelFile(rel string) []byte {
	if r.Root == "" {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(r.Root, rel), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // a fixed path under a panel's folder, refused where it's a link
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(f, maxDynamic))
	if err != nil {
		return nil
	}
	return b
}

// maxOwnFiles is how many of a proxy's own files are read for its domains.
const maxOwnFiles = 64

// ownDomains are the domains a panel's proxy routes from its own files: Coolify's
// proxy/dynamic folder, which holds its dashboard's and any a person added, and
// Dokploy's dokploy.yml, its dashboard's. Each stops answering when the proxy
// hands over its ports, as nothing of them comes as a route.
func (r Reader) ownDomains(kind dockwellv1.PanelKind) []string {
	var files []string
	switch kind {
	case dockwellv1.PanelKind_PANEL_KIND_COOLIFY:
		dir := r.folder(coolifyFolder, "proxy", "dynamic")
		if dir == "" {
			return nil
		}
		entries, err := os.ReadDir(filepath.Join(r.Root, dir))
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if len(files) < maxOwnFiles && e.Type().IsRegular() && (strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
				files = append(files, path.Join(dir, e.Name()))
			}
		}
	case dockwellv1.PanelKind_PANEL_KIND_DOKPLOY:
		files = []string{path.Join(dokployFolder, "traefik", "dynamic", "dokploy.yml")}
	case dockwellv1.PanelKind_PANEL_KIND_UNSPECIFIED:
	}
	var out []string
	for _, f := range files {
		out = append(out, rulesHosts(r.panelFile(f))...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// rulesHosts are the hosts of each Traefik rule in a file of the proxy's.
func rulesHosts(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "rule:") {
			out = append(out, hostsIn(line)...)
		}
	}
	return out
}

// serverURL is the port of a load balancer's server URL, http://<name>:<port>.
var serverURL = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`https?://[^\s:/"']+:(\d+)`) })

// part is a word of a label that names a folder, such as Coolify's UUID or
// Dokploy's appName: nothing that could lead out of the panel's folder.
var part = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[a-z0-9-]+$`) })

// folder is the folder under the panel's, base, that parts name, such as
// /data/coolify/services/<uuid>, where it's one on the server, or "". Each part
// comes from a label, which anyone who runs a container can write, so each must
// be a plain word, and the folder must stay under base. It's read with Lstat: a
// link there is no folder of the panel's.
func (r Reader) folder(base string, parts ...string) string {
	if r.Root == "" {
		return ""
	}
	for _, p := range parts {
		if p != ".git" && !part().MatchString(p) {
			return ""
		}
	}
	p := path.Join(append([]string{base}, parts...)...)
	if !strings.HasPrefix(p, base+"/") {
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
