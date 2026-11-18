// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A database's external access is its port on every address and two rules: one lets
// the listed addresses in, and one keeps everyone else out. No change may take that
// second rule away, or loosen it, while the port stays on every address, as the port
// would then answer anyone: not a whole-spec edit of the service that keeps the port,
// nor a delete of the rule alone. Taking all three away together is how it's turned
// off (ADR 0038, "External access, as built").
func TestExternalAccessKeepsItsDenyRule(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		env := production(t, db)
		run := func(ports ...*resourcev1.PortMapping) Edit {
			return claimant(env, "db", &resourcev1.Container{Image: "postgres:17", Ports: ports}, "srv_A")
		}
		apply(t, e, propose(t, e, "Run db", run()))
		var id string
		if err := db.View(t.Context(), func(tx *store.Tx) error {
			r, err := tx.GetResource(t.Context(), "service", env, "db")
			id = r.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		public := &resourcev1.PortMapping{HostPort: 5432, ContainerPort: 5432}
		allowRef := resource.Ref{Kind: firewall.Kind, Scope: "srv_A", Name: ExternalRule(id, ExternalAllow)}
		denyRef := resource.Ref{Kind: firewall.Kind, Scope: "srv_A", Name: ExternalRule(id, ExternalDeny)}
		deny := func(sources ...string) Edit {
			return Edit{Ref: denyRef, Spec: &resourcev1.FirewallRule{
				Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY, Port: 5432, Sources: sources, Comment: "External access to db",
			}}
		}
		apply(t, e, propose(t, e, "Let 203.0.113.0/24 reach db on port 5432", run(public),
			Edit{Ref: allowRef, Spec: &resourcev1.FirewallRule{
				Action: resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW, Port: 5432, Sources: []string{"203.0.113.0/24"}, Comment: "External access to db",
			}},
			deny()))

		refused := func(what string, edits ...Edit) {
			t.Helper()
			_, err := e.Propose(t.Context(), Proposal{Summary: what, Actor: "usr_test", Edits: edits})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "that port answers anyone") ||
				!strings.Contains(err.Error(), "Turn its external access off on its page") {
				t.Errorf("%s = %v, want it refused, saying the port would answer anyone", what, err)
			}
		}
		// A whole-spec edit of the database that keeps the port, beside the rule's delete.
		withEnv := run(public)
		withEnv.Spec.(*resourcev1.Service).GetApp().Env = map[string]string{"TZ": "UTC"}
		refused("a whole-spec edit that keeps the port and drops the deny rule", withEnv, Edit{Ref: denyRef})
		refused("the deny rule's delete alone", Edit{Ref: denyRef})
		refused("a deny rule that lets an address through", deny("198.51.100.0/24"))

		// The port on the server alone answers no one beyond it, so the rule may go then.
		loopback := &resourcev1.PortMapping{HostPort: 5432, ContainerPort: 5432, HostIp: "127.0.0.1"}
		if _, err := e.Propose(t.Context(), Proposal{Summary: "Keep db to its server", Actor: "usr_test", Edits: []Edit{run(loopback), {Ref: denyRef}}}); err != nil {
			t.Errorf("moving the port to the server alone with the rule = %v, want it planned", err)
		}
		// Turning it off takes all three at once.
		if _, err := e.Propose(t.Context(), Proposal{Summary: "Close db to everyone beyond its server", Actor: "usr_test", Edits: []Edit{run(), {Ref: allowRef}, {Ref: denyRef}}}); err != nil {
			t.Errorf("turning external access off = %v, want it planned", err)
		}
	})
}

// A rule's name says whose external access it keeps, and nothing else reads as one.
func TestAnExternalRuleNamesItsService(t *testing.T) {
	name := ExternalRule("svc_01M7PWVFFEJVFN636WN6AEF71G", ExternalDeny)
	if name != "external-01m7pwvffejvfn636wn6aef71g-deny" {
		t.Errorf("the rule is named %q", name)
	}
	if id, ok := externalOf(name, ExternalDeny); !ok || id != "svc_01M7PWVFFEJVFN636WN6AEF71G" {
		t.Errorf("%s reads as %q, %t", name, id, ok)
	}
	for _, other := range []string{"http", "external-deny", "external-x-allow", "external--deny"} {
		if _, ok := externalOf(other, ExternalDeny); ok {
			t.Errorf("%s reads as an external access rule", other)
		}
	}
}
