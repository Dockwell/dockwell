// SPDX-License-Identifier: AGPL-3.0-only

package route

import (
	"fmt"
	"net/url"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// maxRedirectTo is the longest address a redirect sends to, as browsers and
// servers take URLs up to about 2,000 bytes in a header without trouble.
const maxRedirectTo = 2048

// maxHops is how many redirects a loop check follows from one, past which a chain
// is long enough to be a mistake of its own, and not this redirect's.
const maxHops = 4

// checkRedirect normalizes a redirect's to and refuses one that sends nowhere a
// browser can go, carries what Caddy would read as its own words, or loops back to
// its own path whatever else there is (ADR 0070). A route that redirects sends to no
// service: it answers every request itself.
func checkRedirect(r *resourcev1.Route) error {
	where, d := Where(r), r.GetRedirect()
	if r.GetService() != "" || r.GetPort() != 0 || r.GetHostPort() != 0 {
		return fmt.Errorf("%s redirects, so it sends to no service. Remove the service and its port, or the redirect", where)
	}
	// A redirect has nothing behind it to protect, so it keeps no one out: making a
	// protected route one takes clearing its access in the same edit, which a person
	// approves as it opens the route (ADR 0070, with ADR 0087).
	if a := r.GetAccess(); a != nil {
		switch {
		case len(a.GetUsers()) > 0:
			return fmt.Errorf("%s asks for a password, and a redirect has nothing behind it to protect. Clear its access in the same change", where)
		case a.GetSignIn() != nil:
			return fmt.Errorf("%s asks visitors to sign in, and a redirect has nothing behind it to protect. Clear its access in the same change", where)
		case len(a.GetAllow()) > 0:
			return fmt.Errorf("%s lets in only some addresses, and a redirect has nothing behind it to protect. Clear its access in the same change", where)
		}
	}
	if d.GetTo() == "" {
		return fmt.Errorf("%s redirects, and needs where to: an address such as https://example.com", where)
	}
	if strings.ContainsAny(d.GetTo(), "{}") {
		return fmt.Errorf("%s redirects to %s, which has a { or }, and the proxy would read them as its own words. Write the address without them, %%7B and %%7D in their place", where, d.GetTo())
	}
	if strings.ContainsFunc(d.GetTo(), func(c rune) bool { return c <= ' ' || c == 0x7f }) {
		return fmt.Errorf("%s redirects to an address with a space or a control character in it. Write it with %%20 for a space", where)
	}
	// A prefix to keep is a path route's, keeping what follows it.
	if r.GetPath() == "" || d.GetDropPath() {
		d.KeepPrefix = false
	}
	to, err := normalizeTo(where, d.GetTo(), d.GetDropPath())
	if err != nil {
		return err
	}
	d.To = to
	if len(to) > maxRedirectTo {
		return fmt.Errorf("%s redirects to an address longer than %d characters. Use a shorter one", where, maxRedirectTo)
	}
	// Alone, a route knows it loops only where it lands on its own path exactly, which
	// no longer path of its domain can take from it. Where else it loops depends on the
	// install's other routes, which the control plane checks (CheckAcross).
	if host, path := follow(r, r.GetPath()); host == r.GetDomain() && path == r.GetPath() {
		return fmt.Errorf("%s would send visitors round in a circle, back to itself. Send it to a path another route answers, or to another domain", where)
	}
	return nil
}

// normalizeTo is one way of writing where a redirect sends: the scheme and the host
// in lowercase, without the scheme's own port, and without a / at the end where a
// request's own path goes after it, as that path starts with one. So the checks
// after it compare like with like (ADR 0070).
func normalizeTo(where, raw string, dropPath bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "", fmt.Errorf("%s redirects to %s, which isn't an address a browser can go to. Start it with https:// and a domain, such as https://example.com", where, raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	switch {
	case u.Scheme != "https" && u.Scheme != "http":
		return "", fmt.Errorf("%s redirects to %s, which isn't an address a browser can go to. Start it with https:// and a domain, such as https://example.com", where, raw)
	case u.User != nil:
		return "", fmt.Errorf("%s redirects to %s, which has a user name in it, and browsers warn about those or refuse them. Leave it out", where, raw)
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return "", fmt.Errorf("%s redirects to %s, which has a # part, and browsers keep their own. Leave it out", where, raw)
	case u.RawQuery != "" && !dropPath:
		return "", fmt.Errorf("%s redirects to %s, which has a query, and keeps each request's path and query after it, which can't follow a query. Leave the query out, or send every request to it as it is with drop_path", where, raw)
	}
	host := strings.ToLower(u.Hostname())
	for _, c := range host {
		if c > 0x7e {
			return "", fmt.Errorf("%s redirects to %s, whose domain isn't written in ASCII. Write the domain in its xn-- form, such as xn--bcher-kva.example", where, raw)
		}
	}
	if host == "" {
		return "", fmt.Errorf("%s redirects to %s, which isn't an address a browser can go to. Start it with https:// and a domain, such as https://example.com", where, raw)
	}
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	u.Host = host
	if port != "" {
		u.Host = host + ":" + port
	}
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
		if port != "" {
			u.Host += ":" + port
		}
	}
	if !dropPath {
		u.Path, u.RawPath = strings.TrimSuffix(u.Path, "/"), strings.TrimSuffix(u.RawPath, "/")
	}
	return u.String(), nil
}

// follow is where a redirect sends a request for a path it answers: to's host, and
// to's path with what follows the route's own path after it, or the whole path for
// one that keeps its prefix, or to's path alone for one that drops the path.
func follow(r *resourcev1.Route, reqPath string) (host, path string) {
	d := r.GetRedirect()
	u, err := url.Parse(d.GetTo())
	if err != nil {
		return "", ""
	}
	path = strings.TrimSuffix(u.EscapedPath(), "/")
	if d.GetDropPath() {
		return strings.ToLower(u.Hostname()), path
	}
	rest := reqPath
	if !d.GetKeepPrefix() && r.GetPath() != "" {
		rest = strings.TrimPrefix(reqPath, r.GetPath())
	}
	return strings.ToLower(u.Hostname()), strings.TrimSuffix(path+rest, "/")
}

// covers reports a route answering a path of its domain: its own path and what's
// under it, or every path for one with none.
func covers(r *resourcev1.Route, path string) bool {
	p := r.GetPath()
	return p == "" || path == p || strings.HasPrefix(path, p+"/")
}

// answering is the route of a host that answers a path, by the longest path, as
// Caddy matches them, or false for none.
func answering(all map[resource.Ref]proto.Message, host, path string) (resource.Ref, *resourcev1.Route, bool) {
	var ref resource.Ref
	var best *resourcev1.Route
	for k, spec := range all {
		r, ok := spec.(*resourcev1.Route)
		if !ok || r.GetDomain() != host || !covers(r, path) {
			continue
		}
		if best == nil || len(r.GetPath()) > len(best.GetPath()) || (len(r.GetPath()) == len(best.GetPath()) && k.String() < ref.String()) {
			ref, best = k, r
		}
	}
	return ref, best, best != nil
}

// CheckAcross refuses a redirect that leads back to itself across the install's
// routes as the change leaves them: where a request at its own path lands is
// answered by itself, or by redirects that come back to it within maxHops. The scheme
// counts for nothing, as plain HTTP goes on to HTTPS on a domain with a certificate
// (ADR 0070).
func (*Driver) CheckAcross(ref resource.Ref, desired proto.Message, all map[resource.Ref]proto.Message) error {
	r, ok := desired.(*resourcev1.Route)
	if !ok || r.GetRedirect() == nil {
		return nil
	}
	where := Where(r)
	cur, path := r, r.GetPath()
	for hop := 0; hop <= maxHops; hop++ {
		host, landed := follow(cur, path)
		next, n, found := answering(all, host, landed)
		switch {
		case !found:
			return nil
		case next == ref && hop == 0:
			return fmt.Errorf("%s would send visitors round in a circle, back to itself at %s. Send it to a path another route answers, or to another domain", where, Where(n)+strings.TrimPrefix(landed, n.GetPath()))
		case next == ref:
			return fmt.Errorf("%s would send visitors round in a circle: %s redirects back to it. Send it to the site, or change %s first", where, Where(cur), Where(cur))
		case n.GetRedirect() == nil:
			return nil
		}
		cur, path = n, landed
	}
	return nil
}

// keepRedirect keeps a redirect through an edit that names nothing to send to, as
// whoever writes a whole route without knowing the field, the panel's "Move behind
// what's in front" or an older CLI, would otherwise lose it, or be refused (ADR
// 0070). It refuses such an edit of a route that has no redirect to keep. Keep
// calls it.
func keepRedirect(was, r *resourcev1.Route) error {
	if r.GetRedirect() != nil || r.GetService() != "" || r.GetHostPort() != 0 || r.GetPort() != 0 {
		return nil
	}
	if was.GetRedirect() == nil {
		return errNoTarget
	}
	r.Redirect = proto.Clone(was.GetRedirect()).(*resourcev1.RouteRedirect)
	return nil
}

// checkNewTarget refuses a new route that sends nowhere, as it has no redirect to
// keep. CheckNew calls it.
func checkNewTarget(r *resourcev1.Route) error {
	if r.GetRedirect() == nil && r.GetService() == "" && r.GetHostPort() == 0 {
		return errNoTarget
	}
	return nil
}
