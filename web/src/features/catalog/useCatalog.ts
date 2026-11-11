// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { type CatalogInstall, CatalogService, type CatalogSourceInfo } from "@dockwell/api";
import type { MessageDescriptor } from "@lingui/core";
import { msg } from "@lingui/core/macro";
import { useMemo } from "react";

/** The catalog, sorted by name. */
export function useCatalog() {
  return useQuery(CatalogService.method.listCatalogEntries, {});
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
  const origins = useMemo(() => {
    const out = new Map<string, CatalogSourceInfo>();
    for (const source of sources ?? []) for (const id of source.entries) out.set(id, source);
    return out;
  }, [sources]);
  return { ...query, origins };
}

/**
 * Whether an install is of an entry: of its ID, and from the catalog it comes from,
 * as an install upgrades only from there. Another catalog's app of the same ID is
 * another app.
 */
export function installOf(
  install: CatalogInstall,
  id: string,
  origins: ReadonlyMap<string, CatalogSourceInfo>,
): boolean {
  const source = origins.get(id);
  const pin = install.pin;
  return (
    pin?.entry === id &&
    (source
      ? pin.catalog === source.name && pin.repository === source.source?.repository && pin.path === source.source?.path
      : pin.catalog === "")
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
