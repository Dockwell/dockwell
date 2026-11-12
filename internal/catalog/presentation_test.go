// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// A custom entry must not turn browsing into a request to a third-party host or
// an executable document. Old entries without presentation fields still load.
func TestPresentationIsOptionalAndConstrained(t *testing.T) {
	c, err := catalog.BuiltIn("")
	if err != nil {
		t.Fatal(err)
	}
	original, _ := c.Entry("postgres")
	for _, tc := range []struct {
		name   string
		change func(*catalogv1.Entry)
		valid  bool
	}{
		{"old entry", func(e *catalogv1.Entry) {
			e.Description = ""
			e.Icon = ""
			e.Screenshots = nil
			e.Documentation = ""
			e.AppSource = ""
			e.Features = nil
		}, true},
		{"local media", func(e *catalogv1.Entry) { e.Icon = "/catalog-media/example/logo.svg" }, true},
		{"remote icon", func(e *catalogv1.Entry) { e.Icon = "https://tracker.example/logo.png" }, false},
		{"path traversal", func(e *catalogv1.Entry) { e.Icon = "/catalog-media/../api/logo.png" }, false},
		{"encoded path", func(e *catalogv1.Entry) { e.Icon = "/catalog-media/%2e%2e/logo.png" }, false},
		{"script link", func(e *catalogv1.Entry) { e.Documentation = "javascript:alert(1)" }, false},
		{"script homepage", func(e *catalogv1.Entry) { e.Homepage = "javascript:alert(1)" }, false},
		{"plain HTTP homepage", func(e *catalogv1.Entry) { e.Homepage = "http://example.com" }, false},
		{"credentials", func(e *catalogv1.Entry) { e.AppSource = "https://user:secret@example.com" }, false},
		{"no hostname", func(e *catalogv1.Entry) { e.AppSource = "https:///missing" }, false},
		{"svg screenshot", func(e *catalogv1.Entry) { e.Screenshots[0].Path = "/catalog-media/example.svg" }, false},
		{"uncaptioned screenshot", func(e *catalogv1.Entry) { e.Screenshots[0].Caption = " " }, false},
		{"uncredited screenshot", func(e *catalogv1.Entry) { e.Screenshots[0].Source = "" }, false},
		{"duplicate screenshot", func(e *catalogv1.Entry) { e.Screenshots = append(e.Screenshots, proto.CloneOf(e.Screenshots[0])) }, false},
		{"too many screenshots", func(e *catalogv1.Entry) { e.Screenshots = make([]*catalogv1.Screenshot, 9) }, false},
		{"empty feature", func(e *catalogv1.Entry) { e.Features = []string{" "} }, false},
		{"long feature", func(e *catalogv1.Entry) { e.Features = []string{strings.Repeat("a", 241)} }, false},
		{"too many features", func(e *catalogv1.Entry) { e.Features = make([]string, 13) }, false},
		{"long description", func(e *catalogv1.Entry) { e.Description = strings.Repeat("字", 6001) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := proto.CloneOf(original)
			tc.change(e)
			if err := catalog.Check(e); (err == nil) != tc.valid {
				t.Fatalf("valid = %t, Check = %v", tc.valid, err)
			}
		})
	}
}

// Every bundled page has real media with a recorded origin, and every referenced
// file survives packaging. This reads local files only, never the source hosts.
func TestBuiltInPresentationAndMedia(t *testing.T) {
	c, err := catalog.BuiltIn("")
	if err != nil {
		t.Fatal(err)
	}
	root := "../../web/public/catalog-media"
	raw, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest []struct{ File, SHA256, URL string }
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	credited := map[string]bool{}
	for _, item := range manifest {
		data, err := os.ReadFile(filepath.Join(root, item.File))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != item.SHA256 || !strings.HasPrefix(item.URL, "https://") {
			t.Errorf("%s has lost its provenance", item.File)
		}
		credited["/catalog-media/"+item.File] = true
	}
	for _, e := range c.All() {
		t.Run(e.GetId(), func(t *testing.T) {
			if e.GetDescription() == "" || len(e.GetFeatures()) == 0 || e.GetDocumentation() == "" || e.GetAppSource() == "" {
				t.Error("missing app information")
			}
			if !credited[e.GetIcon()] {
				t.Errorf("uncredited or missing icon %q", e.GetIcon())
			}
			if e.GetWeb() != nil && len(e.GetScreenshots()) == 0 {
				t.Error("an app with an interface needs a preview")
			}
			for _, shot := range e.GetScreenshots() {
				if !credited[shot.GetPath()] {
					t.Errorf("uncredited or missing preview %q", shot.GetPath())
				}
			}
		})
	}
}

func TestPresentationChangesDoNotOfferAnUpgrade(t *testing.T) {
	c, err := catalog.BuiltIn("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.All() {
		pin := &resourcev1.CatalogPin{Entry: e.GetId(), Release: e.GetRelease() - 1, Version: e.GetVersion(), File: []byte(e.GetCompose())}
		if catalog.Upgrades(e, pin) {
			t.Errorf("%s offers a plan for the same Compose file", e.GetId())
		}
	}
}

// What an install's plan says a person needs comes from the entry's fields, a short
// sentence each, for the CLI as for the panel.
func TestFirstWordsSayHowToSignIn(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"postgres": "Sign in to adminer to server db as postgres, with the password in the POSTGRES_PASSWORD secret. Choose PostgreSQL as Adminer's system.",
		"pgadmin": "Sign in to pgadmin as your email, with the password in the PGADMIN_PASSWORD secret. " +
			"Registers a PostgreSQL that has a port of its own at host.docker.internal, which is this server.",
		"nextcloud": "Sign in to nextcloud as the admin user you give, with the password in the NEXTCLOUD_ADMIN_PASSWORD secret. " +
			"Answers at the domain you gave on its first install; another needs adding to its trusted_domains. " +
			"Leave the POSTGRES_PASSWORD secret as it is: its config.php keeps the password it installed with. Its first start takes a minute or two.",
		"gitea": "Its first account is its owner, so make yours before it has a domain. Git over SSH answers on its SSH port, on this server alone.",
		"t3code": "Sign in to t3code with the key in the T3_SETUP_KEY secret. " +
			"Anyone who reaches it over plain HTTP gets a shell, so it needs a domain over HTTPS. Open /__setup on its domain and sign in with the setup key.",
		"cloudflared": "In Cloudflare, publish your domain on this tunnel to http://127.0.0.1:80, then add it to an app here as behind a tunnel.",
		// Its notes reach the CLI's plan as they reach the panel's install sheet.
		"hermes-agent": "Sign in to hermes as admin, with the password in the HERMES_PASSWORD secret. " +
			"Add a domain only behind your VPN: a public one needs Hermes's OAuth provider in place of basic auth. " +
			"Its API answers on the API port, on this server alone, at /v1 with the API_KEY secret.",
		"openclaw": "Sign in to openclaw with the key in the GATEWAY_TOKEN secret. " +
			"Its Control UI takes only its own address, not the panel's Open: reach it through the SSH tunnel to its port. " +
			"New devices wait for approval: in its container, node /app/dist/index.js devices list, then devices approve with its ID.",
	} {
		e, _ := c.Entry(id)
		if got := catalog.FirstWords(e, nil); got != want {
			t.Errorf("%s says %q, want %q", id, got, want)
		}
	}
}
