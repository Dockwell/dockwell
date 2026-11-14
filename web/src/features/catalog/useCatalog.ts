// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { useInfiniteQuery, useQuery } from "@connectrpc/connect-query";
import {
  type CatalogInstall,
  CatalogOrder,
  CatalogService,
  type CatalogSourceInfo,
  type Entry,
  type ListCatalogEntriesRequestSchema,
} from "@dockwell/api";
import type { I18n, MessageDescriptor } from "@lingui/core";
import { msg } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { keepPreviousData } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useState } from "react";

import { sourcesWords } from "./SourceFormats";

/** What a screen asks of the catalog: a page of it, or a few entries by their IDs, a category or a catalog. */
export type CatalogRequest = MessageInitShape<typeof ListCatalogEntriesRequestSchema>;

/**
 * The entries of the catalog a request names, all that match in one answer unless it
 * asks for a page, sorted by name: for a screen that needs a few, by IDs, a category
 * or a catalog. The Catalog page and New app read it a page at a time (ADR 0092).
 */
export function useCatalog(request: CatalogRequest, options: { enabled?: boolean } = {}) {
  return useQuery(CatalogService.method.listCatalogEntries, request, { staleTime: 30_000, ...options });
}

/** How many entries a page of the catalog holds: rows of cards on any screen, a few screens' worth. */
export const CATALOG_PAGE = 48;

/** What narrows a page of the catalog, as the address keeps it. */
export interface CatalogFilters {
  q?: string | undefined;
  category?: string | undefined;
  catalog?: string | undefined;
  installed?: boolean | undefined;
}

/** A value once it has stayed the same for ms, so a page isn't asked for at each letter typed. */
export function useSettled<T>(value: T, ms: number): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(timer);
  }, [value, ms]);
  return settled;
}

/**
 * The categories whose names, in the reader's language, have the words: the control
 * plane translates nothing, so the panel says which it means (ADR 0092).
 */
export function categoriesNamed(words: string, i18n: I18n): string[] {
  const query = words.trim().toLowerCase();
  if (query === "") return [];
  return Object.entries(CATEGORIES)
    .filter(([, label]) => i18n._(label).toLowerCase().includes(query))
    .map(([category]) => category);
}

/**
 * The catalog a page at a time, as filters narrow it, searched on the control plane
 * in the reader's language, with how many match and how many each category has. The
 * pages stay in the query cache, so a way back finds the grid as it was; while new
 * filters are read, the entries of the old ones stay (ADR 0092).
 */
export function useCatalogPages(
  filters: CatalogFilters,
  { pageSize = CATALOG_PAGE, order = CatalogOrder.UNSPECIFIED }: { pageSize?: number; order?: CatalogOrder } = {},
) {
  const { i18n } = useLingui();
  const search = useSettled((filters.q ?? "").trim(), 200);
  const query = useInfiniteQuery(
    CatalogService.method.listCatalogEntries,
    {
      pageSize,
      pageToken: "",
      search,
      searchCategories: categoriesNamed(search, i18n),
      language: i18n.locale,
      category: filters.category ?? "",
      catalog: filters.catalog ?? "",
      installed: filters.installed ?? false,
      order,
    },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (page) => page.nextPageToken || undefined,
      placeholderData: keepPreviousData,
      staleTime: 30_000,
    },
  );
  const pages = query.data?.pages;
  // One of each, as an entry can come twice where the catalog changed between pages.
  const entries = useMemo(() => {
    if (!pages) return undefined;
    const seen = new Set<string>();
    return pages.flatMap((page) => page.entries).filter((e) => !seen.has(e.id) && seen.add(e.id) !== undefined);
  }, [pages]);
  // Each app's other catalogs, by its ID, for the entries of the pages read.
  const also = useMemo(() => {
    const out = new Map<string, string[]>();
    for (const a of pages?.flatMap((page) => page.also) ?? []) out.set(a.id, [...(out.get(a.id) ?? []), a.catalog]);
    return out as ReadonlyMap<string, readonly string[]>;
  }, [pages]);
  return {
    ...query,
    entries,
    also,
    total: pages?.[0]?.total,
    categories: pages?.[0]?.categories ?? [],
    // Words typed that the page hasn't been asked for yet.
    settling: search !== (filters.q ?? "").trim(),
  };
}

/** The catalogs Dockwell reads apps from beside its own, each with what it shows and leaves out. */
export function useSources() {
  return useQuery(CatalogService.method.listCatalogSources, {});
}

/**
 * Where each entry comes from, by its ID. An absent ID is Dockwell's own only
 * once the sources have been read; a failed read must not imply that it is.
 */
export function useOrigins() {
  const query = useSources();
  const sources = query.data?.sources;
  const { origins, also } = useMemo(() => {
    const origins = new Map<string, CatalogSourceInfo>();
    // The sources that have an app a catalog before them lists, as another choice of it (ADR 0078).
    const also = new Map<string, CatalogSourceInfo[]>();
    for (const source of sources ?? []) {
      for (const id of source.entries) origins.set(id, source);
      for (const id of source.also) also.set(id, [...(also.get(id) ?? []), source]);
    }
    return { origins, also };
  }, [sources]);
  return { ...query, origins, also };
}

/**
 * The source an entry comes from when it's another catalog's, to weigh as one: none
 * for Dockwell's own, which a source reads too (ADR 0077), or while it isn't known.
 */
export function stranger(origins: ReadonlyMap<string, CatalogSourceInfo>, id: string): CatalogSourceInfo | undefined {
  const source = origins.get(id);
  return source && !source.own ? source : undefined;
}

/**
 * Whether an install is of an entry: of its ID, and from a catalog that has the app,
 * the one it's listed from or another that has the same app, as an install upgrades
 * only from where it came from. Another catalog's app of the same ID is another app.
 */
export function installOf(
  install: CatalogInstall,
  id: string,
  origins: ReadonlyMap<string, CatalogSourceInfo>,
  also?: ReadonlyMap<string, readonly CatalogSourceInfo[]>,
): boolean {
  const pin = install.pin;
  if (pin?.entry !== id) return false;
  const from = (source: CatalogSourceInfo) =>
    pin.catalog === source.name && pin.repository === source.source?.repository && pin.path === source.source?.path;
  const source = origins.get(id);
  return (source ? from(source) : pin.catalog === "") || (also?.get(id) ?? []).some(from);
}

/**
 * How a person says each catalog, by its source's ID: "Dockwell's own" for the source
 * that reads Dockwell/catalog (ADR 0077) and for the catalog built into older builds
 * (the empty ID), a store's name for a store at its own repository, and otherwise its
 * repository's owner and name. Never the ID, which a tooltip keeps.
 */
export function useSourceNames(): (name: string) => string {
  const { t } = useLingui();
  const sources = useSources().data?.sources;
  const { words, own } = useMemo(() => {
    const others = (sources ?? []).filter((s) => !s.own);
    return { words: sourcesWords(others), own: new Set((sources ?? []).filter((s) => s.own).map((s) => s.name)) };
  }, [sources]);
  return useCallback(
    (name: string) => (name === "" || own.has(name) ? t`Dockwell's own` : (words.get(name) ?? name)),
    [words, own, t],
  );
}

/** What's installed from the catalog. A change applied in the plan sheet reads it again. */
export function useInstalls() {
  return useQuery(CatalogService.method.listCatalogInstalls, {});
}

/** How many installs have a newer release out. */
export function useUpgrades(): number {
  return (useInstalls().data?.installs ?? []).filter((install) => install.upgrade).length;
}

/** The install an environment's services belong to, if they're one. */
export function installIn(installs: readonly CatalogInstall[], environment: string): CatalogInstall | undefined {
  return installs.find((install) => install.environmentId === environment);
}

/** The words for the categories the catalog's entries use. One that isn't here shows as it's written. */
const CATEGORIES: Readonly<Record<string, MessageDescriptor>> = {
  ai: msg`AI`,
  analytics: msg`Analytics`,
  automation: msg`Automation`,
  cms: msg`CMS`,
  databases: msg`Databases`,
  "developer tools": msg`Developer tools`,
  media: msg`Media`,
  monitoring: msg`Monitoring`,
  networking: msg`Networking`,
  notes: msg`Notes`,
  other: msg({ message: "Other", context: "catalog category" }),
  security: msg`Security`,
  storage: msg`Storage`,
};

/** What a person reads for a category. */
export function categoryLabel(category: string): MessageDescriptor | string {
  return CATEGORIES[category] ?? category;
}

/** Two letters that stand for an app: its name's first, as a logo belongs to its project (ADR 0029). */
export function monogram(name: string): string {
  const letters = name.replace(/[^\p{L}\p{N}]/gu, "");
  return `${letters.charAt(0).toUpperCase()}${letters.charAt(1)}`;
}

/** What an entry says, in one language: its line, its paragraphs and its README. */
export interface EntryWords {
  summary: string;
  description: string;
  readme: string;
}

/**
 * What each entry says in the reader's language, where its catalog writes it there,
 * field by field, and otherwise in its own (ADR 0083). The catalog's list carries only
 * each language's summary; an app's page has the rest.
 */
export function useEntryWords(): (entry: Entry | undefined) => EntryWords {
  const { i18n } = useLingui();
  const locale = i18n.locale;
  return useCallback(
    (entry) => {
      const own = { summary: entry?.summary ?? "", description: entry?.description ?? "", readme: entry?.readme ?? "" };
      if (!entry || locale.startsWith("en")) return own;
      const tags = Object.keys(entry.translations);
      const lang = locale.split("-")[0] ?? locale;
      const tag = tags.find((t) => t === locale) ?? tags.find((t) => t.split("-")[0] === lang);
      const words = tag ? entry.translations[tag] : undefined;
      if (!words) return own;
      return {
        summary: words.summary || own.summary,
        description: words.description || own.description,
        // A README in the reader's language takes the place of the description and README in another.
        readme: words.readme || (words.description ? "" : own.readme),
      };
    },
    [locale],
  );
}
