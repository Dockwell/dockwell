// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource/registrylogin"
	"github.com/dockwell/dockwell/internal/store"
)

// The made-up login the tests sign in with, and a private image only it pulls.
const (
	loginUser     = "ci"
	loginPassword = "s3cret-read-token-for-tests"
	privateImage  = "ghcr.io/acme/private:1.0"
	privateDigest = "sha256:5151515151515151515151515151515151515151515151515151515151515151"
)

// privateRegistry stands in for a registry that signs in itself, with Basic, and
// keeps acme/private to those who do. It logs each Authorization it was sent.
type privateRegistry struct {
	srv  *httptest.Server
	mu   sync.Mutex
	sent []string
}

func newPrivateRegistry(t *testing.T) *privateRegistry {
	t.Helper()
	p := &privateRegistry{}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(loginUser+":"+loginPassword))
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.sent = append(p.sent, r.URL.Path+" "+r.Header.Get("Authorization"))
		p.mu.Unlock()
		if r.Header.Get("Authorization") != want {
			w.Header().Set("Www-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/":
		case "/v2/acme/private/manifests/1.0":
			w.Header().Set("Docker-Content-Digest", privateDigest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func loginEdit(name string, l *resourcev1.RegistryLogin) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(registrylogin.Kind), Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_RegistryLogin{RegistryLogin: l}},
	}
}

// An admin checks a login live and saves it as a change; an app whose image its
// registry keeps private then plans, pulls and reads with it, and the plan says
// which. The password is in no plan, change, event or answer, and goes to Docker
// for that image's pull alone (ADR 0086).
func TestAPrivateImagePullsWithASavedLogin(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.Registry[privateImage] = dockertest.Image{Login: loginUser + ":" + loginPassword, Digest: privateDigest}
	reg := newPrivateRegistry(t)
	srv := f.lastDocker
	srv.stop()
	waitConnected(t, f, srv.id.ServerID, false)
	srv.images = agent.ImageReads{Test: reg.srv.URL}
	srv.run(t)
	waitConnected(t, f, srv.id.ServerID, true)
	c := changeClient(b, b.origin)
	ic := dockwellv1connect.NewImageServiceClient(b, b.origin, connect.WithProtoJSON())
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())

	// A probe asks the registry with no login, and takes nothing that signs in.
	reg.mu.Lock()
	before := len(reg.sent)
	reg.mu.Unlock()
	probed, err := ic.CheckRegistryLogin(t.Context(), connect.NewRequest(&dockwellv1.CheckRegistryLoginRequest{Registry: "ghcr.io", Probe: true}))
	if err != nil || probed.Msg.GetState() != dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_ASKS {
		t.Fatalf("a probe = %v, %v", probed, err)
	}
	reg.mu.Lock()
	for _, line := range reg.sent[before:] {
		if strings.TrimSpace(strings.TrimPrefix(line, "/v2/")) != "" {
			t.Errorf("a probe sent %q", line)
		}
	}
	reg.mu.Unlock()
	if _, err := ic.CheckRegistryLogin(t.Context(), connect.NewRequest(&dockwellv1.CheckRegistryLoginRequest{Registry: "ghcr.io", Probe: true, Password: loginPassword})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a probe with a password = %v, want it refused", err)
	}

	// A live check, as typed: a wrong password is refused, the right one signs in.
	check := func(password string) *dockwellv1.CheckRegistryLoginResponse {
		t.Helper()
		got, err := ic.CheckRegistryLogin(t.Context(), connect.NewRequest(&dockwellv1.CheckRegistryLoginRequest{Registry: "ghcr.io", Username: loginUser, Password: password}))
		if err != nil {
			t.Fatal(err)
		}
		return got.Msg
	}
	if got := check("wrong"); got.GetState() != dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_REFUSED {
		t.Errorf("a wrong password checks as %v", got.GetState())
	}
	if got := check(loginPassword); got.GetState() != dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_SIGNED_IN {
		t.Fatalf("the login checks as %v: %s", got.GetState(), got.GetDetail())
	}

	// Without a login, the image can't be planned.
	if _, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: "name: shop\nservices:\n  web:\n    image: " + privateImage + "\n"})); err == nil || !strings.Contains(err.Error(), "save a registry login for ghcr.io") {
		t.Errorf("a private image with no login planned: %v", err)
	}

	saved := propose(t, c, "Pull from GHCR", loginEdit("ghcr-ci", &resourcev1.RegistryLogin{Registry: "ghcr.io", Username: loginUser, Password: loginPassword}))
	if text := protojson.Format(saved); strings.Contains(text, loginPassword) {
		t.Errorf("the login's plan holds its password: %s", text)
	}
	if len(saved.GetPlan().GetServers()) != 0 {
		t.Errorf("a login planned steps on servers: %v", saved.GetPlan().GetServers())
	}
	apply(t, c, saved)

	// Now the image plans and pulls, with the login the plan names.
	res, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: "name: shop\nservices:\n  web:\n    image: " + privateImage + "\n"}))
	if err != nil {
		t.Fatal(err)
	}
	planned := res.Msg.GetChange()
	named := ""
	for _, s := range planned.GetPlan().GetServers() {
		for _, step := range s.GetSteps() {
			if step.GetResource().GetKind() == "service" {
				named = step.GetRegistryLogin()
			}
		}
	}
	if named != "ghcr-ci" {
		t.Errorf("the plan pulls with %q, want ghcr-ci", named)
	}
	done := applied(t, c, planned)
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the deploy ended %v: %s", done.GetState(), done.GetError())
	}
	for _, text := range []string{protojson.Format(planned), protojson.Format(done)} {
		if strings.Contains(text, loginPassword) {
			t.Errorf("a change holds the login's password: %s", text)
		}
	}
	events, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(protojson.Format(events.Msg), loginPassword) {
		t.Error("Activity holds the login's password")
	}
	logins := e.SentLogins()
	if len(logins) == 0 {
		t.Fatal("Docker was sent no login for the private image")
	}
	for _, l := range logins {
		if l != privateImage+" as "+loginUser+":"+loginPassword {
			t.Errorf("Docker was sent a login with %q", l)
		}
	}

	// Its update check signs in as its pull does (ADR 0079), so it can tell.
	u := dockwellv1connect.NewServiceUpdateServiceClient(b, b.origin, connect.WithProtoJSON())
	checked, err := u.CheckServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.CheckServiceUpdatesRequest{Server: "hz-1"}))
	if err != nil {
		t.Fatal(err)
	}
	checkedWeb := false
	for _, s := range checked.Msg.GetServices() {
		if s.GetResource().GetName() != "web" {
			continue
		}
		checkedWeb = true
		if got := s.GetUpdate(); got.GetCheck() != dockwellv1.UpdateCheck_UPDATE_CHECK_UP_TO_DATE {
			t.Errorf("the private image's update check reads %v", got)
		}
	}
	if !checkedWeb {
		t.Errorf("the check lists no web: %v", checked.Msg.GetServices())
	}

	// A form reads the image as its pull would, and says with which login.
	got, err := ic.GetImage(t.Context(), connect.NewRequest(&dockwellv1.GetImageRequest{Reference: privateImage, Project: "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND || got.Msg.GetRegistryLogin() != "ghcr-ci" || got.Msg.GetDigest() != privateDigest {
		t.Errorf("the image read %v with %q", got.Msg.GetAnswer(), got.Msg.GetRegistryLogin())
	}
	anonymous, err := ic.GetImage(t.Context(), connect.NewRequest(&dockwellv1.GetImageRequest{Reference: privateImage, Project: "shop", Anonymous: true}))
	if err != nil {
		t.Fatal(err)
	}
	if anonymous.Msg.GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_FOUND || anonymous.Msg.GetRegistryLogin() != "" {
		t.Errorf("read with no login, the image is %v with %q", anonymous.Msg.GetAnswer(), anonymous.Msg.GetRegistryLogin())
	}

	// A saved login checks again with what it saved.
	again, err := ic.CheckRegistryLogin(t.Context(), connect.NewRequest(&dockwellv1.CheckRegistryLoginRequest{Name: "ghcr-ci"}))
	if err != nil || again.Msg.GetState() != dockwellv1.RegistryLoginState_REGISTRY_LOGIN_STATE_SIGNED_IN {
		t.Errorf("checking the saved login again: %v, %v", again, err)
	}
	listed, err := ic.ListRegistryLogins(t.Context(), connect.NewRequest(&dockwellv1.ListRegistryLoginsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if l := listed.Msg.GetLogins(); len(l) != 1 || l[0].GetName() != "ghcr-ci" || l[0].GetUsername() != loginUser || l[0].GetRegistry() != "ghcr.io" {
		t.Errorf("an admin lists %v", l)
	}
	if strings.Contains(protojson.Format(listed.Msg), loginPassword) {
		t.Error("the list holds the login's password")
	}

	// Below an admin: names and registries alone, nothing to save or check.
	hold := aMember(t, f)
	hold(onInstall(store.RoleDeveloper))
	trimmed, err := ic.ListRegistryLogins(t.Context(), connect.NewRequest(&dockwellv1.ListRegistryLoginsRequest{Project: "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	if l := trimmed.Msg.GetLogins(); len(l) != 1 || l[0].GetName() != "ghcr-ci" || l[0].GetRegistry() != "ghcr.io" || l[0].GetUsername() != "" {
		t.Errorf("a developer lists %v", l)
	}
	_, err = ic.CheckRegistryLogin(t.Context(), connect.NewRequest(&dockwellv1.CheckRegistryLoginRequest{Name: "ghcr-ci"}))
	needsRole(t, "a developer checking a login", err)
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Another", Edits: []*dockwellv1.Edit{
		loginEdit("mine", &resourcev1.RegistryLogin{Registry: "ghcr.io", Username: loginUser, Password: loginPassword}),
	}}))
	if err == nil {
		t.Error("a developer planned a registry login")
	}
}

// A login whose projects leave a project out isn't that project's to pull with,
// and its list doesn't show it.
func TestALoginIsItsProjectsAlone(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	ic := dockwellv1connect.NewImageServiceClient(b, b.origin, connect.WithProtoJSON())
	project := func(name string) *dockwellv1.Edit {
		return &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "project", Name: name}, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}}}
	}
	made := apply(t, c, propose(t, c, "Projects", project("shop"), project("blog")))
	shop := ""
	for _, r := range made.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "project" && r.GetResource().GetName() == "shop" {
			shop = r.GetId()
		}
	}
	apply(t, c, propose(t, c, "Shop's login", loginEdit("shop-ghcr", &resourcev1.RegistryLogin{Registry: "ghcr.io", Username: loginUser, Password: loginPassword, Projects: []string{shop}})))
	for project, want := range map[string]int{"shop": 1, "blog": 0} {
		listed, err := ic.ListRegistryLogins(t.Context(), connect.NewRequest(&dockwellv1.ListRegistryLoginsRequest{Project: project}))
		if err != nil {
			t.Fatal(err)
		}
		if len(listed.Msg.GetLogins()) != want {
			t.Errorf("%s may use %v", project, listed.Msg.GetLogins())
		}
	}
}

// A private image one project pulled with its login doesn't run in a project that
// may not use that login: the server has a copy, and the plan asks the registry
// with the other app's own pull, which it refuses. (Security review of #584, M1.)
func TestAnotherProjectDoesntRunAPrivateImageItCantPull(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.Registry[privateImage] = dockertest.Image{Login: loginUser + ":" + loginPassword, Digest: privateDigest}
	reg := newPrivateRegistry(t)
	srv := f.lastDocker
	srv.stop()
	waitConnected(t, f, srv.id.ServerID, false)
	srv.images = agent.ImageReads{Test: reg.srv.URL}
	srv.run(t)
	waitConnected(t, f, srv.id.ServerID, true)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	project := func(name string) *dockwellv1.Edit {
		return &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "project", Name: name}, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}}}
	}
	made := apply(t, c, propose(t, c, "Projects", project("shop"), project("blog")))
	shop := ""
	for _, r := range made.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "project" && r.GetResource().GetName() == "shop" {
			shop = r.GetId()
		}
	}
	apply(t, c, propose(t, c, "Shop's login", loginEdit("shop-ghcr", &resourcev1.RegistryLogin{Registry: "ghcr.io", Username: loginUser, Password: loginPassword, Projects: []string{shop}})))
	compose := "services:\n  web:\n    image: " + privateImage + "\n"
	shopApp, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: compose, Project: "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	if done := applied(t, c, shopApp.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("shop's app ended %v: %s", done.GetState(), done.GetError())
	}
	pulls := e.Pulls
	_, err = cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: compose, Project: "blog"}))
	if err == nil || !strings.Contains(err.Error(), "save a registry login for ghcr.io") && !strings.Contains(err.Error(), "came with a registry login") {
		t.Errorf("blog planned shop's private image, which the server has: %v", err)
	}
	if e.Pulls != pulls {
		t.Errorf("blog's plan pulled")
	}
}

// A login saved again under its name between a plan and its apply makes the plan
// stale, as its fingerprint covers which revision of which login a pull uses, not
// only its name. (Security review of #584, L1.)
func TestALoginSavedAgainMakesAPlanStale(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.Registry[privateImage] = dockertest.Image{Login: loginUser + ":" + loginPassword, Digest: privateDigest}
	reg := newPrivateRegistry(t)
	srv := f.lastDocker
	srv.stop()
	waitConnected(t, f, srv.id.ServerID, false)
	srv.images = agent.ImageReads{Test: reg.srv.URL}
	srv.run(t)
	waitConnected(t, f, srv.id.ServerID, true)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	login := &resourcev1.RegistryLogin{Registry: "ghcr.io", Username: loginUser, Password: loginPassword}
	apply(t, c, propose(t, c, "Pull from GHCR", loginEdit("ghcr-ci", login)))
	res, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: "name: shop\nservices:\n  web:\n    image: " + privateImage + "\n"}))
	if err != nil {
		t.Fatal(err)
	}
	planned := res.Msg.GetChange()
	// The same name, saved again: another revision of the login.
	apply(t, c, propose(t, c, "Save it again", loginEdit("ghcr-ci", &resourcev1.RegistryLogin{Registry: "ghcr.io", Username: loginUser, Password: loginPassword})))
	_, err = c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: planned.GetId(), Fingerprint: planned.GetPlan().GetFingerprint()}))
	if err == nil {
		done := waitWatch(t, watch(t, c, planned.GetId())).last()
		if done.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatal("a plan applied after the login it pulls with was saved again")
		}
		if !strings.Contains(done.GetError(), "changed since this plan was made") {
			t.Errorf("the plan ended %v: %s", done.GetState(), done.GetError())
		}
	} else if !strings.Contains(err.Error(), "changed since this plan was made") {
		t.Errorf("applying = %v, want the plan stale", err)
	}
}
