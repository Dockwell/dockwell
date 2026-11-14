// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Asker is a proxy that can ask one of its own routes what a visitor gets, from
// this server, as a private route's stranger check does (ADR 0087).
type Asker interface {
	// Ask sends a GET for domain and path to the proxy on this server's loopback, over
	// plain HTTP or HTTPS, with header and nothing else, and follows no redirect. It
	// says what it answered, or why nothing answered.
	Ask(ctx context.Context, domain, path string, plain bool, header http.Header) (Answer, error)
}

// Answer is what a proxy answered a stranger: its status, in words, and where a
// redirect sends them.
type Answer struct {
	Status   int
	Said     string
	Location string
}

// strangerAddress is who a stranger is when a request reaches a route through a
// proxy it names on this server: an address from the range kept for documentation
// (RFC 5737), which no list should name.
const strangerAddress = "192.0.2.1"

// Spellings are what the stranger check also asks a private route as, written as a
// stranger would send them, of its path, or "/" for a root: Tomcat, Jetty and
// Undertow take ";x" off each segment and then resolve "..", IIS reads "\\" as "/",
// a server that doesn't check UTF-8 reads an overlong "/" as one, Python's urlparse
// and Node's URL read //x as a host, and NFKC folds "︰" to "..", so each reads as the
// path there. Caddy's matchers see the path cleaned, so a target that resolves to a
// public path above it, or one beside, goes there unless the domain refuses it.
// Each other path on the domain is asked from below it too, as /api/..;/ would reach
// a private root through a public /api (#606's review and its final check, H2).
func Spellings(path string, beside []string) []string {
	target := path
	if target == "" {
		target = "/"
	}
	rest := strings.TrimPrefix(target, "/")
	out := []string{
		"/;x" + target, "/x/..;" + target, "/%5c" + rest, "/x/;/.." + target, "/x/.;/.." + target, "/x/%3b/.." + target,
		"/x%c0%af..%c0%af" + rest, "//x" + target, "/x/%ef%b8%b0" + target,
	}
	if path != "" {
		out = append([]string{path + ";x"}, out...)
	}
	for _, other := range beside {
		if other == "" || other == path {
			continue
		}
		other = strings.TrimSuffix(other, "/")
		out = append(out, other+"/..;"+target, other+"/.;/.."+target, other+"/x%5c..%5c..%5c"+rest)
	}
	return out
}

// asksFrom is the loopback address a stranger check comes from.
const asksFrom = "127.0.0.1"

// stranger says how a stranger check asks a route, and why it can't when it can't:
// the header it sends, and who the route takes it for.
func stranger(r Route) (header http.Header, who, cantTell string) {
	reaches := !r.Plain || r.PlainForAnyone || len(r.TrustedProxies)+len(r.EdgeProxies) == 0 || Covers(r.TrustedProxies, asksFrom+"/32")
	if !reaches {
		return nil, "", "it answers only the proxies it names, and this server isn't one of them"
	}
	header, who = http.Header{}, asksFrom
	if r.SignIn != "" {
		// As a browser navigates, so sign-in sends them to their provider, as a script
		// would get 401.
		header.Set("Accept", "text/html")
	}
	if Covers(r.TrustedProxies, asksFrom+"/32") {
		// The route believes this server on who its visitor is, as for a tunnel here.
		header.Set("X-Forwarded-For", strangerAddress)
		who = strangerAddress
	}
	if Covers(r.Allow, who+"/32") {
		return nil, "", who + " is on its list, and that's who this server asks as"
	}
	return header, who, ""
}

// AskStrangers asks each private route of routes what a stranger gets, by domain.
// Once the proxy doesn't answer one, the rest aren't asked, as they'd wait as long.
func AskStrangers(ctx context.Context, asker Asker, routes []Route) map[string][]*dockwellv1.StrangerCheck {
	out := map[string][]*dockwellv1.StrangerCheck{}
	down := ""
	for i := range routes {
		r := &routes[i]
		if r.Edited || !keepsOut(*r) {
			continue
		}
		check := &dockwellv1.StrangerCheck{Path: r.Path}
		header, _, cantTell := stranger(*r)
		switch {
		case cantTell != "":
			check.Outcome, check.Said = dockwellv1.StrangerOutcome_STRANGER_OUTCOME_CANT_TELL, "This server can't tell: "+cantTell+"."
		case down != "":
			check.Outcome, check.Said = dockwellv1.StrangerOutcome_STRANGER_OUTCOME_OTHER, down
		default:
			path := r.Path
			if path == "" {
				path = "/"
			}
			got, err := asker.Ask(ctx, r.Domain, path, r.Plain, header)
			if err != nil {
				down = "Nothing answered: " + err.Error()
				check.Outcome, check.Said = dockwellv1.StrangerOutcome_STRANGER_OUTCOME_OTHER, down
				break
			}
			check.Status, check.Said, check.Outcome = uint32(got.Status), got.Said, outcomeOf(got.Status) //nolint:gosec // an HTTP status, 100 to 599
			// A route that signs people in sends a stranger's navigation to its provider,
			// another origin, which is where they're sent (ADR 0087).
			if at := signsInAt(*r, got); at != "" {
				check.Said, check.Outcome = at, dockwellv1.StrangerOutcome_STRANGER_OUTCOME_SENT_TO_SIGN_IN
			}
			// A route's other spellings, which an app may read as its path, should keep a
			// stranger out as well (#606's review, H2).
			for _, spelling := range Spellings(r.Path, pathsOn(routes, r)) {
				if check.GetOutcome() == dockwellv1.StrangerOutcome_STRANGER_OUTCOME_LET_IN {
					break
				}
				if got, err := asker.Ask(ctx, r.Domain, spelling, r.Plain, header); err == nil && outcomeOf(got.Status) == dockwellv1.StrangerOutcome_STRANGER_OUTCOME_LET_IN && signsInAt(*r, got) == "" {
					check.Status, check.Said, check.Outcome = uint32(got.Status), got.Said+" at "+spelling, dockwellv1.StrangerOutcome_STRANGER_OUTCOME_LET_IN //nolint:gosec // an HTTP status
				}
			}
		}
		out[r.Domain] = append(out[r.Domain], check)
	}
	for _, checks := range out {
		slices.SortFunc(checks, func(a, b *dockwellv1.StrangerCheck) int {
			switch {
			case a.GetPath() < b.GetPath():
				return -1
			case a.GetPath() > b.GetPath():
				return 1
			}
			return 0
		})
	}
	return out
}

// signsInAt is the host a route that signs people in sends a stranger to, from a
// redirect to another origin, or "" for anything else.
func signsInAt(r Route, got Answer) string {
	if r.SignIn == "" || got.Status < http.StatusMultipleChoices || got.Status >= http.StatusBadRequest {
		return ""
	}
	u, err := url.Parse(got.Location)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || strings.EqualFold(u.Hostname(), r.Domain) {
		return ""
	}
	return u.Hostname()
}

// keepsOut reports a route that keeps anyone out: with a list, users or sign-in.
func keepsOut(r Route) bool { return len(r.Allow)+len(r.Users) > 0 || r.SignIn != "" }

// pathsOn is the paths of the routes beside r on its domain, each once, so the
// stranger check asks r's spellings from below them too.
func pathsOn(routes []Route, r *Route) []string {
	var out []string
	for i := range routes {
		o := &routes[i]
		if o.Domain == r.Domain && o.Path != r.Path && o.Path != "" && !slices.Contains(out, o.Path) {
			out = append(out, o.Path)
		}
	}
	slices.Sort(out)
	return out
}

// outcomeOf is what a status says a stranger got.
func outcomeOf(status int) dockwellv1.StrangerOutcome {
	switch {
	case status == http.StatusForbidden:
		return dockwellv1.StrangerOutcome_STRANGER_OUTCOME_KEPT_OUT
	case status == http.StatusUnauthorized:
		return dockwellv1.StrangerOutcome_STRANGER_OUTCOME_ASKED_FOR_PASSWORD
	case status > 0 && status < http.StatusBadRequest:
		return dockwellv1.StrangerOutcome_STRANGER_OUTCOME_LET_IN
	}
	return dockwellv1.StrangerOutcome_STRANGER_OUTCOME_OTHER
}
