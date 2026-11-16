// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/url"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

// insideEntry is the test entry with a URL in web's environment that holds its
// password inside it, and a plain value, its port, after a query.
func insideEntry(release int, version, image, query string) string {
	return strings.Replace(testEntry(release, version, image, ""), "PASSWORD: ${PASSWORD}",
		"PASSWORD: ${PASSWORD}\n        URL: postgres://u:${PASSWORD}@db:5432/x?port=${PORT}"+query, 1)
}

// An install of an entry whose service has its password inside a longer value, a
// connection URL, runs with the URL written from the password; a rotation of the
// password reaches the URL, and an upgrade that changes the URL keeps the password
// (#570). Before, the catalog's check left such an entry out.
func TestASecretInsideAValueReachesTheAppThroughAnUpgradeAndARotation(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	env := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, kv := range u.web().Config.Env {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
		return out
	}
	got := env()
	password := got["PASSWORD"]
	if len(password) != 32 {
		t.Fatalf("web runs with a password of %d characters: %v", len(password), got)
	}
	want := func(password, query string) string {
		return "postgres://u:" + url.PathEscape(password) + "@db:5432/x?port=18100" + query
	}
	if got["URL"] != want(password, "") {
		t.Errorf("web reads URL = %q, want %q", got["URL"], want(password, ""))
	}

	// A rotation of the password reaches the URL, encoded as a URL's userinfo.
	rotate := propose(t, u.c, "Set the password anew", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: u.env, Name: "PASSWORD"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "second/one@x"}}},
	})
	u.applied(rotate)
	if got := env(); got["PASSWORD"] != "second/one@x" || got["URL"] != "postgres://u:second%2Fone%40x@db:5432/x?port=18100" {
		t.Errorf("after the rotation web reads PASSWORD %q, URL %q", got["PASSWORD"], got["URL"])
	}

	// An upgrade that changes the URL keeps the password, and writes the new URL.
	u.release(insideEntry(2, "2.0", secondRelease, "&sslmode=disable"))
	u.applied(u.upgrade())
	if got := env(); got["PASSWORD"] != "second/one@x" || got["URL"] != "postgres://u:second%2Fone%40x@db:5432/x?port=18100&sslmode=disable" {
		t.Errorf("after the upgrade web reads PASSWORD %q, URL %q", got["PASSWORD"], got["URL"])
	}
	// What anyone reads of the install holds no password.
	res, err := u.cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Msg.String(), "second/one") || strings.Contains(res.Msg.String(), password) {
		t.Error("the install's record holds the password")
	}
}
