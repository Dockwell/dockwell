// SPDX-License-Identifier: AGPL-3.0-only

package panels

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// The domain handover's evidence (ADR 0089, "The domain handover: evidence and a
// choice"). A reader can't prove a domain unguarded by re-implementing a proxy's
// configuration, so each domain is checked three ways, and a person chooses from
// what nothing was found for: a broad scan of every label on the server and every
// file in the proxy's mounts for another rule that mentions it; the proxy's static
// configuration, as staticLeft reads it; and a request through the proxy for / and
// each path a rule names for it. The agent checks the chosen domains again just
// before it stops the proxy, and refuses where the digest of what it found differs
// from the plan's.

// maxScanFiles is how many files of the proxy's mounts the scan reads, and
// maxScanDepth how deep it goes: more holds every domain back.
const (
	maxScanFiles = 512
	maxScanDepth = 8
)

// mention is a rule that names a domain: where it is, and who it belongs to.
type mention struct {
	// owner is what it belongs to: "container:<name>", "service:<name>",
	// "file:<path>" or "proxy".
	owner string
	// where says it for people, such as "the label traefik.http.routers.a.rule on
	// web-1".
	where string
}

// hostPattern is a HostRegexp rule, or a Caddy wildcard, that can match a domain.
type hostPattern struct {
	re    *regexp.Regexp
	where string
	owner string
}

// scan is what the broad scan found: each domain any rule names, with where, the
// paths a rule names with it, the patterns that can match one, and why every
// domain stays behind, where something couldn't be read.
type scan struct {
	hosts    map[string][]mention
	paths    map[string][]string
	patterns []hostPattern
	held     string
	// What it read, for people.
	containers, services, files int
}

// The forms a rule names a host in, from a label's value or a file's line. Each is
// read case-blind, as Traefik reads its matchers, and HostSNI and Traefik 1's
// Host: count too, as each can route a domain.
var (
	hostCall   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\bHost(?:SNI)?\s*\(([^)]*)\)`) })
	regexpCall = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\bHost(?:SNI)?Regexp\s*\(([^)]*)\)`) })
	hostColon  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\bHost(?:Regexp)?:\s*([^;]+)`) })
	pathCall   = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\bPath(?:Prefix|Regexp)?\s*\(([^)]*)\)`)
	})
	// caddySite is a Caddyfile line that opens a site's block, at the start of a
	// line: its addresses, then {.
	caddySite = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^([^\s#{}][^{}]*?)\s*\{\s*$`) })
	// caddyMatcher is a path a Caddyfile directive matches, such as handle /admin*.
	caddyMatcher = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\s+[a-z_]+\s+(/[^\s{]*)`) })
	// v2Var is a variable in a Traefik 2 HostRegexp, {name:pattern} or {name}.
	v2Var = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\{[a-zA-Z0-9_]+(?::((?:[^{}]|\{[^{}]*\})*))?\}`) })
)

// quotedIn are the quoted strings in a matcher's arguments.
func quotedIn(args string) []string {
	var out []string
	for _, m := range quoted().FindAllStringSubmatch(args, -1) {
		out = append(out, m[1])
	}
	return out
}

// rulesIn reads a text, a label's value or a file's line, for the hosts, the
// patterns and the paths its rules name.
func rulesIn(text string) (hosts, patterns, paths []string) {
	for _, m := range hostCall().FindAllStringSubmatch(text, -1) {
		for _, q := range quotedIn(m[1]) {
			hosts = append(hosts, hostsOrPattern(q, &patterns)...)
		}
	}
	for _, m := range regexpCall().FindAllStringSubmatch(text, -1) {
		patterns = append(patterns, quotedIn(m[1])...)
	}
	for _, m := range hostColon().FindAllStringSubmatch(text, -1) {
		for _, h := range strings.Split(m[1], ",") {
			hosts = append(hosts, hostsOrPattern(strings.TrimSpace(h), &patterns)...)
		}
	}
	for _, m := range pathCall().FindAllStringSubmatch(text, -1) {
		for _, q := range quotedIn(m[1]) {
			paths = append(paths, literalPath(q))
		}
	}
	return hosts, patterns, paths
}

// hostsOrPattern is a named host, lower case, or, for a wildcard, a pattern.
func hostsOrPattern(h string, patterns *[]string) []string {
	h = strings.ToLower(strings.TrimSpace(h))
	if strings.Contains(h, "*") {
		*patterns = append(*patterns, "^"+strings.ReplaceAll(regexp.QuoteMeta(h), `\*`, `[^.]+`)+"$")
		return nil
	}
	if host := hostOf(h); host != "" {
		return []string{host}
	}
	return nil
}

// literalPath is the literal start of a path a rule names: up to the first
// character a pattern or a Caddy matcher gives meaning, such as /api of /api/*.
func literalPath(p string) string {
	if i := strings.IndexAny(p, "*?[({^$\\|+"); i >= 0 {
		p = p[:i]
	}
	if !strings.HasPrefix(p, "/") {
		return "/"
	}
	return p
}

// matcherOf compiles a HostRegexp's pattern as Traefik 3 reads it, a Go regular
// expression, or as Traefik 2 does, with {name:pattern} variables. False where it
// reads as neither: such a rule could match any domain.
func matcherOf(p string) (*regexp.Regexp, bool) {
	if re, err := regexp.Compile("(?i)" + p); err == nil && !v2Var().MatchString(p) {
		return re, true
	}
	var b strings.Builder
	last := 0
	for _, m := range v2Var().FindAllStringSubmatchIndex(p, -1) {
		b.WriteString(regexp.QuoteMeta(p[last:m[0]]))
		if m[2] >= 0 {
			b.WriteString("(?:" + p[m[2]:m[3]] + ")")
		} else {
			b.WriteString(`[^.]+`)
		}
		last = m[1]
	}
	b.WriteString(regexp.QuoteMeta(p[last:]))
	re, err := regexp.Compile("(?i)^" + b.String() + "$")
	return re, err == nil
}

// add records the rules a text names, as owner's, at where.
func (s *scan) add(text, owner, where string) {
	hosts, patterns, paths := rulesIn(text)
	for _, h := range hosts {
		s.hosts[h] = append(s.hosts[h], mention{owner: owner, where: where})
		s.paths[h] = append(s.paths[h], paths...)
	}
	for _, p := range patterns {
		re, ok := matcherOf(p)
		if !ok {
			s.held = cmp.Or(s.held, fmt.Sprintf("a HostRegexp rule in %s can't be read, so it could match any domain", where))
			continue
		}
		s.patterns = append(s.patterns, hostPattern{re: re, where: where, owner: owner})
	}
}

// addCaddy records the sites a Caddy label names, caddy or caddy_<n>, and the paths
// its directives on the same owner match.
func (s *scan) addCaddy(labels map[string]string, owner, who string) {
	for k, v := range labels {
		if !caddyLabel().MatchString(k) {
			continue
		}
		var paths []string
		for k2, v2 := range labels {
			if strings.HasPrefix(k2, k+".") && strings.HasPrefix(strings.TrimSpace(v2), "/") {
				paths = append(paths, literalPath(strings.Fields(v2)[0]))
			}
		}
		var patterns []string
		where := fmt.Sprintf("the label %s on %s", k, who)
		for _, a := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			for _, h := range hostsOrPattern(stripCaddyScheme(a), &patterns) {
				s.hosts[h] = append(s.hosts[h], mention{owner: owner, where: where})
				s.paths[h] = append(s.paths[h], paths...)
			}
		}
		for _, p := range patterns {
			re, _ := matcherOf(p)
			s.patterns = append(s.patterns, hostPattern{re: re, where: where, owner: owner})
		}
	}
}

// stripCaddyScheme is a Caddy site address without its scheme and path, as
// hostOf reads one, keeping a wildcard.
func stripCaddyScheme(a string) string {
	if _, rest, ok := strings.Cut(a, "://"); ok {
		a = rest
	}
	a, _, _ = strings.Cut(a, "/")
	return a
}

// addLabels scans a set of labels, each as key=value, as owner's.
func (s *scan) addLabels(labels map[string]string, owner, who string) {
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		s.add(k+"="+labels[k], owner, fmt.Sprintf("the label %s on %s", k, who))
	}
	s.addCaddy(labels, owner, who)
}

// addFile scans a file of the proxy's, line by line: each rule a line names, and
// each Caddyfile site a block opens, with the paths its directives match.
func (s *scan) addFile(at string, b []byte) {
	owner, where := "file:"+at, "the file "+at
	var site []string
	for _, line := range strings.Split(string(b), "\n") {
		s.add(line, owner, where)
		if m := caddySite().FindStringSubmatch(line); m != nil {
			site = nil
			var patterns []string
			for _, a := range strings.FieldsFunc(m[1], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
				for _, h := range hostsOrPattern(stripCaddyScheme(a), &patterns) {
					s.hosts[h] = append(s.hosts[h], mention{owner: owner, where: where})
					site = append(site, h)
				}
			}
			for _, p := range patterns {
				re, _ := matcherOf(p)
				s.patterns = append(s.patterns, hostPattern{re: re, where: where, owner: owner})
			}
			continue
		}
		if strings.TrimSpace(line) == "}" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			site = nil
			continue
		}
		if m := caddyMatcher().FindStringSubmatch(line); m != nil {
			for _, h := range site {
				s.paths[h] = append(s.paths[h], literalPath(m[1]))
			}
		}
	}
}

// scanServer reads every label on every container and Swarm service on the
// server, the proxy's own command and environment, and every file in the
// proxy's mounts, for the rules each names.
func (r Reader) scanServer(ctx context.Context, kind dockwellv1.PanelKind) scan {
	s := scan{hosts: map[string][]mention{}, paths: map[string][]string{}}
	list, err := r.Docker.ListContainers(ctx, nil)
	if err != nil {
		s.held = "the containers on the server can't be listed, so what else routes a domain can't be told"
		return s
	}
	for i := range list {
		name := containerName(&list[i])
		s.addLabels(list[i].Labels, "container:"+name, name)
		s.containers++
	}
	services, err := r.Docker.ListServices(ctx)
	if err != nil {
		s.held = "the Swarm services on the server can't be listed, so what else routes a domain can't be told"
		return s
	}
	for i := range services {
		sp := &services[i].Spec
		s.addLabels(sp.Labels, "service:"+sp.Name, "the service "+sp.Name)
		s.addLabels(sp.TaskTemplate.ContainerSpec.Labels, "service:"+sp.Name, "the service "+sp.Name)
		s.services++
	}
	proxy := resource.PanelProxy(resourcepbKind(kind))
	c, err := r.Docker.InspectContainer(ctx, proxy)
	switch {
	case docker.IsNotFound(err):
		return s
	case err != nil:
		s.held = cmp.Or(s.held, proxy+" can't be read, so what its own files route can't be told")
		return s
	}
	for _, a := range slices.Concat(c.Config.Entrypoint, c.Config.Cmd, c.Config.Env) {
		s.add(a, "proxy", proxy+"'s own command and environment")
	}
	if r.Root == "" && len(c.Mounts) > 0 {
		s.held = cmp.Or(s.held, "the files in "+proxy+"'s mounts can't be read on this agent")
		return s
	}
	for _, m := range c.Mounts {
		if m.Type != "bind" && m.Type != "volume" || m.Source == "" {
			continue
		}
		if why := r.scanMount(&s, m.Source); why != "" {
			s.held = cmp.Or(s.held, why)
		}
	}
	return s
}

// scanMount reads each file under a mount's source, on the server: a link, a file
// past the size cap, or more files than the scan reads holds every domain back. A
// socket, a pipe or a device holds no rule, and neither do what Traefik keeps its
// certificates and logs in, which aren't read.
func (r Reader) scanMount(s *scan, source string) string {
	root := filepath.Join(r.Root, filepath.Clean("/"+source))
	fi, err := os.Lstat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ""
	case err != nil:
		return fmt.Sprintf("the proxy's mount %s can't be read", source)
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Sprintf("the proxy's mount %s is a link, which Dockwell doesn't follow", source)
	case fi.Mode().IsRegular():
		return r.scanOne(s, source, root, fi)
	case !fi.IsDir():
		return ""
	}
	why := ""
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			why = fmt.Sprintf("the proxy's mount %s can't be read in full", source)
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(root, p)
		at := path.Join(source, filepath.ToSlash(rel))
		if d.IsDir() {
			if rel != "." && strings.Count(filepath.ToSlash(rel), "/") >= maxScanDepth {
				why = fmt.Sprintf("the proxy's mount %s goes deeper than Dockwell reads", source)
				return filepath.SkipAll
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			why = fmt.Sprintf("the proxy's mount holds a link, %s, which Dockwell doesn't follow", at)
			return filepath.SkipAll
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			why = fmt.Sprintf("the proxy's file %s can't be read", at)
			return filepath.SkipAll
		}
		if why = r.scanOne(s, at, p, info); why != "" {
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil && why == "" {
		why = fmt.Sprintf("the proxy's mount %s can't be read in full", source)
	}
	return why
}

// notRules are files in a proxy's mounts that hold no rule: Traefik's and Caddy's
// certificates, keys and logs.
func notRules(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".log", ".crt", ".key", ".pem":
		return true
	}
	return name == "acme.json"
}

// scanOne reads one regular file of the proxy's mounts, at its path on the server.
func (r Reader) scanOne(s *scan, at, onDisk string, fi fs.FileInfo) string {
	if notRules(fi.Name()) {
		return ""
	}
	s.files++
	if s.files > maxScanFiles {
		return "the proxy's mounts hold more files than Dockwell reads"
	}
	if fi.Size() > maxDynamic {
		return fmt.Sprintf("the proxy's file %s is larger than Dockwell reads", at)
	}
	b, ok := readFull(onDisk)
	if !ok {
		return fmt.Sprintf("the proxy's file %s can't be read in full", at)
	}
	s.addFile(at, b)
	return ""
}

// readFull reads a regular file in full, refusing a link or one past the cap.
func readFull(onDisk string) ([]byte, bool) {
	r := Reader{Root: "/"}
	return r.panelFileFull(onDisk)
}

// checks checks each domain a panel's proxy serves for a handover: what layer one
// read of its route, then the scan, then the probe, each holding it back where it
// finds anything. Only what nothing held is asked for over the network.
func (r Reader) checks(ctx context.Context, kind dockwellv1.PanelKind, p *dockwellv1.Panel) []resource.PanelCheck {
	sc := r.scanServer(ctx, kind)
	summary := fmt.Sprintf("labels on %d containers and %d services, and %d files in the proxy's mounts", sc.containers, sc.services, sc.files)
	// What the imported app's own route is: its containers and services, and for a
	// Swarm app Dokploy routes from a file, that file.
	own := map[string]map[string]bool{}
	routed := map[string]string{}
	allow := func(d, owner string) {
		if own[d] == nil {
			own[d] = map[string]bool{}
		}
		own[d][owner] = true
	}
	var domains []string
	for _, a := range p.GetApps() {
		for _, d := range a.GetDomains() {
			domains = append(domains, strings.ToLower(d))
		}
		for _, rt := range a.GetRoutes() {
			d := strings.ToLower(rt.GetDomain())
			domains = append(domains, d)
			if rt.GetLeft() != "" {
				routed[d] = cmp.Or(routed[d], rt.GetLeft())
				continue
			}
			if _, ok := routed[d]; !ok {
				routed[d] = ""
			}
			allow(d, "container:"+rt.GetContainer())
			if a.GetSwarm() {
				for _, svc := range a.GetContainers() {
					allow(d, "service:"+svc)
				}
				if dir := proxyDir(kind); dir != "" {
					allow(d, "file:"+path.Join(dir, a.GetId()+".yml"))
				}
			}
		}
	}
	for _, d := range p.GetDomains() {
		domains = append(domains, strings.ToLower(d))
	}
	slices.Sort(domains)
	domains = slices.Compact(domains)
	out := make([]resource.PanelCheck, 0, len(domains))
	var asks []probeAsk
	for _, d := range domains {
		c := resource.PanelCheck{Domain: d, Scanned: summary}
		for _, m := range sc.hosts[d] {
			if !own[d][m.owner] && !slices.Contains(c.Mentions, m.where) {
				c.Mentions = append(c.Mentions, m.where)
			}
		}
		for _, pt := range sc.patterns {
			if pt.re.MatchString(d) && !own[d][pt.owner] && !slices.Contains(c.Mentions, pt.where) {
				c.Mentions = append(c.Mentions, pt.where)
			}
		}
		left, isRouted := routed[d]
		switch {
		case p.GetProxyLeft() != "":
			c.Held = p.GetProxyLeft()
		case sc.held != "":
			c.Held = sc.held
		case slices.Contains(p.GetDomains(), d):
			c.Held = "the panel's own files route it, and they don't come over"
		case !isRouted:
			c.Held = "Dockwell can't read where it goes"
		case left != "":
			c.Held = left
		case len(c.Mentions) > 0:
			c.Held = "another rule mentions it: " + c.Mentions[0]
		}
		paths := probePaths(sc.paths[d])
		if c.Held == "" && len(paths) > mostProbePaths {
			c.Held = fmt.Sprintf("its rules name %d paths, more than Dockwell asks for", len(paths))
		}
		if c.Held == "" {
			for _, pa := range paths {
				asks = append(asks, probeAsk{check: len(out), domain: d, path: pa})
			}
		}
		out = append(out, c)
	}
	for i, res := range r.probeAll(ctx, asks) {
		c := &out[asks[i].check]
		c.Probes = append(c.Probes, res)
		if !res.Open && c.Held == "" {
			c.Held = fmt.Sprintf("a request for %s through the proxy answered %s", res.Path, res.Answer)
		}
	}
	return out
}

// mostProbePaths is how many paths of a domain are asked for, / and the paths its
// rules name: a domain with more stays behind.
const mostProbePaths = 8

// probePaths are / and the paths any rule names for a domain, each once.
func probePaths(named []string) []string {
	out := []string{"/"}
	for _, p := range named {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out[1:])
	return out
}

// Check reads the panel again and checks domains as a handover's plan chose them,
// just before the proxy stops: PanelEvidence of what it found, or an error naming
// a domain that stays behind now (ADR 0089).
func (r Reader) Check(ctx context.Context, kind string, domains []string) (string, error) {
	pk := resourcepb.PanelKind(kind)
	out, err := r.Read(ctx, pk, true)
	if err != nil {
		return "", err
	}
	if out.GetFound() == nil {
		return "", fmt.Errorf("%s doesn't run here now: %s", resource.PanelName(kind), out.GetNotHere())
	}
	checks := resourcepb.PanelChecksFrom(out.GetFound().GetChecks())
	for _, d := range domains {
		i := slices.IndexFunc(checks, func(c resource.PanelCheck) bool { return strings.EqualFold(c.Domain, d) })
		switch {
		case i < 0:
			return "", fmt.Errorf("%s's proxy doesn't serve %s now", resource.PanelName(kind), d)
		case checks[i].Held != "":
			return "", fmt.Errorf("%s stays behind now: %s", d, checks[i].Held)
		}
	}
	return resource.PanelEvidence(checks, domains), nil
}
