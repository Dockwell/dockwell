// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"net/http"
	"slices"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Asker is a proxy that can ask one of its own routes what a visitor gets, from
// this server, as a private route's stranger check does (ADR 0087).
type Asker interface {
	// Ask sends a GET for domain and path to the proxy on this server's loopback, over
	// plain HTTP or HTTPS, with header and nothing else, and follows no redirect. It
	// says the status it answered with, or why nothing answered.
	Ask(ctx context.Context, domain, path string, plain bool, header http.Header) (status int, said string, err error)
}

// strangerAddress is who a stranger is when a request reaches a route through a
// proxy it names on this server: an address from the range kept for documentation
// (RFC 5737), which no list should name.
const strangerAddress = "192.0.2.1"

// Spellings are what the stranger check also asks a private path as, written as a
// stranger would send them: Tomcat, Jetty and Undertow take ";x" off each segment and
// then resolve "..", and IIS reads "\\" as "/", so each reads as the path there.
func Spellings(path string) []string {
	return []string{path + ";x", "/;x" + path, "/x/..;" + path, "/%5c" + strings.TrimPrefix(path, "/")}
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
		if r.Edited || len(r.Allow)+len(r.Users) == 0 {
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
			status, said, err := asker.Ask(ctx, r.Domain, path, r.Plain, header)
			if err != nil {
				down = "Nothing answered: " + err.Error()
				check.Outcome, check.Said = dockwellv1.StrangerOutcome_STRANGER_OUTCOME_OTHER, down
				break
			}
			check.Status, check.Said, check.Outcome = uint32(status), said, outcomeOf(status) //nolint:gosec // an HTTP status, 100 to 599
			// A path's other spellings, which an app may read as the path, should keep
			// a stranger out as well (#606's review, H2).
			if r.Path == "" {
				break
			}
			for _, spelling := range Spellings(r.Path) {
				if check.GetOutcome() == dockwellv1.StrangerOutcome_STRANGER_OUTCOME_LET_IN {
					break
				}
				if status, said, err := asker.Ask(ctx, r.Domain, spelling, r.Plain, header); err == nil && outcomeOf(status) == dockwellv1.StrangerOutcome_STRANGER_OUTCOME_LET_IN {
					check.Status, check.Said, check.Outcome = uint32(status), said+" at "+spelling, dockwellv1.StrangerOutcome_STRANGER_OUTCOME_LET_IN //nolint:gosec // an HTTP status
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
