// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"os"
	"slices"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/catalog/folder"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/database"
)

func TestBuiltInClickHousePassesCatalogChecks(t *testing.T) {
	raw, err := os.ReadFile("clickhouse.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got := folder.Check(t.Context(), "Built into Dockwell", map[string][]byte{"clickhouse.yaml": raw}, nil, map[string]folder.Taken{}, folder.Options{})
	if len(got.Refused) != 0 || len(got.Entries) != 1 {
		t.Fatalf("the built-in ClickHouse entry failed the catalog check: %+v", got.Refused)
	}
	entry, err := catalog.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	stack, err := compose.Read(t.Context(), compose.Input{
		File: []byte(entry.GetCompose()), Variables: map[string]string{"CLICKHOUSE_PASSWORD": "fixture-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := stack.Services[0].Spec.GetApp().GetCommand()
	if !slices.Equal(command, database.WrapClickHouseAuth(nil)) {
		t.Fatal("the recipe and the managed auth wrapper don't agree after Compose interpolation")
	}
}

func TestClickHouseLinkDefaultsToHTTP(t *testing.T) {
	need := &catalogv1.Need{Name: "events", Label: "Events", Sets: []*catalogv1.NeedSet{
		{Name: "CLICKHOUSE_URL", Part: catalogv1.Part_PART_URL},
		{Name: "CLICKHOUSE_NATIVE_URL", Part: catalogv1.Part_PART_URL, Template: "clickhouse://{host}:{native_port}/{database}"},
		{Name: "CLICKHOUSE_HTTP_PORT", Part: catalogv1.Part_PART_PORT},
		{Name: "CLICKHOUSE_NATIVE_PORT", Part: catalogv1.Part_PART_NATIVE_PORT},
	}}
	got, err := catalog.Connect("web", need, catalog.Way{Need: need, Engine: catalogv1.Engine_ENGINE_CLICKHOUSE, Alias: "db", User: "default", Database: "events"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plain["CLICKHOUSE_HTTP_PORT"] != "8123" || got.Plain["CLICKHOUSE_NATIVE_PORT"] != "9000" {
		t.Fatalf("incorrect ClickHouse ports: %v", got.Plain)
	}
	for name, part := range map[string]string{"CLICKHOUSE_URL": "CLICKHOUSE_HTTP_PORT", "CLICKHOUSE_NATIVE_URL": "CLICKHOUSE_NATIVE_PORT"} {
		if !slices.ContainsFunc(got.Written[name].GetNames(), func(n *resourcev1.TemplateName) bool { return n.GetName() == part }) {
			t.Fatalf("%s did not use %s", name, part)
		}
	}
}
