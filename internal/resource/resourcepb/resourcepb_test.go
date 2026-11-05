// SPDX-License-Identifier: AGPL-3.0-only

package resourcepb

import (
	"errors"
	"reflect"
	"testing"

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
