// SPDX-License-Identifier: AGPL-3.0-only

package gitproviders

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/githubapp"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// N12: GitHubPermissionsURL points at the App whose installation actually
// reaches serviceID and repoURL, never falling back to a different App.
func TestGitHubPermissionsURLNoFallbackToWrongApp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, _ := storetest.Open(t, store.SQLite)
	keeper := secretstest.Keeper(db, resource.NewRegistry(&service.Driver{}).With(githubapp.Type{}))
	now := time.Now().UTC().Truncate(time.Millisecond)

	orgID := id.New(id.Organization)
	projID := id.New(id.Project)
	envID := id.New(id.Environment)
	svcID := id.New(id.Service)
	appMissingID := id.New("gha")
	appHealthyID := id.New("gha")

	sealSpec := func(resID, kind string, msg proto.Message) []byte {
		raw, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
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

	marshal := func(msg proto.Message) []byte {
		raw, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	projSpec := marshal(&resourcev1.Project{})
	envSpec := marshal(&resourcev1.Environment{})
	svcSpec := sealSpec(svcID, "service", &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.26"}}})
	appMissingSpec := sealSpec(appMissingID, string(githubapp.Kind), &resourcev1.GitHubApp{
		AppId: 101, Slug: "app-missing", ClientId: "cid1", PrivateKey: "pk1", WebhookSecret: "ws1",
	})
	appHealthySpec := sealSpec(appHealthyID, string(githubapp.Kind), &resourcev1.GitHubApp{
		AppId: 202, Slug: "app-healthy", ClientId: "cid2", PrivateKey: "pk2", WebhookSecret: "ws2",
	})

	if err := db.Revise(ctx, func(tx *store.RevisionTx) error {
		if err := tx.CreateOrganization(ctx, store.Organization{ID: orgID, Name: "Acme", CreatedAt: now}); err != nil {
			return err
		}
		chg := id.New(id.Change)
		if err := tx.CreateChange(ctx, store.Change{
			ID: chg, Summary: "seed", Actor: "usr_1", State: store.ChangeApplied,
			CreatedAt: now, Plan: []byte(`{}`), Fingerprint: "fp1",
		}); err != nil {
			return err
		}
		for pos, n := range []store.NewRevision{
			{ID: projID, Kind: "project", Scope: orgID, Name: "shop", Number: 1, Spec: projSpec, Change: chg, At: now},
			{ID: envID, Kind: "environment", Scope: projID, Name: "staging", Number: 1, Spec: envSpec, Change: chg, At: now},
			{ID: svcID, Kind: "service", Scope: envID, Name: "web", Number: 1, Spec: svcSpec, Change: chg, At: now},
			{ID: appMissingID, Kind: string(githubapp.Kind), Scope: orgID, Name: "app-missing", Number: 1, Spec: appMissingSpec, Change: chg, At: now},
			{ID: appHealthyID, Kind: string(githubapp.Kind), Scope: orgID, Name: "app-healthy", Number: 1, Spec: appHealthySpec, Change: chg, At: now},
		} {
			n.Position = pos
			if _, err := tx.AddRevision(ctx, n); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.Update(ctx, func(tx *store.Tx) error {
		// App 1 (missing checks) is installed on org1/other, NOT acme/web.
		if err := tx.SetGitHubInstallation(ctx, store.GitHubInstallation{
			ID: 1, AppID: appMissingID, Account: "org1", Organization: true,
			Permissions: map[string]string{"contents": "read"},
			Repositories: []store.GitHubRepository{
				{ID: 11, FullName: "org1/other"},
			},
			UpdatedAt: now,
		}); err != nil {
			return err
		}
		// App 2 (has checks: write) reaches acme/web.
		return tx.SetGitHubInstallation(ctx, store.GitHubInstallation{
			ID: 2, AppID: appHealthyID, Account: "acme", Organization: true,
			Permissions: map[string]string{"contents": "read", "checks": "write"},
			Repositories: []store.GitHubRepository{
				{ID: 22, FullName: "acme/web"},
			},
			UpdatedAt: now,
		})
	}); err != nil {
		t.Fatal(err)
	}

	h := New(Config{DB: db, Keeper: keeper, Web: "https://github.com"})
	// For acme/web (whose App has checks: write), must NOT fall back to app-missing's permissions URL!
	if got := h.GitHubPermissionsURL(ctx, svcID, "https://github.com/acme/web"); got != "" {
		t.Fatalf("GitHubPermissionsURL(acme/web) = %q, want empty (must not fall back to unrelated App)", got)
	}
	// For org1/other (whose App actually lacks checks: write), returns app-missing's permissions URL.
	if got := h.GitHubPermissionsURL(ctx, svcID, "https://github.com/org1/other"); got != "https://github.com/settings/apps/app-missing/permissions" {
		t.Fatalf("GitHubPermissionsURL(org1/other) = %q, want app-missing permissions URL", got)
	}
}

// N6: When an app builds from Git, a developer may bind only its own Git source
// (not even an unclaimed repo); when an app doesn't build from Git, a developer
// may not bind a repository used as another project's Git source; an install
// admin may override both.
func TestValidateDeployRepositoryN6GitSourceAndCrossProjectBuild(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		ctx := t.Context()
		db, _ := storetest.Open(t, backend)
		keeper := secretstest.Keeper(db, resource.NewRegistry(&service.Driver{}).With(githubapp.Type{}))
		now := time.Now().UTC().Truncate(time.Millisecond)

		orgID := id.New(id.Organization)
		projA, envA, svcGitA, svcImgA := id.New(id.Project), id.New(id.Environment), id.New(id.Service), id.New(id.Service)
		projB, envB, svcGitB := id.New(id.Project), id.New(id.Environment), id.New(id.Service)
		appID := id.New("gha")

		sealSpec := func(resID, kind string, msg proto.Message) []byte {
			raw, err := proto.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
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

		gitSvc := func(repo string) *resourcev1.Service {
			return &resourcev1.Service{
				Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "built:1"}},
				Build: &resourcev1.ImageBuild{
					Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: repo, Ref: "main"}},
				},
			}
		}

		marshal := func(msg proto.Message) []byte {
			raw, err := proto.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}
		projASpec := marshal(&resourcev1.Project{})
		envASpec := marshal(&resourcev1.Environment{})
		svcGitASpec := sealSpec(svcGitA, "service", gitSvc("https://github.com/acme/alpha"))
		svcImgASpec := sealSpec(svcImgA, "service", &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.26"}}})
		projBSpec := marshal(&resourcev1.Project{})
		envBSpec := marshal(&resourcev1.Environment{})
		svcGitBSpec := sealSpec(svcGitB, "service", gitSvc("https://github.com/acme/beta"))
		appSpec := sealSpec(appID, string(githubapp.Kind), &resourcev1.GitHubApp{AppId: 1, Slug: "gh", ClientId: "c", PrivateKey: "p", WebhookSecret: "w"})

		if err := db.Revise(ctx, func(tx *store.RevisionTx) error {
			if err := tx.CreateOrganization(ctx, store.Organization{ID: orgID, Name: "Acme", CreatedAt: now}); err != nil {
				return err
			}
			chg := id.New(id.Change)
			if err := tx.CreateChange(ctx, store.Change{
				ID: chg, Summary: "seed", Actor: "usr_1", State: store.ChangeApplied,
				CreatedAt: now, Plan: []byte(`{}`), Fingerprint: "fp1",
			}); err != nil {
				return err
			}
			for pos, n := range []store.NewRevision{
				{ID: projA, Kind: "project", Scope: orgID, Name: "alpha", Number: 1, Spec: projASpec, Change: chg, At: now},
				{ID: envA, Kind: "environment", Scope: projA, Name: "prod", Number: 1, Spec: envASpec, Change: chg, At: now},
				{ID: svcGitA, Kind: "service", Scope: envA, Name: "git-a", Number: 1, Spec: svcGitASpec, Change: chg, At: now},
				{ID: svcImgA, Kind: "service", Scope: envA, Name: "img-a", Number: 1, Spec: svcImgASpec, Change: chg, At: now},
				{ID: projB, Kind: "project", Scope: orgID, Name: "beta", Number: 1, Spec: projBSpec, Change: chg, At: now},
				{ID: envB, Kind: "environment", Scope: projB, Name: "prod", Number: 1, Spec: envBSpec, Change: chg, At: now},
				{ID: svcGitB, Kind: "service", Scope: envB, Name: "git-b", Number: 1, Spec: svcGitBSpec, Change: chg, At: now},
				{ID: appID, Kind: string(githubapp.Kind), Scope: orgID, Name: "gh", Number: 1, Spec: appSpec, Change: chg, At: now},
			} {
				n.Position = pos
				if _, err := tx.AddRevision(ctx, n); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.SetGitHubInstallation(ctx, store.GitHubInstallation{
				ID: 7, AppID: appID, Account: "acme", Organization: true,
				Permissions: map[string]string{"contents": "read", "checks": "write"},
				Repositories: []store.GitHubRepository{
					{ID: 1, FullName: "acme/alpha"},
					{ID: 2, FullName: "acme/beta"},
					{ID: 3, FullName: "acme/unclaimed"},
				},
				UpdatedAt: now,
			})
		}); err != nil {
			t.Fatal(err)
		}

		h := New(Config{DB: db, Keeper: keeper, Web: "https://github.com"})
		if err := db.View(ctx, func(tx *store.Tx) error {
			// Git-built app in project A: developer may bind own Git source (acme/alpha),
			// NOT even an unclaimed repo (acme/unclaimed), while install admin may bind any reached repo.
			if err := h.ValidateDeployRepositoryInTx(ctx, tx, svcGitA, projA, "https://github.com/acme/alpha", false); err != nil {
				t.Errorf("developer binding own Git source failed: %v", err)
			}
			if err := h.ValidateDeployRepositoryInTx(ctx, tx, svcGitA, projA, "https://github.com/acme/unclaimed", false); !errors.Is(err, auth.ErrDeployRepoAdminOnly) {
				t.Errorf("developer binding other repo on Git-built app = %v, want ErrDeployRepoAdminOnly", err)
			}
			if err := h.ValidateDeployRepositoryInTx(ctx, tx, svcGitA, projA, "https://github.com/acme/unclaimed", true); err != nil {
				t.Errorf("admin binding other repo on Git-built app failed: %v", err)
			}

			// Image-only app in project A: developer may NOT bind acme/beta (project B's Git build source),
			// MAY bind acme/unclaimed, and install admin may bind acme/beta.
			if err := h.ValidateDeployRepositoryInTx(ctx, tx, svcImgA, projA, "https://github.com/acme/beta", false); !errors.Is(err, auth.ErrDeployRepoAdminOnly) {
				t.Errorf("developer binding project B Git source = %v, want ErrDeployRepoAdminOnly", err)
			}
			if err := h.ValidateDeployRepositoryInTx(ctx, tx, svcImgA, projA, "https://github.com/acme/unclaimed", false); err != nil {
				t.Errorf("developer binding unclaimed repo failed: %v", err)
			}
			if err := h.ValidateDeployRepositoryInTx(ctx, tx, svcImgA, projA, "https://github.com/acme/beta", true); err != nil {
				t.Errorf("admin binding project B Git source failed: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
