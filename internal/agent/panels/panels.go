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
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// Reader reads a panel on this server.
type Reader struct {
	Docker *docker.Client
	// Root is the server's root, "/", or in a test a folder laid out as one. Empty
	// reads no folder: what Docker says is read alone.
	Root string
	// ProbeAddr is where a handover's check asks the old proxy for each domain,
	// 127.0.0.1:443 where it's empty; a test names its own.
	ProbeAddr string
}

// Read reads what the panel runs, or says why there's nothing to read. With check,
// it checks each domain its proxy serves for a handover too (Panel.checks).
func (r Reader) Read(ctx context.Context, kind dockwellv1.PanelKind, check bool) (*agentv1.ReadPanelResult, error) {
	if r.Docker == nil {
		return nil, errors.New("this agent reads no Docker, so it can't read what a panel runs")
	}
	list, err := r.Docker.ListContainers(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("can't list the containers Docker runs: %w", err)
	}
	var out *agentv1.ReadPanelResult
	switch kind {
	case dockwellv1.PanelKind_PANEL_KIND_COOLIFY:
		out = r.coolify(list)
	case dockwellv1.PanelKind_PANEL_KIND_DOKPLOY:
		services, err := r.Docker.ListServices(ctx)
		if err != nil {
			return nil, fmt.Errorf("can't list the Swarm services Docker runs: %w", err)
		}
		// A copy runs on this server alone, so a swarm of more nodes keeps each
		// Swarm-run app where it is, and one whose nodes can't be read does too.
		nodes := -1
		if info, err := r.Docker.Info(ctx); err == nil {
			nodes = info.Swarm.Nodes
		}
		out = r.dokploy(list, services, nodes)
	default:
		return nil, fmt.Errorf("there's no panel %v to read: name Coolify or Dokploy", kind)
	}
	if p := out.GetFound(); p != nil {
		// What guards every domain at once, the proxy's static configuration and
		// its own files, is read too, and where it can't be read in full, or guards
		// anything, no domain comes over.
		p.ProxyLeft = cmp.Or(r.filesLeft(kind), r.staticLeft(ctx, kind, r.fileMiddlewares(kind)))
		if check {
			p.Checks = resourcepb.PanelChecks(r.checks(ctx, kind, p))
		}
	}
	return out, nil
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
	files := r.fileMiddlewares(p.GetKind())
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
		a.Routes = append(a.Routes, routesOf(l, name, files)...)
		a.FromGit = a.GetFromGit() || kind == "application" && (builtFromGit(c.Image, id) || len(a.GetContainers()) > 1)
	}
	p.Apps = sorted(apps)
	p.Domains = r.ownDomains(p.GetKind(), p.GetApps())
	p.KnownVersion = coolifyKnown().MatchString(p.Version)
	if len(p.Own) == 0 && len(p.Apps) == 0 {
		return &agentv1.ReadPanelResult{NotHere: "Coolify doesn't run here: Docker runs none of its containers, and nothing it labelled"}
	}
	return &agentv1.ReadPanelResult{Found: p}
}

func (r Reader) dokploy(list []docker.ContainerSummary, services []docker.Service, nodes int) *agentv1.ReadPanelResult {
	p := &dockwellv1.Panel{Kind: dockwellv1.PanelKind_PANEL_KIND_DOKPLOY, Folder: dokployFolder}
	files := r.fileMiddlewares(p.GetKind())
	apps := map[string]*dockwellv1.PanelApp{}
	// Each Swarm service's running task, which Swarm names <service>.<slot>.<task>:
	// what it's made again from, and where its domains go until then.
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
			short := strings.TrimPrefix(s.Name, stack+"_")
			a.Containers = append(a.Containers, s.Name)
			a.Tasks = append(a.Tasks, tasks[s.Name])
			a.Restarts = append(a.Restarts, restartOf(s))
			a.SwarmLeft = cmp.Or(a.GetSwarmLeft(), swarmLeft(s, nodes))
			if !dokployOwns(l) {
				// A stack deployed by hand: Dokploy labels each service of its own.
				a.SwarmLeft = cmp.Or(a.GetSwarmLeft(), s.Name+" carries none of Dokploy's labels, so Dockwell doesn't act on it as Dokploy's")
			}
			a.Domains = append(a.Domains, domainsOf(l)...)
			a.Published = append(a.Published, publishedOf(short, s)...)
			for _, rt := range routesOf(l, tasks[s.Name], files) {
				rt.Service = short
				a.Routes = append(a.Routes, rt)
			}
			continue
		}
		if !dokployOwns(l) {
			continue // a service of someone else's
		}
		a := r.dokployApp(s.Name, engineOf(image), l)
		a.Swarm, a.Image = true, image
		a.Containers, a.Tasks, a.Restarts = []string{s.Name}, []string{tasks[s.Name]}, []string{restartOf(s)}
		a.SwarmLeft = swarmLeft(s, nodes)
		a.Published = publishedOf(s.Name, s)
		routes := r.dynamicRoutes(s.Name, tasks[s.Name], files)
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
		a.Routes = append(a.Routes, routesOf(l, name, files)...)
	}
	p.Apps = sorted(apps)
	p.Domains = r.ownDomains(p.GetKind(), p.GetApps())
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
	l = foldTraefik(l)
	var out []string
	for _, k := range slices.Sorted(maps.Keys(l)) {
		v := l[k]
		switch {
		case strings.HasPrefix(k, "traefik.") && strings.HasSuffix(k, ".rule") && traefikOn(l):
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

// foldTraefik is a container's labels with each of Traefik's keys in lower case, as
// Traefik reads them: it folds the prefix, the names and the fields alike, so
// routers.App.Middlewares is routers.app.middlewares. Where two keys differ only
// by case, the one already in lower case stands, as it does in Traefik.
func foldTraefik(l map[string]string) map[string]string {
	out := make(map[string]string, len(l))
	for k, v := range l {
		lower := strings.ToLower(k)
		if !strings.HasPrefix(lower, "traefik.") {
			out[k] = v
			continue
		}
		if _, taken := out[lower]; taken && k != lower {
			continue
		}
		out[lower] = v
	}
	return out
}

// traefikOn reports a container Traefik routes: both panels run it with
// exposedByDefault off, so only one whose traefik.enable reads as true.
func traefikOn(l map[string]string) bool {
	on, err := strconv.ParseBool(l["traefik.enable"])
	return err == nil && on
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
func routesOf(l map[string]string, container string, files map[string]string) []*dockwellv1.PanelRoute {
	l = foldTraefik(l)
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
	if traefikOn(l) {
		for _, r := range traefikRoutes(l, files) {
			add(r.host, r.path, r.port, r.left)
		}
	}
	for _, r := range caddyRoutes(l) {
		add(r.host, r.path, r.port, r.left)
	}
	return sameDomainLeft(out)
}

// sameDomainLeft keeps every route of a domain behind where one is: a plain route
// for the domain's root would serve the path another of its routers guarded, so
// what was behind a login there would answer anyone.
func sameDomainLeft(routes []*dockwellv1.PanelRoute) []*dockwellv1.PanelRoute {
	for _, r := range routes {
		if r.GetLeft() == "" {
			continue
		}
		for _, o := range routes {
			if o.GetDomain() == r.GetDomain() && o.GetLeft() == "" {
				o.Left = "another path of the domain stays behind, as " + r.GetLeft()
			}
		}
	}
	return routes
}

// labelRoute is a domain a proxy's labels route, before it's merged with the
// container's others.
type labelRoute struct {
	host, path, left string
	port             uint32
}

// webEntrypoints are the entrypoints that answer on ports 80 and 443, as Coolify
// and Dokploy name them.
var webEntrypoints = []string{"http", "https", "web", "websecure"}

func traefikRoutes(l, files map[string]string) []labelRoute {
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
		at := "traefik.http.routers." + router + "."
		svc := l[at+"service"]
		if svc == "" && len(services) == 1 {
			svc = services[0]
		}
		port := portOf(l["traefik.http.services."+cmp.Or(svc, router)+".loadbalancer.server.port"])
		hosts, prefix, left := plainRule(l[k])
		if left == "" {
			left = routerFieldsLeft(l, at)
		}
		if entry := l[at+"entrypoints"]; left == "" && entry != "" {
			for _, e := range strings.Split(entry, ",") {
				if !slices.Contains(webEntrypoints, strings.TrimSpace(e)) {
					left = fmt.Sprintf("Traefik serves it on the entrypoint %s, not ports 80 and 443", strings.TrimSpace(e))
				}
			}
		}
		if left == "" {
			left = middlewaresLeft(l, strings.Split(l[at+"middlewares"], ","), files)
		}
		for _, h := range hosts {
			out = append(out, labelRoute{host: h, path: prefix, port: port, left: left})
		}
	}
	return out
}

// routerFields are the fields of a Traefik router a route can do without: what it
// matches, where it sends, its entrypoints and middlewares, read on their own, its
// priority, and the certificate it asks for. Any other, such as tls.options, which
// can ask a client for a certificate, keeps it behind.
var routerFields = []string{"rule", "service", "entrypoints", "middlewares", "priority", "tls", "tls.certresolver"}

// routerFieldsLeft says why a router's fields keep its domain behind, or "".
func routerFieldsLeft(l map[string]string, at string) string {
	for _, k := range slices.Sorted(maps.Keys(l)) {
		field, ok := strings.CutPrefix(k, at)
		if !ok || slices.Contains(routerFields, field) || strings.HasPrefix(field, "tls.domains") {
			continue
		}
		return fmt.Sprintf("its router sets %s, which a route can't say", field)
	}
	return ""
}

// plainRule reads a Traefik rule a route can say: Host() with one or more names,
// and at most one PathPrefix(), joined by &&, and nothing else. Any other matcher,
// such as ClientIP(), Method() or Header(), says who may reach the app or what it
// answers, which a route would drop, so the rule's hosts come back with a reason,
// and its path, which can't be told, as the whole domain's.
func plainRule(rule string) (hosts []string, prefix, left string) {
	hosts = hostsIn(rule)
	m := plainRuleForm().FindStringSubmatch(rule)
	if m == nil {
		return hosts, "", "its rule says more than a host and a path prefix"
	}
	if p := pathRule().FindStringSubmatch(rule); len(p) > 1 && p[1] != "/" {
		prefix = p[1]
	}
	return hosts, prefix, ""
}

// plainRuleForm is Host(names), optionally with one PathPrefix(path), either way
// round, and nothing else.
var plainRuleForm = sync.OnceValue(func() *regexp.Regexp {
	q := "(?:`[^`]*`|\"[^\"]*\")"
	host := `Host\(\s*` + q + `(?:\s*,\s*` + q + `)*\s*\)`
	prefix := `PathPrefix\(\s*` + q + `\s*\)`
	return regexp.MustCompile(`^\s*(?:` + host + `(?:\s*&&\s*` + prefix + `)?|` + prefix + `\s*&&\s*` + host + `)\s*$`)
})

// middlewaresLeft says why a router's middlewares keep its domain from coming as a
// plain route, or "" where each is one a route can do without. A middleware
// defined elsewhere, as in a file of the proxy's, can't be read, so it can't be
// told harmless either.
func middlewaresLeft(l map[string]string, list []string, files map[string]string) string {
	for _, m := range list {
		ref := strings.ToLower(strings.TrimSpace(m))
		name, provider, _ := strings.Cut(ref, "@")
		if name == "" {
			continue
		}
		kind, ok := "", false
		switch provider {
		case "", "docker", "swarm":
			// Defined in the labels of what it routes, as one with no provider is
			// looked for first, and read in full there.
			for k := range l {
				if rest, found := strings.CutPrefix(k, "traefik.http.middlewares."+name+"."); found {
					kind, _, _ = strings.Cut(rest, ".")
				}
			}
			ok = harmlessLabelMiddleware(l, name, kind)
		case "file":
			// Defined in the proxy's own files, as Dokploy's redirect-to-https is,
			// where only its kind is read: one whose settings matter isn't let by.
			kind = files[name]
			ok = kind == "redirectscheme" || kind == "compress"
		}
		switch {
		case kind == "":
			return fmt.Sprintf("it passes through the middleware %s, which Dockwell can't read, so it can't tell what a plain route would drop", ref)
		case !ok:
			return fmt.Sprintf("it passes through the middleware %s (%s), which a plain route would drop, so what it guards would answer anyone", name, kind)
		}
	}
	return ""
}

// harmlessLabelMiddleware reports a middleware, read in full from labels, that a
// plain route can do without: a redirect to HTTPS, compression, headers that only
// set the response's, and a redirect of www as Coolify writes one.
func harmlessLabelMiddleware(l map[string]string, name, kind string) bool {
	switch kind {
	case "redirectscheme", "compress":
		return true
	case "headers":
		return onlyResponseHeaders(l, name)
	case "redirectregex":
		return slices.Contains(coolifyWWW, l["traefik.http.middlewares."+name+".redirectregex.regex"])
	}
	return false
}

// coolifyWWW are the regular expressions of Coolify's redirects to and from www.
var coolifyWWW = []string{`^(http|https)://www\.(.+)`, `^(http|https)://(?:www\.)?(.+)`}

// onlyResponseHeaders reports a headers middleware that sets only what the
// response carries, as Coolify's noindex does: nothing the app reads or trusts.
func onlyResponseHeaders(l map[string]string, name string) bool {
	for k := range l {
		if rest, ok := strings.CutPrefix(k, "traefik.http.middlewares."+name+".headers."); ok && !strings.HasPrefix(rest, "customresponseheaders.") {
			return false
		}
	}
	return true
}

func caddyRoutes(l map[string]string) []labelRoute {
	var out []labelRoute
	for _, k := range slices.Sorted(maps.Keys(l)) {
		if !caddyLabel().MatchString(k) {
			continue
		}
		port, prefix, left := uint32(0), "", ""
		handles := map[string]bool{}
		for sub, v := range l {
			rest, ok := strings.CutPrefix(sub, k+".")
			if !ok {
				continue
			}
			segs := strings.Split(rest, ".")
			if why := caddyLeft(segs); why != "" {
				left = cmp.Or(left, why)
			}
			first := caddyDirective(segs[0])
			if first == "handle" || first == "handle_path" {
				handles[segs[0]] = true
			}
			switch last := caddyDirective(segs[len(segs)-1]); {
			case last == "reverse_proxy":
				// Only to the container, as {{upstreams PORT}} says: anything else,
				// another address or a setting in its words, can't be told.
				m := plainUpstreams().FindStringSubmatch(strings.TrimSpace(v))
				if m == nil {
					left = cmp.Or(left, "its Caddy labels proxy it somewhere other than its own container")
					break
				}
				port = portOf(m[1])
			case last == "redir" && len(segs) == 1:
				if !caddyRedirect().MatchString(strings.TrimSpace(v)) {
					left = cmp.Or(left, "its Caddy labels redirect it in a way a route can't say")
				}
			}
			if len(segs) == 1 && (first == "handle_path" || first == "handle") {
				if p := strings.TrimSuffix(strings.TrimSpace(v), "*"); p != "" && p != "/" {
					prefix = strings.TrimSuffix(p, "/")
					if first == "handle_path" {
						left = cmp.Or(left, "Caddy takes its path off before the app sees it, which a route doesn't")
					}
				}
			}
		}
		if len(handles) > 1 {
			left = cmp.Or(left, "its Caddy labels handle more than one path, which a route can't say")
		}
		for _, a := range strings.FieldsFunc(l[k], func(r rune) bool { return r == ',' || r == ' ' }) {
			h := hostOf(a)
			if h == "" {
				continue
			}
			why := left
			if siteHasPath(a) {
				why = cmp.Or(why, "its Caddy address names a path, which Dockwell doesn't read as a route")
			}
			out = append(out, labelRoute{host: h, path: prefix, port: port, left: why})
		}
	}
	return out
}

// siteHasPath reports a Caddy site address with a path, such as
// https://shop.example.com/admin, which matches that path alone.
func siteHasPath(a string) bool {
	a = strings.TrimSpace(a)
	if _, rest, ok := strings.Cut(a, "://"); ok {
		a = rest
	}
	_, p, ok := strings.Cut(a, "/")
	return ok && p != "" && p != "*"
}

// plainUpstreams is caddy-docker-proxy's {{upstreams PORT}}, with a scheme or not,
// and nothing else.
var plainUpstreams = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^\{\{upstreams(?: (?:http|https|h2c))? (\d+)\}\}$`)
})

// caddyRedirect is a redirect as Coolify writes one, to the same or the www name
// of a host, keeping the request's path.
var caddyRedirect = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(?:https?|\{scheme\})://[a-z0-9.-]+\{uri\}(?: permanent)?$`)
})

// Caddy's directives a plain route can do without, as Coolify writes them: those
// whose words are their own (a header, files to try, compression, a redirect, a
// log), the blocks that hold others (handle, handle_path), and reverse_proxy with
// the settings below it that only say how it proxies.
var (
	caddyArgs     = []string{"header", "try_files", "encode", "redir", "log"}
	caddyBlocks   = []string{"handle", "handle_path"}
	caddyProxying = []string{"stream_close_delay", "flush_interval", "header_down"}
)

// caddyLeft says why a label's directives keep its address from coming as a plain
// route, reading each segment's directive, nested ones too, without its N_ order:
// "" where each is one Caddy's labels can say that a route does without.
func caddyLeft(segs []string) string {
	for i := range segs {
		d := caddyDirective(segs[i])
		switch {
		case strings.HasPrefix(d, "@"):
			return fmt.Sprintf("its Caddy labels match requests by %s, which a plain route would drop", d)
		case slices.Contains(caddyArgs, d):
			return ""
		case slices.Contains(caddyBlocks, d):
			continue
		case d == "reverse_proxy":
			if i+1 < len(segs) && !slices.Contains(caddyProxying, caddyDirective(segs[i+1])) {
				return fmt.Sprintf("its Caddy labels set reverse_proxy's %s, which a plain route would drop", caddyDirective(segs[i+1]))
			}
			return ""
		default:
			return fmt.Sprintf("its Caddy labels say %s, which a plain route would drop", d)
		}
	}
	return ""
}

// caddyDirective is a segment of caddy-docker-proxy's label without the N_ that
// orders it, such as reverse_proxy for 0_reverse_proxy.
func caddyDirective(seg string) string {
	if i := strings.Index(seg, "_"); i > 0 {
		if _, err := strconv.Atoi(seg[:i]); err == nil {
			return seg[i+1:]
		}
	}
	return seg
}

// labelService is the Compose service a container runs as.
const labelService = "com.docker.compose.service"

// pathRule is a PathPrefix() of a Traefik rule, with the path it takes.
var pathRule = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile("PathPrefix\\(\\s*[`\"']([^`\"']+)[`\"']") })

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
func (r Reader) dynamicRoutes(app, task string, files map[string]string) []*dockwellv1.PanelRoute {
	if r.Root == "" || strings.ContainsAny(app, `/\\`) || app == "." || app == ".." {
		return nil
	}
	f := readTraefikFile(r.panelFile(filepath.Join(dokployFolder, "traefik", "dynamic", app+".yml")))
	var out []*dockwellv1.PanelRoute
	for _, name := range f.order {
		rt := f.routers[name]
		hosts, prefix, left := plainRule(rt.rule)
		if left == "" {
			left = middlewaresLeft(nil, rt.middlewares, files)
		}
		for _, h := range hosts {
			if i := slices.IndexFunc(out, func(o *dockwellv1.PanelRoute) bool { return o.GetDomain() == h && o.GetPath() == prefix }); i >= 0 {
				out[i].Left = cmp.Or(out[i].GetLeft(), left)
				continue
			}
			out = append(out, &dockwellv1.PanelRoute{Domain: h, Path: prefix, Container: task, Service: app, Port: f.ports[rt.service], Left: left})
		}
	}
	return sameDomainLeft(out)
}

// traefikFile is what a file of Traefik's dynamic configuration says, as far as a
// route goes: each router's rule, service and middlewares, each service's port,
// and each middleware's kind.
type traefikFile struct {
	routers     map[string]*fileRouter
	order       []string
	ports       map[string]uint32
	middlewares map[string]string
}

type fileRouter struct {
	rule, service string
	middlewares   []string
}

// readTraefikFile reads a file of Traefik's dynamic configuration, as both panels
// write them, line by line: a rule folded onto the next line, as YAML's >- writes
// a long one, reads as one.
func readTraefikFile(b []byte) traefikFile {
	f := traefikFile{routers: map[string]*fileRouter{}, ports: map[string]uint32{}, middlewares: map[string]string{}}
	section, current, list := "", "", ""
	folding := false
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if folding {
			if indent > 6 && trimmed != "" {
				f.routers[current].rule += " " + trimmed
				continue
			}
			folding = false
		}
		key, value, _ := strings.Cut(trimmed, ":")
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch {
		case indent == 2 && (key == "routers" || key == "services" || key == "middlewares"):
			section, current, list = key, "", ""
		case indent < 2:
			section, current, list = "", "", ""
		case indent == 4 && value == "" && !strings.HasPrefix(trimmed, "-"):
			current, list = key, ""
			if section == "routers" && f.routers[key] == nil {
				f.routers[key] = &fileRouter{}
				f.order = append(f.order, key)
			}
		case section == "middlewares" && current != "" && indent == 6 && f.middlewares[current] == "":
			f.middlewares[current] = strings.ToLower(key)
		case section == "routers" && current != "" && list == "middlewares" && strings.HasPrefix(trimmed, "- "):
			f.routers[current].middlewares = append(f.routers[current].middlewares, strings.Trim(strings.TrimSpace(trimmed[2:]), `"'`))
		case section == "routers" && current != "" && key == "middlewares":
			list = key
			for _, m := range strings.Split(strings.Trim(value, "[]"), ",") {
				if m = strings.Trim(strings.TrimSpace(m), `"'`); m != "" {
					f.routers[current].middlewares = append(f.routers[current].middlewares, m)
				}
			}
		case section == "routers" && current != "" && key == "rule":
			list = ""
			rule := strings.TrimSpace(trimmed[len("rule:"):])
			if rule == ">-" || rule == ">" || rule == "|" || rule == "|-" {
				rule, folding = "", true
			}
			f.routers[current].rule = rule
		case section == "routers" && current != "" && key == "service":
			f.routers[current].service, list = value, ""
		case section == "services" && current != "" && (strings.HasPrefix(trimmed, "- url:") || key == "url"):
			if m := serverURL().FindStringSubmatch(trimmed); m != nil {
				f.ports[current] = portOf(m[1])
			}
		}
	}
	return f
}

// fileMiddlewares are the kinds of the middlewares a panel's proxy defines in its
// own files, by name, as a router names one with @file: Dokploy's
// redirect-to-https, in its middlewares.yml, is a redirectScheme.
func (r Reader) fileMiddlewares(kind dockwellv1.PanelKind) map[string]string {
	out := map[string]string{}
	for _, f := range r.proxyFiles(kind) {
		maps.Copy(out, readTraefikFile(r.panelFile(f)).middlewares)
	}
	return out
}

// proxyFiles are the files of a panel's proxy's dynamic configuration: Coolify's
// proxy/dynamic folder, Dokploy's traefik/dynamic.
func (r Reader) proxyFiles(kind dockwellv1.PanelKind) []string {
	var dir string
	switch kind {
	case dockwellv1.PanelKind_PANEL_KIND_COOLIFY:
		dir = r.folder(coolifyFolder, "proxy", "dynamic")
	case dockwellv1.PanelKind_PANEL_KIND_DOKPLOY:
		dir = r.folder(dokployFolder, "traefik", "dynamic")
	case dockwellv1.PanelKind_PANEL_KIND_UNSPECIFIED:
	}
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(r.Root, dir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if len(out) < maxOwnFiles && e.Type().IsRegular() && (strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
			out = append(out, path.Join(dir, e.Name()))
		}
	}
	return out
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
// each of Dokploy's in traefik/dynamic but its apps', such as dokploy.yml, its
// dashboard's, or one a person wrote. Each stops answering when the proxy hands
// over its ports, and a domain one of them routes doesn't come over, as such a
// file can guard a path of it.
func (r Reader) ownDomains(kind dockwellv1.PanelKind, apps []*dockwellv1.PanelApp) []string {
	var out []string
	for _, f := range r.proxyFiles(kind) {
		name := path.Base(f)
		// Dokploy's app files are its applications' routes, read with them, and its
		// middlewares.yml routes nothing.
		if kind == dockwellv1.PanelKind_PANEL_KIND_DOKPLOY && (name == "middlewares.yml" ||
			slices.ContainsFunc(apps, func(a *dockwellv1.PanelApp) bool { return a.GetSwarm() && name == a.GetId()+".yml" })) {
			continue
		}
		out = append(out, rulesHosts(r.panelFile(f))...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// rulesHosts are the hosts of each Traefik rule in a file of the proxy's.
func rulesHosts(b []byte) []string {
	var out []string
	f := readTraefikFile(b)
	for _, name := range f.order {
		out = append(out, hostsIn(f.routers[name].rule)...)
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

// sortWithTasks sorts an app's containers by name, and its Swarm services' running
// tasks with them, so each service is read from its own task.
func sortWithTasks(a *dockwellv1.PanelApp) {
	if len(a.GetTasks()) != len(a.GetContainers()) {
		slices.Sort(a.Containers)
		return
	}
	order := make([]int, len(a.Containers))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(i, j int) int { return strings.Compare(a.Containers[i], a.Containers[j]) })
	names, tasks := make([]string, len(order)), make([]string, len(order))
	restarts := make([]string, len(a.Restarts))
	for to, from := range order {
		names[to], tasks[to] = a.Containers[from], a.Tasks[from]
		if len(restarts) == len(order) {
			restarts[to] = a.Restarts[from]
		}
	}
	a.Containers, a.Tasks, a.Restarts = names, tasks, restarts
}

// dokployOwns says labels are Dokploy's own on what it runs: it gives each of an
// app's, a database's and a stack's services dokploy.project and
// dokploy.application, among others (buildDokployLabels, Dokploy v0.31). The
// reader and the stopper ask the same, so nothing the reader offers is refused
// at plan.
func dokployOwns(l map[string]string) bool {
	return l["dokploy.application"] != "" || l["dokploy.project"] != ""
}

// swarmLeft says why a Swarm service can't be made again as one container on this
// server, or "": a copy is one container, here, with no Swarm to give it secrets,
// configs or a volume made anew for each task (ADR 0089).
func swarmLeft(s *docker.ServiceSpec, nodes int) string {
	n, replicated := s.Mode.Replicas()
	spec := &s.TaskTemplate.ContainerSpec
	switch {
	case !replicated:
		return s.Name + " runs a task on every node"
	case n > 1:
		return fmt.Sprintf("%s runs %d replicas, and a copy would be one", s.Name, n)
	case nodes < 0:
		return "how many nodes the swarm has can't be read, and a copy runs on this server alone"
	case nodes > 1:
		return fmt.Sprintf("the swarm has %d nodes, and a copy runs on this server alone", nodes)
	case len(spec.Secrets) > 0:
		return s.Name + " reads a Swarm secret, which only Swarm's tasks can"
	case len(spec.Configs) > 0:
		return s.Name + " reads a Swarm config, which only Swarm's tasks can"
	}
	for _, m := range spec.Mounts {
		if m.Type == "volume" && m.Source == "" {
			return fmt.Sprintf("%s keeps data in an anonymous volume at %s, which a copy wouldn't find", s.Name, m.Target)
		}
	}
	return ""
}

// restartOf is when Swarm starts a service's task again: "any" where its spec
// doesn't say, as Swarm's default.
func restartOf(s *docker.ServiceSpec) string {
	return cmp.Or(s.TaskTemplate.RestartPolicy.Condition, "any")
}

// sorted are the apps by name, then ID, each with its containers and domains
// sorted, and each domain once.
func sorted[K comparable](apps map[K]*dockwellv1.PanelApp) []*dockwellv1.PanelApp {
	out := slices.Collect(maps.Values(apps))
	for _, a := range out {
		sortWithTasks(a)
		slices.Sort(a.Domains)
		a.Domains = slices.Compact(a.Domains)
	}
	slices.SortFunc(out, func(a, b *dockwellv1.PanelApp) int {
		return cmp.Or(strings.Compare(a.GetName(), b.GetName()), strings.Compare(a.GetId(), b.GetId()))
	})
	return out
}

// filesLeft says why the panel's proxy's own files keep every domain behind, or
// "": a file Traefik's file provider loads (.yml, .yaml or .toml) that can't be read
// in full as Dockwell reads them (a .toml one, or one past the size cap), a link
// or a folder there, more of them than maxOwnFiles, or a TLS option among them
// that asks clients for a certificate. What Traefik doesn't load, such as the
// acme.json it keeps certificates in, guards nothing.
func (r Reader) filesLeft(kind dockwellv1.PanelKind) string {
	dir := proxyDir(kind)
	if r.Root == "" || dir == "" {
		return ""
	}
	fi, err := os.Lstat(filepath.Join(r.Root, dir))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ""
	case err != nil || !fi.IsDir():
		return fmt.Sprintf("the proxy's folder %s can't be read as a folder", dir)
	}
	entries, err := os.ReadDir(filepath.Join(r.Root, dir))
	if err != nil {
		return fmt.Sprintf("the proxy's folder %s can't be read", dir)
	}
	loaded := 0
	for _, e := range entries {
		name, at := e.Name(), path.Join(dir, e.Name())
		ext := strings.ToLower(filepath.Ext(name))
		switch {
		case !e.Type().IsRegular():
			return fmt.Sprintf("the proxy's folder holds %s, which isn't a file Dockwell reads", at)
		case ext == ".toml":
			return fmt.Sprintf("the proxy's file %s is TOML, which Dockwell doesn't read", at)
		case ext != ".yml" && ext != ".yaml":
			continue
		}
		loaded++
		if loaded > maxOwnFiles {
			return fmt.Sprintf("the proxy's folder %s holds more files than Dockwell reads", dir)
		}
		b, ok := r.panelFileFull(at)
		if !ok {
			return fmt.Sprintf("the proxy's file %s can't be read in full", at)
		}
		if strings.Contains(strings.ToLower(string(b)), "clientauth") {
			return fmt.Sprintf("a TLS option in the proxy's file %s asks clients for a certificate", at)
		}
	}
	return ""
}

// proxyDir is the folder of a panel's proxy's dynamic configuration.
func proxyDir(kind dockwellv1.PanelKind) string {
	switch kind {
	case dockwellv1.PanelKind_PANEL_KIND_COOLIFY:
		return path.Join(coolifyFolder, "proxy", "dynamic")
	case dockwellv1.PanelKind_PANEL_KIND_DOKPLOY:
		return path.Join(dokployFolder, "traefik", "dynamic")
	case dockwellv1.PanelKind_PANEL_KIND_UNSPECIFIED:
	}
	return ""
}

// dokployStatic is where Dokploy keeps its Traefik's static configuration, which
// its container reads as /etc/traefik/traefik.yml.
var dokployStatic = path.Join(dokployFolder, "traefik", "traefik.yml")

// staticLeft says why the proxy's static configuration keeps every domain behind,
// or "": its container's command and environment, and for Dokploy its
// traefik.yml. A middleware on an entrypoint runs for every domain, so one that
// isn't harmless, or configuration Dockwell can't read, keeps them all behind.
func (r Reader) staticLeft(ctx context.Context, kind dockwellv1.PanelKind, files map[string]string) string {
	proxy := resource.PanelProxy(resourcepbKind(kind))
	c, err := r.Docker.InspectContainer(ctx, proxy)
	if docker.IsNotFound(err) {
		if _, err := r.Docker.InspectService(ctx, proxy); err == nil {
			return proxy + " runs as a Swarm service, whose static configuration Dockwell doesn't read"
		}
		return ""
	}
	if err != nil {
		return proxy + "'s static configuration can't be read"
	}
	// Only Traefik's official image is read: another proxy, or a Traefik built
	// otherwise, can be configured in ways this reader doesn't know.
	if !officialTraefik(c.Config.Image) {
		return fmt.Sprintf("%s runs %s, and Dockwell reads the static configuration of Traefik's official image alone", proxy, c.Config.Image)
	}
	if why := staticMountsLeft(kind, c); why != "" {
		return why
	}
	for _, f := range flagsOf(slices.Concat(c.Config.Entrypoint, c.Config.Cmd)) {
		if why := flagLeft(kind, c, f[0], f[1], files); why != "" {
			return why
		}
	}
	for _, env := range c.Config.Env {
		key, value, _ := strings.Cut(env, "=")
		up := strings.ToUpper(key)
		if !strings.HasPrefix(up, "TRAEFIK_") {
			continue
		}
		// Traefik reads TRAEFIK_A_B as --a.b.
		flag := "--" + strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(up, "TRAEFIK_"), "_", "."))
		if why := flagLeft(kind, c, flag, strings.ToLower(value), files); why != "" {
			return why
		}
	}
	if kind == dockwellv1.PanelKind_PANEL_KIND_DOKPLOY {
		b, ok := r.panelFileFull(dokployStatic)
		if !ok {
			return "Dokploy's traefik.yml can't be read in full, so what its entrypoints do to every domain can't be told"
		}
		low := strings.ToLower(string(b))
		if strings.Contains(low, "clientauth") || strings.Contains(low, "options:") {
			return "Dokploy's traefik.yml sets TLS options, which can ask clients for a certificate"
		}
		if strings.Contains(low, "defaultrule") {
			return "Dokploy's traefik.yml gives containers a default rule, which routes domains no label names"
		}
		for _, f := range staticProviders(b) {
			if why := flagLeft(kind, c, f[0], f[1], files); why != "" {
				return why
			}
		}
		if why := entryMiddlewaresLeft(staticEntryMiddlewares(b), files); why != "" {
			return why
		}
	}
	return ""
}

// resourcepbKind is a panel's word, as the resource package names it.
func resourcepbKind(kind dockwellv1.PanelKind) string {
	switch kind {
	case dockwellv1.PanelKind_PANEL_KIND_COOLIFY:
		return resource.PanelCoolify
	case dockwellv1.PanelKind_PANEL_KIND_DOKPLOY:
		return resource.PanelDokploy
	case dockwellv1.PanelKind_PANEL_KIND_UNSPECIFIED:
	}
	return ""
}

// entryMiddlewaresLeft says why the middlewares an entrypoint puts on every
// request keep every domain behind, or "": only one defined in the proxy's own
// files whose kind is a redirect to HTTPS or compression is let by.
func entryMiddlewaresLeft(list []string, files map[string]string) string {
	for _, m := range list {
		ref := strings.ToLower(strings.TrimSpace(m))
		name, provider, _ := strings.Cut(ref, "@")
		if name == "" {
			continue
		}
		if kind := files[name]; provider == "file" && (kind == "redirectscheme" || kind == "compress") {
			continue
		}
		return fmt.Sprintf("the proxy passes every request on an entrypoint through the middleware %s, which a route would drop", ref)
	}
	return ""
}

// staticEntryMiddlewares are the middlewares a traefik.yml puts on its
// entrypoints, from entryPoints.<name>.http.middlewares.
func staticEntryMiddlewares(b []byte) []string {
	var out []string
	in, list := false, false
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if indent == 0 {
			in, list = strings.EqualFold(strings.TrimSuffix(trimmed, ":"), "entryPoints"), false
			continue
		}
		if !in {
			continue
		}
		key, value, _ := strings.Cut(trimmed, ":")
		switch {
		case list && strings.HasPrefix(trimmed, "- "):
			out = append(out, strings.Trim(strings.TrimSpace(trimmed[2:]), `"'`))
		case strings.EqualFold(key, "middlewares"):
			list = true
			for _, m := range strings.Split(strings.Trim(strings.TrimSpace(value), "[]"), ",") {
				if m = strings.Trim(strings.TrimSpace(m), `"'`); m != "" {
					out = append(out, m)
				}
			}
		default:
			list = false
		}
	}
	return out
}

// panelFileFull is panelFile, and false where the file can't be read in full: a
// link, not a regular file, or past the size cap.
func (r Reader) panelFileFull(rel string) ([]byte, bool) {
	if r.Root == "" {
		return nil, false
	}
	f, err := os.OpenFile(filepath.Join(r.Root, rel), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // a fixed path under a panel's folder, refused where it's a link
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() || fi.Size() > maxDynamic {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxDynamic+1))
	if err != nil || len(b) > maxDynamic {
		return nil, false
	}
	return b, true
}

// officialTraefik says an image is Traefik's official one, traefik:<tag> on Docker
// Hub, as Coolify and Dokploy run.
func officialTraefik(image string) bool {
	name, _, _ := strings.Cut(strings.ToLower(image), "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	switch name {
	case "traefik", "library/traefik", "docker.io/library/traefik", "index.docker.io/library/traefik", "docker.io/traefik":
		return true
	}
	return false
}

// staticMountsLeft says why a mount of the proxy's keeps every domain behind, or "":
// one over a path Traefik looks for its static configuration in, or a folder of
// it, as such a file would configure the proxy where Dockwell doesn't read. Dokploy's
// own bind of its traefik.yml is the one let by.
func staticMountsLeft(kind dockwellv1.PanelKind, c *docker.Container) string {
	home := "/root"
	for _, env := range c.Config.Env {
		if v, ok := strings.CutPrefix(env, "HOME="); ok && v != "" {
			home = v
		}
	}
	work := cmp.Or(c.Config.WorkingDir, "/")
	var places []string
	for _, dir := range []string{"/etc/traefik", path.Join(home, ".config"), work} {
		for _, ext := range []string{"yml", "yaml", "toml"} {
			places = append(places, path.Join(dir, "traefik."+ext))
		}
	}
	for _, m := range c.Mounts {
		dest := path.Clean("/" + m.Destination)
		if kind == dockwellv1.PanelKind_PANEL_KIND_DOKPLOY && dest == "/etc/traefik/traefik.yml" && path.Clean(m.Source) == dokployStatic {
			continue
		}
		for _, place := range places {
			if dest == place || dest == "/" || strings.HasPrefix(place, dest+"/") {
				return fmt.Sprintf("a mount of the proxy's, %s, covers %s, where Traefik looks for its static configuration", dest, place)
			}
		}
	}
	return ""
}

// flagsOf reads a command's flags as Traefik does: --key=value, or --key value
// where no = follows and the next argument isn't a flag, or --key alone for true.
// Keys and values are lower case.
func flagsOf(args []string) [][2]string {
	var out [][2]string
	for i := 0; i < len(args); i++ {
		a := strings.TrimSpace(args[i])
		if !strings.HasPrefix(a, "-") {
			continue
		}
		key, value, ok := strings.Cut(a, "=")
		if !ok {
			value = "true"
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				value = args[i+1]
				i++
			}
		}
		key = "--" + strings.TrimLeft(strings.ToLower(key), "-")
		out = append(out, [2]string{key, strings.ToLower(value)})
	}
	return out
}

// flagLeft says why one setting of the proxy's static configuration keeps every
// domain behind, or "": a configuration file Dockwell doesn't read, a middleware
// or TLS options on an entrypoint, a default rule, or a provider of routes other
// than Docker, Swarm and the file folder Dockwell reads.
func flagLeft(kind dockwellv1.PanelKind, c *docker.Container, key, value string, files map[string]string) string {
	proxy := resource.PanelProxy(resourcepbKind(kind))
	switch {
	case key == "--configfile":
		if kind != dockwellv1.PanelKind_PANEL_KIND_DOKPLOY || value != "/etc/traefik/traefik.yml" {
			return proxy + " reads a static configuration file Dockwell doesn't read"
		}
	case strings.HasPrefix(key, "--entrypoints.") && strings.HasSuffix(key, ".http.middlewares"):
		return entryMiddlewaresLeft(strings.Split(value, ","), files)
	case strings.HasPrefix(key, "--entrypoints.") && strings.Contains(key, ".http.tls.options"):
		return proxy + "'s entrypoints set TLS options, which can ask clients for a certificate"
	case strings.HasPrefix(key, "--providers."):
		rest := strings.TrimPrefix(key, "--providers.")
		name, sub, _ := strings.Cut(rest, ".")
		switch name {
		case "providersthrottleduration":
		case "docker", "swarm":
			if strings.Contains(sub, "defaultrule") {
				return proxy + " gives containers a default rule, which routes domains no label names"
			}
		case "file":
			switch sub {
			case "", "watch":
			case "directory":
				if dir := hostPathOf(c, strings.TrimSuffix(value, "/")); dir != proxyDir(kind) {
					return fmt.Sprintf("%s reads routes from the folder %s, which isn't the one Dockwell reads", proxy, value)
				}
			default:
				return fmt.Sprintf("%s reads routes from a file Dockwell doesn't read (%s)", proxy, key)
			}
		default:
			return fmt.Sprintf("%s takes routes from the provider %s too, which Dockwell doesn't read", proxy, name)
		}
	}
	return ""
}

// hostPathOf is where a path in the proxy's container is on the server, through
// its mounts, or "" where no mount holds it.
func hostPathOf(c *docker.Container, p string) string {
	best, at := -1, ""
	for _, m := range c.Mounts {
		dest := path.Clean("/" + m.Destination)
		if (p == dest || strings.HasPrefix(p, dest+"/")) && len(dest) > best {
			best, at = len(dest), path.Join(m.Source, strings.TrimPrefix(p, dest))
		}
	}
	return at
}

// staticProviders are the providers a traefik.yml sets, each as the flag that
// would set it: providers.<name>.<key> and its value, or providers.<name> alone.
func staticProviders(b []byte) [][2]string {
	var out [][2]string
	in, nameIndent, name := false, -1, ""
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 {
			in, nameIndent, name = strings.EqualFold(strings.TrimSuffix(trimmed, ":"), "providers"), -1, ""
			continue
		}
		if !in {
			continue
		}
		key, value, _ := strings.Cut(trimmed, ":")
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.ToLower(strings.Trim(strings.TrimSpace(value), `"'`))
		if nameIndent < 0 || indent <= nameIndent {
			nameIndent, name = indent, key
			out = append(out, [2]string{"--providers." + name, cmp.Or(value, "true")})
			continue
		}
		out = append(out, [2]string{"--providers." + name + "." + key, value})
	}
	return out
}

// publishedOf are the ports a Swarm service publishes, for its copy named service.
func publishedOf(service string, s *docker.ServiceSpec) []*dockwellv1.PanelPort {
	var out []*dockwellv1.PanelPort
	for _, p := range s.EndpointSpec.Ports {
		if p.PublishedPort != 0 && p.TargetPort != 0 {
			out = append(out, &dockwellv1.PanelPort{Service: service, Port: p.TargetPort, Published: p.PublishedPort, Protocol: cmp.Or(p.Protocol, "tcp")})
		}
	}
	return out
}
