// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/autoupdate"
	"github.com/dockwell/dockwell/internal/control/owners"
	"github.com/dockwell/dockwell/internal/control/services"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/catalogsource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// ownerTop is an entry's owner, made by a request to its web service, with its
// sign-ups closed by a variable (ADR 0105).
const ownerTop = `owner:
  service: web
  emailInput: OWNER_EMAIL
  userInput: OWNER_USER
  passwordInput: OWNER_PASSWORD
  userPattern: "^[a-z0-9]+$"
  steps:
    - request:
        method: POST
        path: /setup
        contentType: application/json
        body: '{"email":"${OWNER_EMAIL}","user":"${OWNER_USER}","password":"${OWNER_PASSWORD}"}'
        port: 80
  done:
    request:
      path: /me
      port: 80
signups:
  service: web
  variable: SIGNUPS
  closed: "false"
`

// ownerInputs are the inputs ownerTop names.
const ownerInputs = `  - name: OWNER_EMAIL
    label: Owner's email
    type: INPUT_TYPE_EMAIL
  - name: OWNER_USER
    label: Owner's user
    type: INPUT_TYPE_TEXT
  - name: OWNER_PASSWORD
    label: Owner's password
    type: INPUT_TYPE_SECRET
    generate: 24
    generateAs: GENERATE_AS_PASSWORD
`

// ownedEntry is testEntry's app, with what top says of its owner and the inputs it
// names, as release.
func ownedEntry(release int, version, image, inputs, top string) string {
	return fmt.Sprintf(`id: thing
name: Thing
summary: Something with an owner
category: storage
version: %q
release: %d
license: AGPL-3.0-only
appLicense: MIT
source:
  repository: https://github.com/Dockwell/dockwell
web:
  service: web
inputs:
  - name: PORT
    label: Port
    type: INPUT_TYPE_PORT
    defaultValue: "18100"
  - name: PASSWORD
    label: Password
    type: INPUT_TYPE_SECRET
    generate: 32
%scompose: |
  services:
    web:
      image: %s
      environment:
        PASSWORD: ${PASSWORD}
        SIGNUPS: "true"
      ports:
        - "${PORT}:80"
      volumes:
        - data:/data
  volumes:
    data: {}
%s`, version, release, inputs, image, top)
}

// ownerAgent is an agent that makes owners as a test says.
type ownerAgent struct {
	mu     sync.Mutex
	fail   string
	made   bool
	calls  int
	checks []bool
	sent   []map[string]string
	runs   []*resourcev1.OwnerRun
}

func (a *ownerAgent) MakeAppOwner(_ context.Context, _, _ string, run *resourcev1.OwnerRun, values map[string]string, checkFirst bool) (*agentv1.MakeAppOwnerResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.checks = append(a.checks, checkFirst)
	a.sent = append(a.sent, maps.Clone(values))
	a.runs = append(a.runs, proto.CloneOf(run))
	if a.fail != "" && (!checkFirst || !a.made) {
		return &agentv1.MakeAppOwnerResult{Step: 1, Said: a.fail}, nil
	}
	return &agentv1.MakeAppOwnerResult{Made: true}, nil
}

func (a *ownerAgent) ReadAppOwner(context.Context, string, string, *resourcev1.OwnerRun, map[string]string, bool) (*agentv1.ReadAppOwnerResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return &agentv1.ReadAppOwnerResult{Yes: a.made}, nil
}

// owning is an install whose owner the test makes, on a server whose routes run in
// memory.
type owning struct {
	*upgrading
	agent *ownerAgent
}

func startOwning(t *testing.T, backend store.Backend, entry string, install *dockwellv1.InstallCatalogEntryRequest) *owning {
	t.Helper()
	u := &upgrading{t: t, dir: t.TempDir()}
	useCatalogDir(t, u.dir)
	u.release(entry)
	f := startFull(t, backend, nil)
	a := &ownerAgent{}
	f.server.owners.SetAgent(func(string) (owners.Agent, bool) { return a, true })
	u.b = signedIn(t, f)
	u.f = f
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	u.e = f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	pullsAnything(u.e)
	u.c = changeClient(u.b, u.b.origin)
	u.cat = catalogClient(u.b)
	if install != nil {
		u.applied(installApp(t, u.cat, install))
		u.env = u.install().GetEnvironmentId()
	}
	return &owning{upgrading: u, agent: a}
}

// look runs the owner runner once, waits for what it started, and looks again, so
// what a made owner lifted is released.
func (o *owning) look() {
	o.t.Helper()
	r := o.f.server.owners
	for range 2 {
		if err := r.Tick(o.t.Context()); err != nil {
			o.t.Fatal(err)
		}
		r.Wait()
	}
	o.f.engine.Wait()
}

// spec is a resource's stored spec, nil where there's none.
func (o *owning) spec(kind resource.Kind, scopeID, name string) proto.Message {
	o.t.Helper()
	var out proto.Message
	err := o.f.store.View(o.t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(o.t.Context(), string(kind), scopeID, name)
		if err != nil || r.Spec == nil {
			return nil //nolint:nilerr // none
		}
		out, err = o.f.keeper.Decode(o.t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		return err
	})
	if err != nil {
		o.t.Fatal(err)
	}
	return out
}

func (o *owning) route(name string) *resourcev1.Route {
	o.t.Helper()
	r, _ := o.spec(route.Kind, o.env, name).(*resourcev1.Route)
	return r
}

func (o *owning) pin() *resourcev1.CatalogPin {
	o.t.Helper()
	org := ""
	_ = o.f.store.View(o.t.Context(), func(tx *store.Tx) error {
		tree, err := scope.Load(o.t.Context(), tx)
		org = tree.Organization.ID
		return err
	})
	p, _ := o.spec(scope.ProjectKind, org, "thing").(*resourcev1.Project)
	return p.GetCatalog()
}

// addRoute applies a route to web, or with service empty, to a port on the server.
func (o *owning) addRoute(name, domain string, port, hostPort uint32) {
	o.t.Helper()
	r := &resourcev1.Route{Domain: domain, Service: "web", Port: port}
	if hostPort > 0 {
		r = &resourcev1.Route{Domain: domain, HostPort: hostPort}
	}
	ch := propose(o.t, o.c, "A domain "+domain, &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: o.env, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: r}},
	})
	if hostPort > 0 {
		applyAsRoot(o.t, o.b, ch)
		return
	}
	o.applied(ch)
}

// An install whose entry says how its owner is made makes it once its app runs, as
// the person installing it, with its sign-ups closed: it's held until then, so a
// domain added meanwhile, or a route to its port, answers that it isn't open, and
// once the owner is made, Dockwell releases them (ADR 0105).
func TestAnOwnerIsMadeOnceItsAppRunsAndItsHoldLifts(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o := startOwning(t, backend, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		if env := o.web().Config.Env; !slices.Contains(env, "SIGNUPS=false") {
			t.Errorf("web runs with %v, want its sign-ups closed", env)
		}
		in := o.install()
		if in.GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_WAITING || !in.GetHeld() || in.GetOwner().GetEmail() == "" {
			t.Fatalf("the install reads %v, held %v, want its owner waiting with the installer's email, held", in.GetOwner(), in.GetHeld())
		}
		email := in.GetOwner().GetEmail()
		if want, _, _ := strings.Cut(email, "@"); in.GetOwner().GetUser() != strings.ReplaceAll(want, ".", "") {
			t.Errorf("the owner's user is %q, want %q's local part as the pattern takes it", in.GetOwner().GetUser(), email)
		}
		o.addRoute("www", "thing.example.com", 80, 0)
		if !o.route("www").GetHeld() {
			t.Error("a domain added while the owner isn't made answers everyone")
		}
		o.addRoute("by-port", "port.example.com", 0, 18100)
		if !o.route("by-port").GetHeld() {
			t.Error("a route to the install's port, added while it's held, answers everyone")
		}
		o.look()
		if o.agent.calls != 1 || o.agent.checks[0] {
			t.Fatalf("the agent was asked %d times, checking first %v, want once, not checking", o.agent.calls, o.agent.checks)
		}
		sent := o.agent.sent[0]
		if sent["OWNER_EMAIL"] != email || len(sent["OWNER_PASSWORD"]) != 24 {
			t.Errorf("the steps were sent %d values, the email %q and a password of %d characters", len(sent), sent["OWNER_EMAIL"], len(sent["OWNER_PASSWORD"]))
		}
		in = o.install()
		if in.GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_MADE || in.GetHeld() {
			t.Fatalf("once made, the install reads %v, held %v", in.GetOwner(), in.GetHeld())
		}
		if o.route("www").GetHeld() || o.route("by-port").GetHeld() {
			t.Error("the owner is made, and what its hold kept wasn't released")
		}
		o.addRoute("later", "later.example.com", 80, 0)
		if o.route("later").GetHeld() {
			t.Error("a domain added once the owner is made is held")
		}
	})
}

// A failed owner leaves its install installed and held, with what the app said; it
// isn't tried again until a person asks, and then reads whether it's made first, so
// a run that made the account and lost its answer finishes (ADR 0105, H1, M6).
func TestAFailedOwnerKeepsItsInstallHeldUntilItsMade(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o := startOwning(t, backend, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		o.agent.fail = "it answered 500: [hidden] is taken"
		o.look()
		in := o.install()
		if in.GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_FAILED || !strings.Contains(in.GetOwner().GetError(), "Step 1: it answered 500") || !in.GetHeld() {
			t.Fatalf("after a failed step the install reads %v, held %v", in.GetOwner(), in.GetHeld())
		}
		if len(o.e.Containers()) == 0 {
			t.Error("a failed owner took the install away")
		}
		o.look()
		if o.agent.calls != 1 {
			t.Errorf("a failed owner was tried %d times without a person asking", o.agent.calls)
		}
		o.addRoute("www", "thing.example.com", 80, 0)
		if !o.route("www").GetHeld() {
			t.Error("a domain added to an install whose owner failed answers everyone")
		}
		// The account was made, and its answer lost: Try again reads that first.
		o.agent.made = true
		res, err := o.cat.RetryCatalogOwner(t.Context(), connect.NewRequest(&dockwellv1.RetryCatalogOwnerRequest{Id: "thing"}))
		if err != nil || res.Msg.GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_WAITING {
			t.Fatalf("RetryCatalogOwner = %v, %v", res, err)
		}
		o.look()
		if !o.agent.checks[len(o.agent.checks)-1] {
			t.Error("Try again didn't read whether the owner is made first")
		}
		if in := o.install(); in.GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_MADE || in.GetHeld() || o.route("www").GetHeld() {
			t.Errorf("after Try again the install reads %v, held %v, its domain held %v", in.GetOwner(), in.GetHeld(), o.route("www").GetHeld())
		}
	})
}

// An app whose first visitor makes its owner installs held, and stays held through
// every write: a domain, a port on every address, a whole-spec edit that leaves the
// port's hold out, a direct write of its project's pin, and an upgrade, until a
// person opens it (ADR 0105, H1 and M3).
func TestAFirstVisitorAppStaysHeldThroughEveryWrite(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		visitor := "firstVisitorOwns: true\n"
		o := startOwning(t, backend, ownedEntry(1, "1.0", firstRelease, "", visitor), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		if !o.pin().GetHeld() || !o.install().GetHeld() || o.install().GetOwner() != nil {
			t.Fatalf("a first-visitor install: pin held %v, install %v", o.pin().GetHeld(), o.install())
		}
		o.addRoute("www", "thing.example.com", 80, 0)
		if !o.route("www").GetHeld() {
			t.Error("a domain added to a first-visitor install answers everyone")
		}
		// A port on every address is held, and stays held through a whole-spec edit
		// that leaves its hold out.
		web := proto.CloneOf(o.stored("web"))
		web.GetApp().Ports = append(web.GetApp().GetPorts(), &resourcev1.PortMapping{HostPort: 18101, ContainerPort: 81})
		o.edit("A port for everyone", "web", web)
		if p := o.stored("web").GetApp().GetPorts(); !slices.ContainsFunc(p, func(m *resourcev1.PortMapping) bool { return m.GetHostPort() == 18101 && m.GetHeld() }) {
			t.Errorf("a port on every address of a held install isn't held: %v", p)
		}
		web = proto.CloneOf(o.stored("web"))
		for _, p := range web.GetApp().GetPorts() {
			p.Held = false
		}
		o.edit("A whole-spec edit", "web", web)
		if p := o.stored("web").GetApp().GetPorts(); !slices.ContainsFunc(p, func(m *resourcev1.PortMapping) bool { return m.GetHostPort() == 18101 && m.GetHeld() }) {
			t.Errorf("a whole-spec edit dropped a held port's hold: %v", p)
		}
		// A direct write of the project's pin, as a rebuild of it does, keeps the hold
		// and the sign-ups' choice.
		org := ""
		_ = o.f.store.View(t.Context(), func(tx *store.Tx) error {
			tree, err := scope.Load(t.Context(), tx)
			org = tree.Organization.ID
			return err
		})
		project, _ := proto.CloneOf(o.spec(scope.ProjectKind, org, "thing")).(*resourcev1.Project)
		project.Catalog.Held, project.Catalog.SignupsOpen = false, true
		o.applied(propose(t, o.c, "Rewrite the pin", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(scope.ProjectKind), Name: "thing"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: project}},
		}))
		if p := o.pin(); !p.GetHeld() || p.GetSignupsOpen() {
			t.Errorf("a direct write of the pin left held %v, sign-ups open %v", p.GetHeld(), p.GetSignupsOpen())
		}
		// An upgrade rebuilds the pin, and keeps the hold.
		o.release(ownedEntry(2, "2.0", secondRelease, "", visitor))
		o.applied(o.upgrade())
		if !o.pin().GetHeld() || !o.install().GetHeld() {
			t.Error("an upgrade dropped the install's hold")
		}
		// Opening it is the one write that clears it, which a person applies with a
		// passkey.
		res, err := o.cat.OpenCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.OpenCatalogInstallRequest{Id: "thing"}))
		if err != nil {
			t.Fatal(err)
		}
		applyAsRoot(t, o.b, res.Msg.GetChange())
		if o.pin().GetHeld() || o.install().GetHeld() || o.route("www").GetHeld() {
			t.Errorf("once opened: pin held %v, install held %v, domain held %v", o.pin().GetHeld(), o.install().GetHeld(), o.route("www").GetHeld())
		}
		o.addRoute("later", "later.example.com", 80, 0)
		if o.route("later").GetHeld() {
			t.Error("a domain added once the install is open is held")
		}
	})
}

// Deleting an owner that isn't made, by any write, holds its install, so removing
// the owner never lifts the hold (ADR 0105, the lead's rule).
func TestDeletingAnOwnerNotYetMadeHoldsItsInstall(t *testing.T) {
	t.Parallel()
	o := startOwning(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
	if o.pin().GetHeld() {
		t.Fatal("an install whose owner Dockwell makes starts with its pin held")
	}
	o.applied(propose(t, o.c, "Drop the owner", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "app.owner", Scope: o.env, Name: "owner"}, Delete: true}))
	if !o.pin().GetHeld() || !o.install().GetHeld() {
		t.Error("deleting an owner not yet made lifted its install's hold")
	}
	o.addRoute("www", "thing.example.com", 80, 0)
	if !o.route("www").GetHeld() {
		t.Error("a domain added after the owner went answers everyone")
	}
}

// The hold comes from the install's own state, never the entry now: an entry that
// gains owner steps in a release holds no install from before, and one that loses
// them leaves an install whose owner failed held (the re-check's H1).
func TestAReleaseThatGainsOrLosesOwnerStepsKeepsTheInstallsHold(t *testing.T) {
	t.Parallel()
	t.Run("gains", func(t *testing.T) {
		t.Parallel()
		o := startOwning(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, "", ""), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		o.release(ownedEntry(2, "2.0", secondRelease, ownerInputs, ownerTop))
		o.applied(o.upgrade())
		if in := o.install(); in.GetHeld() || in.GetOwner() != nil || o.pin().GetHeld() {
			t.Errorf("an install from before, upgraded to a release with owner steps: %v", in)
		}
		o.addRoute("www", "thing.example.com", 80, 0)
		if o.route("www").GetHeld() {
			t.Error("a domain added to an install from before is held")
		}
	})
	t.Run("loses", func(t *testing.T) {
		t.Parallel()
		o := startOwning(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		o.agent.fail = "it answered 500"
		o.look()
		o.release(ownedEntry(2, "2.0", secondRelease, "", ""))
		o.applied(o.upgrade())
		if !o.pin().GetHeld() || !o.install().GetHeld() {
			t.Error("an upgrade whose release no longer makes the owner left a failed install unheld")
		}
	})
}

// An install by a token with no owner's email waits held for one; once a person
// gives it, the owner is made (ADR 0105, M7).
func TestATokenInstallWaitsHeldForItsOwnersEmail(t *testing.T) {
	t.Parallel()
	o := startOwning(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), nil)
	token := createToken(t, o.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := dockwellv1connect.NewCatalogServiceClient(bearer{token: token, rt: o.b.client.Transport}, o.b.origin, connect.WithProtoJSON())
	o.applied(installApp(t, script, &dockwellv1.InstallCatalogEntryRequest{Id: "thing"}))
	o.env = o.install().GetEnvironmentId()
	o.look()
	in := o.install()
	if in.GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_WAITING_EMAIL || !in.GetHeld() || o.agent.calls != 0 {
		t.Fatalf("a token's install with no email reads %v, held %v, after %d calls", in.GetOwner(), in.GetHeld(), o.agent.calls)
	}
	res, err := o.cat.SetCatalogOwnerEmail(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogOwnerEmailRequest{Id: "thing", Email: "owner@example.com"}))
	if err != nil {
		t.Fatal(err)
	}
	o.applied(res.Msg.GetChange())
	o.look()
	if in := o.install(); in.GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_MADE || in.GetHeld() || in.GetOwner().GetUser() != "owner" {
		t.Errorf("once given an email the install reads %v, held %v", in.GetOwner(), in.GetHeld())
	}
	if got := o.agent.sent[0]["OWNER_EMAIL"]; got != "owner@example.com" {
		t.Errorf("the steps were sent the email %q", got)
	}
}

// Moving a held install's database, which rebuilds its pin, keeps its hold and its
// sign-ups' choice (the re-check's H1).
func TestUsingAnotherDatabaseKeepsTheInstallsHold(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	useCatalogDir(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "blog.yaml"), []byte(blogEntry+"firstVisitorOwns: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	own := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}, sizes: map[string]int64{"blog": 12 << 20}}
	shared := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	postgres, _ := c.Entry("postgres")
	e.Registry[regexp.MustCompile(`image: (postgres:\S+)`).FindStringSubmatch(postgres.GetCompose())[1]] = own.image()
	e.Registry["postgres:17"] = shared.image()
	e.Registry["ghcr.io/acme/shop:1.4"] = dockertest.Image{Config: docker.Config{Cmd: []string{"shop"}}}
	e.Registry["redis:7"] = dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}}
	e.Registry["ghcr.io/acme/blog:1@sha256:"+digest1] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	ch := changeClient(b, b.origin)
	cat := catalogClient(b)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	apply(t, ch, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": "the-administrators"}, false))
	if got := apply(t, ch, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "blog", SignupsOpen: true})); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	pin := func() *resourcev1.CatalogPin {
		res, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range res.Msg.GetInstalls() {
			if in.GetProject() == "blog" {
				return in.GetPin()
			}
		}
		t.Fatal("no blog install")
		return nil
	}
	if p := pin(); !p.GetHeld() || !p.GetSignupsOpen() {
		t.Fatalf("the install's pin: held %v, sign-ups open %v", p.GetHeld(), p.GetSignupsOpen())
	}
	entry, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	var choice string
	for _, o := range entry.Msg.GetNeeds() {
		if o.GetService() == "db" && o.GetProject() != "blog" && o.GetWhy() == "" {
			choice = o.GetChoice()
		}
	}
	res, err := cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "blog", Need: "db", Choice: choice}))
	if err != nil {
		t.Fatal(err)
	}
	if got := apply(t, ch, res.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the move ended %v: %s", got.GetState(), got.GetError())
	}
	if p := pin(); !p.GetHeld() || !p.GetSignupsOpen() {
		t.Errorf("after using another database the pin: held %v, sign-ups open %v", p.GetHeld(), p.GetSignupsOpen())
	}
}

// An automatic update of a held catalog install, which Dockwell plans on its own,
// keeps its hold (the re-check's H1).
func TestAnAutomaticUpdateKeepsTheInstallsHold(t *testing.T) {
	t.Parallel()
	visitor := "firstVisitorOwns: true\n"
	u := startUpgrading(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, "", visitor))
	u.release(ownedEntry(2, "1.1", secondRelease, "", visitor))
	// Every catalog has held release 2 long enough to take it on its own (ADR 0080).
	if err := u.f.store.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.PutSetting(t.Context(), "catalog.release./thing", []byte("2 0 "+longAgo.Format(time.RFC3339Nano))); err != nil {
			return err
		}
		sources, err := tx.ListKind(t.Context(), string(catalogsource.Kind))
		if err != nil {
			return err
		}
		for _, src := range sources {
			if err := tx.PutSetting(t.Context(), "catalog.release."+src.ID+"/thing", []byte(fmt.Sprintf("2 %d %s", src.Revision, longAgo.Format(time.RFC3339Nano)))); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s := dockwellv1connect.NewServiceServiceClient(u.b, u.b.origin, connect.WithProtoJSON())
	list, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var web *dockwellv1.ServiceInfo
	for _, svc := range list.Msg.GetServices() {
		if svc.GetResource().GetName() == "web" {
			web = svc
		}
	}
	b := &bench{f: u.f, b: u.b, e: u.e, c: u.c, s: s}
	b.setAutoUpdate(t, web, &resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_MINOR, At: "03:00", TimeZone: "UTC"})
	r := &autoupdate.Runner{
		DB: u.f.store, Keeper: u.f.keeper, Planner: u.f.server.autoupdates.Planner, Engine: u.f.engine,
		Updates: &newer{found: map[string]services.Newer{web.GetId(): {Image: secondRelease, Kind: "minor", Catalog: true, Found: time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)}}},
		Logger:  slog.New(slog.DiscardHandler), Now: func() time.Time { return time.Date(2026, 11, 15, 3, 5, 0, 0, time.UTC) },
	}
	if err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	u.f.engine.Wait()
	if got := u.web().Config.Image; got != secondRelease {
		t.Fatalf("the automatic update didn't run: web runs %s", got)
	}
	if in := u.install(); !in.GetPin().GetHeld() || !in.GetHeld() {
		t.Errorf("an automatic update dropped the install's hold: %v", in.GetPin())
	}
}

// openApplied plans Open, and a person applies it with a passkey.
func (o *owning) open() {
	o.t.Helper()
	res, err := o.cat.OpenCatalogInstall(o.t.Context(), connect.NewRequest(&dockwellv1.OpenCatalogInstallRequest{Id: "thing"}))
	if err != nil {
		o.t.Fatal(err)
	}
	applyAsRoot(o.t, o.b, res.Msg.GetChange())
	o.f.engine.Wait()
}

// A write of the project that leaves its pin out, as the CLI's and a whole-spec
// write's do, in one change that also adds a route and deletes the owner, leaves the
// install held: the route is written held, the pin reads held, and a route added
// after is held too (the review's H1).
func TestAProjectWriteWithoutItsPinKeepsTheHold(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o := startOwning(t, backend, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		rename := &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(scope.ProjectKind), Name: "thing"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Thing renamed"}}},
		}
		route1 := &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: o.env, Name: "www"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "thing.example.com", Service: "web", Port: 80}}},
		}
		// A rename and a route alone first.
		o.applied(propose(t, o.c, "Rename and add a domain", rename, route1))
		if !o.route("www").GetHeld() || !o.install().GetHeld() {
			t.Errorf("a rename with no pin and a route: the route held %v, the install held %v", o.route("www").GetHeld(), o.install().GetHeld())
		}
		// Then a rename, a route and the owner's deletion, in one change.
		rename.GetSpec().GetProject().DisplayName = "Thing"
		o.applied(propose(t, o.c, "Rename, add a domain and drop the owner", rename, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: o.env, Name: "www2"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "thing2.example.com", Service: "web", Port: 80}}},
		}, &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "app.owner", Scope: o.env, Name: "owner"}, Delete: true}))
		if !o.route("www2").GetHeld() || !o.pin().GetHeld() || !o.install().GetHeld() {
			t.Errorf("rename, route and the owner's deletion: route held %v, pin held %v, install held %v", o.route("www2").GetHeld(), o.pin().GetHeld(), o.install().GetHeld())
		}
		o.addRoute("later", "later.example.com", 80, 0)
		if !o.route("later").GetHeld() {
			t.Error("a route added after answers everyone")
		}
	})
}

// No write but the catalog's makes or changes an app's owner: the steps aren't in
// it, so the agent gets them from the entry at the install's release; a token's or a
// person's write of the owner is refused, whatever it says (the review's H2).
func TestOnlyTheCatalogWritesAnAppsOwner(t *testing.T) {
	t.Parallel()
	o := startOwning(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
	cur, _ := proto.CloneOf(o.spec("app.owner", o.env, "owner")).(*resourcev1.AppOwner)
	cur.Email = "someone@example.com"
	token := createToken(t, o.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	tc := dockwellv1connect.NewChangeServiceClient(bearer{token: token, rt: o.b.client.Transport}, o.b.origin, connect.WithProtoJSON())
	edit := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "app.owner", Scope: o.env, Name: "owner"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AppOwner{AppOwner: cur}},
	}
	for name, c := range map[string]dockwellv1connect.ChangeServiceClient{"token": tc, "person": o.c} {
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Another owner", Edits: []*dockwellv1.Edit{edit}}))
		if err == nil || !strings.Contains(err.Error(), "only the catalog writes an app's owner") {
			t.Errorf("a %s's write of the owner: %v", name, err)
		}
	}
	o.look()
	if o.agent.calls != 1 || len(o.agent.runs[0].GetSteps()) != 1 || o.agent.runs[0].GetSteps()[0].GetRequest().GetPath() != "/setup" {
		t.Fatalf("the agent ran %v", o.agent.runs)
	}
	if got := o.agent.runs[0].GetEmail(); got == "someone@example.com" {
		t.Error("a refused write chose the owner")
	}
}

// A token alone can't open a held install: Open's plan lifts a hold, which a person
// applies with a passkey (the review's M1).
func TestATokenAloneCantOpenAHeldInstall(t *testing.T) {
	t.Parallel()
	o := startOwning(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, "", "firstVisitorOwns: true\n"), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
	o.addRoute("www", "thing.example.com", 80, 0)
	token := createToken(t, o.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	cat := dockwellv1connect.NewCatalogServiceClient(bearer{token: token, rt: o.b.client.Transport}, o.b.origin, connect.WithProtoJSON())
	tc := dockwellv1connect.NewChangeServiceClient(bearer{token: token, rt: o.b.client.Transport}, o.b.origin, connect.WithProtoJSON())
	res, err := cat.OpenCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.OpenCatalogInstallRequest{Id: "thing"}))
	if err != nil {
		t.Fatal(err)
	}
	ch := res.Msg.GetChange()
	if _, err := tc.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); err == nil {
		t.Fatal("a token applied Open on a held install")
	}
	o.f.engine.Wait()
	if !o.pin().GetHeld() || !o.route("www").GetHeld() {
		t.Error("a token's Open lifted the hold")
	}
	applyAsRoot(t, o.b, ch)
	if o.pin().GetHeld() || o.route("www").GetHeld() {
		t.Error("a person's Open with a passkey didn't lift it")
	}
}

// A held install's service can't move onto the server's own network, where every
// port it listens on answers everyone (the review's L6).
func TestAHeldInstallStaysOffTheServersNetwork(t *testing.T) {
	t.Parallel()
	o := startOwning(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
	web := proto.CloneOf(o.stored("web"))
	web.GetApp().Ports, web.GetApp().Networks, web.GetApp().HostNetwork = nil, nil, true
	_, err := o.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Host network", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: o.env, Name: "web"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: web}},
	}}}))
	if err == nil || !strings.Contains(err.Error(), "server's own network") {
		t.Errorf("a held install's service onto the server's network: %v", err)
	}
}

// Opening an install its owner holds opens it in that change, the one a person
// approved, and records the owner opened once it applies (the review's L7).
func TestOpeningAnOwnersInstallOpensItInThatChange(t *testing.T) {
	t.Parallel()
	o := startOwning(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
	o.agent.fail = "it answered 500"
	o.look()
	o.addRoute("www", "thing.example.com", 80, 0)
	o.open()
	if o.route("www").GetHeld() {
		t.Error("the Open a person approved left the route held")
	}
	o.look()
	if in := o.install(); in.GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_OPENED || in.GetHeld() {
		t.Errorf("after Open: %v, held %v", in.GetOwner(), in.GetHeld())
	}
}

// A route to a port on the server, made before a held install published there,
// would dial the held app on 127.0.0.1 from then on, so it's held as the port is
// published, and opening the install releases it with the rest (the re-check's N1).
func TestARouteMadeBeforeAHeldAppTakesItsPortIsHeld(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o := startOwning(t, backend, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		o.addRoute("hp", "port.example.com", 0, 18200)
		if o.route("hp").GetHeld() {
			t.Fatal("a route to a port nothing publishes is held")
		}
		token := createToken(t, o.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		tc := dockwellv1connect.NewChangeServiceClient(bearer{token: token, rt: o.b.client.Transport}, o.b.origin, connect.WithProtoJSON())
		web := proto.CloneOf(o.stored("web"))
		web.GetApp().Ports = append(web.GetApp().Ports, &resourcev1.PortMapping{HostPort: 18200, ContainerPort: 80})
		if got := apply(t, tc, propose(t, tc, "Another port", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: o.env, Name: "web"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: web}},
		})); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the port ended %v: %s", got.GetState(), got.GetError())
		}
		if !o.route("hp").GetHeld() {
			t.Error("a route made before to the held app's new port answers everyone")
		}
		o.open()
		if o.route("hp").GetHeld() {
			t.Error("opening the install left the route to its port held")
		}
	})
}

// No write but the catalog's changes what the pin says the install runs, its values
// and where it's reached, which the owner's run reads (the re-check's N2).
func TestOnlyTheCatalogChangesWhatAPinSaysItRuns(t *testing.T) {
	t.Parallel()
	o := startOwning(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
	before := proto.CloneOf(o.pin())
	token := createToken(t, o.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	tc := dockwellv1connect.NewChangeServiceClient(bearer{token: token, rt: o.b.client.Transport}, o.b.origin, connect.WithProtoJSON())
	pin := proto.CloneOf(before)
	pin.Address, pin.Entry, pin.Release = "https://elsewhere.example.com", "other", 9
	if pin.Values == nil {
		pin.Values = map[string]string{}
	}
	pin.Values["PORT"] = "1"
	if got := apply(t, tc, propose(t, tc, "Rewrite the pin", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(scope.ProjectKind), Name: "thing"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Thing", Catalog: pin}}},
	})); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the write ended %v: %s", got.GetState(), got.GetError())
	}
	after := o.pin()
	if after.GetAddress() != before.GetAddress() || after.GetEntry() != before.GetEntry() || after.GetRelease() != before.GetRelease() || after.GetValues()["PORT"] != before.GetValues()["PORT"] {
		t.Errorf("a token's write changed the pin: %v", after)
	}
	o.look()
	if len(o.agent.runs) != 1 || o.agent.runs[0].GetHost() == "elsewhere.example.com" {
		t.Errorf("the owner ran with %v", o.agent.runs)
	}
}

// Each Open planned counts: the one a person applies records the owner opened,
// though another was planned after it (the re-check's N4).
func TestTheOpenAPersonAppliesRecordsTheOwnerOpened(t *testing.T) {
	t.Parallel()
	o := startOwning(t, store.SQLite, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
	o.agent.fail = "it answered 500"
	o.look()
	o.addRoute("www", "thing.example.com", 80, 0)
	first, err := o.cat.OpenCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.OpenCatalogInstallRequest{Id: "thing"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.cat.OpenCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.OpenCatalogInstallRequest{Id: "thing"})); err != nil {
		t.Fatal(err)
	}
	applyAsRoot(t, o.b, first.Msg.GetChange())
	o.look()
	if in := o.install(); in.GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_OPENED || in.GetHeld() {
		t.Errorf("after the first Open applied: %v, held %v", in.GetOwner(), in.GetHeld())
	}
	o.addRoute("later", "later.example.com", 80, 0)
	if o.route("later").GetHeld() {
		t.Error("a route added after the Open a person applied is held")
	}
}

// A route planned while a first-visitor install is held is written held, and if a
// person applies it after opening the install, Dockwell releases it at its next
// look, as the pin says it's opened, which no write but Open sets or takes back
// (the review's L10).
func TestARoutePlannedWhileHeldIsReleasedOnceTheInstallIsOpened(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o := startOwning(t, backend, ownedEntry(1, "1.0", firstRelease, "", "firstVisitorOwns: true\n"), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		late := propose(t, o.c, "A domain thing.example.com", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: o.env, Name: "www"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "thing.example.com", Service: "web", Port: 80}}},
		})
		o.open()
		if p := o.pin(); !p.GetOpened() || p.GetHeld() {
			t.Fatalf("once opened, the pin reads opened %v, held %v", p.GetOpened(), p.GetHeld())
		}
		o.applied(late)
		if !o.route("www").GetHeld() {
			t.Fatal("the route planned while held was written open; the test needs it held")
		}
		o.look()
		if o.route("www").GetHeld() {
			t.Error("a route planned while held and applied after Open stays held")
		}
		// A write of the pin that leaves opened out keeps it.
		org := ""
		_ = o.f.store.View(t.Context(), func(tx *store.Tx) error {
			tree, err := scope.Load(t.Context(), tx)
			org = tree.Organization.ID
			return err
		})
		project, _ := proto.CloneOf(o.spec(scope.ProjectKind, org, "thing")).(*resourcev1.Project)
		project.Catalog.Opened = false
		o.applied(propose(t, o.c, "Rewrite the pin", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(scope.ProjectKind), Name: "thing"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: project}},
		}))
		if !o.pin().GetOpened() {
			t.Error("a write of the pin took back that it was opened")
		}
	})
}

// pinAsCatalogWrote writes a project's pin as the catalog once did, as Dockwell's own
// change: only the catalog's own write changes what a pin says it runs, its values
// and where it's reached, so a test that sets up an install from before writes it
// so (ADR 0105, the re-check's N2).
func pinAsCatalogWrote(t *testing.T, f *fullServer, ref resource.Ref, project *resourcev1.Project) {
	t.Helper()
	plan, err := f.engine.Propose(context.WithoutCancel(t.Context()), revision.Proposal{
		Summary: "As the catalog wrote it", Actor: revision.SystemActor, Via: revision.ViaAutomatic,
		Edits: []revision.Edit{{Ref: ref, Spec: project, Pin: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Apply(t.Context(), plan.Change); err != nil {
		t.Fatal(err)
	}
	f.engine.Wait()
}
