// SPDX-License-Identifier: AGPL-3.0-only

/** The time ranges, as the URL names them. */
export type LogRange = "15m" | "1h" | "24h";

/** Where a view of logs is, in its URL, so a link shows the same thing. */
export interface LogsSearch {
  range?: LogRange;
  /** A time range of its own, from and to, in milliseconds, as what a tab away for long didn't read. */
  from?: number;
  to?: number;
  /** Which levels show, such as "warn,error". Unset shows all. */
  levels?: string;
  /** Which replica shows, by its label. */
  replica?: string;
  /** Text to find. */
  q?: string;
  /** Search all that the servers keep, rather than what is loaded. */
  deep?: boolean;
  /** Which deploy shows, by the change that made it. */
  change?: string;
  /** Which deploy shows, by its revision. */
  revision?: number;
  /** Cuts long lines at the edge, rather than wrapping them. */
  nowrap?: boolean;
}

const RANGES: readonly LogRange[] = ["15m", "1h", "24h"];

/** Reads a view of logs from its URL, leaving out what isn't one. */
export function readLogsSearch(search: Record<string, unknown>): LogsSearch {
  const out: LogsSearch = {};
  const text = (key: string) => {
    const value = search[key];
    return typeof value === "string" && value !== "" ? value : undefined;
  };
  const range = text("range");
  if (range && (RANGES as readonly string[]).includes(range)) out.range = range as LogRange;
  for (const key of ["levels", "replica", "q", "change"] as const) {
    const value = text(key);
    if (value) out[key] = value;
  }
  const ms = (key: string) => {
    const value = Number(search[key]);
    return Number.isSafeInteger(value) && value > 0 ? value : undefined;
  };
  const from = ms("from");
  const to = ms("to");
  if (!out.range && from !== undefined && to !== undefined && from < to) {
    out.from = from;
    out.to = to;
  }
  if (search["deep"] === true) out.deep = true;
  if (search["nowrap"] === true) out.nowrap = true;
  const revision = Number(search["revision"]);
  if (Number.isSafeInteger(revision) && revision > 0) out.revision = revision;
  return out;
}
