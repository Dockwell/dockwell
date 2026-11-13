// SPDX-License-Identifier: AGPL-3.0-only

import { createQueryOptions, useTransport } from "@connectrpc/connect-query";
import { type CatalogPart, CatalogService, Engine, type Entry } from "@dockwell/api";
import { useQueries } from "@tanstack/react-query";
import { useMemo } from "react";

import { engineName } from "./needs";
import { useCatalog, useOrigins } from "./useCatalog";

/**
 * An engine Dockwell creates a database of, with the catalog's entries that make one:
 * the default first (Dockwell's own, then the newest stable version), so New
 * database shows one choice for each engine however many catalogs have it, and the
 * version, and the catalog, are picked after (#599).
 */
export interface EngineGroup {
  engine: Engine;
  /** What people call it, such as "PostgreSQL", whatever an entry calls itself. */
  name: string;
  entries: readonly Entry[];
}

/**
 * The engine an entry makes a database of: where everything it runs beside its
 * companions is that one engine's database, as PostgreSQL for Dockwell's postgres.
 * A tool, such as Adminer, or an app that brings a database of its own, such as
 * Metabase, makes none.
 */
export function engineOfEntry(entry: Entry, parts: readonly CatalogPart[]): Engine | undefined {
  const companions = new Set(entry.companions.map((c) => c.service));
  const own = parts.filter((part) => !companions.has(part.service));
  const engines = new Set(own.map((part) => part.engine));
  const [engine] = engines;
  return own.length > 0 && engines.size === 1 && engine !== undefined && engine !== Engine.UNSPECIFIED
    ? engine
    : undefined;
}

/** A version that's still being made, which a default passes over: a beta, a release candidate, or a tag that moves. */
const UNSTABLE = /(^|[^a-z])(alpha|beta|rc|dev|pre|preview|nightly|snapshot|canary|edge|latest|main|master)([^a-z]|$)/i;

/** A version's numbers, as 17.11 and 18.7-alpine read 17, 11 and 18, 7. */
function numbers(version: string): number[] {
  return (version.match(/^\D*(\d+(?:\.\d+)*)/)?.[1] ?? "").split(".").filter(Boolean).map(Number);
}

/** Orders versions newest first, a stable one before any still being made. */
export function newestFirst(a: string, b: string): number {
  const unstable = Number(UNSTABLE.test(a)) - Number(UNSTABLE.test(b));
  if (unstable !== 0) return unstable;
  const [x, y] = [numbers(a), numbers(b)];
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    const d = (y[i] ?? -1) - (x[i] ?? -1);
    if (d !== 0) return d;
  }
  return a.localeCompare(b);
}

/**
 * Groups the catalog's entries by the engine each makes, in the order Dockwell knows
 * the engines, each group's default first: Dockwell's own, then the newest stable.
 */
export function groupEngines(
  entries: readonly { entry: Entry; engine: Engine | undefined; own: boolean }[],
): EngineGroup[] {
  const by = new Map<Engine, { entry: Entry; own: boolean }[]>();
  for (const { entry, engine, own } of entries) {
    if (engine === undefined) continue;
    by.set(engine, [...(by.get(engine) ?? []), { entry, own }]);
  }
  return [...by.entries()]
    .sort(([a], [b]) => a - b)
    .map(([engine, list]) => ({
      engine,
      name: engineName(engine),
      entries: list
        .sort(
          (a, b) =>
            Number(b.own) - Number(a.own) ||
            newestFirst(a.entry.version, b.entry.version) ||
            a.entry.id.localeCompare(b.entry.id),
        )
        .map((e) => e.entry),
    }));
}

/**
 * The engines New database makes, from the catalog's databases: each entry's parts
 * say which engine it makes, if any, so the tools and apps in the category stay out.
 * Pending while the catalog, or an entry, is still being read.
 */
export function useEngineGroups(): { groups: EngineGroup[]; pending: boolean } {
  const transport = useTransport();
  const catalog = useCatalog();
  const { origins } = useOrigins();
  const entries = useMemo(
    () => (catalog.data?.entries ?? []).filter((e) => e.category === "databases"),
    [catalog.data],
  );
  const reads = useQueries({
    queries: entries.map((entry) => ({
      ...createQueryOptions(CatalogService.method.getCatalogEntry, { id: entry.id }, { transport }),
      staleTime: 60_000,
    })),
  });
  // A few dozen entries at most, so grouped each render.
  const groups = groupEngines(
    entries.map((entry, i) => {
      const got = reads[i]?.data;
      const source = origins.get(entry.id);
      return {
        entry,
        engine: got ? engineOfEntry(got.entry ?? entry, got.parts) : undefined,
        own: source === undefined || source.own,
      };
    }),
  );
  return { groups, pending: catalog.isPending || reads.some((r) => r.isPending) };
}

/** The group an entry is in, by its ID. */
export function groupOf(groups: readonly EngineGroup[], id: string): EngineGroup | undefined {
  return groups.find((g) => g.entries.some((e) => e.id === id));
}
