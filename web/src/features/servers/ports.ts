// SPDX-License-Identifier: AGPL-3.0-only

import { createQueryOptions, useTransport } from "@connectrpc/connect-query";
import {
  type CheckServerPortsResponse,
  ContainerService,
  Protocol,
  type ServerContainer,
  type ServerPortHeld,
  ServerService,
  type ServiceInfo,
} from "@dockwell/api";
import { type PortCheck, validPortNumber } from "@dockwell/ui";
import { useQueries } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";

import { appOf, nameOf } from "../apps/app";
import { useServices } from "../apps/useServices";
import { useMay } from "../auth/may";

/** What holds a port on a server, as far as Dockwell knows. */
export interface PortHolder {
  port: number;
  udp: boolean;
  /** Its name: an app's, a container's, the proxy's, or what the server says holds it. */
  name: string;
  kind: "app" | "container" | "proxy" | "process";
}

/**
 * The ports Dockwell's proxy takes on every server that serves a domain, and
 * keeps for one that may: 80 and 443 over TCP, and 443 over UDP for HTTP/3
 * (ingress.DefaultPorts).
 */
const PROXY_PORTS: readonly { port: number; udp: boolean }[] = [
  { port: 80, udp: false },
  { port: 443, udp: false },
  { port: 443, udp: true },
];

/** Where a free port is looked for first, as most apps that serve HTTP are published from 8080 up. */
export const FIRST_FREE = 8080;

/** How many ports from 8080, and from each a form watches, the server is asked about. */
const WINDOW = 16;
const AROUND = 4;
/** How long typing pauses before the server is asked again. */
const ASK_AFTER_MS = 400;

/** The ports to ask a server about: a window from 8080, and a few from each a form watches, 64 at most. */
export function portsToAsk(watch: readonly number[]): number[] {
  const out = new Set<number>();
  for (let p = FIRST_FREE; p < FIRST_FREE + WINDOW; p++) out.add(p);
  for (const w of watch) {
    if (!Number.isInteger(w) || w < 1 || w > 65535) continue;
    for (let p = w; p < Math.min(w + AROUND, 65536); p++) out.add(p);
  }
  return [...out].sort((a, b) => a - b).slice(0, 64);
}

/** What the servers said holds the ports asked, as holders. */
export function heldOnServers(answers: readonly (readonly ServerPortHeld[])[]): PortHolder[] {
  return answers.flatMap((list) =>
    list.filter((p) => p.held).map((p) => ({ port: p.port, udp: p.udp, name: p.by, kind: "process" as const })),
  );
}

/** What Dockwell's apps, the containers it didn't make, and its proxy hold on these servers. */
export function heldPorts(
  serverIds: readonly string[],
  services: readonly ServiceInfo[],
  containers: readonly (readonly ServerContainer[])[],
  except?: string,
): PortHolder[] {
  const on = new Set(serverIds);
  const out: PortHolder[] = PROXY_PORTS.map((p) => ({ ...p, name: "", kind: "proxy" as const }));
  for (const service of services) {
    if (service.id === except) continue;
    const app = appOf(service);
    if (!app || !service.deployments.some((d) => on.has(d.serverId))) continue;
    for (const p of app.ports) {
      out.push({ port: p.hostPort, udp: p.protocol === Protocol.UDP, name: nameOf(service), kind: "app" });
    }
  }
  for (const list of containers) {
    for (const c of list) {
      for (const p of c.ports) {
        out.push({ port: p.serverPort, udp: p.protocol === "udp", name: c.name, kind: "container" });
      }
    }
  }
  return out;
}

export interface ServerPorts {
  /** Whether what's known has been read: until then a port reads as being checked. */
  ready: boolean;
  /** What holds a port, if anything Dockwell knows of does. */
  holder: (port: number, udp?: boolean) => PortHolder | undefined;
  /**
   * The first free port from where it starts (8080 unless a form says, such as
   * the port the app listens on), passing over those the form has taken already.
   */
  free: (from?: number, udp?: boolean, taken?: readonly number[]) => number;
  /** What a field says of a typed port, with what holds it named as the screen calls it. */
  check: (value: string, udp: boolean, name: (holder: PortHolder) => string, taken?: readonly number[]) => PortCheck;
}

/**
 * What holds each port on the servers an app runs on, as Dockwell knows it: its
 * own apps' published ports, those of containers it didn't make (which only an
 * admin may list; for anyone else those go unseen), and its proxy's. A process
 * on the server outside Docker isn't known here, and the plan's check is what
 * finds that one. except leaves out an app's own ports, as a form that edits
 * them reads them as its own.
 */
export function useServerPorts(
  serverIds: readonly string[],
  except?: string,
  watch: readonly number[] = [],
): ServerPorts {
  const transport = useTransport();
  const services = useServices();
  // Only an admin may list the containers Dockwell didn't make, or ask the server what holds its
  // ports (ADR 0048); for anyone else those go unseen, and the plan's check still finds them.
  const may = useMay();
  const canList = may(ContainerService.method.listContainers).allowed;
  const canCheck = may(ServerService.method.checkServerPorts).allowed;
  // What the form watches settles a moment after typing pauses, so each key doesn't ask.
  const wanted = portsToAsk(watch).join(",");
  const [asking, setAsking] = useState(wanted);
  useEffect(() => {
    const timer = setTimeout(() => setAsking(wanted), ASK_AFTER_MS);
    return () => clearTimeout(timer);
  }, [wanted]);
  const asked = useQueries({
    queries: (canCheck ? serverIds : []).map((id) => ({
      ...createQueryOptions(
        ServerService.method.checkServerPorts,
        { id, ports: asking.split(",").map((p) => ({ port: Number(p) })) },
        { transport },
      ),
      retry: false,
      staleTime: 10_000,
      // Asking again keeps what was said meanwhile, so a port shown doesn't blank as the form changes.
      placeholderData: (previous: CheckServerPortsResponse | undefined) => previous,
    })),
  });
  const askedKey = asked.map((r) => r.dataUpdatedAt).join(",");
  const reads = useQueries({
    queries: (canList ? serverIds : []).map((server) => ({
      ...createQueryOptions(ContainerService.method.listContainers, { server }, { transport }),
      retry: false,
      staleTime: 10_000,
    })),
  });
  const containers = reads.map((r) => r.data?.containers ?? []);
  const containersKey = reads.map((r) => r.dataUpdatedAt).join(",");
  const ready = services.data !== undefined && reads.every((r) => !r.isPending) && asked.every((r) => !r.isPending);
  // biome-ignore lint/correctness/useExhaustiveDependencies: the containers and what servers said change when their reads do, as their keys say
  const held = useMemo(
    () => [
      ...heldPorts(serverIds, services.data ?? [], containers, except),
      ...heldOnServers(asked.map((r) => r.data?.ports ?? [])),
    ],
    [serverIds.join(","), services.data, containersKey, askedKey, except],
  );
  return useMemo(() => portsFrom(held, ready), [held, ready]);
}

/** What a form asks of the ports held. */
export function portsFrom(held: readonly PortHolder[], ready: boolean): ServerPorts {
  const holder = (port: number, udp = false) => held.find((h) => h.port === port && h.udp === udp);
  return {
    ready,
    holder,
    free: (from = FIRST_FREE, udp = false, taken = []) => {
      // A port under 1024 is a system's, such as 80 for the proxy: an app's is looked for from 8080.
      const start = from >= 1024 && from <= 65535 ? from : FIRST_FREE;
      for (let port = start; port <= 65535; port++) {
        if (!taken.includes(port) && !holder(port, udp)) return port;
      }
      return start;
    },
    check: (value, udp, name, taken = []) => {
      if (!validPortNumber(value)) return { state: "unknown" };
      if (!ready) return { state: "checking" };
      const port = Number(value);
      const by = holder(port, udp);
      if (by) return { state: "taken", by: name(by) };
      if (taken.includes(port)) return { state: "taken" };
      return { state: "free" };
    },
  };
}
