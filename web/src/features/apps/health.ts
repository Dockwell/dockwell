// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import {
  HealthCheckKind,
  HealthService,
  HealthState,
  ListHealthResponseSchema,
  type ServerHealth,
  type ServiceHealth,
} from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { type QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useReducer } from "react";

import { useOnline } from "../../lib/online";
import { ageOf, useWake } from "../../lib/wake";

/** The most ListHealth returns at once. */
const PAGE_SIZE = 1000;

/** How often the list reads how everything runs, while a screen that shows it is open and in front. */
const READ_EVERY_MS = 10_000;

/** How often it reads for a while after a change moved on, as what runs is what changed. */
const READ_SOON_MS = 2_000;
const SOON_FOR_MS = 30_000;

/**
 * How old what was last read may be and still stand, however reads are going: one
 * that fails, one that hangs, a browser gone offline. Past it, nobody here can say,
 * and a list that was green says so rather than stay green. It is three reads' worth.
 */
export const TRUST_FOR_MS = 30_000;

/** How long one page of a read may take, so a read nobody answers gives way to the next. */
export const READ_TIMEOUT_MS = 8_000;

/** How something runs, as far as anything checked. The record of what was deployed is AppState's. */
export type HealthKind = "healthy" | "running" | "starting" | "unhealthy" | "not-running" | "paused" | "unknown";

/**
 * How the design system draws each. Only a check that passed reads as healthy. What
 * nothing checks has a shape of its own, and so does what nobody can say anything of,
 * so neither reads green, and not knowing never reads as not running.
 */
const LOOKS: Readonly<Record<HealthKind, Status>> = {
  healthy: "healthy",
  running: "running",
  starting: "deploying",
  unhealthy: "degraded",
  "not-running": "down",
  paused: "stopped",
  unknown: "unknown",
};

const KINDS: Readonly<Record<HealthState, HealthKind>> = {
  [HealthState.UNSPECIFIED]: "unknown",
  [HealthState.UNKNOWN]: "unknown",
  [HealthState.NO_CHECK]: "running",
  [HealthState.STARTING]: "starting",
  [HealthState.HEALTHY]: "healthy",
  [HealthState.UNHEALTHY]: "unhealthy",
  [HealthState.NOT_RUNNING]: "not-running",
  [HealthState.PAUSED]: "paused",
};

/** What a check last said. */
export interface LastCheck {
  passed: boolean;
  output: string;
  at: Date | undefined;
}

/** How a service runs on one server. */
export interface RunsOnServer {
  serverId: string;
  serverName: string;
  kind: HealthKind;
  status: Status;
  /** Its agent isn't connected, or what it said is too old to stand. */
  stale: boolean;
  /** Since when it has been like this. */
  since: Date | undefined;
  /** What its check last said, where anything has. */
  last: LastCheck | undefined;
  /** How many checks in a row have failed. */
  failing: number;
  /** What checks it. */
  check: HealthCheckKind;
  /** How it last read, when it can't be said now, and when that was known. */
  lastSaid: HealthKind | undefined;
  lastKnown: Date | undefined;
  /** The name of the container this is about, as Docker has it. */
  container: string;
  /** When its agent last read the list of containers, on the server's clock. */
  scannedAt: Date | undefined;
  /** How many containers of it are there: two while one takes over. */
  containers: number;
  /** Since when its agent can't read Docker, on the server's clock, while it is connected and can't. */
  dockerUnreadableSince: Date | undefined;
}

/**
 * What the panel can say of how a service runs. "read" says it comes from the server:
 * "reading" and "failed" say nothing of the app, only that nobody here can tell yet.
 */
export interface AppHealth {
  kind: HealthKind;
  status: Status;
  read: "read" | "reading" | "failed";
  servers: readonly RunsOnServer[];
  /** What checks it, as far as any server says. */
  check: HealthCheckKind;
  /** Why it couldn't be read: this browser is offline, or the control plane doesn't answer. */
  why?: Unread;
  /** When it was last read, where it was, for one that can't be now. */
  heard?: Date;
}

/** Why the panel can't read how things run. */
export type Unread = "offline" | "unreachable";

const date = (at: Parameters<typeof timestampDate>[0] | undefined): Date | undefined =>
  at ? timestampDate(at) : undefined;

function onServer(server: ServerHealth): RunsOnServer {
  const kind = KINDS[server.state];
  // While two are there, the one this is about is the one that reads as the server does.
  const instance = server.instances.find((i) => i.state === server.state) ?? server.instances[0];
  const said = instance ? KINDS[instance.state] : undefined;
  const last = instance?.last;
  return {
    serverId: server.serverId,
    serverName: server.serverName,
    kind,
    status: LOOKS[kind],
    stale: server.stale,
    since: date(server.since),
    last: last ? { passed: last.passed, output: last.output, at: date(last.time) } : undefined,
    failing: instance?.failing ?? 0,
    check: instance?.check ?? HealthCheckKind.UNSPECIFIED,
    lastSaid: kind === "unknown" && said !== "unknown" ? said : undefined,
    lastKnown: kind === "unknown" ? date(instance?.confirmedAt ?? last?.time) : undefined,
    container: instance?.container ?? "",
    containers: server.instances.length,
    scannedAt: date(server.scannedAt),
    dockerUnreadableSince: date(server.dockerUnreadableSince),
  };
}

/** How a service runs, from what the server said of it. */
export function healthOf(service: ServiceHealth): AppHealth {
  const kind = KINDS[service.state];
  const servers = service.servers.map(onServer);
  return {
    kind,
    status: LOOKS[kind],
    read: "read",
    servers,
    check: servers.find((s) => s.check !== HealthCheckKind.UNSPECIFIED)?.check ?? HealthCheckKind.UNSPECIFIED,
  };
}

const unread = (read: "reading" | "failed", why?: Unread, heard?: Date): AppHealth => ({
  kind: "unknown",
  status: LOOKS.unknown,
  read,
  servers: [],
  check: HealthCheckKind.UNSPECIFIED,
  ...(why ? { why } : {}),
  ...(heard ? { heard } : {}),
});

/** The worst first, as a service across servers is as well as its worst, and a project as its worst app. */
const WORST_FIRST: readonly HealthKind[] = [
  "unhealthy",
  "not-running",
  "paused",
  "unknown",
  "starting",
  "running",
  "healthy",
];

/** How a whole project runs: its worst app, with what can't be read last, as a failed read says nothing of it. */
export function worstHealth(all: readonly AppHealth[]): AppHealth {
  let worst: AppHealth | undefined;
  for (const health of all) {
    if (!worst) worst = health;
    else if (health.read !== "read" && worst.read === "read") continue;
    else if (worst.read !== "read" && health.read === "read") worst = health;
    else if (WORST_FIRST.indexOf(health.kind) < WORST_FIRST.indexOf(worst.kind)) worst = health;
  }
  return worst ?? unread("reading");
}

/** Whether a person should look: it doesn't serve, or fails, or nobody can say. Not being able to read here is ours. */
export function healthNeedsALook(health: AppHealth): boolean {
  return (
    health.read === "read" &&
    (health.kind === "unhealthy" ||
      health.kind === "not-running" ||
      health.kind === "paused" ||
      health.kind === "unknown")
  );
}

/** Whether it runs and serves, as far as anyone can say, so what the record says of what it runs stands. */
export function runsAsFarAsAnyoneKnows(health: AppHealth): boolean {
  return (
    health.read === "read" && (health.kind === "healthy" || health.kind === "running" || health.kind === "starting")
  );
}

const soonUntil = new WeakMap<QueryClient, number>();

/** Reads how everything runs now, and often for a while, as a change moved on and what runs is what it changes. */
export function readHealthSoon(queryClient: QueryClient): void {
  soonUntil.set(queryClient, Date.now() + SOON_FOR_MS);
  void queryClient.invalidateQueries({
    queryKey: createConnectQueryKey({ schema: HealthService, cardinality: undefined }),
  });
}

/** How long an agent gets to report on what a change has just made, before what it says counts. */
const REPORT_WITHIN_MS = 30_000;

/**
 * An agent reads Docker on a pace of its own, so for a few seconds after a change a
 * report may be from before it: one that finds no container isn't saying that what the
 * change has just made doesn't run. Until it has looked, or has had time to, nobody here
 * can say, so a deploy's arrival and its row wait.
 */
export function waitingForReport(health: AppHealth, after: Date | undefined, now: number): boolean {
  if (!after || health.kind !== "not-running" || now - after.getTime() > REPORT_WITHIN_MS) return false;
  return health.servers.some(
    (s) => s.kind === "not-running" && (s.scannedAt === undefined || s.scannedAt.getTime() < after.getTime()),
  );
}

/** When a service last deployed anywhere, as far as its record says. */
function lastDeployed(service: ServiceRef): Date | undefined {
  let newest: Date | undefined;
  for (const d of service.deployments ?? []) {
    const at = date(d.updateTime);
    if (at && (!newest || at > newest)) newest = at;
  }
  return newest;
}

/** A service, as far as reading its health needs: its ID, and when it last deployed, where that's known. */
export interface ServiceRef {
  id: string;
  deployments?: readonly { updateTime?: Parameters<typeof timestampDate>[0] | undefined }[];
}

/** How everything runs, as far as this panel has read it. */
export interface HealthView {
  /** How a service runs. A deploy that has just ended waits for its agent to report, from after, or its record. */
  of(service: ServiceRef, after?: Date): AppHealth;
  /** Why nothing here can be read now, if it can't: what a screen says once, above what it shows. */
  unread: { why: Unread; heard: Date | undefined } | undefined;
  /** Whether a first read has come, or failed, so a screen can order what it shows by it. */
  settled: boolean;
}

/** When each client last read how things run, and when its read under way began, by a clock that only moves forward. */
const heardAt = new WeakMap<QueryClient, number>();
const readingSince = new WeakMap<QueryClient, number>();
/** When a read last failed, on the same clock, so a clock that steps can't make one look older than the last good read. */
const failedAt = new WeakMap<QueryClient, number>();

/** Renders again in the soonest of ms, so a reading that runs out of time says so on its own. */
function useRenderIn(...ms: number[]): void {
  const [, tick] = useReducer((n: number) => n + 1, 0);
  const next = Math.min(...ms.filter((m) => m >= 0));
  // Set again on each render, as what's due moves with what's read.
  useEffect(() => {
    if (!Number.isFinite(next)) return;
    const timer = setTimeout(tick, next + 1);
    return () => clearTimeout(timer);
  });
}

/**
 * How every service runs, read a page at a time and kept as one list, so a screen of
 * many apps makes one request, and every screen shares it. It reads on a pace of its
 * own, as the API has no stream of it, only while a screen that shows it is open and
 * in front, and soon after a change moves on (readHealthSoon). What it last read
 * stands for TRUST_FOR_MS, whatever the reads do, by a clock that doesn't step back
 * and one that counts a sleep (ageOf), and a read that takes longer than
 * READ_TIMEOUT_MS gives way. Past that it never shows what it read: while the first
 * read back is under way, as on a screen back from the background or a laptop that
 * woke, it reads as Checking, for that read's time at most, and then it says it
 * can't read, and why, which is never that the app doesn't run.
 */
export function useHealth(): HealthView {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const online = useOnline();
  const queryKey = createConnectQueryKey({
    schema: HealthService.method.listHealth,
    input: {},
    transport,
    cardinality: "finite",
  });
  const { data, dataUpdatedAt, fetchStatus, failureCount } = useQuery({
    queryKey,
    queryFn: async ({ signal }) => {
      // A read's first try starts its time; a try again after a failure doesn't.
      if ((queryClient.getQueryState(queryKey)?.fetchFailureCount ?? 0) === 0) {
        readingSince.set(queryClient, performance.now());
      }
      const client = createClient(HealthService, transport);
      const all = create(ListHealthResponseSchema);
      let pageToken = "";
      try {
        do {
          const page = await client.listHealth(
            { pageSize: PAGE_SIZE, pageToken },
            { signal, timeoutMs: READ_TIMEOUT_MS },
          );
          all.services.push(...page.services);
          pageToken = page.nextPageToken;
        } while (pageToken !== "");
      } catch (err) {
        failedAt.set(queryClient, performance.now());
        throw err;
      }
      heardAt.set(queryClient, performance.now());
      return all;
    },
    staleTime: 0,
    refetchInterval: () => ((soonUntil.get(queryClient) ?? 0) > Date.now() ? READ_SOON_MS : READ_EVERY_MS),
  });
  // Coming back into view, waking, or a clock that steps, looks again, and reads again,
  // as no timer that was waiting ran.
  const [, look] = useReducer((n: number) => n + 1, 0);
  useWake(() => {
    look();
    void queryClient.refetchQueries({ queryKey, type: "active" }, { cancelRefetch: false });
  });
  const age = data === undefined ? Number.POSITIVE_INFINITY : ageOf(dataUpdatedAt, heardAt.get(queryClient));
  const trusted = age <= TRUST_FOR_MS;
  // A read's first try under way, within its time, is Checking, and never what was read,
  // until a read has failed since the last that didn't: then nobody can say, and stays so,
  // rather than come and go with each read that tries again.
  const tried = performance.now() - (readingSince.get(queryClient) ?? Number.NEGATIVE_INFINITY);
  const reading =
    !trusted &&
    fetchStatus === "fetching" &&
    failureCount === 0 &&
    (failedAt.get(queryClient) ?? Number.NEGATIVE_INFINITY) <= (heardAt.get(queryClient) ?? Number.NEGATIVE_INFINITY) &&
    tried < READ_TIMEOUT_MS;
  useRenderIn(trusted ? TRUST_FOR_MS - age : Number.NaN, reading ? READ_TIMEOUT_MS - tried : Number.NaN);
  const why: Unread = online ? "unreachable" : "offline";
  const lastHeard = dataUpdatedAt > 0 ? dataUpdatedAt : undefined;
  const lost = !trusted && !reading;
  return useMemo(() => {
    const heard = lastHeard === undefined ? undefined : new Date(lastHeard);
    const byId = new Map(((trusted ? data?.services : undefined) ?? []).map((s) => [s.id, healthOf(s)]));
    return {
      unread: lost ? { why, heard } : undefined,
      settled: !reading,
      of(service, after) {
        if (reading) return unread("reading");
        if (lost) return unread("failed", why, heard);
        // A service made since the last read says nothing yet.
        const health = byId.get(service.id);
        if (!health) return unread("reading");
        return waitingForReport(health, after ?? lastDeployed(service), Date.now()) ? unread("reading") : health;
      },
    };
  }, [data, trusted, reading, lost, why, lastHeard]);
}
