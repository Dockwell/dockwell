// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func secretEdit(env, name, value string) Edit {
	return Edit{Ref: resource.Ref{Kind: variable.SecretKind, Scope: env, Name: name}, Spec: &resourcev1.Secret{Value: value}}
}

func variableEdit(env, name, value string) Edit {
	return Edit{Ref: resource.Ref{Kind: variable.VariableKind, Scope: env, Name: name}, Spec: &resourcev1.Variable{Value: value}}
}

// user is a service named name, on server, that uses secrets and
// variables by the names it sees them as.
func user(env, name, server string, secrets, variables map[string]string) Edit {
	return Edit{
		Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: name},
		Spec: &resourcev1.Service{
			Runs:    &resourcev1.Service_App{App: &resourcev1.Container{Image: "acme/" + name + ":1"}},
			Secrets: secrets, Variables: variables,
		},
		Servers: []string{server},
	}
}

// containerOf is a service's container on a server.
func containerOf(t *testing.T, s *server, env, name string) *resourcev1.Container {
	t.Helper()
	c, err := s.docker.Inspect(t.Context(), service.Target(resource.Ref{Kind: service.Kind, Scope: env, Name: name}))
	if err != nil || c == nil {
		t.Fatalf("no container for %s: %v", name, err)
	}
	return c
}

func planned(p *Plan, name string) *ResourcePlan {
	for i := range p.Resources {
		if p.Resources[i].Ref.Name == name {
			return &p.Resources[i]
		}
	}
	return nil
}

// Rotating a secret is a plan: it lists every service that uses it, on
// every server each runs on, each restarting with the new value, and
// never the value itself. Applying it restarts them with the new value,
// and undoing it with the old.
func TestRotatingASecretIsAPlan(t *testing.T) { storetest.Each(t, testRotatingASecretIsAPlan) }

func testRotatingASecretIsAPlan(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A", "srv_B")
	env := production(t, db)
	apply(t, e, propose(t, e, "Set up the shop",
		secretEdit(env, "DB_PASSWORD", "correct-horse-1"),
		variableEdit(env, "LOG_LEVEL", "info"),
		user(env, "api", "srv_A", map[string]string{"DATABASE_PASSWORD": "DB_PASSWORD"}, map[string]string{"LOG_LEVEL": ""}),
		user(env, "worker", "srv_B", map[string]string{"DB_PASSWORD": ""}, nil),
		user(env, "static", "srv_A", nil, map[string]string{"LOG_LEVEL": ""}),
	))
	api := containerOf(t, f["srv_A"], env, "api")
	if api.GetEnv()["DATABASE_PASSWORD"] != "correct-horse-1" || api.GetEnv()["LOG_LEVEL"] != "info" {
		t.Fatalf("api runs with %v, want its secret and variable", api.GetEnv())
	}
	if got := api.GetLabels()[binding.LabelKey]; got != "DATABASE_PASSWORD=secret:DB_PASSWORD@1,LOG_LEVEL=variable:LOG_LEVEL@1" {
		t.Errorf("api's label = %q", got)
	}
	if strings.Contains(api.GetLabels()[binding.LabelKey], "correct-horse") {
		t.Error("the label holds the value")
	}
	if c, _ := f["srv_B"].docker.Inspect(t.Context(), service.Target(resource.Ref{Scope: env, Name: "api"})); c != nil {
		t.Error("api runs on srv_B too")
	}
	for _, server := range []string{"srv_A", "srv_B"} {
		if drift, err := e.Drift(t.Context(), server); err != nil || len(drift) != 0 {
			t.Fatalf("drift on %s right after applying: %+v, %v", server, drift, err)
		}
	}

	p := propose(t, e, "Rotate the database password", secretEdit(env, "DB_PASSWORD", "correct-horse-2"))
	var names []string
	for i := range p.Resources {
		names = append(names, p.Resources[i].Ref.Name+" "+p.Resources[i].Action.String())
	}
	if got := strings.Join(names, ", "); got != "DB_PASSWORD update, api replace, worker replace" {
		t.Errorf("the rotation's plan touches %s, want the secret and the two services that use it, restarting", got)
	}
	secretChanges := planned(p, "DB_PASSWORD").Changes
	if len(secretChanges) != 1 || !secretChanges[0].Sensitive || secretChanges[0].From != "" || secretChanges[0].To != "" {
		t.Errorf("the secret's change = %+v, want value, sensitive, with no values", secretChanges)
	}
	if c := change(t, p.Steps(), "revisions[DATABASE_PASSWORD]"); c.From != "1" || c.To != "2" {
		t.Errorf("api's step = %+v, want the secret's revision going from 1 to 2", c)
	}
	var servers []string
	for _, s := range p.Servers {
		for i := range s.Steps {
			step := &s.Steps[i]
			servers = append(servers, s.Server+":"+step.Ref.Name+":"+step.Action.String())
			if step.Effect != resource.EffectRecreate {
				t.Errorf("%s on %s has effect %v, want a restart", step.Ref.Name, s.Server, step.Effect)
			}
		}
	}
	if got := strings.Join(servers, " "); got != "srv_A:api:replace srv_B:worker:replace" {
		t.Errorf("the rotation's steps are %s", got)
	}
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		rec, err := tx.GetChange(t.Context(), p.Change)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(rec.Plan, []byte("correct-horse")) || strings.Contains(rec.Fingerprint, "correct") {
			t.Error("the recorded plan holds the secret")
		}
		return nil
	})
	apply(t, e, p)
	if got := containerOf(t, f["srv_A"], env, "api").GetEnv()["DATABASE_PASSWORD"]; got != "correct-horse-2" {
		t.Errorf("after the rotation api has %q", got)
	}
	if got := containerOf(t, f["srv_B"], env, "worker").GetEnv()["DB_PASSWORD"]; got != "correct-horse-2" {
		t.Errorf("after the rotation worker has %q", got)
	}
	if got := containerOf(t, f["srv_A"], env, "static").GetEnv(); len(got) != 1 {
		t.Errorf("static, which doesn't use the secret, has %v", got)
	}

	undo, err := e.Undo(t.Context(), p.Change, By{Actor: "usr_test", Via: ViaPanel})
	if err != nil {
		t.Fatal(err)
	}
	if planned(undo, "api") == nil || planned(undo, "worker") == nil {
		t.Errorf("undoing the rotation touches %+v, want both services back", undo.Resources)
	}
	apply(t, e, undo)
	if got := containerOf(t, f["srv_A"], env, "api").GetEnv()["DATABASE_PASSWORD"]; got != "correct-horse-1" {
		t.Errorf("after undoing the rotation api has %q", got)
	}
}

// A service can use only what its environment has, and what a service
// uses can't go while it does. A variable and a secret can't share a
// name in one environment.
func TestUsesAreChecked(t *testing.T) { storetest.Each(t, testUsesAreChecked) }

func testUsesAreChecked(t *testing.T, backend store.Backend) {
	e, _, db := setup(t, backend, "srv_A")
	env := production(t, db)
	refuse := func(what, want string, edits ...Edit) {
		t.Helper()
		_, err := e.Propose(t.Context(), Proposal{Summary: what, Actor: "usr_test", Edits: edits})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want it refused with %q", what, err, want)
		}
	}
	refuse("using a secret that isn't there", "uses secret DB_PASSWORD as DATABASE_PASSWORD, which isn't in its environment",
		user(env, "api", "srv_A", map[string]string{"DATABASE_PASSWORD": "DB_PASSWORD"}, nil))
	apply(t, e, propose(t, e, "Set up", secretEdit(env, "DB_PASSWORD", "a"),
		user(env, "api", "srv_A", map[string]string{"DATABASE_PASSWORD": "DB_PASSWORD"}, nil)))
	refuse("deleting a secret a service uses", "uses secret DB_PASSWORD as DATABASE_PASSWORD, which this change deletes",
		Edit{Ref: resource.Ref{Kind: variable.SecretKind, Scope: env, Name: "DB_PASSWORD"}})
	refuse("a variable named as a secret", "there's a secret named DB_PASSWORD in that environment already",
		variableEdit(env, "DB_PASSWORD", "b"))
	refuse("a name no environment takes", "isn't a valid secret name", secretEdit(env, "db-password", "c"))
	refuse("an empty secret", "a secret needs a value", secretEdit(env, "EMPTY", ""))
	// Deleting both in one change is fine.
	apply(t, e, propose(t, e, "Tear down",
		Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: "api"}},
		Edit{Ref: resource.Ref{Kind: variable.SecretKind, Scope: env, Name: "DB_PASSWORD"}}))
}

// A change applies only while what it pinned is still the latest: a
// service planned with a secret's old revision, or a service that started
// using a secret after its rotation was planned, would run with the wrong
// value, so the change is out of date.
func TestPinsHoldUntilApplied(t *testing.T) { storetest.Each(t, testPinsHoldUntilApplied) }

func testPinsHoldUntilApplied(t *testing.T, backend store.Backend) {
	e, _, db := setup(t, backend, "srv_A")
	env := production(t, db)
	apply(t, e, propose(t, e, "Set up", secretEdit(env, "TOKEN", "one"), secretEdit(env, "OTHER", "x")))

	usesIt := propose(t, e, "Run api", user(env, "api", "srv_A", map[string]string{"TOKEN": ""}, nil))
	apply(t, e, propose(t, e, "Rotate", secretEdit(env, "TOKEN", "two")))
	if err := e.Apply(t.Context(), usesIt.Change); !errors.Is(err, ErrStale) || !strings.Contains(err.Error(), "secret TOKEN changed after") {
		t.Errorf("applying a service pinned to a rotated secret: %v, want it out of date", err)
	}

	rotation := propose(t, e, "Rotate other", secretEdit(env, "OTHER", "y"))
	apply(t, e, propose(t, e, "Run worker", user(env, "worker", "srv_A", map[string]string{"OTHER": ""}, nil)))
	if err := e.Apply(t.Context(), rotation.Change); !errors.Is(err, ErrStale) || !strings.Contains(err.Error(), "service worker started using a value") {
		t.Errorf("applying a rotation a service joined since: %v, want it out of date", err)
	}
}

// opening is a Sealer that notes which resources' specs it opens.
type opening struct {
	Sealer
	mu     sync.Mutex
	opened map[string]int
}

func (o *opening) Open(ctx context.Context, tx *store.Tx, at store.SpecAt, stored []byte) ([]byte, error) {
	o.mu.Lock()
	o.opened[at.ResourceID]++
	o.mu.Unlock()
	return o.Sealer.Open(ctx, tx, at, stored)
}

func (o *opening) count(resourceID string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opened[resourceID]
}

// A change to a value finds the services that use it from what each
// revision records it uses, kept in the clear: proposing and applying a
// rotation opens the specs of the services that use the secret, and never
// those of the services that don't, under the commit lock or outside it.
func TestARotationOpensOnlyTheServicesThatUseIt(t *testing.T) {
	storetest.Each(t, testARotationOpensOnlyTheServicesThatUseIt)
}

func testARotationOpensOnlyTheServicesThatUseIt(t *testing.T, backend store.Backend) {
	_, f, db := setup(t, backend, "srv_A")
	kinds := testKinds()
	sealer := &opening{Sealer: secretstest.Keeper(db, kinds), opened: map[string]int{}}
	e := New(db, kinds, f, sealer, slog.New(slog.DiscardHandler))
	env := production(t, db)
	apply(t, e, propose(t, e, "Set up the shop",
		secretEdit(env, "DB_PASSWORD", "correct-horse-1"),
		secretEdit(env, "MAIL_TOKEN", "mail-1"),
		user(env, "api", "srv_A", map[string]string{"DATABASE_PASSWORD": "DB_PASSWORD"}, nil),
		user(env, "mailer", "srv_A", map[string]string{"MAIL_TOKEN": ""}, nil),
		user(env, "static", "srv_A", nil, nil),
	))
	ids := map[string]string{}
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		for _, name := range []string{"api", "mailer", "static"} {
			r, err := tx.GetResource(t.Context(), string(service.Kind), env, name)
			if err != nil {
				t.Fatal(err)
			}
			ids[name] = r.ID
		}
		return nil
	})
	sealer.mu.Lock()
	clear(sealer.opened)
	sealer.mu.Unlock()

	p := propose(t, e, "Rotate the database password", secretEdit(env, "DB_PASSWORD", "correct-horse-2"))
	apply(t, e, p)
	if planned(p, "api") == nil || planned(p, "mailer") != nil || planned(p, "static") != nil {
		t.Errorf("the rotation's plan has %+v, want api alone of the services", p.Resources)
	}
	if sealer.count(ids["api"]) == 0 {
		t.Error("the rotation never opened api's spec; the test doesn't watch what it means to")
	}
	for _, name := range []string{"mailer", "static"} {
		if n := sealer.count(ids[name]); n != 0 {
			t.Errorf("the rotation opened %s's spec %d times, which doesn't use the secret", name, n)
		}
	}
}

// Making a value a secret, or a secret a variable, is one change: it
// deletes the one and makes the other under the same name, and takes in
// every service that uses it, moved to the new kind, under the same names,
// so the switch applies as one plan, restarting each with the same value.
// A service the change edits itself keeps what it says, and so is refused
// if it still uses what the change deletes. Undoing it puts both back.
func TestSwitchingAValuesKindTakesInItsServices(t *testing.T) {
	storetest.Each(t, testSwitchingAValuesKindTakesInItsServices)
}

func testSwitchingAValuesKindTakesInItsServices(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	env := production(t, db)
	apply(t, e, propose(t, e, "Set up",
		variableEdit(env, "API_TOKEN", "tok-1"),
		user(env, "api", "srv_A", nil, map[string]string{"TOKEN": "API_TOKEN", "LOG_LEVEL": "API_TOKEN"}),
		user(env, "worker", "srv_A", nil, map[string]string{"API_TOKEN": ""}),
	))
	deleteVar := Edit{Ref: resource.Ref{Kind: variable.VariableKind, Scope: env, Name: "API_TOKEN"}}

	_, err := e.Propose(t.Context(), Proposal{Summary: "Make API_TOKEN a secret, but keep api on the variable", Actor: "usr_test", Edits: []Edit{
		deleteVar, secretEdit(env, "API_TOKEN", "tok-1"), user(env, "api", "srv_A", nil, map[string]string{"TOKEN": "API_TOKEN"}),
	}})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "uses variable API_TOKEN as TOKEN, which this change deletes") {
		t.Errorf("switching while editing a service that still uses the variable = %v, want it refused", err)
	}

	p := propose(t, e, "Make API_TOKEN a secret", deleteVar, secretEdit(env, "API_TOKEN", "tok-1"))
	var names []string
	for i := range p.Resources {
		names = append(names, p.Resources[i].Ref.Name+" "+string(p.Resources[i].Ref.Kind)+" "+p.Resources[i].Action.String())
	}
	if got := strings.Join(names, ", "); got != "API_TOKEN variable delete, API_TOKEN secret create, api service replace, worker service replace" {
		t.Errorf("the switch's plan touches %s, want both kinds and the two services", got)
	}
	if c := change(t, p.Steps(), "secrets[TOKEN]"); c.Op != resource.OpAdded || c.To != "API_TOKEN" {
		t.Errorf("api's step = %+v, want it to use the secret as TOKEN", c)
	}
	apply(t, e, p)
	api := containerOf(t, f["srv_A"], env, "api")
	if api.GetEnv()["TOKEN"] != "tok-1" || api.GetEnv()["LOG_LEVEL"] != "tok-1" ||
		api.GetLabels()[binding.LabelKey] != "LOG_LEVEL=secret:API_TOKEN@1,TOKEN=secret:API_TOKEN@1" {
		t.Errorf("after the switch api runs with %v, labelled %q; want the secret under both names", api.GetEnv(), api.GetLabels()[binding.LabelKey])
	}
	if worker := containerOf(t, f["srv_A"], env, "worker"); worker.GetLabels()[binding.LabelKey] != "API_TOKEN=secret:API_TOKEN@1" {
		t.Errorf("after the switch worker is labelled %q", worker.GetLabels()[binding.LabelKey])
	}

	back := propose(t, e, "Make API_TOKEN a variable",
		Edit{Ref: resource.Ref{Kind: variable.SecretKind, Scope: env, Name: "API_TOKEN"}}, variableEdit(env, "API_TOKEN", "tok-2"))
	apply(t, e, back)
	// The variable was made, deleted, and made again: its third revision.
	if got := containerOf(t, f["srv_A"], env, "worker").GetLabels()[binding.LabelKey]; got != "API_TOKEN=variable:API_TOKEN@3" {
		t.Errorf("after switching back worker is labelled %q, want the variable's third revision", got)
	}

	undo, err := e.Undo(t.Context(), back.Change, By{Actor: "usr_test", Via: ViaPanel})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, e, undo)
	if got := containerOf(t, f["srv_A"], env, "api").GetLabels()[binding.LabelKey]; got != "LOG_LEVEL=secret:API_TOKEN@3,TOKEN=secret:API_TOKEN@3" {
		t.Errorf("after undoing the switch back api is labelled %q, want the secret again, at its third revision", got)
	}
}

// A sealed spec opens only where it was sealed for. Someone who can write
// the database, without the key file, can't pass one revision of a secret
// off as another, such as a leaked value as its rotation, nor put a spec
// of their own in the clear where a sealed one belongs.
func TestASealedSpecOpensOnlyInItsOwnRevision(t *testing.T) {
	storetest.Each(t, testASealedSpecOpensOnlyInItsOwnRevision)
}

func testASealedSpecOpensOnlyInItsOwnRevision(t *testing.T, backend store.Backend) {
	e, _, db, location := setupAt(t, backend, "srv_A")
	env := production(t, db)
	apply(t, e, propose(t, e, "Set up", secretEdit(env, "DB_PASSWORD", "old-leaked")))
	apply(t, e, propose(t, e, "Rotate", secretEdit(env, "DB_PASSWORD", "new-rotated")))
	var id string
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(variable.SecretKind), env, "DB_PASSWORD")
		if err != nil {
			t.Fatal(err)
		}
		id = r.ID
		return nil
	})
	if backend == store.Postgres {
		storetest.Exec(t, location, `DROP TRIGGER revisions_never_change ON revisions`)
	} else {
		storetest.Exec(t, location, `DROP TRIGGER revisions_never_change`)
	}
	k := secretstest.Keeper(db, e.Kinds())
	for what, q := range map[string]string{
		"revision 1's sealed spec": `UPDATE revisions SET spec = (SELECT r1.spec FROM revisions r1 WHERE r1.resource_id = '` + id + `' AND r1.number = 1)
			WHERE resource_id = '` + id + `' AND number = 2`,
		"a spec in the clear": `UPDATE revisions SET spec = X'0a0a6d792d6f776e2d6b6579' WHERE resource_id = '` + id + `' AND number = 2`,
	} {
		if backend == store.Postgres {
			q = strings.ReplaceAll(q, `X'0a0a6d792d6f776e2d6b6579'`, `'\x0a0a6d792d6f776e2d6b6579'::bytea`)
		}
		storetest.Exec(t, location, q)
		if got, err := k.Reveal(t.Context(), id, "user:usr_test", store.SurfacePanel); err == nil {
			t.Errorf("with %s in revision 2's row, revealing gave %+v, want it refused", what, got)
		}
		if _, err := e.Propose(t.Context(), Proposal{Summary: "Rotate", Actor: "usr_test", Edits: []Edit{secretEdit(env, "DB_PASSWORD", "x")}}); err == nil {
			t.Errorf("with %s in revision 2's row, planning from it worked, want it refused", what)
		}
	}
}
