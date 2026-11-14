// SPDX-License-Identifier: AGPL-3.0-only

import { durationMs, timestampMs } from "@bufbuild/protobuf/wkt";
import { Connection, type Server } from "@dockwell/api";
import { type Status, useUIStrings } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import { useTimeWords } from "../../lib/time";
import { publicAddressOf } from "./network";
import type { FleetState } from "./useFleet";

const STATUS: Partial<Record<Connection, Status>> = {
  [Connection.CONNECTED]: "healthy",
  [Connection.UNREACHABLE]: "down",
  [Connection.PENDING]: "stopped",
};

/**
 * Past this share of its memory or CPU, a meter reads as a warning, as the mocks
 * have it. Whether the server is degraded for it is the control plane's to say,
 * once it has lasted (strained_since), so every page and the CLI agree.
 */
export const HIGH_USE = 90;

/**
 * A server's state as the design system draws it: connected is healthy, gone
 * quiet is down, not yet heard from is stopped. Connected with an agent that
 * can't read Docker, or strained, as the control plane says, is degraded. A
 * state this panel doesn't know, from a newer control plane, is stopped too. A
 * full server's strain is its strainedSince; a summary's, strained.
 */
export function serverStatus(server: {
  connection: Connection;
  dockerUnreadableSince?: unknown;
  strainedSince?: unknown;
  strained?: boolean;
}): Status {
  const connected = server.connection === Connection.CONNECTED;
  if (connected && server.dockerUnreadableSince !== undefined) return "degraded";
  if (connected && (server.strained ?? server.strainedSince !== undefined)) return "degraded";
  return STATUS[server.connection] ?? "stopped";
}

/**
 * What needs a look first, wherever servers are put in that order, so the Overview's lines, its readout and the
 * Servers page agree: one that's down, then a live one that's degraded, such as one that can't read Docker, before
 * one in no state the fleet knows or waiting for its agent.
 */
export const NEED: Record<Status, number> = {
  down: 0,
  degraded: 1,
  unknown: 2,
  stopped: 3,
  deploying: 4,
  running: 5,
  healthy: 6,
};

/** Servers, or their tiles, what needs a look first by the state each is drawn in, and in their own order otherwise. */
export function byNeed<T>(items: readonly T[], statusOf: (item: T) => Status): T[] {
  return [...items].sort((a, b) => NEED[statusOf(a)] - NEED[statusOf(b)]);
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
  // A server that joined and waits for its agent isn't one that stopped, whatever its glyph.
  const state =
    server.connection === Connection.PENDING ? t`Waiting for its agent` : strings.status[serverStatus(server)];
  return { uptime, up, phoneSub: [state, up].filter(Boolean).join(" · ") };
}

/** What the shell shows of one server: what changes when it is renamed, connects or drops, not with each sample. */
export interface ServerSummary {
  id: string;
  name: string;
  connection: Connection;
  /** When it was last heard from, for one that isn't connected, as only then does this hold still. */
  lastSeen?: number;
  /** When its agent connected, for one that's connected now. */
  connectedAt?: number;
  /** The address people reach it on, from its inventory. */
  address: string | undefined;
  /** Since when its agent can't read Docker, for one that's connected and can't. */
  dockerUnreadableSince?: number;
  /** True for one that's connected with memory or CPU past HIGH_USE. */
  strained?: boolean;
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
  /**
   * How many servers are in each state. Unknown counts what is in none of the others.
   * Docker counts the connected ones whose agent can't read Docker, and strained
   * the other connected ones short of memory or CPU, as the control plane says,
   * so a server is counted once, for the reason its row gives.
   */
  counts: { connected: number; down: number; waiting: number; unknown: number; docker: number; strained: number };
}

/** Sums up state for the shell, reusing what it can of the view before it so equal parts keep their identity. */
export function fleetView(state: FleetState, prev?: FleetView): FleetView {
  const servers = state.status === "connecting" ? undefined : summaries(state.servers, prev?.servers);
  const counts = { connected: 0, down: 0, waiting: 0, unknown: 0, docker: 0, strained: 0 };
  for (const server of servers ?? []) {
    if (server.connection === Connection.CONNECTED) {
      counts.connected++;
      // One reason each, as the server rows give it: Docker first, then strain.
      if (server.dockerUnreadableSince !== undefined) counts.docker++;
      else if (server.strained) counts.strained++;
    } else if (server.connection === Connection.UNREACHABLE) counts.down++;
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
      address: publicAddressOf(server) ?? primaryAddress(server.inventory?.addresses ?? []),
      ...(server.connection !== Connection.CONNECTED && server.lastSeenTime
        ? { lastSeen: timestampMs(server.lastSeenTime) }
        : {}),
      ...(server.connection === Connection.CONNECTED && server.connectTime
        ? { connectedAt: timestampMs(server.connectTime) }
        : {}),
      ...(server.connection === Connection.CONNECTED && server.dockerUnreadableSince
        ? { dockerUnreadableSince: timestampMs(server.dockerUnreadableSince) }
        : {}),
      ...(server.connection === Connection.CONNECTED && server.strainedSince ? { strained: true } : {}),
    };
    const old = prev?.[i];
    return old && sameSummary(old, summary) ? old : summary;
  });
  return prev && next.length === prev.length && next.every((s, i) => s === prev[i]) ? prev : next;
}

function sameSummary(a: ServerSummary, b: ServerSummary): boolean {
  return (
    a.id === b.id &&
    a.name === b.name &&
    a.connection === b.connection &&
    a.address === b.address &&
    a.lastSeen === b.lastSeen &&
    a.connectedAt === b.connectedAt &&
    a.dockerUnreadableSince === b.dockerUnreadableSince &&
    a.strained === b.strained
  );
}

function sameView(a: FleetView, b: FleetView): boolean {
  return (
    a.status === b.status &&
    a.servers === b.servers &&
    a.counts.connected === b.counts.connected &&
    a.counts.strained === b.counts.strained &&
    a.counts.down === b.counts.down &&
    a.counts.waiting === b.counts.waiting &&
    a.counts.unknown === b.counts.unknown &&
    a.counts.docker === b.counts.docker
  );
}

/** An address as its bytes, 4 or 16 of them, or undefined for what isn't one. */
function addressBytes(ip: string): number[] | undefined {
  if (!ip.includes(":")) {
    const parts = ip.split(".");
    if (parts.length !== 4 || parts.some((part) => !/^\d{1,3}$/.test(part) || Number(part) > 255)) return undefined;
    return parts.map(Number);
  }
  const last = ip.slice(ip.lastIndexOf(":") + 1);
  const dotted = last.includes(".") ? addressBytes(last) : undefined;
  if (last.includes(".") && !dotted) return undefined;
  const hex = dotted ? ip.slice(0, ip.lastIndexOf(":") + 1) : ip;
  const [head, tail, ...more] = hex.split("::");
  if (more.length > 0) return undefined;
  const groups = (side: string | undefined) => (side ? side.split(":").filter((group) => group !== "") : []);
  const before = groups(head);
  const after = groups(tail);
  const room = (dotted ? 6 : 8) - before.length - after.length;
  if (tail === undefined ? room !== 0 : room < 1) return undefined;
  const words = [...before, ...Array<string>(tail === undefined ? 0 : room).fill("0"), ...after];
  if (words.some((word) => !/^[0-9a-f]{1,4}$/i.test(word))) return undefined;
  const hexBytes = words.flatMap((word) => [Number.parseInt(word, 16) >> 8, Number.parseInt(word, 16) & 255]);
  return [...hexBytes, ...(dotted ?? [])];
}

/** A range as its bytes and how many of its bits count. */
function parseRange(range: string): { bytes: number[]; bits: number } {
  const [address = "", bits = ""] = range.split("/");
  return { bytes: addressBytes(address) ?? [], bits: Number(bits) };
}

/**
 * The ranges that decide whether the internet can reach an address, from the "Globally
 * Reachable" column of IANA's special-purpose registries. These two lists are
 * notReachable and reachableInside in internal/resource/address.go, which says what
 * they are, and why three documentation ranges are left out. The same table holds both.
 */
const NOT_REACHABLE = [
  "0.0.0.0/8",
  "10.0.0.0/8",
  "100.64.0.0/10",
  "127.0.0.0/8",
  "169.254.0.0/16",
  "172.16.0.0/12",
  "192.0.0.0/24",
  "192.0.2.0/24",
  "192.88.99.2/32",
  "192.168.0.0/16",
  "198.18.0.0/15",
  "224.0.0.0/4",
  "240.0.0.0/4",
  "::/128",
  "::1/128",
  "64:ff9b:1::/48",
  "100::/64",
  "100:0:0:1::/64",
  "2001::/23",
  "3fff::/20",
  "5f00::/16",
  "fc00::/7",
  "fe80::/10",
  "ff00::/8",
].map(parseRange);

const REACHABLE_INSIDE = [
  "192.0.0.9/32",
  "192.0.0.10/32",
  "2001:1::1/128",
  "2001:1::2/128",
  "2001:1::3/128",
  "2001:3::/32",
  "2001:4:112::/48",
  "2001:20::/28",
  "2001:30::/28",
].map(parseRange);

/** How long the longest range of a list that holds an address is, or -1. */
function mostSpecific(ranges: readonly { bytes: number[]; bits: number }[], bytes: readonly number[]): number {
  let longest = -1;
  for (const { bytes: range, bits } of ranges) {
    if (range.length !== bytes.length || bits <= longest) continue;
    const whole = Math.floor(bits / 8);
    const mask = 0xff & (0xff << (8 - (bits % 8)));
    const same = range.slice(0, whole).every((byte, i) => byte === bytes[i]);
    if (same && (bits % 8 === 0 || ((range[whole] ?? 0) & mask) === ((bytes[whole] ?? 0) & mask))) longest = bits;
  }
  return longest;
}

/**
 * Whether an address isn't one the internet can route to: private, shared, link-local,
 * loopback, or one of the other ranges IANA says isn't reachable, such as the benchmarking
 * one mihomo's TUN takes. resource.IsPublicAddress is the definition, and this mirrors it,
 * held to the same table by internal/resource/testdata/addresses.json. A server's name,
 * which stands in for an address it hasn't reported yet, isn't private.
 */
export function isPrivateAddress(ip: string): boolean {
  const [plain = "", zone] = ip.split("%");
  const bytes = addressBytes(plain);
  if (!bytes) return false;
  if (zone !== undefined) return true;
  const mapped =
    bytes.length === 16 && bytes.slice(0, 10).every((byte) => byte === 0) && bytes[10] === 255 && bytes[11] === 255;
  const unmapped = mapped ? bytes.slice(12) : bytes;
  return mostSpecific(REACHABLE_INSIDE, unmapped) < mostSpecific(NOT_REACHABLE, unmapped);
}

/** The address people reach a server on: its first public IPv4, then public IPv6, then whatever it has. */
export function primaryAddress(addresses: readonly string[]): string | undefined {
  const isPublic = addresses.filter((ip) => !isPrivateAddress(ip));
  return isPublic.find((ip) => !ip.includes(":")) ?? isPublic[0] ?? addresses[0];
}
