// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"log/slog"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func claimant(env, name string, app *resourcev1.Container, servers ...string) Edit {
	if app.Image == "" {
		app.Image = "nginx:1.27"
	}
	return Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: name}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}, Servers: servers}
}

func backNetwork(env string, servers ...string) Edit {
	return Edit{Ref: resource.Ref{Kind: network.Kind, Scope: env, Name: "back"}, Spec: &resourcev1.Network{}, Servers: servers}
}

// A stored service is sealed, and a change to another service alone touches no
// volume or network, so nothing else opens it: the check of names has to, or it
// sees nothing stored at all, and says nothing.
func TestANameCantTakeOneThatIsStored(t *testing.T) {
	storetest.Each(t, testANameCantTakeOneThatIsStored)
}

func testANameCantTakeOneThatIsStored(t *testing.T, backend store.Backend) {
	e, _, db := setupDocker(t, backend, "srv_A", "srv_B")
	env := production(t, db)
	apply(t, e, propose(t, e, "Set up", backNetwork(env, "srv_A", "srv_B"),
		claimant(env, "db", &resourcev1.Container{Networks: []string{"back"}}, "srv_A"),
		claimant(env, "web", &resourcev1.Container{Networks: []string{"back"}, Aliases: []string{"site"}}, "srv_A"),
	))
	refused := map[string]Edit{
		"another's name":                  claimant(env, "web", &resourcev1.Container{Networks: []string{"back"}, Aliases: []string{"db"}}, "srv_A"),
		"another's name, in another case": claimant(env, "web", &resourcev1.Container{Networks: []string{"back"}, Aliases: []string{"DB"}}, "srv_A"),
		"another's alias":                 claimant(env, "api", &resourcev1.Container{Networks: []string{"back"}, Aliases: []string{"site"}}, "srv_A"),
		"a new service's own name":        claimant(env, "site", &resourcev1.Container{Networks: []string{"back"}}, "srv_A"),
	}
	for name := range refused {
		edit := refused[name]
		t.Run(name, func(t *testing.T) {
			_, err := e.Propose(t.Context(), Proposal{Summary: name, Edits: []Edit{edit}})
			if err == nil || !strings.Contains(err.Error(), "would both answer to") {
				t.Errorf("Propose = %v, want it refused", err)
			}
		})
	}
	// On a server the other isn't on, each has a network of its own.
	apart := claimant(env, "site", &resourcev1.Container{Networks: []string{"back"}}, "srv_B")
	if _, err := e.Propose(t.Context(), Proposal{Summary: "Apart", Edits: []Edit{apart}}); err != nil {
		t.Errorf("a name on another server = %v", err)
	}
	// But moving one there meets what it would clash with.
	p := propose(t, e, "Run site", claimant(env, "site", &resourcev1.Container{Networks: []string{"back"}}, "srv_B"))
	apply(t, e, p)
	moved := claimant(env, "web", &resourcev1.Container{Networks: []string{"back"}, Aliases: []string{"site"}}, "srv_B")
	if _, err := e.Propose(t.Context(), Proposal{Summary: "Move web", Edits: []Edit{moved}}); err == nil || !strings.Contains(err.Error(), "would both answer to site") {
		t.Errorf("moving onto a server where it clashes = %v, want it refused", err)
	}
}

// Only what could clash opens another service's spec: a new image, or anything
// else that leaves every name where it was, opens none, as a rotation doesn't.
func TestANewImageOpensNoOtherService(t *testing.T) {
	storetest.Each(t, testANewImageOpensNoOtherService)
}

func testANewImageOpensNoOtherService(t *testing.T, backend store.Backend) {
	_, f, db := setup(t, backend, "srv_A")
	kinds := testKinds()
	sealer := &opening{Sealer: secretstest.Keeper(db, kinds), opened: map[string]int{}}
	e := New(db, kinds, f, sealer, slog.New(slog.DiscardHandler))
	env := production(t, db)
	apply(t, e, propose(t, e, "Set up",
		claimant(env, "web", &resourcev1.Container{Aliases: []string{"site"}, ExternalNetworks: []string{"edge"}}, "srv_A"),
		claimant(env, "other", &resourcev1.Container{ExternalNetworks: []string{"edge"}}, "srv_A"),
	))
	var otherID string
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(service.Kind), env, "other")
		otherID = r.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reset := func() {
		sealer.mu.Lock()
		clear(sealer.opened)
		sealer.mu.Unlock()
	}
	reset()
	propose(t, e, "A new image", claimant(env, "web", &resourcev1.Container{Image: "nginx:1.28", Aliases: []string{"site"}, ExternalNetworks: []string{"edge"}}, "srv_A"))
	if n := sealer.count(otherID); n != 0 {
		t.Errorf("a new image opened another service's spec %d times", n)
	}
	reset()
	propose(t, e, "A new name", claimant(env, "web", &resourcev1.Container{Aliases: []string{"site", "www"}, ExternalNetworks: []string{"edge"}}, "srv_A"))
	if sealer.count(otherID) == 0 {
		t.Error("a new name opened no other service's spec, so it can't have looked for a clash")
	}
}

// A network on one server and not another: a plan is refused on the one that
// hasn't got it, by name, and placing a service on another server counts as new
// access there, as it reaches that server's network.
func TestANetworkOnOneServerAndNotAnother(t *testing.T) {
	storetest.Each(t, testANetworkOnOneServerAndNotAnother)
}

func testANetworkOnOneServerAndNotAnother(t *testing.T, backend store.Backend) {
	e, engines, db := setupDocker(t, backend, "srv_A", "srv_B")
	env := production(t, db)
	engines["srv_A"].AddNetwork(docker.Network{Name: "edge", Driver: "bridge"})
	spec := func() *resourcev1.Container { return &resourcev1.Container{ExternalNetworks: []string{"edge"}} }
	if _, err := e.Propose(t.Context(), Proposal{Summary: "Both", Edits: []Edit{claimant(env, "web", spec(), "srv_A", "srv_B")}}); err == nil ||
		!strings.Contains(err.Error(), "the network edge isn't on this server") || !strings.Contains(err.Error(), "srv_B") {
		t.Errorf("planning on a server without it = %v, want it refused, by name, for srv_B", err)
	}
	first := propose(t, e, "One", claimant(env, "web", spec(), "srv_A"))
	if !first.HostAccess() {
		t.Fatalf("joining it plans no host access: %+v", first.Resources)
	}
	apply(t, e, first)
	same := propose(t, e, "Same", claimant(env, "web", &resourcev1.Container{Image: "nginx:1.28", ExternalNetworks: []string{"edge"}}, "srv_A"))
	if same.HostAccess() {
		t.Errorf("a new image on the same server plans host access: %+v", same.Resources)
	}
	engines["srv_B"].AddNetwork(docker.Network{Name: "edge", Driver: "bridge"})
	spread := propose(t, e, "Spread", claimant(env, "web", spec(), "srv_A", "srv_B"))
	if !spread.HostAccess() {
		t.Errorf("placing it on another server plans no host access, so a token applies it: %+v", spread.Resources)
	}
	if n := engines["srv_B"].Network("edge"); n == nil {
		t.Error("planning took the network")
	}
}

// A rotation of a service on a network moves no name, so it opens no other service
// on that network, as ADR 0022 has it for a rotation.
func TestARotationOnANetworkOpensNoOtherService(t *testing.T) {
	storetest.Each(t, testARotationOnANetworkOpensNoOtherService)
}

func testARotationOnANetworkOpensNoOtherService(t *testing.T, backend store.Backend) {
	_, f, db := setup(t, backend, "srv_A")
	kinds := testKinds()
	sealer := &opening{Sealer: secretstest.Keeper(db, kinds), opened: map[string]int{}}
	e := New(db, kinds, f, sealer, slog.New(slog.DiscardHandler))
	env := production(t, db)
	api := user(env, "api", "srv_A", map[string]string{"DATABASE_PASSWORD": "DB_PASSWORD"}, nil)
	api.Spec.(*resourcev1.Service).GetApp().ExternalNetworks = []string{"edge"}
	apply(t, e, propose(t, e, "Set up", secretEdit(env, "DB_PASSWORD", "correct-horse-1"), api,
		claimant(env, "other", &resourcev1.Container{ExternalNetworks: []string{"edge"}}, "srv_A"),
	))
	var otherID string
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(service.Kind), env, "other")
		otherID = r.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sealer.mu.Lock()
	clear(sealer.opened)
	sealer.mu.Unlock()
	p := propose(t, e, "Rotate", secretEdit(env, "DB_PASSWORD", "correct-horse-2"))
	if planned(p, "api") == nil {
		t.Fatalf("the rotation doesn't take in the service that uses it: %+v", p.Resources)
	}
	if n := sealer.count(otherID); n != 0 {
		t.Errorf("a rotation opened another service on the network %d times", n)
	}
}
