# 0092. The catalog a page at a time

- Status: Proposed
- Date: 2026-11-13

## Context

`ListCatalogEntries` answered the whole catalog in one message: every entry, and every other catalog's entry of an app another lists first (#616). With Dockwell's own catalog and the stores the control plane converts ([ADR 0078](0078-other-stores-read-on-the-server.md), [ADR 0083](0083-a-stores-own-logos-and-words-and-stores-one-click-away.md)), that is hundreds of entries, sent, parsed and laid out at once, and it grows with every source. The panel searched and filtered them itself (`web/src/features/catalog/search.ts` and `CatalogPage`), so every screen that showed one app read them all. New app is to offer the catalog first (#617), which needs the most likely apps at once and the rest as someone types.

## Decision

### The server pages, searches and filters

`ListCatalogEntriesRequest` takes `page_size` (at most 200) and `page_token`, and the filters a page offers: `search`, `category`, `catalog` (a source's name), `installed`, `ids`, and `order`. It answers one page, `total` (how many match across every page), `next_page_token`, and `categories`, each category with how many entries match every filter but the category, which no single page can say. A request with no page size lists everything that matches, as before, so the CLI, the e2e scripts and an older panel go on working; the CLI's `--page-size` and `--page-token` come from the fields, and it says when there are more.

The search is the panel's, moved: the words, case aside, anywhere in an entry's name, what it does in its own words or in the reader's `language` (the same tag, or another of the same language, as the panel picks it), its category's ID, or its features. A category's name in the reader's language is the panel's, as the control plane translates nothing, so the panel sends the categories its words name as `search_categories`, and an entry in one matches too. Results are those the panel found itself.

`catalog` narrows to the apps a source has, its own entry of each app another catalog lists first in that entry's place, as the panel narrowed it. `installed` is the apps installed here from the catalog their entry comes from, as an install of another catalog's app of the same ID is another app ([ADR 0078](0078-other-stores-read-on-the-server.md)). `order` is by name, or `CATALOG_ORDER_FEATURED`: the apps most people start with, a list the control plane holds, then those installed here most, then by name, for New app before anyone types.

A page token is the place of the page's first entry. A token past the end, from a catalog that changed between pages, is refused with how to start again.

### The control plane holds one index

The index is each entry as the catalog lists it (without its README, [ADR 0083](0083-a-stores-own-logos-and-words-and-stores-one-click-away.md)), in name order, with what a search reads lowercased once, in its own language and each it has words in, and each source's own entries of apps listed first elsewhere. It is made once for each catalog, kept while a hash of every entry's and alternative's ID, version, release, name and source stays the same, and every request reads it: a page is one pass over it, and no request copies it. The listed entries are the same messages the catalog holds where they have no README to leave out.

### The panel reads a page

The Catalog page reads 48 entries a page and the next as the reader scrolls near the end, keeps its pages in the query cache so the grid and its place come back on the way back, and keeps its filters in the address. The first page paints with placeholders where cards go, never a spinner over the grid; a change of filters keeps the grid until the new page answers. Screens that need a few entries ask for them: by `ids`, by `category`, or by `catalog`, and `GetCatalogEntry` reads one.

## Consequences

- Hundreds of entries no longer cross the wire to show a dozen. The CLI lists everything by default, and pages when asked.
- The categories offered are the server's counts of every filter but the category and the words (`listed`), so the choices stay put as words are typed, as they did before the catalog paged; `count` says how many of each the words find, for the way to another category when the chosen one has none.
- The index costs memory once: entries are shared with the catalog, and the search's words are a few hundred bytes an entry for each language.
- A catalog that changes between two pages can skip or repeat an entry at the seam; a reader who scrolls on sees the new catalog from there. Reading the first page again starts over.
