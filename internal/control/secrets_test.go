// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
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
	return stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_REVEAL_SECRET, secretID)
}

// stepUpFor confirms action on target with the browser's passkey, and
// returns the proof.
func stepUpFor(t *testing.T, b *browser, action dockwellv1.StepUpAction, target string) string {
	t.Helper()
	c := b.authClient()
	begin, err := c.BeginStepUp(t.Context(), connect.NewRequest(&dockwellv1.BeginStepUpRequest{Action: action, Target: target}))
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
	// What isn't a secret is refused as that, before any passkey is asked
	// for, and a proof given with it stays unspent: here, a secret while
	// it is deleted, whose proof works once it is made again, with its ID.
	apply(t, c, propose(t, c, "Remove it", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "secret", Name: "DB_PASSWORD"}, Delete: true}))
	waiting := stepUp(t, b, secret.GetId())
	for _, proof := range []string{"", waiting} {
		if _, err := reveal(proof); reasonOf(err) != "not_found" {
			t.Errorf("revealing a deleted secret, proof %t: %v, want not_found", proof != "", err)
		}
	}
	apply(t, c, propose(t, c, "Add it back", secretSpec("DB_PASSWORD", "s3cret-reveal-me")))
	if got, err := reveal(waiting); err != nil || got.GetValue() != "s3cret-reveal-me" {
		t.Errorf("revealing with the proof a refusal left = %v, %v", got, err)
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
	if len(reveals) != 2 || reveals[0].Target != secret.GetId() || !strings.HasPrefix(reveals[0].Actor, "user:") || reveals[0].Surface != store.SurfacePanel ||
		strings.Contains(reveals[0].Detail, "s3cret") {
		t.Errorf("reveals in the audit log = %+v", reveals)
	}
	// The timeline names the secret, and the environment it lives in, so
	// the panel can open that environment's list.
	timeline, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ACCESS},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var shown *dockwellv1.Event
	for _, e := range timeline.Msg.GetEvents() {
		if e.GetAction() == "secret.reveal" {
			shown = e
		}
	}
	if s := shown.GetSubject(); s.GetId() != secret.GetId() || s.GetName() != "DB_PASSWORD" || s.GetScope() != listedEnv(t, b) {
		t.Errorf("the reveal on the timeline is about %v, want DB_PASSWORD in %s", s, listedEnv(t, b))
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
			line := []string{x.GetName() + "=" + x.GetValue() + " by " + x.GetUpdatedBy().GetName()}
			for _, u := range x.GetUsedBy() {
				line = append(line, u.GetService()+":"+u.GetName())
			}
			got = append(got, strings.Join(line, " "))
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

// Through the API, as the panel calls it: adding a passkey, removing one,
// setting up codes and making a token each take a proof from a step-up for
// that action, which no token can begin. A link from dockwell recover
// still lets someone with no passkey here add one, just after signing in
// with it, and removing a passkey over REST carries its proof in the body.
func TestChangingHowYouSignInTakesAPasskeyThroughTheAPI(t *testing.T) {
	storetest.Each(t, testChangingHowYouSignInTakesAPasskeyThroughTheAPI)
}

func testChangingHowYouSignInTakesAPasskeyThroughTheAPI(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	c := b.authClient()
	if _, err := c.BeginAddPasskey(t.Context(), connect.NewRequest(&dockwellv1.BeginAddPasskeyRequest{})); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("adding a passkey without a step-up: %v, want %s", err, authapi.ReasonNeedsStepUp)
	}
	if _, err := c.BeginTotpSetup(t.Context(), connect.NewRequest(&dockwellv1.BeginTotpSetupRequest{})); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("setting up codes without a step-up: %v, want %s", err, authapi.ReasonNeedsStepUp)
	}
	if _, err := c.BeginTotpSetup(t.Context(), connect.NewRequest(&dockwellv1.BeginTotpSetupRequest{
		Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_UP_CODES, ""),
	})); err != nil {
		t.Errorf("setting up codes with a step-up: %v", err)
	}
	// That setup stays this session's alone, and 5 wrong codes end it, on
	// the timeline. Another session is told nothing is waiting for it.
	confirm := func(c dockwellv1connect.AuthServiceClient) error {
		_, err := c.ConfirmTotpSetup(t.Context(), connect.NewRequest(&dockwellv1.ConfirmTotpSetupRequest{Code: "000000"}))
		return err
	}
	other := newBrowser(t, b.origin, f.api, nil)
	other.auth = b.auth
	other.signIn(t)
	if err := confirm(other.authClient()); reasonOf(err) != "no_pending_totp" {
		t.Errorf("confirming another session's setup: %v, want no_pending_totp", err)
	}
	for i := range 4 {
		if err := confirm(c); reasonOf(err) != "totp_mismatch" {
			t.Errorf("wrong code %d: %v, want totp_mismatch", i+1, err)
		}
	}
	if err := confirm(c); reasonOf(err) != "totp_setup_ended" {
		t.Errorf("the fifth wrong code: %v, want totp_setup_ended", err)
	}
	if err := confirm(c); reasonOf(err) != "no_pending_totp" {
		t.Errorf("a code once the setup ended: %v, want no_pending_totp", err)
	}
	timeline, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ACCESS},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(timeline.Msg.GetEvents(), func(e *dockwellv1.Event) bool { return e.GetAction() == "totp.setup_discarded" }) {
		t.Errorf("the timeline doesn't show the setup that 5 wrong codes ended: %v", timeline.Msg.GetEvents())
	}
	if _, err := c.CancelTotpSetup(t.Context(), connect.NewRequest(&dockwellv1.CancelTotpSetupRequest{})); err != nil {
		t.Errorf("cancelling with nothing waiting: %v", err)
	}
	if _, err := c.BeginTotpSetup(t.Context(), connect.NewRequest(&dockwellv1.BeginTotpSetupRequest{
		Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_UP_CODES, ""),
	})); err != nil {
		t.Errorf("setting up codes again with a step-up: %v", err)
	}
	if _, err := c.CancelTotpSetup(t.Context(), connect.NewRequest(&dockwellv1.CancelTotpSetupRequest{})); err != nil {
		t.Errorf("cancelling a setup: %v", err)
	}
	if err := confirm(c); reasonOf(err) != "no_pending_totp" {
		t.Errorf("a code for a cancelled setup: %v, want no_pending_totp", err)
	}

	tokens := dockwellv1connect.NewTokenServiceClient(b, b.origin, connect.WithProtoJSON())
	if _, err := tokens.CreateToken(t.Context(), connect.NewRequest(&dockwellv1.CreateTokenRequest{
		Name: "CI", Scope: dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY,
		Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_ADD_PASSKEY, ""),
	})); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("making a token with a proof for adding a passkey: %v, want %s", err, authapi.ReasonNeedsStepUp)
	}

	// A second passkey, from another authenticator, with a step-up.
	second := &authtest.Authenticator{}
	addWith := func(b *browser, proof string, a *authtest.Authenticator) (*dockwellv1.Passkey, error) {
		c := b.authClient()
		begin, err := c.BeginAddPasskey(t.Context(), connect.NewRequest(&dockwellv1.BeginAddPasskeyRequest{Proof: proof}))
		if err != nil {
			return nil, err
		}
		cred, _, err := a.Create(begin.Msg.GetOptionsJson(), b.origin)
		if err != nil {
			t.Fatal(err)
		}
		res, err := c.FinishAddPasskey(t.Context(), connect.NewRequest(&dockwellv1.FinishAddPasskeyRequest{
			CeremonyId: begin.Msg.GetCeremonyId(), CredentialJson: cred, Name: "YubiKey",
		}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetPasskey(), nil
	}
	yubikey, err := addWith(b, stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_ADD_PASSKEY, ""), second)
	if err != nil {
		t.Fatalf("adding a passkey with a step-up: %v", err)
	}

	// Removing it, over REST, takes a proof for that passkey.
	path := "/v1/passkeys/" + yubikey.GetId() + ":remove"
	if status, why, body := callREST(t, b, http.MethodPost, path, nil, "{}"); status != http.StatusForbidden || why != authapi.ReasonNeedsStepUp {
		t.Errorf("POST %s without a proof = %d (reason %q) %s, want 403 %s", path, status, why, body, authapi.ReasonNeedsStepUp)
	}
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_REMOVE_PASSKEY, yubikey.GetId())
	if status, why, body := callREST(t, b, http.MethodPost, path, nil, `{"proof": "`+proof+`"}`); status != http.StatusOK {
		t.Errorf("POST %s with a proof = %d (reason %q) %s, want 200", path, status, why, body)
	}
	listed, err := c.ListPasskeys(t.Context(), connect.NewRequest(&dockwellv1.ListPasskeysRequest{}))
	if err != nil || len(listed.Msg.GetPasskeys()) != 1 {
		t.Errorf("passkeys after removing the second = %v, %v, want the first alone", listed, err)
	}

	// No token begins a step-up for any of these.
	token := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}
	for _, action := range []dockwellv1.StepUpAction{
		dockwellv1.StepUpAction_STEP_UP_ACTION_ADD_PASSKEY, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN,
	} {
		if _, err := dockwellv1connect.NewAuthServiceClient(token, b.origin).BeginStepUp(t.Context(), connect.NewRequest(&dockwellv1.BeginStepUpRequest{
			Action: action,
		})); reasonOf(err) != authapi.ReasonNeedsSession {
			t.Errorf("a token beginning a step-up for %v: %v, want %s", action, err, authapi.ReasonNeedsSession)
		}
	}

	// A browser with no passkey signs in with a link from dockwell
	// recover, and adds one at once.
	link, _, err := f.auth.CreateSignInLink(t.Context(), "", "cli:root@hz-1")
	if err != nil {
		t.Fatal(err)
	}
	phone := newBrowser(t, b.origin, f.api, nil)
	if _, err := phone.authClient().SignInWithLink(t.Context(), connect.NewRequest(&dockwellv1.SignInWithLinkRequest{Token: link.Token})); err != nil {
		t.Fatal(err)
	}
	if _, err := addWith(phone, "", phone.auth); err != nil {
		t.Errorf("adding a passkey just after signing in with a link from dockwell recover: %v", err)
	}
	if _, err := dockwellv1connect.NewTokenServiceClient(phone, phone.origin, connect.WithProtoJSON()).CreateToken(t.Context(),
		connect.NewRequest(&dockwellv1.CreateTokenRequest{Name: "CI", Scope: dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY})); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("making a token just after signing in with a link, without a step-up: %v, want %s", err, authapi.ReasonNeedsStepUp)
	}
}

// A read-write API token may propose a secret's value, and its plan can't
// tell it whether the value is the one the secret has: the right value
// and a wrong one plan alike, each a rotation, with fingerprints of their
// own.
func TestATokenCantConfirmAGuessByProposingIt(t *testing.T) {
	storetest.Each(t, testATokenCantConfirmAGuessByProposingIt)
}

func testATokenCantConfirmAGuessByProposingIt(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Add the database password", secretSpec("DB_PASSWORD", "hunter2")))
	script := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}
	tc := changeClient(script, b.origin)
	wrong := propose(t, tc, "Guess", secretSpec("DB_PASSWORD", "letmein")).GetPlan()
	right := propose(t, tc, "Guess", secretSpec("DB_PASSWORD", "hunter2")).GetPlan()
	if right.GetFingerprint() == wrong.GetFingerprint() {
		t.Error("two guesses share a fingerprint")
	}
	right.Fingerprint, wrong.Fingerprint = "", ""
	for _, p := range []*dockwellv1.Plan{right, wrong} {
		for _, r := range p.GetResources() {
			r.Id = ""
		}
	}
	if !proto.Equal(right, wrong) {
		t.Errorf("the right value plans\n%v\nand a wrong one\n%v", right, wrong)
	}
	if right.GetSummary().GetChange() != 1 {
		t.Errorf("setting the value the secret has plans %v, want a change", right.GetSummary())
	}
}

// docker compose up from an export, with no value in its shell, sends a secret's name
// alone. Docker keeps that in the container's Config.Env and takes it out of what
// runs, so the container hasn't got it, and that's drift. Read as an empty value it
// wasn't, and #142's step on real Docker found it.
func TestANameWithNoValueIsntInAContainersEnvironment(t *testing.T) {
	storetest.Each(t, testANameWithNoValueIsntInAContainersEnvironment)
}

func testANameWithNoValueIsntInAContainersEnvironment(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.AddLocalImage("acme/api:1", dockertest.Image{})
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Run api", secretSpec("DB_PASSWORD", "s3cret-value-9Rk2"), serviceSpec("api", map[string]string{"DATABASE_PASSWORD": "DB_PASSWORD"})))
	name := docker.Project(listedEnv(t, b)) + "-api-1"
	drift := func() []*dockwellv1.Drift {
		t.Helper()
		res, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetDrift()
	}
	if d := drift(); len(d) != 0 {
		t.Fatalf("drift with api as asked = %v", d)
	}
	e.Edit(name, func(c *docker.Container) {
		for i, kv := range c.Config.Env {
			if strings.HasPrefix(kv, "DATABASE_PASSWORD=") {
				c.Config.Env[i] = "DATABASE_PASSWORD"
			}
		}
	})
	if got := e.Container(name); got == nil || !slices.Contains(got.Config.Env, "DATABASE_PASSWORD") {
		t.Fatalf("the test didn't take the value out: %+v", got)
	}
	found := drift()
	if len(found) != 1 || found[0].GetResource().GetName() != "api" {
		t.Errorf("drift with api without its secret = %v, want api", found)
	}
	if strings.Contains(fmt.Sprint(found), "s3cret-value-9Rk2") {
		t.Errorf("drift holds the value: %v", found)
	}
}

// A name with no value takes the image's value out of what runs, and a spec that
// doesn't name it says the image's stands. Read as nothing at all, that was no drift,
// though what runs had lost something. The check of #142 found this.
func TestANameWithNoValueOverAnImagesIsDrift(t *testing.T) {
	storetest.Each(t, testANameWithNoValueOverAnImagesIsDrift)
}

func testANameWithNoValueOverAnImagesIsDrift(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.AddLocalImage("acme/api:1", dockertest.Image{Config: docker.Config{Env: []string{"PATH=/bin", "LOG_FORMAT=json"}}})
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Run api", serviceSpec("api", nil)))
	name := docker.Project(listedEnv(t, b)) + "-api-1"
	drift := func() []*dockwellv1.Drift {
		t.Helper()
		res, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetDrift()
	}
	if d := drift(); len(d) != 0 {
		t.Fatalf("drift with api as asked = %v", d)
	}
	e.Edit(name, func(c *docker.Container) {
		i := slices.Index(c.Config.Env, "LOG_FORMAT=json")
		if i < 0 {
			t.Fatalf("the container doesn't have the image's value: %v", c.Config.Env)
		}
		c.Config.Env[i] = "LOG_FORMAT"
	})
	found := drift()
	if len(found) != 1 || found[0].GetResource().GetName() != "api" || found[0].GetChanges()[0].GetPath() != "app.env[LOG_FORMAT]" {
		t.Errorf("drift with the image's value taken out = %v, want api, at app.env[LOG_FORMAT]", found)
	}
	// A name of no image's takes nothing out, so it's nothing to say.
	e.Edit(name, func(c *docker.Container) {
		c.Config.Env[slices.Index(c.Config.Env, "LOG_FORMAT")] = "LOG_FORMAT=json"
		c.Config.Env = append(c.Config.Env, "UNSET_IN_ITS_SHELL")
	})
	if d := drift(); len(d) != 0 {
		t.Errorf("drift with a name that takes nothing out = %v", d)
	}
}

// A known limit, by design, which ADR 0022 and docs/security/secrets.md state, and
// this holds to what they say. Drift sees whether a service has a value it uses, and
// never what that value is: nothing that reads a container has it to compare, and
// comparing one is what ADR 0022 is careful not to offer. If this starts failing, what
// they say has changed too.
func TestDriftSeesThatAValueIsThereAndNeverWhatItIs(t *testing.T) {
	storetest.Each(t, testDriftSeesThatAValueIsThereAndNeverWhatItIs)
}

func testDriftSeesThatAValueIsThereAndNeverWhatItIs(t *testing.T, backend store.Backend) {
	const name, value = "DATABASE_PASSWORD", "s3cret-value-4Pz8"
	for what, tc := range map[string]struct {
		edit  func(env []string, i int) []string
		drift bool
	}{
		"set to nothing":     {edit: func(env []string, i int) []string { env[i] = name + "="; return env }},
		"set to a wrong one": {edit: func(env []string, i int) []string { env[i] = name + "=wrong"; return env }},
		"a bare name":        {edit: func(env []string, i int) []string { env[i] = name; return env }, drift: true},
		"gone":               {edit: func(env []string, i int) []string { return slices.Delete(env, i, i+1) }, drift: true},
	} {
		t.Run(what, func(t *testing.T) {
			f := startFull(t, backend, nil)
			b := signedIn(t, f)
			e := f.addDockerServer(t)
			e.AddLocalImage("acme/api:1", dockertest.Image{})
			c := changeClient(b, b.origin)
			apply(t, c, propose(t, c, "Run api", secretSpec("DB_PASSWORD", value), serviceSpec("api", map[string]string{name: "DB_PASSWORD"})))
			e.Edit(docker.Project(listedEnv(t, b))+"-api-1", func(c *docker.Container) {
				i := slices.IndexFunc(c.Config.Env, func(kv string) bool { return strings.HasPrefix(kv, name+"=") })
				if i < 0 {
					t.Fatalf("the container hasn't got the value: %v", c.Config.Env)
				}
				c.Config.Env = tc.edit(c.Config.Env, i)
			})
			res, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			if got := len(res.Msg.GetDrift()) == 1; got != tc.drift {
				t.Errorf("drift = %v, want drift to be %v", res.Msg.GetDrift(), tc.drift)
			}
		})
	}
}
