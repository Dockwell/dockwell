// SPDX-License-Identifier: AGPL-3.0-only

package resource

import "testing"

// A handover's digest is of what was checked of the chosen domains alone: an open
// answer counts as open whatever its code, the order of what was found doesn't
// count, and anything else that changes for a chosen domain does.
func TestPanelEvidence(t *testing.T) {
	blog := PanelCheck{Domain: "blog.example.com", Probes: []PanelProbe{{Path: "/", Answer: "200", Open: true}, {Path: "/api", Answer: "404", Open: true}}, Scanned: "labels on 4 containers"}
	wiki := PanelCheck{Domain: "wiki.example.com", Held: "a request for / through the proxy answered 401"}
	base := PanelEvidence([]PanelCheck{blog, wiki}, []string{"blog.example.com"})
	same := blog
	same.Probes = []PanelProbe{{Path: "/api", Answer: "502", Open: true}, {Path: "/", Answer: "200", Open: true}}
	same.Scanned = "labels on 5 containers"
	otherWiki := wiki
	otherWiki.Held = ""
	if got := PanelEvidence([]PanelCheck{otherWiki, same}, []string{"BLOG.example.com", "blog.example.com"}); got != base {
		t.Errorf("the same evidence digests as %s, not %s", got, base)
	}
	mentioned := blog
	mentioned.Mentions = []string{"the label traefik.http.routers.a.rule on sso"}
	closed := blog
	closed.Probes = []PanelProbe{{Path: "/", Answer: "401"}}
	for name, got := range map[string]string{
		"a new mention":      PanelEvidence([]PanelCheck{mentioned}, []string{"blog.example.com"}),
		"a closed answer":    PanelEvidence([]PanelCheck{closed}, []string{"blog.example.com"}),
		"another choice":     PanelEvidence([]PanelCheck{blog, wiki}, []string{"blog.example.com", "wiki.example.com"}),
		"a domain unchecked": PanelEvidence(nil, []string{"blog.example.com"}),
	} {
		if got == base {
			t.Errorf("%s digests as before", name)
		}
	}
}
