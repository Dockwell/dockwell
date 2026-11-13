// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/revision"
)

// A new one from a catalog whose entry names its image by tag, as a store's does, is
// pinned by digest before the install keeps its service in its pin, as the app's own
// images are, and the plan says so (the review of #577, its L3).
func TestANewOneIsPinnedByDigest(t *testing.T) {
	builtIn, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	mysql, ok := builtIn.Entry("mysql")
	if !ok {
		t.Fatal("the catalog has no mysql")
	}
	mysql = proto.CloneOf(mysql)
	mysql.Compose = regexp.MustCompile(`image: (mysql:[0-9.]+)@sha256:[0-9a-f]{64}`).ReplaceAllString(mysql.GetCompose(), "image: $1")
	app := &catalogv1.Entry{
		Id: "app", Name: "App", Version: "1.0",
		Compose: "services:\n  app:\n    image: ghcr.io/acme/app:1@sha256:" + strings.Repeat("1", 64) + "\n    environment:\n      DB_HOST: ${DB_HOST}\n      DB_PASSWORD: ${DB_PASSWORD}\n",
		Needs: []*catalogv1.Need{{
			Name: "db", Label: "Database",
			Accepts: []*catalogv1.NeedAccept{{Engine: catalogv1.Engine_ENGINE_MYSQL}},
			Sets: []*catalogv1.NeedSet{
				{Name: "DB_HOST", Part: catalogv1.Part_PART_HOST},
				{Name: "DB_PASSWORD", Part: catalogv1.Part_PART_PASSWORD},
			},
		}},
	}
	s := sourced{catalog: (&catalog.Catalog{}).With([]*catalogv1.Entry{mysql, app}), origins: map[string]origin{}}
	asked := &digestsAsked{answer: goodDigest}
	h := (&Handler{}).PinWith(asked)
	needed, notes, err := h.meetNeeds(t.Context(), s, app, nil, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if file := string(needed.Pinned["db"].GetFile()); !strings.Contains(file, "@"+goodDigest("")) {
		t.Errorf("the pin keeps the new MySQL by tag:\n%s", file)
	}
	if !slices.ContainsFunc(notes, func(n revision.Note) bool { return n.Reason == "catalog_pins_images" }) {
		t.Errorf("the plan doesn't say what it pinned: %v", notes)
	}
}
