// SPDX-License-Identifier: AGPL-3.0-only

package registrylogins

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

const ecrRegistry = "123456789012.dkr.ecr.eu-west-1.amazonaws.com"

func saved(name, registry string, projects ...string) Saved {
	return Saved{ID: "rgl_" + name, Name: name, Spec: &resourcev1.RegistryLogin{Registry: registry, Username: "ci", Password: "token-" + name, Projects: projects}}
}

// A pull uses the login it names, if its project may; with none named, the only
// one for its registry its project may use; and with several, none, as the app
// picks. A login it may not use reads as one that isn't there.
func TestAPullPicksItsLogin(t *testing.T) {
	all := []Saved{
		saved("ghcr-ci", "ghcr.io"),
		saved("ghcr-shop", "ghcr.io", "prj_shop"),
		saved("hub", "docker.io", "prj_shop"),
		saved("lab", "registry.example.com", "prj_lab"),
	}
	for _, c := range []struct {
		project, host, named, want string
		code                       connect.Code
	}{
		{project: "prj_shop", host: "docker.io", want: "hub"},
		{project: "prj_blog", host: "docker.io"},
		{project: "prj_blog", host: "ghcr.io", want: "ghcr-ci"},
		// Two for ghcr.io: the app names one.
		{project: "prj_shop", host: "ghcr.io"},
		{project: "prj_shop", host: "ghcr.io", named: "ghcr-shop", want: "ghcr-shop"},
		{project: "prj_blog", host: "ghcr.io", named: "ghcr-shop", code: connect.CodeNotFound},
		{project: "prj_shop", host: "ghcr.io", named: "nope", code: connect.CodeNotFound},
		{project: "prj_shop", host: "ghcr.io", named: "hub", code: connect.CodeFailedPrecondition},
		{project: "prj_lab", host: "registry.example.com", want: "lab"},
	} {
		got, ok, err := pick(all, c.project, c.host, c.named)
		switch {
		case c.code != 0:
			if connect.CodeOf(err) != c.code {
				t.Errorf("%+v: %v, want %v", c, err, c.code)
			}
			if err != nil && strings.Contains(err.Error(), "prj_") {
				t.Errorf("%+v: a refusal says which projects may use it: %v", c, err)
			}
		case err != nil:
			t.Errorf("%+v: %v", c, err)
		case c.want == "" && ok:
			t.Errorf("%+v picked %s, want none", c, got.Name)
		case c.want != "" && (!ok || got.Name != c.want):
			t.Errorf("%+v picked %q, want %q", c, got.Name, c.want)
		}
	}
}

// What a service runs, as the kinds that pull name it: none for one Dockwell builds.
func TestOfFindsTheImageAPullNames(t *testing.T) {
	c := &resourcev1.Container{Image: "ghcr.io/acme/web:1", RegistryLogin: "ghcr-ci"}
	if image, named, ok := Of(c); !ok || image != "ghcr.io/acme/web:1" || named != "ghcr-ci" {
		t.Errorf("Of(container) = %q, %q, %v", image, named, ok)
	}
	s := &resourcev1.Service{Runs: &resourcev1.Service_App{App: c}}
	if image, named, ok := Of(s); !ok || image != "ghcr.io/acme/web:1" || named != "ghcr-ci" {
		t.Errorf("Of(service) = %q, %q, %v", image, named, ok)
	}
	s.Build = &resourcev1.ImageBuild{}
	if _, _, ok := Of(s); ok {
		t.Error("an app Dockwell builds pulls its image")
	}
	if _, _, ok := Of(&resourcev1.Volume{}); ok {
		t.Error("a volume pulls an image")
	}
}

// fakeECR answers GetAuthorizationToken for a key, as ECR does, and counts the
// tokens it gave.
func fakeECR(t *testing.T, registry string) (*ECR, *atomic.Int32) {
	t.Helper()
	var given atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Target") != "AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken" {
			t.Errorf("ECR was asked %q", r.Header.Get("X-Amz-Target"))
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/") || !strings.Contains(auth, "/eu-west-1/ecr/aws4_request") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		given.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"authorizationData": []map[string]any{{
			"authorizationToken": base64.StdEncoding.EncodeToString([]byte("AWS:ecr-token")),
			"expiresAt":          float64(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC).Unix()),
			"proxyEndpoint":      "https://" + registry,
		}}})
	}))
	t.Cleanup(srv.Close)
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	e := NewECR()
	e.Client, e.Endpoint = srv.Client(), func(string) string { return srv.URL }
	e.Now = func() time.Time { return now }
	return e, &given
}

// ECR's token is got with the key, kept until an hour before it ends, then got
// again, and signs in to the key's own account's registry alone.
func TestECRsTokenIsRefreshed(t *testing.T) {
	e, given := fakeECR(t, ecrRegistry)
	key := &resourcev1.RegistryLoginEcr{AccessKey: "AKIAEXAMPLE", SecretKey: "secret", Region: "eu-west-1"}
	clock := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return clock }
	for range 2 {
		token, err := e.Token(t.Context(), ecrRegistry, key)
		if err != nil || token != "ecr-token" {
			t.Fatalf("Token = %q, %v", token, err)
		}
	}
	if given.Load() != 1 {
		t.Errorf("ECR gave %d tokens for two pulls, want one kept", given.Load())
	}
	clock = clock.Add(11*time.Hour + time.Minute)
	if _, err := e.Token(t.Context(), ecrRegistry, key); err != nil || given.Load() != 2 {
		t.Errorf("an hour before its end, the token wasn't got again: %d, %v", given.Load(), err)
	}
	if _, err := e.Token(t.Context(), "210987654321.dkr.ecr.eu-west-1.amazonaws.com", key); err == nil {
		t.Error("a key's token was taken for another account's registry")
	}
	wrong := &resourcev1.RegistryLoginEcr{AccessKey: "AKIAWRONG", SecretKey: "secret", Region: "eu-west-1"}
	if _, err := e.Token(t.Context(), ecrRegistry, wrong); !IsECRRefused(err) {
		t.Errorf("a key AWS refused = %v, want refused", err)
	}
	host, password, err := e.Registry(t.Context(), "eu-west-1", key)
	if err != nil || host != ecrRegistry || password != "ecr-token" {
		t.Errorf("Registry = %q, %q, %v", host, password, err)
	}
	if err != nil && strings.Contains(err.Error(), "secret") {
		t.Errorf("an error holds the key's secret: %v", err)
	}
}

// A registry ECR names outside the region asked is no answer.
func TestECRAnswersForItsRegionAlone(t *testing.T) {
	e, _ := fakeECR(t, "123456789012.dkr.ecr.us-east-1.amazonaws.com")
	key := &resourcev1.RegistryLoginEcr{AccessKey: "AKIAEXAMPLE", SecretKey: "secret", Region: "eu-west-1"}
	if _, _, err := e.Registry(t.Context(), "eu-west-1", key); err == nil {
		t.Error("a registry in another region was taken")
	}
	if _, err := e.Token(t.Context(), "registry.example.com", key); err == nil || errors.Is(err, errECRRefused) {
		t.Errorf("a token for a registry that isn't ECR's = %v", err)
	}
}

// ECR's API is never asked in a region that isn't a region's name. (L2.)
func TestECRIsAskedInARegionAlone(t *testing.T) {
	e, given := fakeECR(t, ecrRegistry)
	e.Endpoint = func(region string) string { return "https://api.ecr." + region + ".amazonaws.com" }
	if _, _, err := e.Registry(t.Context(), "eu-west-1.attacker.example/x", &resourcev1.RegistryLoginEcr{AccessKey: "AKIAEXAMPLE", SecretKey: "secret"}); err == nil {
		t.Error("ECR was asked in a region that isn't one")
	}
	if given.Load() != 0 {
		t.Error("a token was got")
	}
}

// A build gets the logins it names, and no other: naming none gives it none, as its
// Dockerfile can read any image a login it's given can. One the project may not use,
// two for one registry, or one for a registry on the loopback, which a build's steps
// share, refuses the plan. (Security review of #591, H1 and H2.)
func TestABuildGetsTheLoginsItNamesAlone(t *testing.T) {
	all := []Saved{
		saved("ghcr-ci", "ghcr.io"),
		saved("ghcr-shop", "ghcr.io", "prj_shop"),
		saved("hub", "docker.io", "prj_shop"),
		saved("lab", "registry.example.com", "prj_lab"),
		saved("local", "localhost:5000"),
		saved("loop4", "127.0.0.1:5000"),
		saved("loop6", "[::1]:5000"),
	}
	names := func(s []Saved) string {
		var out []string
		for _, x := range s {
			out = append(out, x.Name)
		}
		return strings.Join(out, ",")
	}
	if got, err := pickBuild(all, "prj_shop", nil, false); err != nil || len(got) != 0 {
		t.Errorf("naming none, shop builds with %v, %v", names(got), err)
	}
	if got, err := pickBuild(all, "prj_shop", []string{"ghcr-shop", "hub"}, false); err != nil || names(got) != "ghcr-shop,hub" {
		t.Errorf("naming two, shop builds with %v, %v", names(got), err)
	}
	if _, err := pickBuild(all, "prj_shop", []string{"ghcr-ci", "ghcr-shop"}, false); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("naming two logins for one registry = %v", err)
	}
	if _, err := pickBuild(all, "prj_blog", []string{"lab"}, false); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("naming another project's login = %v", err)
	}
	for _, loop := range []string{"local", "loop4", "loop6"} {
		_, err := pickBuild(all, "prj_shop", []string{loop}, false)
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("naming %s, on the loopback = %v", loop, err)
		}
	}
	if _, err := pickBuild(all, "prj_blog", []string{"hub"}, true); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("applying a login the project may no longer use = %v", err)
	}
}

// An apply builds with the revision of each login its plan saw, and refuses one
// saved again since. (Security review of #591, L4.)
func TestABuildsApplyHoldsToThePlannedRevisions(t *testing.T) {
	ghcr := saved("ghcr-ci", "ghcr.io")
	ghcr.Revision = 3
	if err := samePlanned([]Saved{ghcr}, []string{"ghcr-ci"}, []string{"rgl_ghcr-ci@3"}); err != nil {
		t.Errorf("the planned revision = %v", err)
	}
	for what, versions := range map[string][]string{"another revision": {"rgl_ghcr-ci@2"}, "none": nil, "another login's": {"rgl_other@3"}} {
		if err := samePlanned([]Saved{ghcr}, []string{"ghcr-ci"}, versions); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("%s = %v", what, err)
		}
	}
}
