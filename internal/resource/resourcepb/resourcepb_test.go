// SPDX-License-Identifier: AGPL-3.0-only

package resourcepb

import (
	"errors"
	"maps"
	"reflect"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

// A step reads back as it was written.
func TestAStepReadsBack(t *testing.T) {
	s := resource.Step{
		Ref: resource.Ref{Kind: "firewall.rule", Scope: "srv_1", Name: "ssh"},
		Decision: resource.Decision{
			Action: resource.ActionReplace, Effect: resource.EffectRestart, Destroys: true, Warnings: []resource.Warning{resource.WarningMayLockOut},
		},
	}
	got, err := FromStep(Step(s, ""))
	if err != nil || got.Action != s.Action || got.Effect != s.Effect || !got.Destroys || len(got.Warnings) != 1 || got.Ref != s.Ref {
		t.Errorf("FromStep(Step(%+v)) = %+v, %v", s, got, err)
	}
}

// How a step replaces what runs, and the restart it is, survive the trip to the
// agent and back, or the agent would stop first where a plan said it wouldn't.
func TestARolloutReadsBack(t *testing.T) {
	s := resource.Step{
		Ref: resource.Ref{Kind: "service", Scope: "env_1", Name: "web"},
		Decision: resource.Decision{
			Action: resource.ActionReplace, Effect: resource.EffectRoll, Restart: "chg_1",
			Rollout: &resource.RolloutPlan{
				Rolling: true, Ports: []string{"8080/tcp"}, Volumes: []string{"data"}, Paths: []string{"/srv/a"}, ImageData: []string{"/var/lib/x"}, Chosen: true,
				Drain: 30 * time.Second, StopGrace: time.Minute, HealthyWithin: 2 * time.Minute, Others: []string{"env_1-web-2"}, NoCheck: true,
				RoutePorts: map[string]uint32{"www": 9090},
			},
		},
	}
	got, err := FromStep(Step(s, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got.Effect != resource.EffectRoll || got.Restart != "chg_1" {
		t.Errorf("read back as %+v", got)
	}
	if !reflect.DeepEqual(got.Rollout, s.Rollout) {
		t.Errorf("the rollout read back as %+v, want %+v", got.Rollout, s.Rollout)
	}
	none, err := FromStep(Step(resource.Step{Decision: resource.Decision{Action: resource.ActionCreate}}, ""))
	if err != nil || none.Rollout != nil {
		t.Errorf("a step with no rollout read back with %+v, %v", none.Rollout, err)
	}
	if b := (resource.Decision{Downtime: time.Second, UntilHealthy: time.Minute, Rollout: s.Rollout}).Budget(); b != time.Minute+2*time.Minute+30*time.Second+time.Minute {
		t.Errorf("the step's budget is %s", b)
	}
}

// A step that says what this build doesn't know, as from a newer agent, is
// an error, never a step read as something else: an unknown action read as
// none would be skipped, and a dropped warning could hide a destructive one.
func TestAStepThisBuildDoesntKnowIsAnError(t *testing.T) {
	known := func() *dockwellv1.PlanStep {
		return Step(resource.Step{Decision: resource.Decision{Action: resource.ActionCreate, Warnings: []resource.Warning{resource.WarningMayLockOut}}}, "")
	}
	for what, change := range map[string]func(*dockwellv1.PlanStep){
		"action":  func(p *dockwellv1.PlanStep) { p.Action = 42 },
		"effect":  func(p *dockwellv1.PlanStep) { p.Effect = 42 },
		"warning": func(p *dockwellv1.PlanStep) { p.Warnings = append(p.Warnings, 42) },
	} {
		p := known()
		change(p)
		_, err := FromStep(p)
		var unknown *UnknownError
		if !errors.As(err, &unknown) || unknown.What != what || unknown.Value != 42 {
			t.Errorf("a step with %s 42 = %v, want an UnknownError for it", what, err)
		}
	}
}

// A step's word on the ingress survives the trip to the agent and back, and
// one this build doesn't know fails rather than read as nothing.
func TestAStepCarriesItsIngressEffect(t *testing.T) {
	step := resource.Step{Decision: resource.Decision{
		Action:  resource.ActionCreate,
		Ingress: &resource.IngressEffect{Change: resource.IngressStops, Proxy: "Caddy", Ports: []string{"80/tcp"}, PanelDomain: "panel.example.com"},
		Domain:  &resource.DomainCheck{Domain: "a.example.com", State: resource.DomainCantTell, Found: []string{"1.2.3.4"}, Here: []string{"10.0.0.1"}},
	}}
	p := Step(step, "")
	got, err := FromStep(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Ingress, step.Ingress) {
		t.Errorf("Ingress = %+v, want %+v", got.Ingress, step.Ingress)
	}
	if !reflect.DeepEqual(got.Domain, step.Domain) {
		t.Errorf("Domain = %+v, want %+v", got.Domain, step.Domain)
	}
	p.Ingress.Change = 99
	if _, err := FromStep(p); err == nil {
		t.Error("a step with an ingress change this build doesn't know was read")
	}
}

// Every kind of access a plan can hold has a name in the API, or a person would
// approve what a surface can't say.
func TestEveryAccessKindHasAName(t *testing.T) {
	for k := resource.AccessPath; k <= resource.AccessPrivileged; k++ {
		got := HostAccess([]resource.Access{{Kind: k}})
		if got[0].GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_UNSPECIFIED {
			t.Errorf("the kind of access %d has no name in the API", k)
		}
	}
	if got := HostAccess([]resource.Access{{Kind: resource.AccessPrivileged, SameAsRoot: true}}); !got[0].GetSameAsRoot() {
		t.Error("what is the same as root doesn't say so in the API")
	}
}

// All a step takes from an agent's word on host access is where a path leads: what
// it reaches, and how far that goes, is the control plane's to decide.
func TestAStepTakesOnlyWhereAPathLeadsFromAnAgent(t *testing.T) {
	sent := resource.Step{
		Ref:      resource.Ref{Kind: "container", Scope: "srv_A", Name: "web"},
		Decision: resource.Decision{Action: resource.ActionCreate, Leads: map[string]string{"/srv/link": "/etc", "/srv/a": "/srv/b"}},
	}
	p := Step(sent, "res_1")
	p.HostAccess = append(p.HostAccess, &dockwellv1.HostAccess{Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOCKER_SOCKET, Path: "/lie", SameAsRoot: false})
	got, err := FromStep(p)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got.Leads, sent.Leads) {
		t.Errorf("leads = %v, want %v", got.Leads, sent.Leads)
	}
	if len(got.HostAccess) != 0 {
		t.Errorf("an agent's word on access came through: %v", got.HostAccess)
	}
	// The order on the wire doesn't move, as a plan's fingerprint reads what comes of it.
	if first := Step(sent, "res_1").GetHostAccess()[0].GetPath(); first != "/srv/a" {
		t.Errorf("the first lead on the wire is %s", first)
	}
}

// So does which app a route leaves.
func TestARouteMovingSurvivesTheWayToTheAPI(t *testing.T) {
	s := resource.Step{Decision: resource.Decision{Moves: &resource.RouteMove{Where: "a.example.com", From: "api", To: "nginx", ToPort: 8080}}}
	back, err := FromStep(Step(s, "rte_1"))
	if err != nil {
		t.Fatal(err)
	}
	if back.Moves == nil || *back.Moves != *s.Moves {
		t.Errorf("it came back as %+v", back.Moves)
	}
}

// What a visitor finds crosses from the agent to the control plane and the API whole.
func TestARouteGoingSurvivesTheWayToTheAPI(t *testing.T) {
	for _, find := range []resource.VisitorsFind{resource.VisitorsLoseIt, resource.VisitorsRefused, resource.VisitorsTLSError, resource.VisitorsNotFound, resource.VisitorsRest, resource.VisitorsNothingBehind} {
		s := resource.Step{Decision: resource.Decision{Goes: &resource.RouteGoes{Where: "a.example.com/api", Domain: "a.example.com", Find: find}}}
		back, err := FromStep(Step(s, "rte_1"))
		if err != nil {
			t.Fatal(err)
		}
		if back.Goes == nil || *back.Goes != *s.Goes {
			t.Errorf("%d came back as %+v", find, back.Goes)
		}
	}
}
