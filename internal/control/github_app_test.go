// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// fakeGitHub is GitHub's API as connecting an App needs it: it makes App 42 from
// a manifest's code, takes a JWT any of the App's keys signed, has installation 7
// on acme, and keeps what was set and removed.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex
	// keys are the App's keys, as GitHub holds them: the first it made, and any
	// generated on its page since.
	keys []*rsa.PrivateKey
	// pem is the first key, as the conversion gives it.
	pem string
	// codes are the manifests' codes not yet traded.
	codes map[string]bool
	// secrets are the webhook secrets set, in order; uninstalled the installations
	// removed.
	secrets     []string
	uninstalled []int64
	// deliveries is the App's latest deliveries, as JSON; head the commit a
	// branch names now, for its commits API; heads how often that was asked;
	// retryAfter has it refuse with a secondary limit, for that many seconds.
	deliveries string
	head       func() string
	heads      int
	retryAfter int
	// gone has GitHub say installation 7 was removed.
	gone bool
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGitHub{
		t: t, keys: []*rsa.PrivateKey{k}, codes: map[string]bool{"code-1": true},
		pem: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// newKey is a key generated on the App's page, as a .pem file.
func (f *fakeGitHub) newKey() string {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.keys = append(f.keys, k)
	f.mu.Unlock()
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

func (f *fakeGitHub) asApp(r *http.Request) bool {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(jwt, ".")
	if !ok || len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, sum[:], sig) == nil {
			return true
		}
	}
	return false
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	no := func(code int, msg string) {
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"message":%q}`, msg)
	}
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/app-manifests/") && strings.HasSuffix(path, "/conversions"):
		code := strings.TrimSuffix(strings.TrimPrefix(path, "/app-manifests/"), "/conversions")
		f.mu.Lock()
		ok := f.codes[code]
		delete(f.codes, code)
		f.mu.Unlock()
		if !ok {
			no(404, "Not Found")
			return
		}
		b, _ := json.Marshal(map[string]any{
			"id": 42, "slug": "dockwell-deploy", "client_id": "Iv1.abc", "client_secret": "never-kept",
			"webhook_secret": "first-webhook-secret", "pem": f.pem, "owner": map[string]string{"login": "acme", "type": "Organization"},
		})
		_, _ = w.Write(b)
	case strings.HasPrefix(path, "/repos/") && strings.Contains(path, "/commits/"):
		if r.Header.Get("Authorization") != "Bearer ghs_listing" || r.Header.Get("Accept") != "application/vnd.github.sha" {
			no(401, "Bad credentials")
			return
		}
		f.mu.Lock()
		f.heads++
		retry, head := f.retryAfter, f.head
		f.mu.Unlock()
		if retry > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			no(403, "You have exceeded a secondary rate limit")
			return
		}
		sha := head()
		w.Header().Set("Etag", `"`+sha+`"`)
		w.Header().Set("X-Ratelimit-Limit", "5000")
		w.Header().Set("X-Ratelimit-Remaining", "4999")
		if r.Header.Get("If-None-Match") == `"`+sha+`"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = fmt.Fprint(w, sha)
	case !strings.HasPrefix(path, "/installation/") && !f.asApp(r):
		no(401, "A JSON web token could not be decoded")
	case path == "/app/hook/deliveries":
		f.mu.Lock()
		d := f.deliveries
		f.mu.Unlock()
		if d == "" {
			d = "[]"
		}
		_, _ = fmt.Fprint(w, d)
	case path == "/app":
		_, _ = fmt.Fprint(w, `{"id":42,"slug":"dockwell-deploy"}`)
	case path == "/app/installations/7" && r.Method == http.MethodGet:
		f.mu.Lock()
		gone := f.gone
		f.mu.Unlock()
		if gone {
			no(404, "Not Found")
			return
		}
		_, _ = fmt.Fprint(w, `{"id":7,"app_id":42,"account":{"login":"acme","type":"Organization"},"repository_selection":"selected"}`)
	case path == "/app/installations/7" && r.Method == http.MethodDelete:
		f.mu.Lock()
		f.uninstalled = append(f.uninstalled, 7)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/app/installations/") && strings.HasSuffix(path, "/access_tokens"):
		_, _ = fmt.Fprint(w, `{"token":"ghs_listing","expires_at":"2099-01-01T00:00:00Z"}`)
	case path == "/app/hook/config" && r.Method == http.MethodPatch:
		var body struct{ Secret string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.secrets = append(f.secrets, body.Secret)
		f.mu.Unlock()
		_, _ = fmt.Fprint(w, `{}`)
	case path == "/installation/repositories":
		if r.Header.Get("Authorization") != "Bearer ghs_listing" {
			no(401, "Bad credentials")
			return
		}
		_, _ = fmt.Fprint(w, `{"total_count":2,"repositories":[{"id":5,"full_name":"acme/web","private":true,"default_branch":"main"},{"id":6,"full_name":"acme/api","default_branch":"trunk"}]}`)
	default:
		no(404, "Not Found")
	}
}

// An admin connects GitHub in one click and a passkey: the App GitHub makes is kept
// as a change, its key and webhook secret in no answer, plan or event; an
// installation GitHub sends the browser back with is checked as the App's, and its
// repositories are what a project's apps may build from, until a plan narrows it.
// A new secret is set on GitHub, a new key is kept once GitHub takes it, and
// removing the App uninstalls it.
func TestAnAdminConnectsGitHubAsAnApp(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) {
		c.GitHub = GitHubConfig{API: gh.srv.URL, Client: gh.srv.Client()}
	})
	b := signedIn(t, f)
	ctx := t.Context()
	gp := dockwellv1connect.NewGitProviderServiceClient(b, b.origin, connect.WithProtoJSON())

	// Start asks for a passkey, then gives the form GitHub's page takes.
	if _, err := gp.StartGitHubApp(ctx, connect.NewRequest(&dockwellv1.StartGitHubAppRequest{Organization: "acme"})); reasonOf(err) != "needs_step_up" {
		t.Errorf("starting with no passkey = %v", err)
	}
	started, err := gp.StartGitHubApp(ctx, connect.NewRequest(&dockwellv1.StartGitHubAppRequest{
		Organization: "acme", Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CONNECT_GITHUB_APP, ""),
	}))
	if err != nil {
		t.Fatal(err)
	}
	action, _ := url.Parse(started.Msg.GetActionUrl())
	state := action.Query().Get("state")
	if action.Path != "/organizations/acme/settings/apps/new" || len(state) < 40 {
		t.Fatalf("the form goes to %s", started.Msg.GetActionUrl())
	}
	var manifest struct {
		Hook        struct{ URL string } `json:"hook_attributes"`
		Permissions map[string]string    `json:"default_permissions"`
	}
	if err := json.Unmarshal([]byte(started.Msg.GetManifest()), &manifest); err != nil || !strings.Contains(manifest.Hook.URL, "/hooks/github/ghw_") ||
		len(manifest.Permissions) != 2 {
		t.Errorf("the manifest is %s, %v", started.Msg.GetManifest(), err)
	}

	// GitHub sends the browser back with a code: the App is kept, once.
	if _, err := gp.FinishGitHubApp(ctx, connect.NewRequest(&dockwellv1.FinishGitHubAppRequest{Code: "code-1", State: "another"})); reasonOf(err) != "github_state" {
		t.Errorf("finishing with another state = %v", err)
	}
	finished, err := gp.FinishGitHubApp(ctx, connect.NewRequest(&dockwellv1.FinishGitHubAppRequest{Code: "code-1", State: state}))
	if err != nil {
		t.Fatal(err)
	}
	app := finished.Msg.GetApp()
	if app.GetName() != "dockwell-deploy" || app.GetAppId() != 42 || !app.GetOwnerIsOrganization() ||
		finished.Msg.GetInstallUrl() != "https://github.com/apps/dockwell-deploy/installations/new" ||
		!strings.HasPrefix(app.GetKeyFingerprint(), "SHA256:") {
		t.Fatalf("the App is %v, installed at %s", app, finished.Msg.GetInstallUrl())
	}
	if _, err := gp.FinishGitHubApp(ctx, connect.NewRequest(&dockwellv1.FinishGitHubAppRequest{Code: "code-1", State: state})); reasonOf(err) != "github_state" {
		t.Errorf("finishing twice = %v", err)
	}
	c := changeClient(b, b.origin)
	kept, err := c.GetChange(ctx, connect.NewRequest(&dockwellv1.GetChangeRequest{Id: finished.Msg.GetChangeId()}))
	if err != nil || kept.Msg.GetChange().GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the change that keeps it = %v, %v", kept, err)
	}
	events, err := dockwellv1connect.NewEventServiceClient(b, b.origin, connect.WithProtoJSON()).ListEvents(ctx, connect.NewRequest(&dockwellv1.ListEventsRequest{PageSize: 100}))
	if err != nil {
		t.Fatal(err)
	}
	for _, said := range []string{protojson.Format(kept.Msg), protojson.Format(events.Msg), protojson.Format(finished.Msg)} {
		if strings.Contains(said, "first-webhook-secret") || strings.Contains(said, "PRIVATE KEY") || strings.Contains(said, "never-kept") {
			t.Fatal("an answer holds the App's key or a secret")
		}
	}

	// The installation GitHub sends the browser back with is checked as the App's.
	if _, err := gp.AddGitHubInstallation(ctx, connect.NewRequest(&dockwellv1.AddGitHubInstallationRequest{Id: app.GetId(), InstallationId: 8})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("another App's installation = %v", err)
	}
	added, err := gp.AddGitHubInstallation(ctx, connect.NewRequest(&dockwellv1.AddGitHubInstallationRequest{Id: app.GetId(), InstallationId: 7}))
	if err != nil {
		t.Fatal(err)
	}
	if in := added.Msg.GetInstallation(); in.GetAccount() != "acme" || in.GetRepositoryCount() != 2 || !in.GetEveryProject() {
		t.Errorf("the installation is %v", in)
	}
	var project, other string
	if err := f.store.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if len(tree.Projects) > 0 {
			project = tree.Projects[0].ID
		}
		return err
	}); err != nil || project == "" {
		t.Fatalf("the install's project = %q, %v", project, err)
	}
	other = "prj_01JD6Z3R8M9QF2K7T4XW5BNCZZ"
	repos, err := gp.ListGitRepositories(ctx, connect.NewRequest(&dockwellv1.ListGitRepositoriesRequest{Project: project}))
	if err != nil || len(repos.Msg.GetRepositories()) != 2 || repos.Msg.GetRepositories()[1].GetFullName() != "acme/web" ||
		!repos.Msg.GetRepositories()[1].GetPrivate() || repos.Msg.GetRepositories()[1].GetUrl() != "https://github.com/acme/web" {
		t.Fatalf("the project's repositories = %v, %v", repos, err)
	}

	// A plan narrows the installation to another project, and this one sees none.
	narrow, err := c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Narrow acme", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "github.app", Name: "dockwell-deploy"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_GithubApp{GithubApp: &resourcev1.GitHubApp{
			Reaches: []*resourcev1.GitHubInstallationReach{{InstallationId: 7, Projects: []string{other}}},
		}}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := apply(t, c, narrow.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("narrowing ended %v: %s", got.GetState(), got.GetError())
	}
	if repos, err := gp.ListGitRepositories(ctx, connect.NewRequest(&dockwellv1.ListGitRepositoriesRequest{Project: project})); err != nil || len(repos.Msg.GetRepositories()) != 0 {
		t.Errorf("a narrowed installation still gives %v, %v", repos, err)
	}

	// A new webhook secret is set on GitHub, as the App.
	if _, err := gp.RotateGitHubWebhookSecret(ctx, connect.NewRequest(&dockwellv1.RotateGitHubWebhookSecretRequest{
		Id: app.GetId(), Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GITHUB_APP, app.GetId()),
	})); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	secrets := append([]string(nil), gh.secrets...)
	gh.mu.Unlock()
	if len(secrets) != 1 || len(secrets[0]) != 64 {
		t.Errorf("GitHub was given the secrets %d", len(secrets))
	}

	// A key GitHub doesn't take is refused; one made on the App's page is kept.
	stranger, _ := rsa.GenerateKey(rand.Reader, 2048)
	strangerPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(stranger)}))
	if _, err := gp.ReplaceGitHubPrivateKey(ctx, connect.NewRequest(&dockwellv1.ReplaceGitHubPrivateKeyRequest{
		Id: app.GetId(), PrivateKey: strangerPEM, Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GITHUB_APP, app.GetId()),
	})); reasonOf(err) != "github_key_refused" {
		t.Errorf("a key GitHub doesn't take = %v", err)
	}
	replaced, err := gp.ReplaceGitHubPrivateKey(ctx, connect.NewRequest(&dockwellv1.ReplaceGitHubPrivateKeyRequest{
		Id: app.GetId(), PrivateKey: gh.newKey(), Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GITHUB_APP, app.GetId()),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if r := replaced.Msg; r.GetOldFingerprint() != app.GetKeyFingerprint() || r.GetFingerprint() == r.GetOldFingerprint() ||
		r.GetKeysUrl() != "https://github.com/organizations/acme/settings/apps/dockwell-deploy" {
		t.Errorf("replacing the key said %v", r)
	}
	listed, err := gp.ListGitProviders(ctx, connect.NewRequest(&dockwellv1.ListGitProvidersRequest{}))
	if err != nil || len(listed.Msg.GetApps()) != 1 || listed.Msg.GetApps()[0].GetKeyFingerprint() != replaced.Msg.GetFingerprint() {
		t.Fatalf("the Apps listed = %v, %v", listed, err)
	}
	if in := listed.Msg.GetApps()[0].GetInstallations(); len(in) != 1 || in[0].GetEveryProject() || len(in[0].GetProjects()) != 1 {
		t.Errorf("the narrowed installation lists as %v", in)
	}

	// Removing it uninstalls it, as the App signs with its new key, and forgets it.
	removed, err := gp.RemoveGitHubApp(ctx, connect.NewRequest(&dockwellv1.RemoveGitHubAppRequest{
		Id: app.GetId(), Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_REMOVE_GITHUB_APP, app.GetId()),
	}))
	if err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	uninstalled := append([]int64(nil), gh.uninstalled...)
	gh.mu.Unlock()
	if removed.Msg.GetUninstalled() != 1 || fmt.Sprint(uninstalled) != "[7]" ||
		removed.Msg.GetDeleteUrl() != "https://github.com/organizations/acme/settings/apps/dockwell-deploy/advanced" {
		t.Errorf("removing it said %v, and GitHub uninstalled %v", removed.Msg, uninstalled)
	}
	if listed, err := gp.ListGitProviders(ctx, connect.NewRequest(&dockwellv1.ListGitProvidersRequest{})); err != nil || len(listed.Msg.GetApps()) != 0 {
		t.Errorf("after removing it, the Apps listed = %v, %v", listed, err)
	}
}
