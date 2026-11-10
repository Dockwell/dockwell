// SPDX-License-Identifier: AGPL-3.0-only

import { createQueryOptions, useTransport } from "@connectrpc/connect-query";
import { ContainerService, Protocol, type ServerContainer, type ServiceInfo } from "@dockwell/api";
import { type PortCheck, validPortNumber } from "@dockwell/ui";
import { useQueries } from "@tanstack/react-query";
import { useMemo } from "react";

import { appOf, nameOf } from "../apps/app";
import { useMay } from "../auth/may";
import { useServices } from "../apps/useServices";

/** What holds a port on a server, as far as Dockwell knows. */
export interface PortHolder {
  port: number;
  udp: boolean;
  /** Its name: an app's, a container's, or the proxy's. */
  name: string;
  kind: "app" | "container" | "proxy";
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
export function useServerPorts(serverIds: readonly string[], except?: string): ServerPorts {
  const transport = useTransport();
  const services = useServices();
  // Only an admin may list the containers Dockwell didn't make (ADR 0048); for anyone else they go unseen.
  const canList = useMay()(ContainerService.method.listContainers).allowed;
  const reads = useQueries({
    queries: (canList ? serverIds : []).map((server) => ({
      ...createQueryOptions(ContainerService.method.listContainers, { server }, { transport }),
      retry: false,
      staleTime: 10_000,
    })),
  });
  const containers = reads.map((r) => r.data?.containers ?? []);
  const containersKey = reads.map((r) => r.dataUpdatedAt).join(",");
  const ready = services.data !== undefined && reads.every((r) => !r.isPending);
  // biome-ignore lint/correctness/useExhaustiveDependencies: the containers change when their reads do, as containersKey says
  const held = useMemo(
    () => heldPorts(serverIds, services.data ?? [], containers, except),
    [serverIds.join(","), services.data, containersKey, except],
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
