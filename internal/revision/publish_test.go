// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/resource"
)

// The zone's rule in a shared tunnel is an admin's to apply, and taking over what
// someone else made at Cloudflare is as good as root, with a passkey (ADR 0104,
// section 5).
func TestPublishingAccessIsAnAdmins(t *testing.T) {
	rule := resource.Access{Kind: resource.AccessTunnelRule, Path: "*.example.com", Detail: "web-1", Address: "http://127.0.0.1:80"}
	if got := installWide(rule); got != "adds a rule to a Cloudflare tunnel it shares with others" {
		t.Errorf("the rule: %q", got)
	}
	if rule.AsGoodAsRoot() {
		t.Error("the rule is an admin's, not root's")
	}
	takeover := resource.Access{Kind: resource.AccessCloudflareTakeover, Path: "vpn.example.com", Detail: "A 203.0.113.9"}
	if !takeover.AsGoodAsRoot() || installWide(takeover) != "gets as much as root on its server" {
		t.Errorf("a takeover: %v %q", takeover.AsGoodAsRoot(), installWide(takeover))
	}
	if words := resource.AccessWords(takeover); !strings.Contains(words, "made in Cloudflare by someone else") {
		t.Errorf("words: %q", words)
	}
}

// A route's steps at Cloudflare are told apart in its progress, and say where they
// write.
func TestProviderStepsHaveTheirOwnProgress(t *testing.T) {
	ref := resource.Ref{Kind: routeKind, Scope: "env_1", Name: "wiki"}
	own := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}
	rule := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate, Provider: &resource.ProviderStep{Action: resource.ProviderAddRule, Target: "web-1", Name: "*.example.com"}}}
	record := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate, Provider: &resource.ProviderStep{Action: resource.ProviderCreateRecord, Target: "example.com", Name: "wiki.example.com"}}}
	plan := &Plan{Servers: []ServerPlan{{Server: "srv_1", Name: "hz-1", Steps: []resource.Step{rule, record, own}}}}
	p := newProgress(plan, []item{{ref: ref, id: "res_wiki"}})
	if len(p.Steps) != 3 || p.index("srv_1", "res_wiki") != 2 || p.indexOf("srv_1", "res_wiki", rule.Provider.Key()) != 0 {
		t.Fatalf("progress: %+v", p.Steps)
	}
	if got := p.Steps[0].Message; got != "Waiting: Adding *.example.com to the tunnel web-1 at Cloudflare" {
		t.Errorf("a rule's words: %q", got)
	}
}
