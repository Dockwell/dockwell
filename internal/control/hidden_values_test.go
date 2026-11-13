// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A hidden value of an app's own becomes a secret of its environment in one plan,
// with the value the app has, which nobody reads on the way; from then on a passkey
// reveals it, as any secret. One that stays the app's own is revealed with a passkey
// too, for that value alone, and on record (ADR 0081).
func TestAHiddenValueMovesToASecretOrIsRevealed(t *testing.T) {
	storetest.Each(t, testAHiddenValueMovesToASecretOrIsRevealed)
}

func testAHiddenValueMovesToASecretOrIsRevealed(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	c := changeClient(b, b.origin)
	svc := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	v := variablesClient(b, b.origin)

	// An app as a person's own Compose file makes one: its values hidden.
	made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Run web",
		Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "web"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
				Image: "nginx:1.27", Env: map[string]string{"SESSION_SECRET": "hunter2-example", "MODE": "production"},
			}}}}},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, made.Msg.GetChange())
	read := func() *dockwellv1.ServiceInfo {
		t.Helper()
		return onlyService(t, svc)
	}
	web := read()
	if got := web.GetSpec().GetApp().GetEnv(); got["SESSION_SECRET"] != "" || got["MODE"] != "" {
		t.Fatalf("a person's own values read as %v", got)
	}

	// Revealing one takes a passkey for that value.
	if _, err := v.RevealServiceValue(t.Context(), connect.NewRequest(&dockwellv1.RevealServiceValueRequest{ServiceId: web.GetId(), Name: "MODE"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("a reveal with no passkey: %v", err)
	}
	other := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_REVEAL_SERVICE_VALUE, web.GetId()+":SESSION_SECRET")
	if _, err := v.RevealServiceValue(t.Context(), connect.NewRequest(&dockwellv1.RevealServiceValueRequest{ServiceId: web.GetId(), Name: "MODE", Proof: other})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("a reveal with a passkey for another value: %v", err)
	}
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_REVEAL_SERVICE_VALUE, web.GetId()+":MODE")
	got, err := v.RevealServiceValue(t.Context(), connect.NewRequest(&dockwellv1.RevealServiceValueRequest{ServiceId: web.GetId(), Name: "MODE", Proof: proof}))
	if err != nil || got.Msg.GetValue() != "production" {
		t.Fatalf("RevealServiceValue = %v, %v", got, err)
	}
	if _, err := v.RevealServiceValue(t.Context(), connect.NewRequest(&dockwellv1.RevealServiceValueRequest{ServiceId: web.GetId(), Name: "NONE", Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_REVEAL_SERVICE_VALUE, web.GetId()+":NONE")})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("a value it hasn't got: %v", err)
	}
	events, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events.Msg.GetEvents(), func(e *dockwellv1.Event) bool {
		return e.GetAction() == "service.value_reveal" && e.GetSubject().GetId() == web.GetId() && e.GetSubject().GetValueName() == "MODE"
	}) {
		t.Error("the reveal isn't on record")
	}

	// Made a secret, with the value the app has, in one plan that never says it.
	spec := proto.CloneOf(web.GetSpec())
	spec.GetApp().Env = nil
	moving, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Make SESSION_SECRET a secret",
		Edits: []*dockwellv1.Edit{{
			Resource: web.GetResource(), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: spec}},
			BaseRevision: web.GetRevision(), KeepEnv: []string{"MODE"}, EnvToSecrets: []string{"SESSION_SECRET"},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prototext.Format(moving.Msg.GetChange()), "hunter2-example") {
		t.Fatal("the plan says the value it moves")
	}
	apply(t, c, moving.Msg.GetChange())
	web = read()
	if _, still := web.GetSpec().GetApp().GetEnv()["SESSION_SECRET"]; still || web.GetSpec().GetSecrets()["SESSION_SECRET"] != "SESSION_SECRET" {
		t.Fatalf("after the move web has env %v and secrets %v", web.GetSpec().GetApp().GetEnv(), web.GetSpec().GetSecrets())
	}
	list, err := v.ListVariables(t.Context(), connect.NewRequest(&dockwellv1.ListVariablesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(list.Msg.GetVariables(), func(x *dockwellv1.EnvironmentVariable) bool { return x.GetName() == "SESSION_SECRET" })
	if i < 0 || !list.Msg.GetVariables()[i].GetSecret() {
		t.Fatalf("the environment has %v", list.Msg.GetVariables())
	}
	secret := list.Msg.GetVariables()[i]
	shown, err := v.RevealSecret(t.Context(), connect.NewRequest(&dockwellv1.RevealSecretRequest{Id: secret.GetId(), Proof: stepUp(t, b, secret.GetId())}))
	if err != nil || shown.Msg.GetValue() != "hunter2-example" {
		t.Fatalf("the secret holds %v, %v", shown, err)
	}
	if ct := e.Serving(docker.Project(web.GetResource().GetScope()) + "-web-1"); ct == nil || !hasEnv(ct.Config.Env, "SESSION_SECRET=hunter2-example") {
		t.Errorf("Docker runs %+v", ct)
	}

	// Making a secret of a name the environment has already is refused.
	web = read()
	spec = proto.CloneOf(web.GetSpec())
	spec.GetApp().Env = nil
	spec.Secrets = map[string]string{}
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Make MODE a secret twice",
		Edits: []*dockwellv1.Edit{
			{
				Resource: &dockwellv1.ResourceRef{Kind: "variable", Scope: web.GetResource().GetScope(), Name: "MODE"},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: "x"}}},
			},
			{
				Resource: web.GetResource(), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: spec}},
				BaseRevision: web.GetRevision(), EnvToSecrets: []string{"MODE"},
			},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "makes MODE already") {
		t.Errorf("moving into a name the change makes: %v", err)
	}
}
