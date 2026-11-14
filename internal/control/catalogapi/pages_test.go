// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// aCatalog is a catalog of a few apps from Dockwell's own and a store's source, with
// one app both have.
func aCatalog() sourced {
	entries := []*catalogv1.Entry{
		{Id: "adminer", Name: "Adminer", Category: "databases", Summary: "Browse tables", Version: "6"},
		{Id: "ghost", Name: "Ghost", Category: "cms", Summary: "A blog", Version: "6", Features: []string{"newsletters"}},
		{Id: "memos", Name: "Memos", Category: "notes", Summary: "Short notes", Version: "1", Translations: map[string]*catalogv1.Translation{"zh-CN": {Summary: "备忘录"}}},
		{Id: "postgres", Name: "PostgreSQL", Category: "databases", Summary: "A database", Version: "17"},
		{Id: "wordpress", Name: "WordPress", Category: "cms", Summary: "Websites", Version: "7"},
		{Id: "zammad", Name: "Zammad", Category: "support", Summary: "Help desk", Version: "6"},
	}
	s := sourced{catalog: (&catalog.Catalog{}).With(entries), origins: map[string]origin{}, also: map[string][]alternative{}}
	for _, e := range entries {
		s.origins[e.GetId()] = origin{catalog: "dockwell", own: true}
	}
	s.origins["zammad"] = origin{catalog: "onepanel"}
	s.also["ghost"] = []alternative{{from: origin{catalog: "onepanel"}, entry: &catalogv1.Entry{Id: "ghost", Name: "Ghost", Category: "cms", Version: "5.9"}}}
	return s
}

func pageIDs(res *dockwellv1.ListCatalogEntriesResponse) []string {
	var out []string
	for _, e := range res.GetEntries() {
		out = append(out, e.GetId()+"@"+e.GetVersion())
	}
	return out
}

// The catalog answers a page at a time, in name order, with how many match and the
// token to the next page, and every entry at once where no page size is asked for
// (ADR 0092).
func TestTheCatalogPages(t *testing.T) {
	var p pageIndex
	idx := p.of(aCatalog())
	all, err := idx.page(&dockwellv1.ListCatalogEntriesRequest{}, nil)
	if err != nil || len(all.GetEntries()) != 6 || all.GetNextPageToken() != "" || all.GetTotal() != 6 {
		t.Fatalf("every entry: %v, %v", pageIDs(all), err)
	}
	var got []string
	token := ""
	for range 4 {
		res, err := idx.page(&dockwellv1.ListCatalogEntriesRequest{PageSize: 4, PageToken: token}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.GetTotal() != 6 {
			t.Errorf("total %d", res.GetTotal())
		}
		got = append(got, pageIDs(res)...)
		if token = res.GetNextPageToken(); token == "" {
			break
		}
	}
	if want := pageIDs(all); !slices.Equal(got, want) {
		t.Errorf("pages %v, want %v", got, want)
	}
	// A page's alternatives are its own entries'.
	first, _ := idx.page(&dockwellv1.ListCatalogEntriesRequest{PageSize: 2}, nil)
	if len(first.GetAlso()) != 1 || first.GetAlso()[0].GetId() != "ghost" {
		t.Errorf("the first page's alternatives: %v", first.GetAlso())
	}
	for _, bad := range []*dockwellv1.ListCatalogEntriesRequest{
		{PageSize: -1},
		{PageToken: "not a token"},
		{PageToken: pageToken(7)},
	} {
		if _, err := idx.page(bad, nil); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
	if big, _ := idx.page(&dockwellv1.ListCatalogEntriesRequest{PageSize: 10_000}, nil); len(big.GetEntries()) != 6 {
		t.Errorf("a large page: %v", pageIDs(big))
	}
}

// Search and the filters are the panel's, on the server: words in an entry's name,
// what it does in its own words or the reader's, its category or its features, a
// category the reader names in their language, one category, one catalog with its
// own entry of each app it has, the apps installed here, and given IDs; and the
// categories say how many match every filter but the category.
func TestTheCatalogSearchesAndFilters(t *testing.T) {
	var p pageIndex
	idx := p.of(aCatalog())
	for name, c := range map[string]struct {
		req       *dockwellv1.ListCatalogEntriesRequest
		installed map[string]int
		want      []string
	}{
		"a name":                   {&dockwellv1.ListCatalogEntriesRequest{Search: "  POSTGRE "}, nil, []string{"postgres@17"}},
		"what it does":             {&dockwellv1.ListCatalogEntriesRequest{Search: "help desk"}, nil, []string{"zammad@6"}},
		"a feature":                {&dockwellv1.ListCatalogEntriesRequest{Search: "newsletter"}, nil, []string{"ghost@6"}},
		"a category's ID":          {&dockwellv1.ListCatalogEntriesRequest{Search: "notes"}, nil, []string{"memos@1"}},
		"the reader's words":       {&dockwellv1.ListCatalogEntriesRequest{Search: "备忘", Language: "zh-CN"}, nil, []string{"memos@1"}},
		"their language's words":   {&dockwellv1.ListCatalogEntriesRequest{Search: "备忘", Language: "zh-TW"}, nil, []string{"memos@1"}},
		"not in English":           {&dockwellv1.ListCatalogEntriesRequest{Search: "备忘", Language: "en"}, nil, nil},
		"a category they name":     {&dockwellv1.ListCatalogEntriesRequest{Search: "数据库", SearchCategories: []string{"databases"}}, nil, []string{"adminer@6", "postgres@17"}},
		"a category":               {&dockwellv1.ListCatalogEntriesRequest{Category: "cms"}, nil, []string{"ghost@6", "wordpress@7"}},
		"a store's apps":           {&dockwellv1.ListCatalogEntriesRequest{Catalog: "onepanel"}, nil, []string{"ghost@5.9", "zammad@6"}},
		"installed here":           {&dockwellv1.ListCatalogEntriesRequest{Installed: true}, map[string]int{"memos": 1}, []string{"memos@1"}},
		"given IDs":                {&dockwellv1.ListCatalogEntriesRequest{Ids: []string{"zammad", "adminer"}}, nil, []string{"adminer@6", "zammad@6"}},
		"featured, then installed": {&dockwellv1.ListCatalogEntriesRequest{Order: dockwellv1.CatalogOrder_CATALOG_ORDER_FEATURED}, map[string]int{"zammad": 3, "memos": 1}, []string{"wordpress@7", "postgres@17", "ghost@6", "zammad@6", "memos@1", "adminer@6"}},
	} {
		res, err := idx.page(c.req, c.installed)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := pageIDs(res); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	res, _ := idx.page(&dockwellv1.ListCatalogEntriesRequest{Category: "cms", Search: "s"}, nil)
	var counts []string
	for _, c := range res.GetCategories() {
		counts = append(counts, c.GetCategory()+":"+strings.Repeat("|", int(c.GetCount())))
	}
	// Every entry has an s somewhere: the categories count them all, whichever is chosen.
	if want := []string{"cms:||", "databases:||", "notes:|", "support:|"}; !slices.Equal(counts, want) {
		t.Errorf("categories %v, want %v", counts, want)
	}
	// Words that find nothing leave the categories offered as they were, each with none found.
	none, _ := idx.page(&dockwellv1.ListCatalogEntriesRequest{Search: "zzz"}, nil)
	var offered []string
	for _, c := range none.GetCategories() {
		offered = append(offered, c.GetCategory()+":"+strconv.Itoa(int(c.GetCount()))+"/"+strconv.Itoa(int(c.GetListed())))
	}
	if want := []string{"cms:0/2", "databases:0/2", "notes:0/1", "support:0/1"}; !slices.Equal(offered, want) {
		t.Errorf("categories for words that find nothing %v, want %v", offered, want)
	}
}

// The control plane holds one index while the catalog stays the same, and makes it
// again only once the catalog changes (ADR 0092).
func TestTheIndexIsMadeOnceForEachCatalog(t *testing.T) {
	var p pageIndex
	s := aCatalog()
	first := p.of(s)
	if p.of(aCatalog()) != first {
		t.Error("the same catalog made a second index")
	}
	changed := aCatalog()
	changed.also["ghost"][0].entry.Version = "6.1"
	if p.of(changed) == first {
		t.Error("a changed catalog kept the old index")
	}
}
