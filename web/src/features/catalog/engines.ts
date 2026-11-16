// SPDX-License-Identifier: AGPL-3.0-only

import type { Engine, Entry } from "@dockwell/api";
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
 * The engines New database makes, from the catalog's databases, in one request: the
 * control plane says which engine each entry makes, if any, so the tools and apps in
 * the category stay out, and every tile arrives at once (#723). Pending while the
 * catalog is read.
 */
export function useEngineGroups(): { groups: EngineGroup[]; pending: boolean } {
  const catalog = useCatalog({ category: "databases", withEngines: true });
  const { origins } = useOrigins();
  const groups = useMemo(() => {
    const engines = catalog.data?.engines ?? {};
    return groupEngines(
      (catalog.data?.entries ?? [])
        .filter((e) => e.category === "databases")
        .map((entry) => {
          const source = origins.get(entry.id);
          return { entry, engine: engines[entry.id], own: source === undefined || source.own };
        }),
    );
  }, [catalog.data, origins]);
  return { groups, pending: catalog.isPending };
}

/** The group an entry is in, by its ID. */
export function groupOf(groups: readonly EngineGroup[], id: string): EngineGroup | undefined {
  return groups.find((g) => g.entries.some((e) => e.id === id));
}
