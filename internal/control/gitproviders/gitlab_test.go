// SPDX-License-Identifier: AGPL-3.0-only

package gitproviders

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/gitlab"
	"github.com/dockwell/dockwell/internal/gitref"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/gitlabconnection"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

type gitlabFixture struct {
	db     *store.DB
	h      *Handler
	projID string
	envID  string
	now    *time.Time
}

func seedGitLabFixture(t *testing.T, backend store.Backend, glClient *gitlab.Client, conns ...struct {
	name   string
	url    string
	token  string
	prefix string
},
) gitlabFixture {
	t.Helper()
	ctx := t.Context()
	db, _ := storetest.Open(t, backend)
	keeper := secretstest.Keeper(db, resource.NewRegistry().With(gitlabconnection.Type{}))
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	orgID := id.New(id.Organization)
	projID := id.New(id.Project)
	envID := id.New(id.Environment)

	marshal := func(msg proto.Message) []byte {
		raw, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	sealSpec := func(resID, kind string, msg proto.Message) []byte {
		raw := marshal(msg)
		var out []byte
		if err := db.Update(ctx, func(tx *store.Tx) error {
			var err error
			out, err = keeper.Seal(ctx, tx, store.InRevision(resID, kind, 1), raw)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	revs := []store.NewRevision{
		{ID: projID, Kind: "project", Scope: orgID, Name: "shop", Number: 1, Spec: marshal(&resourcev1.Project{}), At: now},
		{ID: envID, Kind: "environment", Scope: projID, Name: "prod", Number: 1, Spec: marshal(&resourcev1.Environment{}), At: now},
	}
	for _, c := range conns {
		cid := id.New("glc")
		spec := sealSpec(cid, string(gitlabconnection.Kind), &resourcev1.GitLabConnection{
			InstanceUrl: c.url,
			Token:       c.token,
			PathPrefix:  c.prefix,
		})
		revs = append(revs, store.NewRevision{
			ID: cid, Kind: string(gitlabconnection.Kind), Scope: orgID, Name: c.name, Number: 1, Spec: spec, At: now,
		})
	}
	if err := db.Revise(ctx, func(tx *store.RevisionTx) error {
		if err := tx.CreateOrganization(ctx, store.Organization{ID: orgID, Name: "Acme", CreatedAt: now}); err != nil {
			return err
		}
		chg := id.New(id.Change)
		if err := tx.CreateChange(ctx, store.Change{
			ID: chg, Summary: "seed", Actor: "usr_1", State: store.ChangeApplied,
			CreatedAt: now, Plan: []byte(`{}`), Fingerprint: "fp-gl",
		}); err != nil {
			return err
		}
		for pos := range revs {
			revs[pos].Change = chg
			revs[pos].Position = pos
			if _, err := tx.AddRevision(ctx, revs[pos]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	h := New(Config{
		DB:     db,
		Keeper: keeper,
		GitLab: glClient,
		Now:    func() time.Time { return now },
	})
	return gitlabFixture{db: db, h: h, projID: projID, envID: envID, now: &now}
}

// TestGitLabProbeOutsideTxGatedAndCached proves N7:
//   - Slow GitLab probes and project listings do NOT hold any store read
//     connection open (db.ReadInUse() == 0 while the network call is blocked).
//   - Outbound GitLab probes are bounded by glGate (gitlabReadsAtOnce=2),
//     returning CodeResourceExhausted with reason "too_many_readings" when busy.
//   - Positive/negative probe answers are cached per normalized repo URL for
//     2 minutes and re-probed after TTL expiry.
func TestGitLabProbeOutsideTxGatedAndCached(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		ctx := t.Context()
		var probeHits atomic.Int32
		probeEntered := make(chan struct{}, 4)
		releaseProbe := make(chan struct{})
		listEntered := make(chan struct{}, 2)
		releaseList := make(chan struct{})

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/info/refs") {
				probeHits.Add(1)
				if strings.Contains(r.URL.Path, "/acme/slow") {
					select {
					case probeEntered <- struct{}{}:
					default:
					}
					<-releaseProbe
				}
				w.Header().Set("WWW-Authenticate", `Basic realm="GitLab"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Path == "/api/v4/projects" {
				select {
				case listEntered <- struct{}{}:
				default:
				}
				<-releaseList
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 1, "path_with_namespace": "acme/multi", "web_url": "https://gitlab.example.com/acme/multi"},
				})
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)

		fx := seedGitLabFixture(t, backend, &gitlab.Client{HTTP: srv.Client(), TestBase: srv.URL},
			struct{ name, url, token, prefix string }{"corp-a", "https://gitlab.example.com", "glpat-a", "acme"},
			struct{ name, url, token, prefix string }{"corp-b", "https://gitlab.example.com", "glpat-b", ""},
		)

		// 1. Start 2 slow probes to fill glGate (size 2).
		type pinRes struct {
			login string
			err   error
		}
		res1 := make(chan pinRes, 1)
		res2 := make(chan pinRes, 1)
		go func() {
			l, err := fx.h.PinGitLogin(ctx, fx.projID, "https://gitlab.example.com/acme/slow-1", "")
			res1 <- pinRes{l, err}
		}()
		go func() {
			l, err := fx.h.PinGitLogin(ctx, fx.projID, "https://gitlab.example.com/acme/slow-2", "")
			res2 <- pinRes{l, err}
		}()
		for range 2 {
			select {
			case <-probeEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for slow GitLab probes to enter handler")
			}
		}

		// While both slow probes are stalled in outbound HTTP, ZERO store read
		// connections may be held open!
		if inUse := fx.db.ReadInUse(); inUse != 0 {
			close(releaseProbe)
			close(releaseList)
			t.Fatalf("db.ReadInUse() = %d while slow GitLab probes are in flight, want 0 (N7)", inUse)
		}

		// A 3rd concurrent probe for a distinct URL must hit the busy gate with
		// CodeResourceExhausted / "too_many_readings".
		_, busyErr := fx.h.PinGitLogin(ctx, fx.projID, "https://gitlab.example.com/acme/third", "")
		var busyCE *connect.Error
		if !errors.As(busyErr, &busyCE) || busyCE.Code() != connect.CodeResourceExhausted || busyCE.Meta().Get(authapi.ReasonKey) != "too_many_readings" {
			close(releaseProbe)
			close(releaseList)
			t.Fatalf("3rd concurrent probe err = %v, want ResourceExhausted/too_many_readings (N7)", busyErr)
		}

		// Release probes and wait for list disambiguation; verify db.ReadInUse()==0
		// during ListProjects too!
		close(releaseProbe)
		select {
		case <-listEntered:
		case <-time.After(5 * time.Second):
			close(releaseList)
			t.Fatal("timed out waiting for GitLab ListProjects to enter handler")
		}
		if inUse := fx.db.ReadInUse(); inUse != 0 {
			close(releaseList)
			t.Fatalf("db.ReadInUse() = %d while slow GitLab ListProjects is in flight, want 0 (N7)", inUse)
		}
		close(releaseList)

		for idx, ch := range []chan pinRes{res1, res2} {
			r := <-ch
			if r.err != nil || r.login != "gitlab:corp-a" {
				t.Fatalf("slow probe %d = (%q, %v), want (gitlab:corp-a, nil)", idx+1, r.login, r.err)
			}
		}

		// 2. Probe cache hit: asking for the same repo (even with .git / uppercase)
		// must NOT hit the network again within 2 minutes.
		hitsBefore := probeHits.Load()
		got, err := fx.h.PinGitLogin(ctx, fx.projID, "https://GITLAB.EXAMPLE.COM/acme/slow-1.git", "")
		if err != nil || got != "gitlab:corp-a" {
			t.Fatalf("cached PinGitLogin = (%q, %v), want (gitlab:corp-a, nil)", got, err)
		}
		if probeHits.Load() != hitsBefore {
			t.Fatalf("probe hits increased from %d to %d on cached URL (N7)", hitsBefore, probeHits.Load())
		}

		// 3. Advancing past TTL re-probes cleanly.
		*fx.now = fx.now.Add(gitlabProbeCacheTTL + time.Second)
		got, err = fx.h.PinGitLogin(ctx, fx.projID, "https://gitlab.example.com/acme/fast-after-ttl", "")
		if err != nil || got != "gitlab:corp-a" {
			t.Fatalf("post-TTL PinGitLogin = (%q, %v), want (gitlab:corp-a, nil)", got, err)
		}
		if probeHits.Load() <= hitsBefore {
			t.Fatalf("expected fresh probe after TTL expiry; hits stayed %d", probeHits.Load())
		}
	})
}

// TestGitLabProbeUnknownVsPrivateAndAuthErrorHint proves N8:
//   - 200 -> probePublic (not pinned, cached)
//   - 401, 404, 403+WWW-Authenticate -> probePrivate (pinned, cached)
//   - 403 without WWW-Authenticate, 429, 503 -> probeUnknown (not pinned, NOT cached)
//   - When an unpinned private repo then fails Resolve/Branches with
//     ErrAuthRequired, the error preserves ErrAuthRequired AND names the
//     matching GitLab connection that could fetch it.
func TestGitLabProbeUnknownVsPrivateAndAuthErrorHint(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	var statusFor map[string]int
	var authHeaderFor map[string]string
	hits := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		code := statusFor[r.URL.Path]
		if code == 0 {
			code = http.StatusNotFound
		}
		if hdr := authHeaderFor[r.URL.Path]; hdr != "" {
			w.Header().Set("WWW-Authenticate", hdr)
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)

	statusFor = map[string]int{
		"/acme/public/info/refs":        http.StatusOK,
		"/acme/priv-401/info/refs":      http.StatusUnauthorized,
		"/acme/priv-404/info/refs":      http.StatusNotFound,
		"/acme/priv-403-auth/info/refs": http.StatusForbidden,
		"/acme/waf-403/info/refs":       http.StatusForbidden,
		"/acme/rate-429/info/refs":      http.StatusTooManyRequests,
		"/acme/err-503/info/refs":       http.StatusServiceUnavailable,
	}
	authHeaderFor = map[string]string{
		"/acme/priv-403-auth/info/refs": `Basic realm="GitLab"`,
	}

	fx := seedGitLabFixture(t, store.SQLite, &gitlab.Client{HTTP: srv.Client(), TestBase: srv.URL},
		struct{ name, url, token, prefix string }{"corp", "https://gitlab.example.com", "glpat-1", "acme"},
	)

	cases := []struct {
		repo       string
		wantLogin  string
		wantCached bool
	}{
		{"acme/public", "", true},
		{"acme/priv-401", "gitlab:corp", true},
		{"acme/priv-404", "gitlab:corp", true},
		{"acme/priv-403-auth", "gitlab:corp", true},
		{"acme/waf-403", "", false},
		{"acme/rate-429", "", false},
		{"acme/err-503", "", false},
	}
	for _, tc := range cases {
		u := "https://gitlab.example.com/" + tc.repo
		got, err := fx.h.PinGitLogin(ctx, fx.projID, u, "")
		if err != nil || got != tc.wantLogin {
			t.Fatalf("PinGitLogin(%s) = (%q, %v), want (%q, nil) (N8)", tc.repo, got, err, tc.wantLogin)
		}
		before := hits["/"+tc.repo+"/info/refs"]
		got2, err := fx.h.PinGitLogin(ctx, fx.projID, u, "")
		if err != nil || got2 != tc.wantLogin {
			t.Fatalf("second PinGitLogin(%s) = (%q, %v), want (%q, nil)", tc.repo, got2, err, tc.wantLogin)
		}
		after := hits["/"+tc.repo+"/info/refs"]
		if tc.wantCached && after != before {
			t.Fatalf("%s should be cached, but probe hits grew %d -> %d", tc.repo, before, after)
		}
		if !tc.wantCached && after != before+1 {
			t.Fatalf("%s is probeUnknown and must NOT be cached, but probe hits stayed %d -> %d (N8)", tc.repo, before, after)
		}
	}

	// Now simulate the N8 build/branch failure after an unknown probe left
	// git_login empty: Sources.Resolve and Sources.Branches hit 401 and must
	// name GitLab connection "corp" while keeping errors.Is(..., ErrAuthRequired).
	statusFor["/acme/err-503/info/refs"] = http.StatusUnauthorized
	rewriter := &rewriteTestSources{
		base: &gitref.Resolver{Client: srv.Client()},
		from: "https://gitlab.example.com",
		to:   srv.URL,
	}
	sources := &Sources{Base: rewriter}
	sources.SetApps(fx.h)

	scopedCtx := revision.WithGitScope(ctx, fx.envID)
	_, _, rerr := sources.Resolve(scopedCtx, "https://gitlab.example.com/acme/err-503", "main")
	if !errors.Is(rerr, gitref.ErrAuthRequired) {
		t.Fatalf("Sources.Resolve err = %v, want errors.Is(..., gitref.ErrAuthRequired)", rerr)
	}
	if !strings.Contains(rerr.Error(), "corp") || !strings.Contains(rerr.Error(), "gitlab:corp") {
		t.Fatalf("Sources.Resolve err = %q, want it to name GitLab connection corp (N8)", rerr.Error())
	}

	_, _, berr := sources.Branches(revision.WithGitScope(ctx, fx.projID), "https://gitlab.example.com/acme/err-503")
	if !errors.Is(berr, gitref.ErrAuthRequired) {
		t.Fatalf("Sources.Branches err = %v, want errors.Is(..., gitref.ErrAuthRequired)", berr)
	}
	if !strings.Contains(berr.Error(), "corp") {
		t.Fatalf("Sources.Branches err = %q, want it to name GitLab connection corp (N8)", berr.Error())
	}
}

type rewriteTestSources struct {
	base *gitref.Resolver
	from string
	to   string
}

func (r *rewriteTestSources) Resolve(ctx context.Context, u, ref string) (string, string, error) {
	return r.base.Resolve(ctx, strings.Replace(u, r.from, r.to, 1), ref)
}

func (r *rewriteTestSources) Branches(ctx context.Context, u string) (string, []string, error) {
	return r.base.Branches(ctx, strings.Replace(u, r.from, r.to, 1))
}

// TestGitLabProbeProductionNetguardBlocksLoopback proves N10: with the
// production GitLab client (HTTP == nil), probeAnonymousGitLab uses netguard
// and refuses loopback targets without ever dialing them.
func TestGitLabProbeProductionNetguardBlocksLoopback(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	h := New(Config{GitLab: &gitlab.Client{}}) // HTTP == nil -> production netguard path
	outcome, err := h.probeAnonymousGitLab(t.Context(), srv.URL+"/acme/secret")
	if err != nil {
		t.Fatalf("probeAnonymousGitLab(loopback) err = %v, want nil", err)
	}
	if outcome != probeUnknown {
		t.Fatalf("probeAnonymousGitLab(loopback) outcome = %v, want probeUnknown (N10)", outcome)
	}
	if hits.Load() != 0 {
		t.Fatalf("loopback server received %d probe hits; want 0 (netguard must block loopback, N10)", hits.Load())
	}
}

// TestNoteGitLabStatusDoesNotGuessListProjects proves N10: NoteGitLabStatus for
// CONNECTED with no prior check does not guess ListProjects=true, and with a
// prior check preserves ListProjects=false while updating PostStatuses=true.
func TestNoteGitLabStatusDoesNotGuessListProjects(t *testing.T) {
	t.Parallel()
	h := New(Config{})

	// 1. No saved check for rev 1 -> capabilities stays nil (does not guess ListProjects=true).
	h.NoteGitLabStatus("glc_1", 1, dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED, "")
	if got := h.gitlabChecks["glc_1"].capabilities; got != nil {
		t.Fatalf("capabilities with no prior check = %+v, want nil (N10)", got)
	}

	// 2. Prior check had ListProjects=false -> NoteGitLabStatus(CONNECTED) keeps ListProjects=false.
	h.noteGitLabCheck("glc_2", 1, dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED, "",
		&dockwellv1.GitLabCapabilities{BuildPrivate: true, ListProjects: false, PostStatuses: false})
	h.NoteGitLabStatus("glc_2", 1, dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED, "")
	got := h.gitlabChecks["glc_2"].capabilities
	if got == nil || !got.GetBuildPrivate() || got.GetListProjects() || !got.GetPostStatuses() {
		t.Fatalf("capabilities after NoteGitLabStatus = %+v, want BuildPrivate=true ListProjects=false PostStatuses=true (N10)", got)
	}
}
