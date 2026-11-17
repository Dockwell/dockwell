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
