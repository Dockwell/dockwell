// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { type ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Process, type ProcessList, ProcessService, type ServiceInfo } from "@dockwell/api";
import { useEffect, useState } from "react";

import { isSessionEnded, reasonOf } from "../../lib/errors";
import { keepTrying } from "../../lib/retry";

/** How often the control plane reads the list again, as internal/control/processes has it. */
export const PROCESSES_EVERY_MS = 2_000;

const BACKOFF = { minMs: 1_000, maxMs: 15_000 };

export interface ProcessesState {
  /** The latest list, while there is one. */
  list: ProcessList | undefined;
  /** Why the watch failed last, until it hears again. */
  error: ConnectError | undefined;
}

/**
 * What runs on a server, as its agent reads it every 2 s while this is open
 * (ADR 0041). The watch reconnects after a failure, and stops for good on one
 * trying again won't fix: an agent too old to list processes, or a session that
 * ended.
 */
export function useProcesses(serverId: string, enabled = true): ProcessesState {
  const transport = useTransport();
  const [state, setState] = useState<ProcessesState>({ list: undefined, error: undefined });
  useEffect(() => {
    if (!enabled) return;
    const abort = new AbortController();
    const client = createClient(ProcessService, transport);
    void keepTrying(
      abort.signal,
      BACKOFF,
      async (connected, signal) => {
        for await (const res of client.watchProcesses({ serverId }, { signal })) {
          connected();
          setState({ list: res.processes, error: undefined });
        }
        return undefined;
      },
      (err) => {
        const error = err as ConnectError;
        setState((s) => ({ ...s, error }));
        return isSessionEnded(err) || reasonOf(err) === "agent_outdated" || reasonOf(err) === "not_found"
          ? "done"
          : undefined;
      },
    );
    return () => abort.abort();
  }, [transport, serverId, enabled]);
  return state;
}

/** What a process belongs to, for the line under its name. */
export type Owner =
  | { kind: "dockwell" }
  | { kind: "app"; name: string; id: string }
  | { kind: "container"; name: string }
  | { kind: "unit"; name: string }
  | { kind: "none" };

/** The app a container runs, by its Compose labels: Dockwell's project is its environment's ID in lower case. */
export function appOfContainer(
  project: string,
  service: string,
  services: readonly ServiceInfo[],
): ServiceInfo | undefined {
  if (!project || !service) return undefined;
  return services.find(
    (s) => s.resource?.name === service && (s.resource?.scope ?? "").toLowerCase() === project.toLowerCase(),
  );
}

/** What a process belongs to: Dockwell itself, an app, a container, a systemd unit, or nothing that says. */
export function ownerOf(process: Process, services: readonly ServiceInfo[]): Owner {
  if (process.protected === "dockwell") return { kind: "dockwell" };
  const c = process.container;
  if (c) {
    const app = appOfContainer(c.composeProject, c.composeService, services);
    if (app?.resource) return { kind: "app", name: app.resource.name, id: app.id };
    return { kind: "container", name: c.name || c.id.slice(0, 12) };
  }
  if (process.unit) return { kind: "unit", name: process.unit.replace(/\.service$/, "") };
  return { kind: "none" };
}

/** How the list sorts, and which way. */
export type SortKey = "cpu" | "memory" | "pid" | "name" | "uptime";
export interface Sort {
  key: SortKey;
  descending: boolean;
}

/** How each column sorts first: numbers from the most, names and PIDs from the start. */
export const FIRST_WAY: Readonly<Record<SortKey, boolean>> = {
  cpu: true,
  memory: true,
  pid: false,
  name: false,
  uptime: true,
};

const names = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });

/** The processes in the order sort says, ties broken by CPU, then PID, so rows don't swap as they update. */
export function sortProcesses(list: readonly Process[], sort: Sort): Process[] {
  const started = (p: Process) => (p.startTime ? timestampMs(p.startTime) : Number.POSITIVE_INFINITY);
  const by = (a: Process, b: Process): number => {
    switch (sort.key) {
      case "cpu":
        return a.cpuPercent - b.cpuPercent;
      case "memory":
        return Number(a.memoryBytes - b.memoryBytes);
      case "pid":
        return a.pid - b.pid;
      case "name":
        return names.compare(a.name, b.name);
      case "uptime":
        // Running longest is the most uptime.
        return started(b) - started(a);
    }
  };
  return [...list].sort((a, b) => {
    const first = by(a, b);
    if (first !== 0) return sort.descending ? -first : first;
    return b.cpuPercent - a.cpuPercent || a.pid - b.pid;
  });
}

/** Whether a process matches what someone typed: its name, PID, user, command, or what it belongs to. */
export function matchesProcess(process: Process, owner: Owner, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  const owned = owner.kind === "app" || owner.kind === "container" || owner.kind === "unit" ? owner.name : "";
  const container = process.container?.name ?? "";
  return [process.name, String(process.pid), process.user, process.command, owned, container].some((field) =>
    field.toLowerCase().includes(q),
  );
}

/** A group of processes: what one container or systemd unit runs, added up. */
export interface ProcessGroup {
  key: string;
  owner: Owner;
  processes: Process[];
  cpuPercent: number;
  memoryBytes: bigint;
}

/** Processes grouped by what they belong to, as Containers or Services shows them, busiest first. */
export function groupProcesses(
  list: readonly Process[],
  services: readonly ServiceInfo[],
  by: "containers" | "services",
): ProcessGroup[] {
  const groups = new Map<string, ProcessGroup>();
  for (const p of list) {
    const key = by === "containers" ? (p.container?.id ?? "") : p.container ? "" : p.unit;
    if (!key) continue;
    let group = groups.get(key);
    if (!group) {
      group = { key, owner: ownerOf(p, services), processes: [], cpuPercent: 0, memoryBytes: 0n };
      if (group.owner.kind === "dockwell" && p.unit)
        group.owner = { kind: "unit", name: p.unit.replace(/\.service$/, "") };
      groups.set(key, group);
    }
    group.processes.push(p);
    group.cpuPercent += p.cpuPercent;
    group.memoryBytes += p.memoryBytes;
  }
  return [...groups.values()].sort(
    (a, b) => b.cpuPercent - a.cpuPercent || Number(b.memoryBytes - a.memoryBytes) || a.key.localeCompare(b.key),
  );
}
