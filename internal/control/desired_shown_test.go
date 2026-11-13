// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
)

// A revision that only shows one of an app's values changes nothing on its
// server, so no step applies it. An agent that connects lacking what the server
// runs, as one with a new cache does, is still sent it, with the values it uses,
// and keeps it: the send compares what reaches the server, not the revision
// number (#593). A server already in this state heals the same way, on connect.
func TestAnAgentKeepsAnAppAfterARevisionThatOnlyShowsAValue(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	s := f.lastDocker
	server := s.id.ServerID
	c := changeClient(b, b.origin)
	shop := func(shown []string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(service.Kind), Name: "shop"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs:     &resourcev1.Service_App{App: &resourcev1.Container{Image: "ghcr.io/acme/shop:1.4", Env: map[string]string{"SHOP_TITLE": "Acme shop"}}},
				Secrets:  map[string]string{"API_TOKEN": "API_TOKEN"},
				ShownEnv: shown,
			}}},
		}
	}
	made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Run shop",
		Edits: []*dockwellv1.Edit{
			{
				Resource: &dockwellv1.ResourceRef{Kind: "secret", Name: "API_TOKEN"},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "tok-s3cret"}}},
			},
			shop(nil),
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, made.Msg.GetChange())
	shown, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Show shop's title", Edits: []*dockwellv1.Edit{shop([]string{"SHOP_TITLE"})},
	}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, shown.Msg.GetChange())

	// The agent comes back keeping what its server runs, in a new cache, so it lacks
	// shop, as an agent from before ADR 0054 or one whose records were lost does.
	s.stop()
	s.keeps = true
	s.connect(t, f)
	filled, err := f.engine.Remember(t.Context(), server)
	if err != nil {
		t.Fatalf("the fill on connect refused: %v", err)
	}
	if filled.Kept != 1 || filled.WithValues != 1 {
		t.Fatalf("the fill kept %+v, want shop, with its values", filled)
	}
	a, ok := f.agents.Agent(server)
	if !ok {
		t.Fatal("hz-1 isn't connected")
	}
	held, _, keeps := a.(interface {
		Holds() (map[resource.Ref]int64, string, bool)
	}).Holds()
	kept := false
	for ref := range held {
		kept = kept || ref.Kind == service.Kind && ref.Name == "shop"
	}
	if !keeps || !kept {
		t.Fatalf("hz-1's agent keeps %v", held)
	}
	// And the next connect has nothing left to send.
	if again, err := f.engine.Remember(t.Context(), server); err != nil || again.Kept != 0 {
		t.Errorf("a second fill = %+v, %v; want nothing to send", again, err)
	}
}
