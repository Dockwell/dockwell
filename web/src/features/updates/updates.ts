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

/**
 * How often an open page asks again while the server's agent reads its updates, and
 * otherwise. Asking is cheap, as the control plane answers from what it read until
 * that's 10 minutes old, and it's how a page hears of an install someone else started.
 */
const CHECKING_EVERY_MS = 2_000;
const ASK_EVERY_MS = 15_000;

/**
 * A server's package updates, while a page that shows them is open (ADR 0043). The
 * control plane asks the agent only when what it has is stale, so every page that
 * shows them shares this one answer.
 */
export function useServerUpdates(serverId: string, { ask = true }: { ask?: boolean } = {}) {
  return useQuery(
    UpdateService.method.getServerUpdates,
    { id: serverId },
    {
      // Asked only for someone who may read them (ADR 0059): the tab and the card are left out for
      // anyone else, and the page says their role doesn't cover it, without asking.
      enabled: ask,
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
  /** What updating Docker installs: its engine, and what else restarts it, such as containerd. */
  dockerNames: string[];
  /** Every update that waits. */
  total: number;
  /** Updates that take effect only once the server reboots. */
  rebooting: PackageUpdate[];
  /** Where the security fixes and the rest come from, each once, as the package manager says. */
  securityFrom: string[];
  otherFrom: string[];
}

/** Docker's engine, by its package's name in Docker's repository, Debian and Ubuntu, and Fedora. */
const DOCKER_ENGINES = new Set(["docker-ce", "docker.io", "moby-engine", "docker-engine"]);

/** Whether a package is Docker's engine, which the Docker card shows rather than a list. */
export function isDockerEngine(name: string): boolean {
  return DOCKER_ENGINES.has(name);
}

export function summarize(updates: ServerUpdates | undefined): UpdatesSummary {
  const packages = updates?.packages ?? [];
  // The engine itself: containerd and runc restart Docker too, and stay in the lists with their own versions.
  const docker = packages.find((p) => DOCKER_ENGINES.has(p.name));
  // Docker's engine has a panel of its own, and a security fix in it counts as one.
  const security = packages.filter((p) => p.security);
  const other = packages.filter((p) => !p.security && p !== docker);
  return {
    security,
    other,
    docker,
    dockerNames: docker
      ? [
          docker.name,
          ...packages.filter((p) => p !== docker && p.effect === PackageEffect.DOCKER_RESTART).map((p) => p.name),
        ]
      : [],
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

/**
 * The command that installs what waits by hand, on a server with this package
 * manager: every update, as an upgrade takes, with what a new kernel brings in too.
 */
export function upgradeCommand(m: PackageManager | undefined): string {
  switch (m) {
    case PackageManager.APT:
      return "sudo apt-get upgrade --with-new-pkgs";
    case PackageManager.DNF:
      return "sudo dnf upgrade";
    case PackageManager.APK:
      return "apk upgrade";
  }
  return "";
}

export { splitVersion } from "../../lib/versions";

/** Dockwell's newest release against what runs, where both are known. */
export type ReleaseStanding = "current" | "behind" | "unknown";

export function releaseStanding(release: DockwellRelease | undefined): ReleaseStanding {
  const running = release?.controlPlane ?? "";
  const newest = release?.newest ?? "";
  if (!isRelease(running) || !isRelease(newest)) return "unknown";
  return compareVersions(running, newest) < 0 ? "behind" : "current";
}

const SEMVER = /^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$/;

/** Whether v is a release's version, as semver has them, and not a development build's. */
export function isRelease(v: string): boolean {
  return SEMVER.test(v.trim());
}

/** Orders two release versions as semver does, a release after its pre-releases. */
export function compareVersions(a: string, b: string): number {
  const ma = SEMVER.exec(a.trim());
  const mb = SEMVER.exec(b.trim());
  if (!ma || !mb) return a.localeCompare(b);
  for (let i = 1; i <= 3; i++) {
    const d = Number(ma[i]) - Number(mb[i]);
    if (d !== 0) return Math.sign(d);
  }
  const pa = ma[4] ?? "";
  const pb = mb[4] ?? "";
  if (pa === pb) return 0;
  if (!pa) return 1;
  if (!pb) return -1;
  const xa = pa.split(".");
  const xb = pb.split(".");
  for (let i = 0; i < Math.min(xa.length, xb.length); i++) {
    const x = xa[i] ?? "";
    const y = xb[i] ?? "";
    const nx = /^\d+$/.test(x);
    const ny = /^\d+$/.test(y);
    if (nx && ny && Number(x) !== Number(y)) return Math.sign(Number(x) - Number(y));
    if (nx !== ny) return nx ? -1 : 1;
    if (!nx && x !== y) return x < y ? -1 : 1;
  }
  return Math.sign(xa.length - xb.length);
}

/** Where Dockwell's releases are, unless a mirror says otherwise. */
export const GITHUB_RELEASES = "https://github.com/Dockwell/dockwell/releases";

/**
 * The command that installs Dockwell's release version on a server, or upgrades it
 * in place, from that release's own install script, as root, from releases, with
 * the mirror named where it isn't GitHub's. Undefined where either isn't known.
 */
export function installCommand(releases: string, version: string): string | undefined {
  const v = plainVersion(version);
  const base = releases.replace(/\/+$/, "");
  if (!base || !isRelease(v)) return undefined;
  const mirror = base === GITHUB_RELEASES ? "" : ` --mirror ${base}`;
  return `curl -fsSL ${base}/download/v${v}/install.sh | sudo sh -s -- --version ${v}${mirror}`;
}

/** A version for people, without the "v" a build may carry. */
export function plainVersion(v: string): string {
  return v.replace(/^v(?=\d)/, "");
}

/** The distribution's own name, the first word of the OS's: "Ubuntu" for "Ubuntu 24.04.1 LTS". */
export function distributionOf(osName: string | undefined): string {
  return osName?.split(/\s+/)[0] ?? "";
}
