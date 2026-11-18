// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// externalStand is a database with external access on srv_A, as the database's page
// gives it: its port tagged external, an allow of 203.0.113.0/24 and a deny of
// everyone else, applied. srv_B is a second server it can go to.
type externalStand struct {
	t                *testing.T
	e                *Engine
	db               *store.DB
	env, id          string
	allowA, denyA    resource.Ref
	allowB, denyB    resource.Ref
	tagged, loopback *resourcev1.PortMapping
}

func newExternalStand(t *testing.T, b store.Backend) *externalStand {
	t.Helper()
	e, _, db := setup(t, b, "srv_A", "srv_B")
	s := &externalStand{t: t, e: e, db: db, env: production(t, db)}
	apply(t, e, propose(t, e, "Run db", s.run([]string{"srv_A"})))
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), "service", s.env, "db")
		s.id = r.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ref := func(server, which string) resource.Ref {
		return resource.Ref{Kind: firewall.Kind, Scope: server, Name: ExternalRule(s.id, which)}
	}
	s.allowA, s.denyA, s.allowB, s.denyB = ref("srv_A", ExternalAllow), ref("srv_A", ExternalDeny), ref("srv_B", ExternalAllow), ref("srv_B", ExternalDeny)
	s.tagged = &resourcev1.PortMapping{HostPort: 5432, ContainerPort: 5432, External: true}
	s.loopback = &resourcev1.PortMapping{HostPort: 5432, ContainerPort: 5432, HostIp: "127.0.0.1"}
	apply(t, e, propose(t, e, "Let 203.0.113.0/24 reach db on port 5432", s.run([]string{"srv_A"}, s.tagged), s.allow(s.allowA, "203.0.113.0/24"), s.deny(s.denyA)))
	return s
}

func (s *externalStand) run(servers []string, ports ...*resourcev1.PortMapping) Edit {
	return claimant(s.env, "db", &resourcev1.Container{Image: "postgres:17", Ports: ports}, servers...)
}

func (s *externalStand) allow(ref resource.Ref, sources ...string) Edit {
	return Edit{Ref: ref, Spec: &resourcev1.FirewallRule{
		Action: resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW, Port: 5432, Sources: sources, Comment: "External access to db",
	}}
}

func (s *externalStand) deny(ref resource.Ref, sources ...string) Edit {
	return Edit{Ref: ref, Spec: &resourcev1.FirewallRule{
		Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY, Port: 5432, Sources: sources, Comment: "External access to db",
	}}
}

// refused says a proposal is refused, with words that say why.
func (s *externalStand) refused(what, says string, edits ...Edit) {
	s.t.Helper()
	_, err := s.e.Propose(s.t.Context(), Proposal{Summary: what, Actor: "usr_test", Edits: edits})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), says) {
		s.t.Errorf("%s = %v, want it refused, saying %q", what, err, says)
	}
}

// planLines is what a plan does, a line for each resource: its ref and action.
func planLines(p *Plan) []string {
	var out []string
	for i := range p.Resources {
		r := &p.Resources[i]
		out = append(out, fmt.Sprintf("%s %s %v", r.Ref.Kind, r.Ref.Scope+"/"+r.Ref.Name, r.Action))
	}
	slices.Sort(out)
	return out
}

// A database's rules follow its tagged port: moved to another server, or run on a
// second one beside it, its rules come along, as it has them, and where it no longer
// runs they go (#849's H1). Before, either planned with 5432 on every address of
// srv_B and no rule there.
func TestExternalAccessRulesFollowTheDatabase(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		s := newExternalStand(t, b)
		has := func(what string, p *Plan, want ...string) {
			t.Helper()
			got := planLines(p)
			for _, w := range want {
				if !slices.Contains(got, w) {
					t.Errorf("%s plans %v, without %q", what, got, w)
				}
			}
		}
		create, del := resource.ActionCreate.String(), resource.ActionDelete.String()
		moved := propose(t, s.e, "Move db to srv_B", s.run([]string{"srv_B"}, s.tagged))
		has("moving db to srv_B", moved,
			"firewall.rule srv_B/"+s.allowB.Name+" "+create, "firewall.rule srv_B/"+s.denyB.Name+" "+create,
			"firewall.rule srv_A/"+s.allowA.Name+" "+del, "firewall.rule srv_A/"+s.denyA.Name+" "+del)
		beside := propose(t, s.e, "Run db on srv_B too", s.run([]string{"srv_A", "srv_B"}, s.tagged))
		has("running db on srv_B too", beside, "firewall.rule srv_B/"+s.allowB.Name+" "+create, "firewall.rule srv_B/"+s.denyB.Name+" "+create)
		for _, line := range planLines(beside) {
			if strings.Contains(line, "srv_A/external-") {
				t.Errorf("running db on srv_B too touches its rules on srv_A: %s", line)
			}
		}
		// What the rules let in on srv_B is the list srv_A has.
		apply(t, s.e, beside)
		if err := s.db.View(t.Context(), func(tx *store.Tx) error {
			r, err := tx.GetResource(t.Context(), string(firewall.Kind), "srv_B", s.allowB.Name)
			if err != nil {
				return err
			}
			got := &resourcev1.FirewallRule{}
			if err := proto.Unmarshal(r.Spec, got); err != nil {
				return err
			}
			if !slices.Equal(got.GetSources(), []string{"203.0.113.0/24"}) || got.GetPort() != 5432 {
				t.Errorf("srv_B's allow lets in %v on %d", got.GetSources(), got.GetPort())
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// No change, by any path, leaves the tagged port answering more than its list: a
// whole-spec edit that drops the deny, the deny's delete or loosening, the allow
// widened, another allow of anyone on the port, the port on one public address or
// on ::0 with no deny, or the tag moved to a port the rules don't cover (#849's M3
// and M4).
func TestExternalAccessKeepsItsRules(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		s := newExternalStand(t, b)
		gone := "its rule that keeps everyone else out is gone"
		withEnv := s.run([]string{"srv_A"}, s.tagged)
		withEnv.Spec.(*resourcev1.Service).GetApp().Env = map[string]string{"TZ": "UTC"}
		s.refused("a whole-spec edit that keeps the port and drops the deny rule", gone, withEnv, Edit{Ref: s.denyA})
		s.refused("the deny rule's delete alone", gone, Edit{Ref: s.denyA})
		s.refused("a deny rule that lets an address through", "no longer covers port 5432 from anywhere", s.deny(s.denyA, "198.51.100.0/24"))
		s.refused("the allow rule from anywhere", "lets in more than a list may", s.allow(s.allowA))
		s.refused("the allow rule from 0.0.0.0/0 and ::/0", "lets in more than a list may", s.allow(s.allowA, "0.0.0.0/0", "::/0"))
		s.refused("another allow from anywhere on 5432", "past db's external access list", rule("srv_A", "open-pg", 5432))
		onePublic := proto.CloneOf(s.tagged)
		onePublic.HostIp = "198.51.100.10"
		s.refused("the port on one public address, deny deleted", gone, s.run([]string{"srv_A"}, onePublic), Edit{Ref: s.denyA})
		anyV6 := proto.CloneOf(s.tagged)
		anyV6.HostIp = "::0"
		s.refused("the port on ::0, deny deleted", gone, s.run([]string{"srv_A"}, anyV6), Edit{Ref: s.denyA})
		other := proto.CloneOf(s.tagged)
		other.HostPort = 5433
		s.refused("the tag moved to 5433, rules left on 5432", "no longer covers port 5433", s.run([]string{"srv_A"}, other))

		// What leaves the port answering nobody beyond the list plans: turning it off,
		// the port on the server alone, and the rules on a server it leaves going.
		for what, edits := range map[string][]Edit{
			"turning it off":               {s.run([]string{"srv_A"}), {Ref: s.allowA}, {Ref: s.denyA}},
			"the port on the server alone": {s.run([]string{"srv_A"}, s.loopback), {Ref: s.allowA}, {Ref: s.denyA}},
			"another list":                 {s.allow(s.allowA, "198.51.100.0/24")},
		} {
			if _, err := s.e.Propose(t.Context(), Proposal{Summary: what, Actor: "usr_test", Edits: edits}); err != nil {
				t.Errorf("%s = %v, want it planned", what, err)
			}
		}
	})
}

// Rolling the database back to the revision that had its tagged port, once external
// access is off, would bring the port back with no rules: it's refused, saying to
// turn it on again from its page (#849's H2). Before, RollBackTo planned it.
func TestARollbackDoesntBringBackAnUnguardedPort(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		s := newExternalStand(t, b)
		apply(t, s.e, propose(t, s.e, "Close db to everyone beyond its server", s.run([]string{"srv_A"}), Edit{Ref: s.allowA}, Edit{Ref: s.denyA}))
		var onRev int
		if err := s.db.View(t.Context(), func(tx *store.Tx) error {
			r, err := tx.GetResourceByID(t.Context(), s.id)
			onRev = r.Revision - 1
			return err
		}); err != nil {
			t.Fatal(err)
		}
		_, err := s.e.RollBackTo(t.Context(), s.id, onRev, func(store.Resource, proto.Message) (string, []Note) { return "Roll back db", nil }, By{Actor: "usr_test", Via: ViaAPI})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "Turn its external access off, or on again, from its page") {
			t.Errorf("rolling db back to r%d after its external access went = %v, want it refused", onRev, err)
		}
	})
}

// Removing the database's server and moving it takes its rules to where it goes;
// leaving or stopping it plans as before, compromised or not (#849's M8). Before, the
// move was refused with words that asked for a change on the server leaving.
func TestRemovingAServerMovesItsDatabasesRules(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		s := newExternalStand(t, b)
		for _, r := range []RemoveServer{
			{Server: "srv_A", Apps: AppsMove, MoveTo: "srv_B"},
			{Server: "srv_A", Apps: AppsMove, MoveTo: "srv_B", Compromised: true},
			{Server: "srv_A", Apps: AppsLeave, Compromised: true},
			{Server: "srv_A", Apps: AppsStop},
		} {
			p, err := s.e.RemoveServer(t.Context(), r, By{Actor: "usr_test"})
			if err != nil {
				t.Errorf("removing srv_A (%s, compromised %t) = %v", r.Apps, r.Compromised, err)
				continue
			}
			if r.Apps != AppsMove {
				continue
			}
			got := planLines(p)
			for _, want := range []string{"firewall.rule srv_B/" + s.allowB.Name, "firewall.rule srv_B/" + s.denyB.Name} {
				if !slices.ContainsFunc(got, func(l string) bool { return strings.HasPrefix(l, want+" ") }) {
					t.Errorf("removing srv_A and moving db plans %v, without %s", got, want)
				}
			}
		}
	})
}

// Deleting the database deletes its rules in the same change, so no list it had
// lets anyone reach what comes next on that port (#849's M7).
func TestDeletingADatabaseTakesItsExternalRules(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		s := newExternalStand(t, b)
		apply(t, s.e, propose(t, s.e, "Delete db", Edit{Ref: resource.Ref{Kind: "service", Scope: s.env, Name: "db"}}))
		if err := s.db.View(t.Context(), func(tx *store.Tx) error {
			for _, r := range []resource.Ref{s.allowA, s.denyA} {
				got, err := tx.GetResource(t.Context(), string(r.Kind), r.Scope, r.Name)
				if err == nil && got.Spec != nil {
					t.Errorf("%s stays after db is deleted", r.Name)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// A rule's name says whose external access it keeps, and nothing else reads as one.
func TestAnExternalRuleNamesItsService(t *testing.T) {
	name := ExternalRule("svc_01M7PWVFFEJVFN636WN6AEF71G", ExternalDeny)
	if name != "external-01m7pwvffejvfn636wn6aef71g-deny" {
		t.Errorf("the rule is named %q", name)
	}
	if id, ok := ExternalOf(name, ExternalDeny); !ok || id != "svc_01M7PWVFFEJVFN636WN6AEF71G" {
		t.Errorf("%s reads as %q, %t", name, id, ok)
	}
	for _, other := range []string{"http", "external-deny", "external-x-allow", "external--deny", "external-a-b-deny"} {
		if _, ok := ExternalOf(other, ExternalDeny); ok {
			t.Errorf("%s reads as an external access rule", other)
		}
	}
}

// What external access lets in is one network's range at most, 32 of them at most,
// and never everyone, however it's written (#849's M2).
func TestExternalAccessNeverLetsEveryoneIn(t *testing.T) {
	for _, given := range [][]string{
		nil,
		{""},
		{"0.0.0.0/0"},
		{"::/0"},
		{"::ffff:0.0.0.0/96"},
		{"::ffff:0:0/95"},
		{"::/64"},
		{"0.0.0.0/1", "128.0.0.0/1"},
		{"::/1", "8000::/1"},
		{"10.0.0.0/7"},
		{"2001:db8::/31"},
		{"not an address"},
	} {
		if got, err := AllowFrom(given); err == nil {
			t.Errorf("AllowFrom(%q) = %v, want it refused", given, got)
		}
	}
	many := make([]string, mostAllowFrom+1)
	for i := range many {
		many[i] = fmt.Sprintf("198.51.%d.0/24", i)
	}
	if _, err := AllowFrom(many); err == nil {
		t.Errorf("AllowFrom of %d ranges planned", len(many))
	}
	for given, want := range map[string][]string{
		"203.0.113.7":              {"203.0.113.7/32"},
		"10.0.0.0/8":               {"10.0.0.0/8"},
		"2001:db8::/32":            {"2001:db8::/32"},
		"::ffff:203.0.113.0/120":   {"203.0.113.0/24"},
		"203.0.113.7, 203.0.113.7": {"203.0.113.7/32"},
	} {
		got, err := AllowFrom(strings.Split(given, ","))
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("AllowFrom(%q) = %v, %v, want %v", given, got, err, want)
		}
	}
}

// A port is beyond its server unless it's on loopback alone (#849's M4).
func TestAPortIsPublicUnlessOnLoopback(t *testing.T) {
	for ip, want := range map[string]bool{
		"": true, "0.0.0.0": true, "::": true, "::0": true, "[::]": true, "198.51.100.10": true, "2001:db8::1": true,
		"127.0.0.1": false, "127.0.0.2": false, "::1": false, "[::1]": false, "::ffff:127.0.0.1": false,
	} {
		if got := Public(ip); got != want {
			t.Errorf("Public(%q) = %t, want %t", ip, got, want)
		}
	}
}

// Turning external access on loads its rules before the port they guard opens, so the
// port never answers before its list does (#849's M6). Before, the service's step ran
// first, and its port answered everyone until the rules loaded.
func TestExternalRulesLoadBeforeThePortOpens(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		s := newExternalStand(t, b)
		apply(t, s.e, propose(t, s.e, "Close db to everyone beyond its server", s.run([]string{"srv_A"}), Edit{Ref: s.allowA}, Edit{Ref: s.denyA}))
		on := propose(t, s.e, "Let 203.0.113.0/24 reach db on port 5432", s.run([]string{"srv_A"}, s.tagged), s.allow(s.allowA, "203.0.113.0/24"), s.deny(s.denyA))
		apply(t, s.e, on)
		s.e.Wait()
		var order []string
		if err := s.db.View(t.Context(), func(tx *store.Tx) error {
			c, err := tx.GetChange(t.Context(), on.Change)
			if err != nil {
				return err
			}
			p, err := decodeProgress(c.Progress)
			if err != nil {
				return err
			}
			steps := slices.Clone(p.Steps)
			slices.SortStableFunc(steps, func(a, b StepProgress) int { return a.Started.Compare(b.Started) })
			for _, st := range steps {
				order = append(order, string(st.Ref.Kind))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(order, []string{"firewall.rule", "firewall.rule", "service"}) {
			t.Errorf("turning it on ran %v, want both rules before the service", order)
		}
	})
}
