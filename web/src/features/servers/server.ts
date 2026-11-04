// SPDX-License-Identifier: AGPL-3.0-only

import { durationMs } from "@bufbuild/protobuf/wkt";
import { Connection, type Server } from "@dockwell/api";
import { type Status, useUIStrings } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import { useTimeWords } from "../../lib/time";
import type { FleetState } from "./useFleet";

const STATUS: Partial<Record<Connection, Status>> = {
  [Connection.CONNECTED]: "healthy",
  [Connection.UNREACHABLE]: "down",
  [Connection.PENDING]: "stopped",
};

/**
 * A server's state as the design system draws it: connected is healthy, gone
 * quiet is down, not yet heard from is stopped. A state this panel doesn't
 * know, from a newer control plane, is stopped too.
 */
export function serverStatus(server: Pick<Server, "connection">): Status {
  return STATUS[server.connection] ?? "stopped";
}

/** How a page puts a server in words, from useServerWords. */
export interface ServerWords {
  /** How long it has been up, "3 days", while its agent reports. */
  uptime: string | undefined;
  /** The same for a line of facts, "up 3 days", or empty. */
  up: string;
  /** The phone header's line under its name: "Healthy · up 3 days". */
  phoneSub: string;
}

/**
 * A server's state and uptime in words, as the overview, its own page and
 * the server list say them. Uptime is from the last report, so it says
 * nothing once the agent stops reporting.
 */
export function useServerWords(server: Server): ServerWords {
  const { t } = useLingui();
  const strings = useUIStrings();
  const words = useTimeWords();
  const live = server.connection === Connection.CONNECTED;
  const uptime = live && server.stats?.uptime ? words.span(durationMs(server.stats.uptime)) : undefined;
  const up = uptime ? t`up ${uptime}` : "";
  return { uptime, up, phoneSub: [strings.status[serverStatus(server)], up].filter(Boolean).join(" · ") };
}

/** What the shell shows of one server: what changes when it is renamed, connects or drops, not with each sample. */
export interface ServerSummary {
  id: string;
  name: string;
  connection: Connection;
  /** The address people reach it on, from its inventory. */
  address: string | undefined;
}

/**
 * The fleet as the shell shows it: how many servers there are and how they
 * are, but not their numbers. Its parts stay the same objects from one stats
 * sample to the next, so the rail, top bar and command bar don't redraw for
 * each one.
 */
export interface FleetView {
  /** Whether the watch is live. While it reconnects, the rest is what it knew last. */
  status: FleetState["status"];
  /** Every server, or undefined until the first snapshot arrives. */
  servers: readonly ServerSummary[] | undefined;
  /** True from the second server on. A single-server user never sees fleet concepts before then. */
  fleet: boolean;
  /** The one server, while there is exactly one. */
  only: ServerSummary | undefined;
  /** How many servers are in each state. Unknown counts what is in none of the others. */
  counts: { connected: number; down: number; waiting: number; unknown: number };
}

/** Sums up state for the shell, reusing what it can of the view before it so equal parts keep their identity. */
export function fleetView(state: FleetState, prev?: FleetView): FleetView {
  const servers = state.status === "connecting" ? undefined : summaries(state.servers, prev?.servers);
  const counts = { connected: 0, down: 0, waiting: 0, unknown: 0 };
  for (const server of servers ?? []) {
    if (server.connection === Connection.CONNECTED) counts.connected++;
    else if (server.connection === Connection.UNREACHABLE) counts.down++;
    else if (server.connection === Connection.PENDING) counts.waiting++;
    else counts.unknown++;
  }
  const next: FleetView = {
    status: state.status,
    servers,
    fleet: (servers?.length ?? 0) > 1,
    only: servers?.length === 1 ? servers[0] : undefined,
    counts,
  };
  return prev && sameView(prev, next) ? prev : next;
}

function summaries(servers: readonly Server[], prev: readonly ServerSummary[] | undefined): readonly ServerSummary[] {
  const next = servers.map((server, i): ServerSummary => {
    const summary = {
      id: server.id,
      name: server.name,
      connection: server.connection,
      address: primaryAddress(server.inventory?.addresses ?? []),
    };
    const old = prev?.[i];
    return old && sameSummary(old, summary) ? old : summary;
  });
  return prev && next.length === prev.length && next.every((s, i) => s === prev[i]) ? prev : next;
}

function sameSummary(a: ServerSummary, b: ServerSummary): boolean {
  return a.id === b.id && a.name === b.name && a.connection === b.connection && a.address === b.address;
}

function sameView(a: FleetView, b: FleetView): boolean {
  return (
    a.status === b.status &&
    a.servers === b.servers &&
    a.counts.connected === b.counts.connected &&
    a.counts.down === b.counts.down &&
    a.counts.waiting === b.counts.waiting &&
    a.counts.unknown === b.counts.unknown
  );
}

/** Whether ip is in a private, shared or unique-local range, so not reachable from the internet. */
export function isPrivateAddress(ip: string): boolean {
  if (ip.includes(":")) return /^f[cd]/i.test(ip);
  const [a = 0, b = 0] = ip.split(".").map(Number);
  return (
    a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168) || (a === 100 && b >= 64 && b <= 127)
  );
}

/** The address people reach a server on: its first public IPv4, then public IPv6, then whatever it has. */
export function primaryAddress(addresses: readonly string[]): string | undefined {
  const isPublic = addresses.filter((ip) => !isPrivateAddress(ip));
  return isPublic.find((ip) => !ip.includes(":")) ?? isPublic[0] ?? addresses[0];
}
