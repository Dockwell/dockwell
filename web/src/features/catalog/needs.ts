// SPDX-License-Identifier: AGPL-3.0-only

import { type CatalogNeedOption, Engine, type Need } from "@dockwell/api";

// What an app needs, and how an install meets it (ADR 0084). For now each need is
// met by a new one beside the app: the entry's own service, or the catalog's entry
// for another engine the need takes, which GetCatalogEntry lists as its options.

/** Engines by their own names, which no language translates. */
const ENGINE_NAMES: Record<Engine, string> = {
  [Engine.UNSPECIFIED]: "",
  [Engine.POSTGRESQL]: "PostgreSQL",
  [Engine.MYSQL]: "MySQL",
  [Engine.MARIADB]: "MariaDB",
  [Engine.REDIS]: "Redis",
  [Engine.VALKEY]: "Valkey",
  [Engine.MONGODB]: "MongoDB",
  [Engine.CLICKHOUSE]: "ClickHouse",
};

/** The catalog's entry for each engine, as Dockwell's own catalog names them. */
const ENGINE_ENTRIES: Partial<Record<Engine, string>> = {
  [Engine.POSTGRESQL]: "postgres",
  [Engine.MYSQL]: "mysql",
  [Engine.MARIADB]: "mariadb",
  [Engine.REDIS]: "redis",
  [Engine.VALKEY]: "valkey",
  [Engine.MONGODB]: "mongodb",
  [Engine.CLICKHOUSE]: "clickhouse",
};

/** The catalog's entries for every engine, which a need may be met from. */
export const ENGINE_ENTRY_IDS: readonly string[] = Object.values(ENGINE_ENTRIES);

/**
 * The catalog's entries for the engines a need takes, of those the catalog has, in the
 * need's order: where to go when nothing here can make one yet.
 */
export function entriesFor(need: Need, has: (id: string) => boolean): { id: string; engine: Engine }[] {
  const out: { id: string; engine: Engine }[] = [];
  for (const accept of need.accepts) {
    const id = ENGINE_ENTRIES[accept.engine];
    if (id && has(id) && !out.some((e) => e.id === id)) {
      out.push({ id, engine: accept.engine });
    }
  }
  return out;
}

/**
 * How apps sharing a server keep apart: on Redis or Valkey, by key prefixes, where
 * the need sets one and every user on the server has one, as the control plane says,
 * or not at all, so they read and change each other's data; on other engines, each in
 * a database of its own (ADR 0084).
 */
export function keepsApart(option: CatalogNeedOption): "databases" | "prefixes" | "no" {
  if (option.engine !== Engine.REDIS && option.engine !== Engine.VALKEY) return "databases";
  return option.keepsApart ? "prefixes" : "no";
}

/** What people call an engine, such as "MariaDB". */
export function engineName(engine: Engine): string {
  return ENGINE_NAMES[engine] ?? "";
}

/**
 * What a need takes, in one line of names and comparators, such as
 * "MySQL ≥8.0 · MariaDB ≥10.6": names and numbers read the same in every language.
 */
export function takesLine(need: Need): string {
  return need.accepts
    .map((a) => {
      const versions = a.versions.replaceAll(">=", "≥").replaceAll("<=", "≤").trim();
      return versions ? `${engineName(a.engine)} ${versions}` : engineName(a.engine);
    })
    .join(" · ");
}

/** The options for each of an entry's needs, by the need's name, the default first. */
export function optionsByNeed(
  options: readonly CatalogNeedOption[],
): ReadonlyMap<string, readonly CatalogNeedOption[]> {
  const out = new Map<string, CatalogNeedOption[]>();
  for (const o of options) {
    const list = out.get(o.need) ?? [];
    list.push(o);
    out.set(o.need, list);
  }
  return out;
}

/**
 * What InstallCatalogEntryRequest.needs takes for each need: the choice picked, or the
 * default where none is. With onlyChanged, just those that differ from the default, as
 * the CLI command says no more than it must.
 */
export function needsRequest(
  needs: readonly Need[],
  options: ReadonlyMap<string, readonly CatalogNeedOption[]>,
  picked: Readonly<Record<string, string>>,
  onlyChanged = false,
): Record<string, string> {
  const out: Record<string, string> = {};
  for (const need of needs) {
    const first = options.get(need.name)?.[0]?.choice;
    const choice = picked[need.name] ?? first;
    if (!choice || (onlyChanged && choice === first)) continue;
    out[need.name] = choice;
  }
  return out;
}
