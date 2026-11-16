// SPDX-License-Identifier: AGPL-3.0-only

package panels

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// A domain whose router redirects it to its other half, www.example.com to
// example.com or the other way round, keeping the path, comes over as a route that
// answers with that redirect (ADR 0070): Coolify writes one as a redirectregex in
// labels, or a redir in Caddy's, and Dokploy as a redirectRegex in its own files.
// Each is read by what it does to a request, as Traefik and Caddy would do it; one
// that does anything else keeps its domain behind.

// wwwRegexes are the regular expressions of the redirects to and from www the
// panels write: Coolify's, and Dokploy's presets as it writes them and escaped.
var wwwRegexes = []string{
	`^(http|https)://www\.(.+)`, `^(http|https)://(?:www\.)?(.+)`,
	`^https?://www.(.+)`, `^https?://www\.(.+)`, `^https?://(?:www.)?(.+)`, `^https?://(?:www\.)?(.+)`,
}

// otherHalf is the www name of a domain, or the domain of a www name.
func otherHalf(host string) string {
	if rest, ok := strings.CutPrefix(host, "www."); ok {
		return rest
	}
	return "www." + host
}

// regexRedirect says what a redirectregex does to a request for host, as Traefik
// does it: matched against the request's whole URL, and replaced, it redirects
// where what it gives differs. applies is false where it leaves every request as
// it is; why says what a route can't say, where it does something else than send
// each request to the other half of host, path and query kept.
func regexRedirect(host, regex, replacement, permanent string) (rd *resourcev1.RouteRedirect, applies bool, why string) {
	if !slices.Contains(wwwRegexes, regex) {
		return nil, false, fmt.Sprintf("it redirects by %s, which isn't a redirect to or from www Dockwell can read", regex)
	}
	re, err := regexp.Compile(regex)
	if err != nil {
		return nil, false, fmt.Sprintf("its redirect's expression %s doesn't compile", regex)
	}
	to := "https://" + otherHalf(host)
	moved := 0
	for _, rest := range []string{"/", "/a/b?c=d"} {
		before := "https://" + host + rest
		if !re.MatchString(before) {
			continue
		}
		after := re.ReplaceAllString(before, replacement)
		if after == before {
			continue
		}
		if after != to+rest {
			return nil, false, fmt.Sprintf("it redirects %s to %s, not to %s keeping the path", before, after, to+rest)
		}
		moved++
	}
	switch moved {
	case 0:
		return nil, false, ""
	case 1:
		return nil, false, "its redirect sends some requests on and leaves others, which a route can't say"
	}
	return &resourcev1.RouteRedirect{To: to, Temporary: !truthy(permanent)}, true, ""
}

// truthy reads a Traefik boolean, from a label or a file, as Traefik does.
func truthy(v string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	return err == nil && b
}

// caddyTarget is a redir's address as Coolify writes one: a scheme or {scheme},
// a host, then {uri}, and permanent or not.
var caddyTarget = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(?:https?|\{scheme\})://([a-z0-9.-]+)\{uri\}( permanent)?$`)
})

// caddyRedirectOf says what a site's redir does to a request for host: send it to
// the other half of host, keeping the path, or something a route can't say.
func caddyRedirectOf(host, redir string) (*resourcev1.RouteRedirect, string) {
	m := caddyTarget().FindStringSubmatch(strings.TrimSpace(redir))
	switch {
	case m == nil:
		return nil, "its Caddy labels redirect it in a way a route can't say"
	case m[1] == host:
		return nil, "its Caddy labels redirect it to itself"
	case m[1] != otherHalf(host):
		return nil, fmt.Sprintf("its Caddy labels redirect it to %s, which isn't its www name or the domain of it", m[1])
	}
	return &resourcev1.RouteRedirect{To: "https://" + m[1], Temporary: m[2] == ""}, ""
}

// plainEntrypoints are the entrypoints that answer plain HTTP on port 80, as
// Coolify and Dokploy name them.
var plainEntrypoints = []string{"http", "web"}

// plainOnly reports a router on plain HTTP's entrypoints alone: one with none
// named is on every entrypoint, HTTPS's included.
func plainOnly(entrypoints []string) bool {
	if len(entrypoints) == 0 {
		return false
	}
	for _, e := range entrypoints {
		if !slices.Contains(plainEntrypoints, strings.ToLower(strings.TrimSpace(e))) {
			return false
		}
	}
	return true
}

// routerRedirect reads where a Traefik router's middlewares redirect a request for
// host, in their order, as the first that redirects answers: where each is one
// middlewaresLeft let by. own is the provider the router is in, where a middleware
// named without one is looked for. upgrades says it's on plain HTTP alone and sends
// it to HTTPS, which says nothing of what's served over HTTPS; on HTTPS, a
// redirectscheme does nothing.
func routerRedirect(l map[string]string, list []string, files map[string]string, own, host string, entrypoints []string) (rd *resourcev1.RouteRedirect, upgrades bool, why string) {
	for _, m := range list {
		name, provider, _ := strings.Cut(strings.ToLower(strings.TrimSpace(m)), "@")
		if name == "" {
			continue
		}
		defs, kind := middlewareOf(l, files, own, name, provider)
		at := "traefik.http.middlewares." + name + "."
		switch kind {
		case "redirectscheme":
			upgrades = plainOnly(entrypoints)
		case "redirectregex":
			rd, applies, why := regexRedirect(host, defs[at+"redirectregex.regex"], defs[at+"redirectregex.replacement"], defs[at+"redirectregex.permanent"])
			if why != "" {
				return nil, upgrades, why
			}
			if applies {
				return rd, upgrades, ""
			}
		}
	}
	return nil, upgrades, ""
}

// middlewareOf finds a middleware a router names: in the proxy's own files, where
// it names that provider, or names none and is in a file itself, as Traefik looks
// for one with no provider in the router's own first; otherwise in the labels of
// what it routes. defs are its settings, keyed as labels are, and kind is its kind,
// or "" where it's defined nowhere Dockwell reads.
func middlewareOf(l, files map[string]string, own, name, provider string) (defs map[string]string, kind string) {
	// No panel writes a name with a dot, and every redirect setting's key has them,
	// which share the files' map: one with a dot reads as a middleware Dockwell
	// can't read, so it holds what passes through it.
	if strings.Contains(name, ".") {
		return nil, ""
	}
	switch {
	case provider == "file" || provider == "" && own == "file":
		return files, files[name]
	case provider == "" || provider == "docker" || provider == "swarm":
		at := "traefik.http.middlewares." + name + "."
		for k := range l {
			if rest, ok := strings.CutPrefix(k, at); ok {
				kind, _, _ = strings.Cut(rest, ".")
			}
		}
		return l, kind
	}
	return nil, ""
}

// routeMerger puts the routers of one domain and path together: what one can't
// bring, the domain can't, and they must agree on whether it redirects, and where.
// A router that sends plain HTTP to HTTPS first says nothing of that, as the one
// for HTTPS answers what it's sent to; one alone says what it does.
type routeMerger struct {
	decided, guessed map[*dockwellv1.PanelRoute]bool
}

func newRouteMerger() routeMerger {
	return routeMerger{decided: map[*dockwellv1.PanelRoute]bool{}, guessed: map[*dockwellv1.PanelRoute]bool{}}
}

// add puts what one router says of r with what the others did.
func (m routeMerger) add(r *dockwellv1.PanelRoute, rd *resourcev1.RouteRedirect, upgrades bool) {
	switch {
	case upgrades && !m.decided[r] && !m.guessed[r]:
		r.Redirect, m.guessed[r] = rd, true
	case upgrades:
	case !m.decided[r]:
		r.Redirect, m.decided[r] = rd, true
	case !proto.Equal(r.GetRedirect(), rd):
		r.Left = cmp.Or(r.GetLeft(), "the proxy's routers for it don't agree on whether it redirects, so Dockwell can't tell what answers it")
	}
}

// settle keeps behind a redirect of a path alone, which the handover doesn't
// bring, then every route of a domain where one stays (sameDomainLeft), and keeps
// no redirect on a route that stays behind.
func settle(routes []*dockwellv1.PanelRoute) []*dockwellv1.PanelRoute {
	for _, r := range routes {
		if r.GetRedirect() != nil && r.GetPath() != "" && r.GetLeft() == "" {
			r.Left = "its router redirects a path of the domain alone, which the handover doesn't bring. Add it as a redirect from Domains"
		}
	}
	routes = sameDomainLeft(routes)
	for _, r := range routes {
		if r.GetLeft() != "" {
			r.Redirect = nil
		}
	}
	return routes
}
