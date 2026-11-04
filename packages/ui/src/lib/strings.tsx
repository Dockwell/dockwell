// SPDX-License-Identifier: AGPL-3.0-only

import { createContext, type ReactNode, useContext } from "react";

import type { LogLevel } from "../components/LogStream/LogStream";
import type { PlanAction, PlanKind } from "../components/PlanDiff/PlanDiff";
import type { Status } from "./status";

/**
 * The words components say on their own: status names, accessible labels,
 * empty states and the like. English by default; an app passes translations
 * through UIStringsProvider, so this package needs no i18n library. Words
 * that depend on a count or a name are functions, so a translation can
 * reorder them and choose plural forms.
 */
export interface UIStrings {
  status: Readonly<Record<Status, string>>;
  /** Accessible name of a tag's remove button, given the tag's text. */
  remove: (what: string) => string;
  /** TextField: shown beside the label of a field that may stay empty. */
  optional: string;
  /** Sheet: accessible name of the close button. */
  close: string;
  /** Toast: accessible name of the dismiss button. */
  dismiss: string;
  scopeBar: {
    /** Accessible name of the scope navigation. */
    label: string;
    /** Accessible name of the lock on an environment that needs approval. */
    protectedEnvironment: string;
  };
  commandBar: {
    /** Accessible name of the command bar. */
    label: string;
    /** Default placeholder, also the input's accessible name. */
    placeholder: string;
    /** Accessible name of the list of results. */
    results: string;
    /** Shown when nothing matches the query. */
    empty: (query: string) => string;
    /** Footer hint after the ↑ ↓ key caps. */
    move: string;
    /** Footer hint after the ↵ key cap. */
    run: string;
    /** Footer text naming where commands apply. */
    scope: (scope: ReactNode) => ReactNode;
  };
  /** Sparkline: default accessible name, given the number of samples and the latest value. */
  sparkline: (samples: number, latest: number) => string;
  /** Metric: read after the metric's label to name its sparkline, as in "CPU trend". */
  trend: string;
  /** Table: shown when there are no rows and the caller gave no empty state. */
  tableEmpty: string;
  fleetGrid: {
    /** A number of servers: the legend total and a group's default meta. */
    servers: (count: number) => string;
    /** A legend entry, such as "3 degraded". */
    count: (count: number, status: Status) => string;
    /** Accessible name of a tile, given the server's name, its translated status and its CPU percentage. */
    tile: (name: string, status: string, cpu: number | null | undefined) => string;
    /** Accessible name of a group with no label. */
    group: string;
    /** The readout while no server is hovered, focused or selected. */
    hint: string;
    cpu: string;
    memory: string;
  };
  logStream: {
    /** The header while following a live stream. */
    live: string;
    /** The header after following stops. */
    paused: string;
    /** Button that resumes following. */
    follow: string;
    /** Button that stops following. */
    pause: string;
    /** Read before a line's text, for levels that carry a glyph. */
    level: Readonly<Record<Exclude<LogLevel, "info">, string>>;
  };
  planDiff: {
    /** The default action word on each resource's row. */
    action: Readonly<Record<PlanAction, string>>;
    /** The kind shown after a resource's name. */
    kind: Readonly<Record<PlanKind, string>>;
    /** The summary, each after a count: "3 to create". Replacements count apart from changes. */
    summary: Readonly<{ create: string; change: string; replace: string; destroy: string }>;
    /** Stands in for a secret value, which is never shown. */
    hidden: string;
    /** Read before an old and a new value. */
    from: string;
    to: string;
  };
  /** Timeline: the default undo button. */
  undo: string;
}

const count = (n: number) => n.toLocaleString("en");
const one = (n: number, singular: string, plural: string) => `${count(n)} ${n === 1 ? singular : plural}`;

export const englishStrings: UIStrings = {
  status: {
    healthy: "Healthy",
    degraded: "Degraded",
    down: "Down",
    deploying: "Deploying",
    stopped: "Stopped",
  },
  remove: (what) => `Remove ${what}`,
  optional: "Optional",
  close: "Close",
  dismiss: "Dismiss",
  scopeBar: {
    label: "Scope",
    protectedEnvironment: "Protected environment",
  },
  commandBar: {
    label: "Command bar",
    placeholder: "Search or run a command",
    results: "Results",
    empty: (query) => `Nothing matches “${query}”. Try a server, app or action name.`,
    move: "to move",
    run: "to run",
    scope: (scope) => (
      <>
        In <b>{scope}</b>
      </>
    ),
  },
  sparkline: (samples, latest) =>
    `Trend over ${one(samples, "sample", "samples")}, latest ${latest.toLocaleString("en", { maximumFractionDigits: 2 })}`,
  trend: "trend",
  tableEmpty: "Nothing here yet.",
  fleetGrid: {
    servers: (n) => one(n, "server", "servers"),
    count: (n, status) => `${count(n)} ${englishStrings.status[status].toLowerCase()}`,
    tile: (name, status, cpu) => (cpu == null ? `${name}, ${status}` : `${name}, ${status}, CPU ${cpu}%`),
    group: "Servers",
    hint: "Hover or focus a server to inspect it.",
    cpu: "CPU",
    memory: "Memory",
  },
  logStream: {
    live: "Live",
    paused: "Paused",
    follow: "Follow",
    pause: "Pause",
    level: { warn: "Warning", error: "Error" },
  },
  planDiff: {
    action: {
      create: "Create",
      update: "Update in place",
      replace: "Replace",
      destroy: "Destroy",
    },
    kind: {
      app: "app",
      service: "service",
      domain: "domain",
      secret: "secret",
      database: "database",
      job: "job",
      volume: "volume",
      backup: "backup",
      package: "package",
      server: "server",
      firewall: "firewall",
    },
    summary: { create: "to create", change: "to change", replace: "to replace", destroy: "to destroy" },
    hidden: "hidden",
    from: "from",
    to: "to",
  },
  undo: "Undo",
};

const StringsContext = createContext<UIStrings>(englishStrings);

export function UIStringsProvider({ strings, children }: { strings: UIStrings; children: ReactNode }) {
  return <StringsContext value={strings}>{children}</StringsContext>;
}

export function useUIStrings(): UIStrings {
  return useContext(StringsContext);
}
