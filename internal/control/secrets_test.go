// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/base64"
	"encoding/hex"
	"maps"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func secretSpec(name, value string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: value}}},
	}
}

func variableSpec(name, value string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: value}}},
	}
}

// serviceSpec runs a service on servers that uses secrets, by the names it
// sees them as.
func serviceSpec(name string, secrets map[string]string, servers ...string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: name},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs:    &resourcev1.Service_App{App: &resourcev1.Container{Image: "acme/" + name + ":1"}},
			Secrets: secrets,
		}}},
		Servers: servers,
	}
}

func variablesClient(c connect.HTTPClient, origin string) dockwellv1connect.VariableServiceClient {
	return dockwellv1connect.NewVariableServiceClient(c, origin, connect.WithProtoJSON())
}

// listed is the environment's variable or secret named name.
func listed(t *testing.T, v dockwellv1connect.VariableServiceClient, name string) *dockwellv1.EnvironmentVariable {
	t.Helper()
	res, err := v.ListVariables(t.Context(), connect.NewRequest(&dockwellv1.ListVariablesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range res.Msg.GetVariables() {
		if x.GetName() == name {
			return x
		}
	}
	t.Fatalf("no %s in %v", name, res.Msg.GetVariables())
	return nil
}

// stepUp confirms revealing secretID with the browser's passkey.
func stepUp(t *testing.T, b *browser, secretID string) string {
	t.Helper()
	c := b.authClient()
	begin, err := c.BeginStepUp(t.Context(), connect.NewRequest(&dockwellv1.BeginStepUpRequest{
		Action: dockwellv1.StepUpAction_STEP_UP_ACTION_REVEAL_SECRET, Target: secretID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := b.auth.Get(begin.Msg.GetOptionsJson(), b.origin, nil)
	if err != nil {
		t.Fatal(err)
	}
	done, err := c.FinishStepUp(t.Context(), connect.NewRequest(&dockwellv1.FinishStepUpRequest{CeremonyId: begin.Msg.GetCeremonyId(), CredentialJson: resp}))
	if err != nil {
		t.Fatal(err)
	}
	return done.Msg.GetProof()
}

// Revealing a secret takes a signed-in session and a fresh passkey
// assertion for that secret, which works once; it is audited, and no API
// token can do it, or begin the assertion. Listing never shows the value.
func TestRevealingASecretTakesAPasskey(t *testing.T) {
	storetest.Each(t, testRevealingASecretTakesAPasskey)
}

func testRevealingASecretTakesAPasskey(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Add the database password", secretSpec("DB_PASSWORD", "s3cret-reveal-me"), variableSpec("LOG_LEVEL", "info")))
	v := variablesClient(b, b.origin)
	secret := listed(t, v, "DB_PASSWORD")
	if !secret.GetSecret() || secret.GetValue() != "" || secret.GetRevision() != 1 || secret.GetUpdatedBy().GetName() != "Mira Chen" {
		t.Errorf("the listed secret = %v, want it without its value", secret)
	}
	if variable := listed(t, v, "LOG_LEVEL"); variable.GetSecret() || variable.GetValue() != "info" {
		t.Errorf("the listed variable = %v, want it with its value", variable)
	}

	reveal := func(proof string) (*dockwellv1.RevealSecretResponse, error) {
		res, err := v.RevealSecret(t.Context(), connect.NewRequest(&dockwellv1.RevealSecretRequest{Id: secret.GetId(), Proof: proof}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	if _, err := reveal(""); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("revealing without a passkey: %v, want %s", err, authapi.ReasonNeedsStepUp)
	}
	proof := stepUp(t, b, secret.GetId())
	got, err := reveal(proof)
	if err != nil || got.GetValue() != "s3cret-reveal-me" || got.GetRevision() != 1 {
		t.Fatalf("revealing with a passkey = %v, %v", got, err)
	}
	if _, err := reveal(proof); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("revealing twice with one passkey assertion: %v, want %s", err, authapi.ReasonNeedsStepUp)
	}
	other := stepUp(t, b, "sec_somethingelse")
	if _, err := reveal(other); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("revealing with an assertion for another secret: %v, want %s", err, authapi.ReasonNeedsStepUp)
	}

	var reveals []store.Event
	_ = f.store.View(t.Context(), func(tx *store.Tx) error {
		events, err := tx.ListEvents(t.Context(), 1000)
		for i := range events {
			if events[i].Action == "secret.reveal" {
				reveals = append(reveals, events[i])
			}
		}
		return err
	})
	if len(reveals) != 1 || reveals[0].Target != secret.GetId() || !strings.HasPrefix(reveals[0].Actor, "user:") || reveals[0].Surface != store.SurfacePanel ||
		strings.Contains(reveals[0].Detail, "s3cret") {
		t.Errorf("reveals in the audit log = %+v", reveals)
	}

	for _, scope := range []dockwellv1.TokenScope{dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE} {
		token := createToken(t, b, scope)
		script := bearer{token: token, rt: b.client.Transport}
		if _, err := variablesClient(script, b.origin).RevealSecret(t.Context(), connect.NewRequest(&dockwellv1.RevealSecretRequest{
			Id: secret.GetId(), Proof: stepUp(t, b, secret.GetId()),
		})); reasonOf(err) != authapi.ReasonNeedsSession {
			t.Errorf("revealing with a %v token and a person's proof: %v, want %s", scope, err, authapi.ReasonNeedsSession)
		}
		if _, err := dockwellv1connect.NewAuthServiceClient(script, b.origin).BeginStepUp(t.Context(), connect.NewRequest(&dockwellv1.BeginStepUpRequest{
			Action: dockwellv1.StepUpAction_STEP_UP_ACTION_REVEAL_SECRET, Target: secret.GetId(),
		})); reasonOf(err) != authapi.ReasonNeedsSession {
			t.Errorf("a %v token beginning a step-up: %v, want %s", scope, err, authapi.ReasonNeedsSession)
		}
		// A token may list what there is, without the secret's value.
		if s := listed(t, variablesClient(script, b.origin), "DB_PASSWORD"); s.GetValue() != "" {
			t.Errorf("a token listed the secret's value")
		}
	}
}

// A server receives the values of the services placed on it, over its
// agent's stream, and no others: a service's container on one server
// holds its secret, and the other server's never see it.
func TestValuesReachOnlyTheServersThatRunTheService(t *testing.T) {
	storetest.Each(t, testValuesReachOnlyTheServersThatRunTheService)
}

func testValuesReachOnlyTheServersThatRunTheService(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addNamedServer(t, "hz-1", true)
	hz2 := f.addNamedServer(t, "hz-2", true)
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Run the shop",
		secretSpec("API_TOKEN", "only-for-hz1-api"), secretSpec("WORKER_TOKEN", "only-for-hz2-worker"),
		serviceSpec("api", map[string]string{"TOKEN": "API_TOKEN"}, hz1.id.ServerID),
		serviceSpec("worker", map[string]string{"TOKEN": "WORKER_TOKEN"}, hz2.id.ServerID),
	))
	env := listedEnv(t, b)
	held := func(s *testServer) string {
		var all strings.Builder
		for _, name := range []string{"api", "worker"} {
			if got, _ := s.docker.Inspect(t.Context(), service.Target(resource.Ref{Scope: env, Name: name})); got != nil {
				for _, k := range slices.Sorted(maps.Keys(got.GetEnv())) {
					all.WriteString(name + ":" + k + "=" + got.GetEnv()[k] + " ")
				}
			}
		}
		return all.String()
	}
	if got := held(hz1); got != "api:TOKEN=only-for-hz1-api " {
		t.Errorf("hz-1 holds %q, want only api's secret", got)
	}
	if got := held(hz2); got != "worker:TOKEN=only-for-hz2-worker " {
		t.Errorf("hz-2 holds %q, want only worker's secret", got)
	}
}

// An environment's variables and secrets list by name, a page at a time,
// each with the services that use it, under every name each uses it as.
// Listing opens no sealed spec: a variable's isn't sealed, and what each
// service uses is kept in the clear beside it, by reference. So with every
// data key gone, as from a damaged database, the list still answers in
// full, and a secret's value still can't be read.
func TestListingValuesOpensNoSpec(t *testing.T) { storetest.Each(t, testListingValuesOpensNoSpec) }

func testListingValuesOpensNoSpec(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addNamedServer(t, "hz-1", true)
	c := changeClient(b, b.origin)
	api := serviceSpec("api", map[string]string{"DATABASE_PASSWORD": "DB_PASSWORD", "REPLICA_PASSWORD": "DB_PASSWORD"}, hz1.id.ServerID)
	api.GetSpec().GetService().Variables = map[string]string{"LOG_LEVEL": ""}
	apply(t, c, propose(t, c, "Run the shop",
		secretSpec("DB_PASSWORD", "s3cret-listed-never"), variableSpec("LOG_LEVEL", "info"), variableSpec("API_URL", "https://api.example"),
		secretSpec("MAIL_TOKEN", "s3cret-mail"), api,
		serviceSpec("worker", map[string]string{"DB_PASSWORD": ""}, hz1.id.ServerID),
	))
	f.execSQL(t, "DELETE FROM data_keys")

	v := variablesClient(b, b.origin)
	var got []string
	token, pages := "", 0
	for {
		res, err := v.ListVariables(t.Context(), connect.NewRequest(&dockwellv1.ListVariablesRequest{PageSize: 3, PageToken: token}))
		if err != nil {
			t.Fatalf("ListVariables, page %d = %v", pages+1, err)
		}
		pages++
		for _, x := range res.Msg.GetVariables() {
			line := x.GetName() + "=" + x.GetValue() + " by " + x.GetUpdatedBy().GetName()
			for _, u := range x.GetUsedBy() {
				line += " " + u.GetService() + ":" + u.GetName()
			}
			got = append(got, line)
		}
		if token = res.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	want := []string{
		"API_URL=https://api.example by Mira Chen",
		"DB_PASSWORD= by Mira Chen api:DATABASE_PASSWORD api:REPLICA_PASSWORD worker:DB_PASSWORD",
		"LOG_LEVEL=info by Mira Chen api:LOG_LEVEL",
		"MAIL_TOKEN= by Mira Chen",
	}
	if pages != 2 || !slices.Equal(got, want) {
		t.Errorf("ListVariables, 3 at a time, in %d pages =\n%s\nwant, in 2 pages,\n%s", pages, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := v.ListVariables(t.Context(), connect.NewRequest(&dockwellv1.ListVariablesRequest{PageToken: "bm9uc2Vuc2U"})); reasonOf(err) != "invalid" {
		t.Errorf("a page token ListVariables didn't give = %v, want it invalid", err)
	}
	id := listed(t, v, "DB_PASSWORD").GetId()
	if got, err := v.RevealSecret(t.Context(), connect.NewRequest(&dockwellv1.RevealSecretRequest{Id: id, Proof: stepUp(t, b, id)})); err == nil {
		t.Errorf("revealing a secret whose data key is gone = %v, want it refused", got.Msg)
	}
}

func listedEnv(t *testing.T, b *browser) string {
	t.Helper()
	res, err := variablesClient(b, b.origin).ListVariables(t.Context(), connect.NewRequest(&dockwellv1.ListVariablesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.GetEnvironmentId()
}

// encodings are the forms a value could take in a database: as it is, in
// hex, and in base64 at each alignment, less the characters an alignment
// makes uncertain.
func encodings(value string) []string {
	out := []string{value, hex.EncodeToString([]byte(value))}
	for pad := range 3 {
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawURLEncoding} {
			e := enc.EncodeToString(append(make([]byte, pad), value...))
			e = strings.TrimRight(e, "=")
			skip := (pad*8 + 5) / 6
			out = append(out, e[skip:len(e)-2])
		}
	}
	return out
}

// A database backup alone holds no secret: after secrets are made, used by
// services, rotated, undone, revealed and left planned, and a container is
// run with a secret in its environment, no table holds any of them, in
// any encoding, while the servers that run them do.
func TestADatabaseDumpHoldsNoSecret(t *testing.T) { storetest.Each(t, testADatabaseDumpHoldsNoSecret) }

func testADatabaseDumpHoldsNoSecret(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addNamedServer(t, "hz-1", true)
	hz2 := f.addNamedServer(t, "hz-2", true)
	c := changeClient(b, b.origin)
	values := []string{
		"pw-alpha-7Hq2mXv9Lr4Kd8Zs", "pw-beta-3Fn6pWc1Tj5Yb2Ug", "pw-gamma-9Rk4eSa8Mh2Vx6Qd",
		"pw-delta-5Lt1oGy7Ce3Bn9Wf", "env-epsilon-2Dp8iJr6Ns4Hu1Ka",
	}
	apply(t, c, propose(t, c, "Run the shop",
		secretSpec("DB_PASSWORD", values[0]),
		serviceSpec("api", map[string]string{"DATABASE_PASSWORD": "DB_PASSWORD"}, hz1.id.ServerID),
		serviceSpec("worker", map[string]string{"DB_PASSWORD": ""}, hz2.id.ServerID),
	))
	// A container run by hand, with a secret in its environment, the gap ADR
	// 0017 left: its spec is sealed too.
	byHand := webEdit("nginx:1.27", map[string]string{"API_KEY": values[4]})
	byHand.Resource.Scope = hz1.id.ServerID
	apply(t, c, propose(t, c, "Run web", byHand))
	rotation := apply(t, c, propose(t, c, "Rotate the password", secretSpec("DB_PASSWORD", values[1])))
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: rotation.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, undo.Msg.GetChange())
	discarded := propose(t, c, "Try another password", secretSpec("DB_PASSWORD", values[2]))
	if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: discarded.GetId()})); err != nil {
		t.Fatal(err)
	}
	propose(t, c, "A password for later", secretSpec("LATER", values[3]))
	v := variablesClient(b, b.origin)
	id := listed(t, v, "DB_PASSWORD").GetId()
	if got, err := v.RevealSecret(t.Context(), connect.NewRequest(&dockwellv1.RevealSecretRequest{Id: id, Proof: stepUp(t, b, id)})); err != nil ||
		got.Msg.GetValue() != values[0] {
		t.Fatalf("RevealSecret = %v, %v", got, err)
	}

	// The servers have what runs there, so the values were real.
	api, _ := hz1.docker.Inspect(t.Context(), service.Target(resource.Ref{Scope: listedEnv(t, b), Name: "api"}))
	if api.GetEnv()["DATABASE_PASSWORD"] != values[0] {
		t.Fatalf("api runs with %v; the test didn't run what it meant to", api.GetEnv())
	}
	dump := storetest.Dump(t, f.db)
	for _, table := range []string{"revisions", "change_items", "changes", "audit_events", "data_keys", "key_encryption_keys"} {
		if len(dump[table]) == 0 {
			t.Errorf("the dump has no %s; the test reads the wrong database", table)
		}
	}
	for table, rows := range dump {
		for _, row := range rows {
			for _, value := range values {
				for _, enc := range encodings(value) {
					if strings.Contains(row, enc) {
						t.Errorf("%s holds a secret, as %q: %q", table, enc, row)
					}
				}
			}
		}
	}
}
