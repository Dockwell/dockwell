// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const proxy = "http://127.0.0.1:80"

func doc(t *testing.T, rules ...Rule) *Document {
	t.Helper()
	b, err := json.Marshal(map[string]any{"ingress": rules})
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestHostMatches(t *testing.T) {
	for _, c := range []struct {
		rule, host string
		want       bool
	}{
		{"", "anything.example.com", true},
		{"*.example.com", "wiki.example.com", true},
		{"*.example.com", "a.b.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", "badexample.com", false},
		{"Wiki.Example.com", "wiki.example.COM", true},
		{"wiki.example.com", "wiki2.example.com", false},
	} {
		if got := HostMatches(c.rule, c.host); got != c.want {
			t.Errorf("%q takes %q: %v", c.rule, c.host, got)
		}
	}
}

// The panel's names and the zone rule, as the re-check's N1 asks the stand-in to
// hold all three ways the panel can be served.
func TestTheZoneRuleBesideThePanel(t *testing.T) {
	rt := Routing{Panel: []string{"panel.example.com"}, PanelService: "http://127.0.0.1:7600", Proxy: proxy}

	// The panel by its own rule: the zone rule goes in after it.
	byOwn := doc(t, Rule{Hostname: "panel.example.com", Service: "http://127.0.0.1:7600"}, CatchAll)
	need := NeedFor(byOwn, rt, "example.com", "wiki.example.com")
	if need.Refused != nil || need.Nothing || need.Rule.Hostname != "*.example.com" || need.Edit.At != 1 {
		t.Fatalf("by its own rule: %+v", need)
	}
	if FirstFor(need.After.Rules, "panel.example.com") != 0 {
		t.Fatal("the panel's rule should still serve it")
	}

	// The panel by the catch-all alone: the wildcard would take it, so it's refused,
	// by the panel's name.
	byCatchAll := doc(t, Rule{Service: "http://127.0.0.1:7600"})
	need = NeedFor(byCatchAll, rt, "example.com", "wiki.example.com")
	var moved Moved
	if !errors.As(need.Refused, &moved) || !moved.Panel || moved.Name != "panel.example.com" {
		t.Fatalf("by the catch-all: %+v", need)
	}

	// The panel by an earlier wildcard that already reaches the proxy, as a panel
	// behind the server's own proxy is: nothing is written.
	byWildcard := doc(t, Rule{Hostname: "*.example.com", Service: proxy}, CatchAll)
	if need := NeedFor(byWildcard, Routing{Panel: []string{"panel.example.com"}, Proxy: proxy}, "example.com", "wiki.example.com"); !need.Nothing {
		t.Fatalf("by an earlier wildcard: %+v", need)
	}
}

func TestNeedFor(t *testing.T) {
	rt := Routing{Panel: []string{"panel.example.com"}, PanelService: "http://127.0.0.1:7600", Proxy: proxy}
	panel := Rule{Hostname: "panel.example.com", Service: "http://127.0.0.1:7600"}

	// A catch-all that reaches the proxy already: nothing to write.
	if need := NeedFor(doc(t, panel, Rule{Service: "http://localhost"}), rt, "example.com", "wiki.example.com"); !need.Nothing {
		t.Errorf("a catch-all to the proxy: %+v", need)
	}

	// An earlier wildcard elsewhere catches it first, by name.
	caught := NeedFor(doc(t, panel, Rule{Hostname: "*.example.com", Service: "http://127.0.0.1:8080"}, CatchAll), rt, "example.com", "wiki.example.com")
	if caught.Refused == nil || caught.Refused.Error() != "*.example.com in the tunnel catches wiki.example.com first, to http://127.0.0.1:8080" {
		t.Errorf("an earlier wildcard: %+v", caught)
	}
	// A rule for a path of the name, sent elsewhere, catches it too.
	if need := NeedFor(doc(t, panel, Rule{Hostname: "wiki.example.com", Path: "^/api", Service: "http://127.0.0.1:8080"}, CatchAll), rt, "example.com", "wiki.example.com"); need.Refused == nil {
		t.Errorf("a path sent elsewhere: %+v", need)
	}

	// A name in the zone whose DNS points at the tunnel, and goes somewhere real by
	// the catch-all, would move: refused by its name.
	rtFTP := rt
	rtFTP.Names = []string{"ftp.example.com"}
	need := NeedFor(doc(t, panel, Rule{Service: "http://127.0.0.1:2121"}), rtFTP, "example.com", "wiki.example.com")
	if need.Refused == nil || need.Refused.Error() != "the change would move ftp.example.com, which goes to http://127.0.0.1:2121 now" {
		t.Errorf("a name that would move: %+v", need)
	}
	// One the catch-all answers 404 may reach the proxy, which answers it 404 too.
	if need := NeedFor(doc(t, panel, CatchAll), rtFTP, "example.com", "wiki.example.com"); need.Refused != nil {
		t.Errorf("a 404 name: %v", need.Refused)
	}

	// The apex takes a rule of its own, which the wildcard doesn't cover.
	withZone := doc(t, panel, Rule{Hostname: "*.example.com", Service: proxy}, CatchAll)
	apex := NeedFor(withZone, rt, "example.com", "example.com")
	if apex.Refused != nil || apex.Rule.Hostname != "example.com" {
		t.Errorf("the apex: %+v", apex)
	}
	// A panel at the apex: its own rule is never added to, but the zone's wildcard,
	// which matches no panel name, may go in after the panel's rule.
	atApex := Routing{Panel: []string{"example.com"}, PanelService: "http://127.0.0.1:7600", Proxy: proxy}
	apexPanel := doc(t, Rule{Hostname: "example.com", Service: "http://127.0.0.1:7600"}, CatchAll)
	if need := NeedFor(apexPanel, atApex, "example.com", "example.com"); need.Refused == nil {
		t.Errorf("the apex rule over the panel's: %+v", need)
	}
	if need := NeedFor(apexPanel, atApex, "example.com", "wiki.example.com"); need.Refused != nil || need.Rule.Hostname != "*.example.com" {
		t.Errorf("the zone rule beside an apex panel: %+v", need)
	}

	// The panel's apps domain, a wildcard of its own (ADR 0061), stays served by its
	// rule; with no rule of its own, the zone's wildcard would take it.
	apps := Routing{Panel: []string{"panel.example.com", "*.apps.example.com"}, PanelService: "http://127.0.0.1:7600", Proxy: proxy}
	appsRule := Rule{Hostname: "*.apps.example.com", Service: "http://127.0.0.1:7600"}
	if need := NeedFor(doc(t, panel, appsRule, CatchAll), apps, "example.com", "wiki.example.com"); need.Refused != nil {
		t.Errorf("an apps domain with its own rule: %v", need.Refused)
	}
	need = NeedFor(doc(t, panel, Rule{Service: "http://127.0.0.1:7600"}), apps, "example.com", "wiki.example.com")
	var moved Moved
	if !errors.As(need.Refused, &moved) || !moved.Panel {
		t.Errorf("an apps domain served by the catch-all: %+v", need)
	}
	// A rule Dockwell would add for a name under the panel's serves the panel.
	if !apps.Serves(Rule{Hostname: "x.panel.example.com", Service: proxy}) || !apps.Serves(Rule{Hostname: "*.apps.example.com", Service: proxy}) ||
		apps.Serves(Rule{Hostname: "*.example.com", Service: proxy}) || !apps.Serves(Rule{Hostname: "other.example.com", Service: "http://localhost:7600"}) {
		t.Error("Serves")
	}
}

// cloudflared's own matcher, offline, agrees with the routing check on which rule
// takes a name and where its root goes, where DOCKWELL_TEST_CLOUDFLARED names
// cloudflared.
func TestRoutingAgreesWithCloudflared(t *testing.T) {
	binary := os.Getenv("DOCKWELL_TEST_CLOUDFLARED")
	if binary == "" {
		t.Skip("DOCKWELL_TEST_CLOUDFLARED names no cloudflared")
	}
	rules := []Rule{
		{Hostname: "panel.example.com", Service: "http://127.0.0.1:7600"},
		{Hostname: "api.example.com", Path: "^/v1", Service: "http://127.0.0.1:8081"},
		{Hostname: "*.apps.example.com", Service: "http://127.0.0.1:7601"},
		{Hostname: "example.com", Service: "http://127.0.0.1:8082"},
		{Hostname: "*.example.com", Service: proxy},
		CatchAll,
	}
	var yml strings.Builder
	yml.WriteString("tunnel: 327d9d41-b009-413d-8494-7eeeefb3ed2a\ningress:\n")
	for _, r := range rules {
		yml.WriteString("  - ")
		if r.Hostname != "" {
			yml.WriteString("hostname: \"" + r.Hostname + "\"\n    ")
		}
		if r.Path != "" {
			yml.WriteString("path: \"" + r.Path + "\"\n    ")
		}
		yml.WriteString("service: " + r.Service + "\n")
	}
	file := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(file, []byte(yml.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	matched := regexp.MustCompile(`service: (\S+)`)
	for _, name := range []string{"panel.example.com", "api.example.com", "x.apps.example.com", "example.com", "wiki.example.com", "a.b.example.com", "other.example.net"} {
		out, err := exec.Command(binary, "tunnel", "--config", file, "ingress", "rule", "https://"+name+"/").CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		m := matched.FindSubmatch(out)
		if m == nil {
			t.Fatalf("%s: cloudflared said %s", name, out)
		}
		if got := WayOf(rules, name).Goes(); got != string(m[1]) {
			t.Errorf("%s: the routing check says %s, cloudflared %s", name, got, m[1])
		}
	}
}

// The panel's names include the domain it moves from, during a move, and its apps
// domain with every name under it: no rule Dockwell writes may serve one, and a zone
// rule that would take one from where it goes now is refused, by its name (ADR 0104,
// section 4; ADR 0063 and ADR 0061).
func TestTheGuardHoldsTheOldDomainAndTheAppsDomain(t *testing.T) {
	names := PanelNames{Served: []string{"panel.example.com", ""}, Moving: "Old.Example.com", Apps: "apps-example.net", Service: "http://127.0.0.1:7600"}
	if got := strings.Join(names.Names(), " "); got != "panel.example.com old.example.com apps-example.net *.apps-example.net" {
		t.Fatalf("the panel's names: %s", got)
	}
	rt := Routing{Panel: names.Names(), PanelService: names.Service, Proxy: proxy}
	for _, host := range []string{"old.example.com", "apps-example.net", "*.apps-example.net", "grafana.apps-example.net"} {
		if !rt.Serves(Rule{Hostname: host, Service: proxy}) {
			t.Errorf("a rule for %s should be the panel's", host)
		}
	}
	if rt.Serves(Rule{Hostname: "*.example.com", Service: proxy}) {
		t.Error("the zone's wildcard beside the old domain isn't the panel's by itself")
	}
	// A zone rule for the apps domain's own zone serves the panel's apps.
	if need := NeedFor(doc(t, CatchAll), rt, "apps-example.net", "wiki.apps-example.net"); !errors.Is(need.Refused, ErrServesPanel) {
		t.Fatalf("a zone rule over the apps domain: %+v", need)
	}
	// The old domain served by the catch-all alone: the zone rule would take it.
	byCatchAll := doc(t, Rule{Service: "http://127.0.0.1:7600"})
	var moved Moved
	if need := NeedFor(byCatchAll, rt, "example.com", "wiki.example.com"); !errors.As(need.Refused, &moved) || !moved.Panel {
		t.Fatalf("the old domain by the catch-all: %+v", need)
	}
	// Served by its own rule, with the domain in use, the zone rule goes in after.
	byOwn := doc(t, Rule{Hostname: "panel.example.com", Service: "http://127.0.0.1:7600"}, Rule{Hostname: "old.example.com", Service: "http://127.0.0.1:7600"}, CatchAll)
	if need := NeedFor(byOwn, rt, "example.com", "wiki.example.com"); need.Refused != nil || need.Edit.At != 2 {
		t.Fatalf("the old domain by its own rule: %+v", need)
	}
}
