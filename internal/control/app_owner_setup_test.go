// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A held install's setup link is a person's, with a passkey, never a token's: it
// writes the hash of a key, good for an hour, on the install's held routes of the
// domain, and gives the key back in the address alone. Every other write keeps it as
// it was, and opening the install clears it with the hold (ADR 0105).
func TestASetupLinkIsAPersonsAndOnlyItsOwnWriteChangesIt(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o := startOwning(t, backend, ownedEntry(1, "1.0", firstRelease, "", "firstVisitorOwns: true\n"), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		o.addRoute("www", "thing.example.com", 80, 0)
		if !o.route("www").GetHeld() {
			t.Fatal("the test needs the route held")
		}
		token := createToken(t, o.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		tcat := dockwellv1connect.NewCatalogServiceClient(bearer{token: token, rt: o.b.client.Transport}, o.b.origin, connect.WithProtoJSON())
		if _, err := tcat.SetUpCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.SetUpCatalogInstallRequest{Id: "thing"})); reasonOf(err) != "needs_session" {
			t.Errorf("a token's setup link: %v", err)
		}
		if _, err := o.cat.SetUpCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.SetUpCatalogInstallRequest{Id: "thing"})); reasonOf(err) != "needs_step_up" {
			t.Errorf("without a passkey: %v", err)
		}
		project := o.install().GetProjectId()
		proof := stepUpFor(t, o.b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_UP_CATALOG_INSTALL, project)
		res, err := o.cat.SetUpCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.SetUpCatalogInstallRequest{Id: "thing", Proof: proof}))
		if err != nil {
			t.Fatal(err)
		}
		o.f.engine.Wait()
		link, err := url.Parse(res.Msg.GetUrl())
		if err != nil || link.Scheme != "https" || link.Host != "thing.example.com" || link.Path != "/.well-known/dockwell-setup" {
			t.Fatalf("the link is %q", res.Msg.GetUrl())
		}
		key := link.Query().Get("key")
		sum := sha256.Sum256([]byte(key))
		setup := o.route("www").GetSetup()
		if len(key) < 40 || setup.GetKeyHash() != hex.EncodeToString(sum[:]) {
			t.Errorf("the route's setup is %v for the key %q", setup, key)
		}
		if until := time.Unix(setup.GetUntil(), 0); until.Before(time.Now().Add(59*time.Minute)) || until.After(time.Now().Add(61*time.Minute)) {
			t.Errorf("the link works until %v", until)
		}
		if !o.route("www").GetHeld() {
			t.Error("a setup link opened the route")
		}
		// The key is on no change.
		changes, err := o.c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(prototext.Format(changes.Msg), key) {
			t.Error("a change holds the setup link's key")
		}
		// A whole-spec write that leaves the link out, and a token's that writes its
		// own, keep it as it was.
		mine := proto.CloneOf(o.route("www"))
		mine.Setup = nil
		o.applied(propose(t, o.c, "A route as it reads", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: o.env, Name: "www"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: mine}},
		}))
		tc := dockwellv1connect.NewChangeServiceClient(bearer{token: token, rt: o.b.client.Transport}, o.b.origin, connect.WithProtoJSON())
		forged := proto.CloneOf(o.route("www"))
		forged.Setup = &resourcev1.RouteSetup{KeyHash: strings.Repeat("0", 64), Until: time.Now().Add(time.Hour).Unix()}
		apply(t, tc, propose(t, tc, "A link of my own", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: o.env, Name: "www"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: forged}},
		}))
		if got := o.route("www").GetSetup(); !proto.Equal(got, setup) {
			t.Errorf("other writes left the setup as %v, want %v", got, setup)
		}
		o.open()
		if r := o.route("www"); r.GetHeld() || r.GetSetup() != nil {
			t.Errorf("once opened, the route is held %v with setup %v", r.GetHeld(), r.GetSetup())
		}
		if _, err := o.cat.SetUpCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.SetUpCatalogInstallRequest{Id: "thing"})); reasonOf(err) != "not_held" {
			t.Errorf("an open install's setup link: %v", err)
		}
	})
}

// A domain served over plain HTTP gets no setup link, as its Secure cookie wouldn't
// be kept there (ADR 0105).
func TestAPlainDomainGetsNoSetupLink(t *testing.T) {
	t.Parallel()
	o := startOwning(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, "", "firstVisitorOwns: true\n"), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
	r := &resourcev1.Route{Domain: "thing.example.com", Service: "web", Port: 80, Tls: "off", PlainForAnyone: true}
	applyAsRoot(t, o.b, propose(t, o.c, "A plain domain", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: o.env, Name: "www"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: r}},
	}))
	if !o.route("www").GetHeld() {
		t.Fatal("the test needs the route held")
	}
	if _, err := o.cat.SetUpCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.SetUpCatalogInstallRequest{Id: "thing"})); reasonOf(err) != "no_https" {
		t.Errorf("a plain domain's setup link: %v", err)
	}
}

// While a person sets a failed owner's app up through its setup link, its done is
// read for the page, which says it's set up once done reads it; before the link,
// nothing reads it (ADR 0105).
func TestAFailedOwnersAppReadsSetUpThroughItsSetupLink(t *testing.T) {
	t.Parallel()
	o := startOwning(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
	o.agent.fail = "it answered 500"
	o.look()
	o.addRoute("www", "thing.example.com", 80, 0)
	o.agent.mu.Lock()
	o.agent.made = true
	o.agent.mu.Unlock()
	o.look()
	if in := o.install(); in.GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_FAILED || in.GetOwner().GetSetUp() {
		t.Fatalf("before a setup link: %v", in.GetOwner())
	}
	proof := stepUpFor(t, o.b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_UP_CATALOG_INSTALL, o.install().GetProjectId())
	if _, err := o.cat.SetUpCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.SetUpCatalogInstallRequest{Id: "thing", Proof: proof})); err != nil {
		t.Fatal(err)
	}
	o.f.engine.Wait()
	o.look()
	if in := o.install(); in.GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_FAILED || !in.GetOwner().GetSetUp() || !in.GetHeld() {
		t.Errorf("set up through the link: %v, held %v", in.GetOwner(), in.GetHeld())
	}
}

// What becomes of making an owner is on Activity once, as it turns failed, then
// made on a retry: the app, who, and what the app said, scrubbed, and never the
// password (ADR 0105, events 150).
func TestWhatBecomesOfAnOwnerIsOnActivity(t *testing.T) {
	t.Parallel()
	o := startOwning(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
	o.agent.fail = "it answered 500"
	o.look()
	o.look()
	o.agent.mu.Lock()
	o.agent.fail = ""
	o.agent.mu.Unlock()
	if _, err := o.cat.RetryCatalogOwner(t.Context(), connect.NewRequest(&dockwellv1.RetryCatalogOwnerRequest{Id: "thing"})); err != nil {
		t.Fatal(err)
	}
	o.look()
	res, err := dockwellv1connect.NewEventServiceClient(o.b, o.b.origin, connect.WithProtoJSON()).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_APP_OWNER}, PageSize: 50,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range res.Msg.GetEvents() {
		actions = append(actions, e.GetAction())
		if a := e.GetAppOwner(); a.GetApp() != "Thing" || a.GetEmail() == "" {
			t.Errorf("%s says %v", e.GetAction(), a)
		}
		if e.GetAction() == "app_owner.failed" && !strings.Contains(e.GetAppOwner().GetError(), "500") {
			t.Errorf("the failure says %q", e.GetAppOwner().GetError())
		}
	}
	if strings.Join(actions, ",") != "app_owner.made,app_owner.failed" {
		t.Errorf("Activity has %v, newest first, want made then failed, once each", actions)
	}
	for _, sent := range o.agent.sent {
		if p := sent["OWNER_PASSWORD"]; p != "" && strings.Contains(prototext.Format(res.Msg), p) {
			t.Error("an event holds the owner's password")
		}
	}
}
