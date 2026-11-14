// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// The most entries one page holds.
const mostPage = 200

// featured are the apps most people start with, in the order New app offers them,
// where the catalog has them: what a newcomer is most likely to know (ADR 0092).
var featured = []string{
	"wordpress", "postgres", "nextcloud", "gitea", "n8n", "uptime-kuma",
	"ghost", "plausible", "vaultwarden", "grafana", "jellyfin", "mysql",
}

// A catalogIndex is what the catalog's pages are cut from: each app's entry as the
// catalog lists it, in name order, with the words a search reads, lowercased once,
// and each catalog's own entry of an app another lists first. The control plane
// holds one while the catalog stays the same, and every request reads it, so a
// page costs a pass over it and no copy (ADR 0092).
type catalogIndex struct {
	key     [32]byte
	entries []*indexed
	// theirs is each catalog's own entry of an app another catalog lists first, by
	// the catalog's source's name and then the app's ID.
	theirs map[string]map[string]*indexed
	// also are each app's entries in the catalogs after the one it's listed from,
	// as ListCatalogEntries answers them, by its ID.
	also map[string][]*dockwellv1.CatalogAlternative
}

// An indexed entry is one app's entry as it's listed, and what a search reads of it.
type indexed struct {
	entry *catalogv1.Entry
	// catalog is the source it's listed from, by name, empty for the catalog built
	// into Dockwell.
	catalog string
	// words is what a search reads in its own language, and said what it reads in
	// another, by the language's tag, where the entry has words in it.
	words string
	said  map[string]string
}

// haystack is what a search reads of an entry, lowercased, with what it does in
// summary: its name, what it does, its category and its features, as the panel's
// search read them before the catalog paged.
func haystack(e *catalogv1.Entry, summary string) string {
	return strings.ToLower(strings.Join([]string{e.GetName(), e.GetSummary(), summary, e.GetCategory(), strings.Join(e.GetFeatures(), " ")}, " "))
}

func indexEntry(e *catalogv1.Entry, from string) *indexed {
	in := &indexed{entry: listed(e), catalog: from, words: haystack(e, e.GetSummary())}
	for tag, words := range e.GetTranslations() {
		if s := words.GetSummary(); s != "" {
			if in.said == nil {
				in.said = map[string]string{}
			}
			in.said[tag] = haystack(e, s)
		}
	}
	return in
}

// wordsIn is what a search reads of an entry for a reader of language: its words in
// that language, or one of the same language elsewhere, or its own, as the panel
// shows them (useEntryWords).
func (in *indexed) wordsIn(language string) string {
	if language == "" || strings.HasPrefix(language, "en") || len(in.said) == 0 {
		return in.words
	}
	if s, ok := in.said[language]; ok {
		return s
	}
	base, _, _ := strings.Cut(language, "-")
	tags := make([]string, 0, len(in.said))
	for tag := range in.said {
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	for _, tag := range tags {
		if b, _, _ := strings.Cut(tag, "-"); b == base {
			return in.said[tag]
		}
	}
	return in.words
}

// indexKey says what the catalog holds, so the index is made again only when it
// changes: each entry and each alternative whole, marshalled deterministically, with
// the catalog it comes from, as a search reads its summary, category, features and
// words in other languages too (the review of #621).
func indexKey(s sourced) [32]byte {
	h := sha256.New()
	marshal := proto.MarshalOptions{Deterministic: true}
	var buf []byte
	write := func(e *catalogv1.Entry, from string) {
		var err error
		if buf, err = marshal.MarshalAppend(buf[:0], e); err != nil {
			// What can't be marshalled can't be compared: no key matches it twice.
			fmt.Fprintf(h, "%p", e)
		}
		fmt.Fprintf(h, "%s\x00%d\x00", from, len(buf))
		h.Write(buf)
	}
	for _, e := range s.catalog.All() {
		write(e, s.origins[e.GetId()].catalog)
	}
	for _, id := range sortedKeys(s.also) {
		for _, a := range s.also[id] {
			write(a.entry, id+"\x00"+a.from.catalog)
		}
	}
	var key [32]byte
	h.Sum(key[:0])
	return key
}

// pageIndex holds the catalog's index for as long as the catalog is the same.
type pageIndex struct {
	mu  sync.Mutex
	idx *catalogIndex
}

// of is the index of the catalog s reads, made once for each catalog.
func (p *pageIndex) of(s sourced) *catalogIndex {
	key := indexKey(s)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.idx != nil && p.idx.key == key {
		return p.idx
	}
	idx := &catalogIndex{key: key, theirs: map[string]map[string]*indexed{}, also: map[string][]*dockwellv1.CatalogAlternative{}}
	for _, e := range s.catalog.All() {
		idx.entries = append(idx.entries, indexEntry(e, s.origins[e.GetId()].catalog))
	}
	for _, id := range sortedKeys(s.also) {
		for _, a := range s.also[id] {
			in := indexEntry(a.entry, a.from.catalog)
			if idx.theirs[a.from.catalog] == nil {
				idx.theirs[a.from.catalog] = map[string]*indexed{}
			}
			idx.theirs[a.from.catalog][id] = in
			idx.also[id] = append(idx.also[id], &dockwellv1.CatalogAlternative{Id: id, Catalog: a.from.catalog, Entry: in.entry})
		}
	}
	p.idx = idx
	return idx
}

// pageToken is where a page starts, as ListCatalogEntries says it: the place of its
// first entry among those that match.
func pageToken(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("catalog:" + strconv.Itoa(offset)))
}

func pageOffset(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	n, ok := strings.CutPrefix(string(raw), "catalog:")
	offset, aerr := strconv.Atoi(n)
	if err != nil || !ok || aerr != nil || offset < 0 {
		return 0, errors.New("page_token isn't one ListCatalogEntries gave. Leave it out to start at the first page")
	}
	return offset, nil
}

// page answers one page of the catalog: the entries that pass every filter, in the
// order asked for, from the place the token says, and how many there are with how
// many each category has. installed is the apps installed here, and installs how
// many times each, both by ID, read only where the request asks for them.
func (idx *catalogIndex) page(req *dockwellv1.ListCatalogEntriesRequest, installed map[string]int) (*dockwellv1.ListCatalogEntriesResponse, error) {
	if req.GetPageSize() < 0 {
		return nil, errors.New("page_size can't be below 0. Leave it out to list every entry")
	}
	offset, err := pageOffset(req.GetPageToken())
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(strings.TrimSpace(req.GetSearch()))
	named := map[string]bool{}
	for _, c := range req.GetSearchCategories() {
		named[c] = true
	}
	ids := map[string]bool{}
	for _, id := range req.GetIds() {
		ids[id] = true
	}
	theirs := idx.theirs[req.GetCatalog()]
	var matched []*indexed
	counts, listed := map[string]int{}, map[string]int{}
	for _, in := range idx.entries {
		id := in.entry.GetId()
		// Narrowed to one catalog, its own entry of each app it has, in the place of
		// the entry of the catalog that lists the app first.
		if req.GetCatalog() != "" {
			if t := theirs[id]; t != nil {
				in = t
			} else if in.catalog != req.GetCatalog() {
				continue
			}
		}
		if len(ids) > 0 && !ids[id] || req.GetInstalled() && installed[id] == 0 {
			continue
		}
		listed[in.entry.GetCategory()]++
		if query != "" && !strings.Contains(in.wordsIn(req.GetLanguage()), query) && !named[in.entry.GetCategory()] {
			continue
		}
		counts[in.entry.GetCategory()]++
		if req.GetCategory() == "" || in.entry.GetCategory() == req.GetCategory() {
			matched = append(matched, in)
		}
	}
	if req.GetOrder() == dockwellv1.CatalogOrder_CATALOG_ORDER_FEATURED {
		rank := func(id string) int {
			if i := slices.Index(featured, id); i >= 0 {
				return i
			}
			return len(featured)
		}
		slices.SortStableFunc(matched, func(a, b *indexed) int {
			x, y := a.entry.GetId(), b.entry.GetId()
			return cmp.Or(cmp.Compare(rank(x), rank(y)), cmp.Compare(installed[y], installed[x]))
		})
	}
	total := len(matched)
	// A token past the end, where the catalog shrank since the page before, is the
	// last page: empty, with nothing after it, as the grid keeps what it has.
	offset = min(offset, total)
	end := total
	if size := int(req.GetPageSize()); size > 0 {
		end = min(offset+min(size, mostPage), total)
	}
	out := &dockwellv1.ListCatalogEntriesResponse{Total: int32(total)} //nolint:gosec // a catalog holds thousands at most
	for _, in := range matched[offset:end] {
		out.Entries = append(out.Entries, in.entry)
		out.Also = append(out.Also, idx.also[in.entry.GetId()]...)
	}
	if end < total {
		out.NextPageToken = pageToken(end)
	}
	for _, c := range sortedKeys(listed) {
		out.Categories = append(out.Categories, &dockwellv1.CatalogCategoryCount{Category: c, Count: int32(counts[c]), Listed: int32(listed[c])}) //nolint:gosec // as total
	}
	return out, nil
}

// installedHere is how many projects run each app from the catalog its entry comes
// from, by the entry's ID: an install of an app of the same ID from another catalog
// is another app. It counts only the projects the caller holds a role in, as an
// install admin holds every one, so neither the installed filter nor the featured
// order says what another project runs (ADR 0049).
func (h *Handler) installedHere(ctx context.Context, s sourced) (map[string]int, error) {
	caller, ok := place.CallerFrom(ctx)
	everything := !ok || store.RoleRank(place.Holds(caller.Grants, place.Install)) >= store.RoleRank(store.RoleAdmin)
	out := map[string]int{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			if !everything && place.Holds(caller.Grants, place.Place{Project: p.ID}) == "" {
				continue
			}
			pin, err := h.pinOf(ctx, tx, tree.Organization.ID, p.Name)
			if err != nil {
				return err
			}
			if pin == nil {
				continue
			}
			if _, ok := s.entryFor(pin); ok {
				out[pin.GetEntry()]++
			}
		}
		return nil
	})
	return out, err
}

// invalid is a request that names what it can't, said as connect says it.
func invalid(err error) error {
	return connect.NewError(connect.CodeInvalidArgument, err)
}
