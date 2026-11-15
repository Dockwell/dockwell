// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A token's grants are its maker's, each capped at its role, and with projects,
// one grant on each of them alone at what its maker holds there, with none on the
// install. An owner counts as an admin in a project, and a project its maker holds
// nothing in is dropped (ADR 0071).
func TestNarrowCapsAndNarrowsGrants(t *testing.T) {
	grant := func(project, role string) store.Grant {
		return store.Grant{UserID: "usr_ada", ProjectID: project, Role: role}
	}
	for _, tt := range []struct {
		name     string
		grants   []store.Grant
		projects []string
		role     string
		want     []store.Grant
	}{
		{"not narrowed", []store.Grant{grant("", store.RoleAdmin)}, nil, "", []store.Grant{grant("", store.RoleAdmin)}},
		{
			"an admin capped at developer",
			[]store.Grant{grant("", store.RoleAdmin)},
			nil, store.RoleDeveloper,
			[]store.Grant{grant("", store.RoleDeveloper)},
		},
		{
			"a cap above what's held changes nothing",
			[]store.Grant{grant("prj_shop", store.RoleViewer)},
			nil, store.RoleDeveloper,
			[]store.Grant{grant("prj_shop", store.RoleViewer)},
		},
		{
			"an admin narrowed to a project",
			[]store.Grant{grant("", store.RoleAdmin)},
			[]string{"prj_shop"},
			"",
			[]store.Grant{grant("prj_shop", store.RoleAdmin)},
		},
		{
			"the owner counts as an admin in a project",
			[]store.Grant{grant("", store.RoleOwner)},
			[]string{"prj_shop"},
			"",
			[]store.Grant{grant("prj_shop", store.RoleAdmin)},
		},
		{
			"narrowed and capped",
			[]store.Grant{grant("", store.RoleOwner)},
			[]string{"prj_shop", "prj_blog"},
			store.RoleViewer,
			[]store.Grant{grant("prj_shop", store.RoleViewer), grant("prj_blog", store.RoleViewer)},
		},
		{
			"the highest of the install's and the project's",
			[]store.Grant{grant("", store.RoleViewer), grant("prj_shop", store.RoleDeveloper)},
			[]string{"prj_shop", "prj_blog"},
			"",
			[]store.Grant{grant("prj_shop", store.RoleDeveloper), grant("prj_blog", store.RoleViewer)},
		},
		{
			"a project its maker doesn't hold is dropped",
			[]store.Grant{grant("prj_shop", store.RoleDeveloper)},
			[]string{"prj_shop", "prj_blog"},
			"",
			[]store.Grant{grant("prj_shop", store.RoleDeveloper)},
		},
		{"nothing held, nothing given", nil, []string{"prj_shop"}, "", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Narrow(tt.grants, tt.projects, tt.role); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Narrow(%v, %v, %q) = %v, want %v", tt.grants, tt.projects, tt.role, got, tt.want)
			}
		})
	}
}

// A token is made narrowed only to what CreateTokenWithin allows: a developer or a
// viewer below its maker, up to 100 projects that exist, each said in the audit log;
// and the grants it acts with are capped from then on (ADR 0071).
func TestCreateTokenWithinChecksItsReach(t *testing.T) {
	storetest.Each(t, testCreateTokenWithinChecksItsReach)
}

func testCreateTokenWithinChecksItsReach(t *testing.T, b store.Backend) {
	f := newFixture(t, b, "")
	ctx := t.Context()
	owner := ownerPrincipal(f.claim(t, localhost, &authtest.Authenticator{}))
	within := func(reach TokenReach) (NewToken, error) {
		t.Helper()
		return f.svc.CreateTokenWithin(ctx, localhost, owner, "CI", store.ScopeReadWrite, nil, nil, reach, f.proof(t, owner, StepUpCreateToken, ""))
	}
	many := make([]string, maxTokenProjects+1)
	for i := range many {
		many[i] = fmt.Sprintf("prj_%03d", i)
	}
	for _, tt := range []struct {
		name  string
		reach TokenReach
		says  string
	}{
		{"a cap at admin", TokenReach{Role: store.RoleAdmin}, "a developer or a viewer at most"},
		{"a cap that isn't a role", TokenReach{Role: "root"}, "a developer or a viewer at most"},
		{"too many projects", TokenReach{Projects: many}, "up to 100 projects"},
		{"a project that isn't there", TokenReach{Projects: []string{"prj_nowhere"}}, "there's no project prj_nowhere"},
	} {
		if _, err := within(tt.reach); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.says) {
			t.Errorf("%s: %v, want ErrInvalid saying %q", tt.name, err, tt.says)
		}
	}

	made, err := within(TokenReach{Role: store.RoleDeveloper})
	if err != nil {
		t.Fatal(err)
	}
	if made.Token.Role != store.RoleDeveloper || !made.Token.Narrowed() {
		t.Errorf("the token made is %+v, want it capped at developer", made.Token)
	}
	p, err := f.svc.AuthenticateToken(ctx, localhost, made.Secret, AccessWrite, "/test/Call")
	if err != nil {
		t.Fatal(err)
	}
	grants, err := f.svc.Grants(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if Highest(grants) != store.RoleDeveloper {
		t.Errorf("the owner's developer-capped token acts with %v, want a developer's grants", grants)
	}
	err = f.db.View(ctx, func(tx *store.Tx) error {
		evs, err := tx.ListEvents(ctx, 100)
		for i := range evs {
			if e := &evs[i]; e.Action == "token.create" && e.Target == made.Token.ID && !strings.HasSuffix(e.Detail, ", as a developer at most") {
				t.Errorf("token.create says %q, want it to name the cap", e.Detail)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Managing members is an admin's of the whole install, as the caller's grants
// stand narrowed for a token, as every call's are: an admin's token capped below
// an admin, or narrowed to projects, holds no admin of the install, so it's
// refused past the API's gate too (ADR 0071, the review's L5).
func TestAnAdminsNarrowedTokenManagesNoMembers(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		ctx := t.Context()
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleAdmin, "203.0.113.5:1")
		sam := f.join(t, "sam@example.com", store.RoleViewer, "203.0.113.6:1")
		for name, tok := range map[string]*store.APIToken{
			"capped at developer":   {ID: "tok_capped", UserID: ada.p.User.ID, Role: store.RoleDeveloper},
			"narrowed to a project": {ID: "tok_narrowed", UserID: ada.p.User.ID, Projects: []string{"prj_shop"}},
		} {
			p := Principal{User: ada.p.User, Token: tok}
			if _, err := f.svc.ConfirmProtections(ctx, onDomain, p, standingOf("default/production protected")); !errors.Is(err, ErrAdminOnly) {
				t.Errorf("ada's token %s confirming the protections: %v, want ErrAdminOnly", name, err)
			}
			if _, err := f.svc.PlanMemberRemoval(ctx, p, sam.p.User.ID); !errors.Is(err, ErrAdminOnly) {
				t.Errorf("ada's token %s planning sam's removal: %v, want ErrAdminOnly", name, err)
			}
		}
		if _, err := f.svc.PlanMemberRemoval(ctx, ada.p, sam.p.User.ID); err != nil {
			t.Errorf("ada, an admin, planning sam's removal: %v", err)
		}
	})
}
