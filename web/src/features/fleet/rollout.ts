// SPDX-License-Identifier: AGPL-3.0-only

import { Connection, type Server } from "@dockwell/api";

import { shortVersion } from "../shell/version";

/**
 * A fleet action's rollout as the panel previews it (ADR 0094), by the rule the
 * control plane plans with (internal/revision/waves.go): one server on its own
 * first when there's a canary, then waves of the batch, or of a quarter of the
 * servers rounded up when the batch is left to it. Fewer than two servers have no
 * waves. The plan has the last word: it leaves out what's offline and what needs
 * nothing, so its waves can be fewer.
 */
export function waveSizes(servers: number, canary: boolean, batch: number): number[] {
  if (servers < 2) return [];
  const out: number[] = [];
  let rest = servers;
  if (canary) {
    out.push(1);
    rest--;
  }
  const size = batch > 0 ? batch : Math.ceil(servers / 4);
  while (rest > 0) {
    const n = Math.min(size, rest);
    out.push(n);
    rest -= n;
  }
  return out;
}

/** The servers a fleet action goes to in order, as the plan orders them: by name, with the control server last. */
export function inOrder(servers: readonly Server[]): Server[] {
  return [...servers].sort((a, b) =>
    a.controlServer !== b.controlServer ? (a.controlServer ? 1 : -1) : a.name.localeCompare(b.name),
  );
}

/** Whether a server's agent is connected, so it can plan its step. */
export function connected(server: Server): boolean {
  return server.connection === Connection.CONNECTED;
}

/**
 * The release the control plane upgrades agents to: its own version, which its
 * server's agent runs, when that's a release; undefined for a development build,
 * which upgrades none.
 */
export function agentRelease(servers: readonly Server[]): string | undefined {
  const version = servers.find((s) => s.controlServer)?.agentBuild?.version;
  if (!version) return undefined;
  const short = shortVersion(version);
  return /^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(short) && !short.endsWith("dev") ? short : undefined;
}

/** Whether a server's agent runs version already, by the short version it says. */
export function runs(server: Server, version: string): boolean {
  const v = server.agentBuild?.version;
  return v !== undefined && shortVersion(v) === version;
}
