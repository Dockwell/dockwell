// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  type CatalogAlternativeSchema,
  CatalogOrder,
  type Entry,
  EntrySchema,
  type ListCatalogEntriesRequest,
  type ListCatalogEntriesResponse,
  ListCatalogEntriesResponseSchema,
} from "@dockwell/api";

type EntryInit = MessageInitShape<typeof EntrySchema>;

/** The apps the control plane puts first, as internal/control/catalogapi/pages.go has them. */
const FEATURED = [
  "wordpress",
  "postgres",
  "nextcloud",
  "gitea",
  "n8n",
  "uptime-kuma",
  "ghost",
  "plausible",
  "vaultwarden",
  "grafana",
  "jellyfin",
  "mysql",
];

/**
 * A stand-in for ListCatalogEntries that answers as the control plane does: the
 * entries a request's filters keep, searched in its language, in name order or
 * featured first, a page at a time, with how many match and each category's count
 * (ADR 0091). origins names the catalog each entry comes from, also each other
 * catalog's entry of an app, and installed how many times each app is installed here.
 */
export function catalogPages(
  entries: readonly (Entry | EntryInit)[],
  {
    also = [],
    origins = {},
    installed = {},
  }: {
    also?: readonly MessageInitShape<typeof CatalogAlternativeSchema>[];
    origins?: Readonly<Record<string, string>>;
    installed?: Readonly<Record<string, number>>;
  } = {},
): (req: ListCatalogEntriesRequest) => ListCatalogEntriesResponse {
  const all = entries.map((e) => ("$typeName" in e ? (e as Entry) : create(EntrySchema, e as EntryInit)));
  const theirs = (catalog: string, id: string) => {
    const a = also.find((x) => x.catalog === catalog && x.id === id);
    return a?.entry ? create(EntrySchema, a.entry) : undefined;
  };
  return (req) => {
    const query = req.search.trim().toLowerCase();
    const words = (e: Entry) => {
      const lang = req.language.split("-")[0] ?? "";
      const tags = Object.keys(e.translations);
      const tag = req.language.startsWith("en")
        ? undefined
        : (tags.find((t) => t === req.language) ?? tags.find((t) => t.split("-")[0] === lang));
      const said = (tag ? e.translations[tag]?.summary : "") || e.summary;
      return `${e.name} ${e.summary} ${said} ${e.category} ${e.features.join(" ")}`.toLowerCase();
    };
    const counts = new Map<string, number>();
    const offered = new Map<string, number>();
    let matched: Entry[] = [];
    for (const listed of all) {
      let e = listed;
      if (req.catalog) {
        const t = theirs(req.catalog, listed.id);
        if (t) e = t;
        else if ((origins[listed.id] ?? "") !== req.catalog) continue;
      }
      if (req.ids.length > 0 && !req.ids.includes(e.id)) continue;
      if (req.installed && !installed[e.id]) continue;
      offered.set(e.category, (offered.get(e.category) ?? 0) + 1);
      if (query && !words(e).includes(query) && !req.searchCategories.includes(e.category)) continue;
      counts.set(e.category, (counts.get(e.category) ?? 0) + 1);
      if (!req.category || e.category === req.category) matched.push(e);
    }
    if (req.order === CatalogOrder.FEATURED) {
      const rank = (id: string) => (FEATURED.includes(id) ? FEATURED.indexOf(id) : FEATURED.length);
      matched = [...matched].sort((a, b) => rank(a.id) - rank(b.id) || (installed[b.id] ?? 0) - (installed[a.id] ?? 0));
    }
    const offset = req.pageToken ? Number(req.pageToken) : 0;
    const end = req.pageSize > 0 ? Math.min(offset + req.pageSize, matched.length) : matched.length;
    const page = matched.slice(offset, end);
    return create(ListCatalogEntriesResponseSchema, {
      entries: page,
      also: also.filter((a) => page.some((e) => e.id === a.id)),
      total: matched.length,
      nextPageToken: end < matched.length ? String(end) : "",
      categories: [...offered.entries()]
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([category, n]) => ({ category, count: counts.get(category) ?? 0, listed: n })),
    });
  };
}
