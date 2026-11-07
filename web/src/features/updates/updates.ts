// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import {
  type DockwellRelease,
  PackageEffect,
  PackageManager,
  type PackageUpdate,
  type ServerUpdates,
  UpdateService,
} from "@dockwell/api";

/** How often an open page asks again while the server's agent reads its updates, and otherwise. */
const CHECKING_EVERY_MS = 2_000;
const ASK_EVERY_MS = 10 * 60_000;

/**
 * A server's package updates, while a page that shows them is open (ADR 0043). The
 * control plane asks the agent only when what it has is stale, so every page that
 * shows them shares this one answer.
 */
export function useServerUpdates(serverId: string) {
  return useQuery(
    UpdateService.method.getServerUpdates,
    { id: serverId },
    {
      staleTime: 30_000,
      retry: false,
      refetchInterval: (query) => (query.state.data?.updates?.checking ? CHECKING_EVERY_MS : ASK_EVERY_MS),
    },
  );
}

/** What waits on a server, sorted into what each screen shows. */
export interface UpdatesSummary {
  /** Security fixes, but Docker's engine. */
  security: PackageUpdate[];
  /** Everything else that waits, but Docker's engine. */
  other: PackageUpdate[];
  /** Docker's engine, which restarts every container unless it keeps them. */
  docker: PackageUpdate | undefined;
  /** Every update that waits. */
  total: number;
  /** Updates that take effect only once the server reboots. */
  rebooting: PackageUpdate[];
  /** Where the security fixes and the rest come from, each once, as the package manager says. */
  securityFrom: string[];
  otherFrom: string[];
}

export function summarize(updates: ServerUpdates | undefined): UpdatesSummary {
  const packages = updates?.packages ?? [];
  const docker = packages.find((p) => p.effect === PackageEffect.DOCKER_RESTART);
  const rest = packages.filter((p) => p !== docker);
  const security = rest.filter((p) => p.security);
  const other = rest.filter((p) => !p.security);
  return {
    security,
    other,
    docker,
    total: packages.length,
    rebooting: packages.filter((p) => p.effect === PackageEffect.REBOOT),
    securityFrom: origins(security),
    otherFrom: origins(other),
  };
}

function origins(ps: PackageUpdate[]): string[] {
  return [...new Set(ps.map((p) => p.origin).filter(Boolean))];
}

/** The package manager's own name, which the server's command uses. */
export function managerName(m: PackageManager | undefined): string {
  switch (m) {
    case PackageManager.APT:
      return "apt";
    case PackageManager.DNF:
      return "dnf";
    case PackageManager.APK:
      return "apk";
  }
  return "";
}

/** The command that installs what waits by hand, on a server with this package manager. */
export function upgradeCommand(m: PackageManager | undefined): string {
  switch (m) {
    case PackageManager.APT:
      return "sudo apt-get upgrade";
    case PackageManager.DNF:
      return "sudo dnf upgrade";
    case PackageManager.APK:
      return "apk upgrade";
  }
  return "";
}

/**
 * The available version split where it starts to differ from the installed one,
 * at the start of the part that changed, so "3.0.13-0ubuntu3.5" against
 * "3.0.13-0ubuntu3.4" shows its last part as what's new.
 */
export function splitVersion(installed: string, available: string): [same: string, changed: string] {
  if (!installed) return ["", available];
  let i = 0;
  while (i < installed.length && i < available.length && installed[i] === available[i]) i++;
  while (i > 0 && !/[.\-~+:]/.test(available[i - 1] ?? "")) i--;
  return [available.slice(0, i), available.slice(i)];
}

/** Dockwell's newest release against what runs, where both are known. */
export type ReleaseStanding = "current" | "behind" | "unknown";

export function releaseStanding(release: DockwellRelease | undefined): ReleaseStanding {
  const running = parseVersion(release?.controlPlane ?? "");
  const newest = parseVersion(release?.newest ?? "");
  if (!running || !newest) return "unknown";
  for (let i = 0; i < 3; i++) {
    const a = running[i] ?? 0;
    const b = newest[i] ?? 0;
    if (a !== b) return a > b ? "current" : "behind";
  }
  return "current";
}

/** A release's version, as its three numbers, or undefined for a build that isn't one. */
function parseVersion(v: string): number[] | undefined {
  const m = /^v?(\d+)\.(\d+)\.(\d+)$/.exec(v.trim());
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : undefined;
}

/** A version for people, without the "v" a build may carry. */
export function plainVersion(v: string): string {
  return v.replace(/^v(?=\d)/, "");
}

/** The distribution's own name, the first word of the OS's: "Ubuntu" for "Ubuntu 24.04.1 LTS". */
export function distributionOf(osName: string | undefined): string {
  return osName?.split(/\s+/)[0] ?? "";
}
