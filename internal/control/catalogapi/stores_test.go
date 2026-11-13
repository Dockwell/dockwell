// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// A source reads one of the stores Dockwell knows only at the store's own
// repository, in its format, at its top: a copy of its layout elsewhere, or the same
// repository read as another format, is a source of its own (ADR 0083).
func TestStoreOf(t *testing.T) {
	onepanel := resourcev1.CatalogFormat_CATALOG_FORMAT_ONEPANEL
	for _, c := range []struct {
		repository, path string
		format           resourcev1.CatalogFormat
		want             string
	}{
		{"https://github.com/1Panel-dev/appstore", "", onepanel, "onepanel"},
		{"https://github.com/1panel-dev/appstore.git", "", onepanel, "onepanel"},
		{"https://github.com/1Panel-dev/appstore/", "", onepanel, "onepanel"},
		{"https://github.com/acme/appstore", "", onepanel, ""},
		{"https://github.com/1Panel-dev/appstore", "apps", onepanel, ""},
		{"https://github.com/1Panel-dev/appstore", "", resourcev1.CatalogFormat_CATALOG_FORMAT_DOCKWELL, ""},
		{"https://github.com/coollabsio/coolify", "", resourcev1.CatalogFormat_CATALOG_FORMAT_COOLIFY, "coolify"},
		{"https://github.com/Dokploy/templates", "", resourcev1.CatalogFormat_CATALOG_FORMAT_DOKPLOY, "dokploy"},
		{"https://github.com/runtipi/runtipi-appstore", "", resourcev1.CatalogFormat_CATALOG_FORMAT_RUNTIPI, "runtipi"},
		{"https://github.com/IceWhaleTech/CasaOS-AppStore", "", resourcev1.CatalogFormat_CATALOG_FORMAT_CASAOS, "casaos"},
		{"https://github.com/getumbrel/umbrel-apps", "", resourcev1.CatalogFormat_CATALOG_FORMAT_UMBREL, "umbrel"},
		{"https://github.com/Dockwell/catalog", "entries", resourcev1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED, ""},
	} {
		spec := &resourcev1.CatalogSource{Repository: c.repository, Path: c.path, Format: c.format}
		if got := storeOf(spec).ID; got != c.want {
			t.Errorf("storeOf(%s, %q, %s) = %q, want %q", c.repository, c.path, c.format, got, c.want)
		}
	}
}

// The catalog lists an entry without its README and with only the summary of what it
// says in other languages, leaving the entry it read as it was, as its page reads it
// whole.
func TestListedLeavesOutLongWords(t *testing.T) {
	e := &catalogv1.Entry{Id: "notes", Readme: "## About", Translations: map[string]*catalogv1.Translation{
		"zh-CN": {Summary: "笔记", Description: "长", Readme: "## 介绍"},
		"de":    {Readme: "## Über"},
	}}
	got := listed(e)
	if got.GetReadme() != "" || len(got.GetTranslations()) != 1 {
		t.Fatalf("listed = %v", got)
	}
	if zh := got.GetTranslations()["zh-CN"]; zh.GetSummary() != "笔记" || zh.GetDescription() != "" || zh.GetReadme() != "" {
		t.Errorf("listed's Chinese = %v", zh)
	}
	if e.GetReadme() != "## About" || e.GetTranslations()["zh-CN"].GetReadme() != "## 介绍" || len(e.GetTranslations()) != 2 {
		t.Errorf("listed changed the entry it read: %v", e)
	}
	plain := &catalogv1.Entry{Id: "plain"}
	if listed(plain) != plain {
		t.Error("listed copied an entry with nothing to leave out")
	}
}
