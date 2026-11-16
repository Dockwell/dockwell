// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// The note for a port that moves to the server alone names the SSH tunnel first, as
// that reaches it in any case, and offers a domain only for the page an app is meant
// to serve on one: never a companion tool such as Adminer, nor a database, nor any
// service of an app that holds 80 or 443 itself, as a proxy may (#752's review, M2).
func TestTheMoveNoteOffersADomainOnlyWhereOneIsMeant(t *testing.T) {
	entry := &catalogv1.Entry{
		Web:        &catalogv1.Web{Service: "web"},
		Databases:  []string{"db"},
		Companions: []*catalogv1.Companion{{Service: "tool"}},
	}
	f := found{ports: map[string][]*resourcev1.PortMapping{
		"web":  {{HostPort: 18100}},
		"tool": {{HostPort: 18300}, {HostPort: 18301}},
		"db":   {{HostPort: 15432}},
	}}
	everywhere := map[string][]string{"web": {"18100"}, "tool": {"18300", "18301"}, "db": {"15432"}}
	notes := func(public map[string][]string) map[string]string {
		out := map[string]string{}
		for _, n := range f.toServerAlone(public, everywhere, everywhere, domainFor(entry, public), "web") {
			out[n.Names[0]] = n.Message
		}
		return out
	}
	got := notes(nil)
	for service, want := range map[string]string{
		"web":  "web's port 18100 answers on this server alone after this, not on every address. Reach it with the SSH tunnel on its page, or add a domain to web.",
		"tool": "tool's ports 18300 and 18301 answer on this server alone after this, not on every address. Reach them through SSH tunnels to ports 18300 and 18301.",
		"db":   "db's port 15432 answers on this server alone after this, not on every address. Reach it through an SSH tunnel to port 15432.",
	} {
		if got[service] != want {
			t.Errorf("%s's note:\n%q\nwant\n%q", service, got[service], want)
		}
	}
	// An app that holds 443 itself, as a proxy may, leaves the ingress none.
	if got := notes(map[string][]string{"proxy": {"443"}})["web"]; got != "web's port 18100 answers on this server alone after this, not on every address. Reach it with the SSH tunnel on its page." {
		t.Errorf("web's note beside a proxy on 443: %q", got)
	}
}

// The note names the tunnel on the app's page only for the port the page shows one
// for: the web service's first port on the server alone. Another port of it, behind
// one already there, gets the tunnel to its own port (#752's re-check, N3).
func TestTheMoveNoteNamesThePagesTunnelOnlyWhereThePageShowsIt(t *testing.T) {
	entry := &catalogv1.Entry{Web: &catalogv1.Web{Service: "web"}}
	f := found{ports: map[string][]*resourcev1.PortMapping{
		"web": {{HostPort: 18100, HostIp: "127.0.0.1"}, {HostPort: 18101}},
	}}
	everywhere := map[string][]string{"web": {"18101"}}
	published := map[string][]string{"web": {"18100", "18101"}}
	notes := f.toServerAlone(nil, published, everywhere, domainFor(entry, nil), "web")
	want := "web's port 18101 answers on this server alone after this, not on every address. Reach it through an SSH tunnel to port 18101, or add a domain to web."
	if len(notes) != 1 || notes[0].Message != want {
		t.Errorf("the note: %v, want %q", notes, want)
	}
}
