// SPDX-License-Identifier: AGPL-3.0-only

/** A server as the fleet has it, for what its agent says of its CPU. */
interface Reported {
  architecture?: string | undefined;
}

/**
 * Why an app that runs on the architectures given can't go on a server, or undefined
 * where it can: "unknown" for a server that hasn't said its architecture yet, or the
 * server's own architecture where it isn't one of them. An app that names none runs
 * anywhere (ADR 0102). The control plane refuses the same; this only says it first.
 */
export function unfitOn(runsOn: readonly string[], server: Reported): string | undefined {
  if (runsOn.length === 0) return undefined;
  const arch = server.architecture ?? "";
  if (arch === "") return "unknown";
  return runsOn.includes(arch) ? undefined : arch;
}

/** What can't go on a server, and why, of apps that run on the architectures given, or undefined where all can. */
export interface Unfit {
  /** The first app that can't, by its name. */
  app: string;
  /** Its architectures, joined, such as "amd64". */
  runsOn: string;
  /** Why, as unfitOn says. */
  why: string;
}

/** The first of the apps given that can't go on a server, and why (#771). */
export function appsUnfitOn(
  apps: readonly { name: string; architectures: readonly string[] }[],
  server: Reported,
): Unfit | undefined {
  for (const app of apps) {
    const why = unfitOn(app.architectures, server);
    if (why !== undefined) return { app: app.name, runsOn: app.architectures.join(" or "), why };
  }
  return undefined;
}
