// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import {
  Connection,
  type Container,
  DeploymentState,
  type ServiceDeployment,
  type ServiceInfo,
  type ServiceRevision,
} from "@dockwell/api";
import type { Status } from "@dockwell/ui";

import type { ServerSummary } from "../servers/server";

/** What a service runs, as the first screens know it: an app from an image. */
export function appOf(service: ServiceInfo): Container | undefined {
  return service.spec?.runs.case === "app" ? service.spec.runs.value : undefined;
}

/** The image a service runs, or empty for a kind that has none. */
export function imageOf(service: ServiceInfo): string {
  return appOf(service)?.image ?? "";
}

export function nameOf(service: ServiceInfo): string {
  return service.resource?.name ?? service.id;
}

/** The environment a service lives in, by ID. */
export function environmentOf(service: ServiceInfo): string {
  return service.resource?.scope ?? "";
}

/** When a service's latest revision was made, which is when it last deployed. */
export function deployedAt(service: ServiceInfo): Date {
  return service.updateTime ? timestampDate(service.updateTime) : new Date(0);
}

/** Where an app stands, from its latest revision's deployments and whether their servers answer. */
export type AppStateKind =
  /** Every server has the latest revision, and it passed the check its step made. */
  | "live"
  /** A change is applying it, and a server is at work on it. */
  | "deploying"
  /** A change is applying it, and a server it needs isn't connected. */
  | "waiting"
  /** A step failed, and it isn't rolled back yet. */
  | "failed"
  /** It was live, but a server it runs on isn't connected, so its state isn't known. */
  | "offline"
  /** Its last deploy failed, and Dockwell put back what ran before. */
  | "rolled-back"
  /** No server has it, as when each server it ran on was removed. */
  | "unplaced";

export interface AppState {
  kind: AppStateKind;
  /** How the design system draws it. */
  status: Status;
  /** The servers the state is about, by name: those that failed, wait or are offline. */
  servers: string[];
  /** Why a deployment failed, when one did. */
  error: string;
}

const LOOKS: Readonly<Record<AppStateKind, Status>> = {
  live: "healthy",
  deploying: "deploying",
  waiting: "degraded",
  failed: "down",
  offline: "degraded",
  "rolled-back": "degraded",
  unplaced: "stopped",
};

/**
 * Where an app stands. It is honest about what Dockwell knows: live means the
 * revision deployed and passed the check its step made, not that the app is
 * healthy now, which comes with rollouts (#28). A server the fleet doesn't
 * list, or doesn't yet, says nothing either way.
 */
export function appState(
  service: Pick<ServiceInfo, "deployments" | "rolledBackChangeId">,
  servers: readonly ServerSummary[] | undefined,
): AppState {
  const placed = service.deployments;
  const away = (d: ServiceDeployment) => {
    const server = servers?.find((s) => s.id === d.serverId);
    return server !== undefined && server.connection !== Connection.CONNECTED;
  };
  const state = (kind: AppStateKind, on: readonly ServiceDeployment[] = [], error = ""): AppState => ({
    kind,
    status: LOOKS[kind],
    servers: on.map((d) => d.serverName),
    error,
  });
  if (placed.length === 0) return state("unplaced");
  const failed = placed.filter((d) => d.state === DeploymentState.FAILED);
  if (failed.length > 0) return state("failed", failed, failed.find((d) => d.error !== "")?.error ?? "");
  const pending = placed.filter((d) => d.state === DeploymentState.PENDING);
  const stuck = pending.filter(away);
  if (stuck.length > 0) return state("waiting", stuck);
  if (pending.length > 0) return state("deploying", pending);
  const offline = placed.filter(away);
  if (offline.length > 0) return state("offline", offline);
  return state(service.rolledBackChangeId !== "" ? "rolled-back" : "live");
}

/** The order of concern: what needs you first, what is moving next, then the calm. */
const CONCERN: Readonly<Record<AppStateKind, number>> = {
  failed: 0,
  waiting: 1,
  offline: 2,
  "rolled-back": 3,
  deploying: 4,
  unplaced: 5,
  live: 6,
};

/** Which of two states is the more concerning. */
export function worse(a: AppState, b: AppState): AppState {
  return CONCERN[b.kind] < CONCERN[a.kind] ? b : a;
}

/**
 * What the list shows as one thing: one app, or a stack of the services that
 * share a network or a volume in an environment. A Compose file's services
 * share its default network unless it says otherwise, so a five-service
 * stack is one group, and it stays one after an edit, an import that adds a
 * service, or a rollback of one of them: a group is what is connected now,
 * not where each came from. See docs/adr/0026-the-app-screens.md.
 */
export interface AppGroup {
  /** Stays the same as long as its oldest service does. */
  key: string;
  environment: string;
  /** By name. */
  services: ServiceInfo[];
  /** More than one service. */
  stack: boolean;
}

/** What connects a service to others: the environment's networks and volumes it uses. */
function links(service: ServiceInfo): string[] {
  const app = appOf(service);
  if (!app) return [];
  const out = app.networks.map((n) => `network:${n}`);
  for (const mount of app.mounts) {
    if (mount.source.case === "volume") out.push(`volume:${mount.source.value}`);
  }
  return out;
}

const byName = (a: ServiceInfo, b: ServiceInfo) => nameOf(a).localeCompare(nameOf(b));

/** Groups services by what connects them, each environment apart, in the order they come. */
export function groupApps(services: readonly ServiceInfo[]): AppGroup[] {
  const parent = new Map<string, string>();
  const find = (id: string): string => {
    let at = id;
    for (let up = parent.get(at); up !== undefined && up !== at; up = parent.get(at)) at = up;
    parent.set(id, at);
    return at;
  };
  const claimed = new Map<string, string>();
  for (const service of services) {
    parent.set(service.id, service.id);
    for (const link of links(service)) {
      const key = `${environmentOf(service)}/${link}`;
      const other = claimed.get(key);
      if (other === undefined) claimed.set(key, service.id);
      else parent.set(find(service.id), find(other));
    }
  }
  const groups = new Map<string, AppGroup>();
  for (const service of services) {
    const root = find(service.id);
    const group = groups.get(root);
    if (group) {
      group.services.push(service);
      group.stack = true;
    } else {
      groups.set(root, { key: service.id, environment: environmentOf(service), services: [service], stack: false });
    }
  }
  const out = [...groups.values()];
  for (const group of out) group.services.sort(byName);
  return out;
}

/** The services a service runs with: the others of its group. */
export function companions(service: ServiceInfo, services: readonly ServiceInfo[]): ServiceInfo[] {
  const group = groupApps(services).find((g) => g.services.some((s) => s.id === service.id));
  return group?.services.filter((s) => s.id !== service.id) ?? [];
}

/** A group's state: its most concerning service's. */
export function groupState(group: AppGroup, servers: readonly ServerSummary[] | undefined): AppState {
  return group.services.map((s) => appState(s, servers)).reduce(worse);
}

/** When a group last deployed: its newest service's. */
export function groupDeployedAt(group: AppGroup): Date {
  return group.services.map(deployedAt).reduce((a, b) => (b > a ? b : a));
}

/** Newest first, with what needs you above it all, so a row never hides what is wrong. */
export function sortGroups(groups: readonly AppGroup[], servers: readonly ServerSummary[] | undefined): AppGroup[] {
  const looks = new Map(groups.map((g) => [g.key, CONCERN[groupState(g, servers).kind]]));
  return [...groups].sort((a, b) => {
    // Whatever needs you, or is moving, comes first; the calm ones keep their order among themselves.
    const concern = Math.min(looks.get(a.key) ?? 6, 5) - Math.min(looks.get(b.key) ?? 6, 5);
    if (concern !== 0) return concern;
    const when = groupDeployedAt(b).getTime() - groupDeployedAt(a).getTime();
    return when !== 0 ? when : a.key.localeCompare(b.key);
  });
}

/** Whether a service matches what someone typed: part of its name, or of its image. */
export function matches(service: ServiceInfo, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (q === "") return true;
  return nameOf(service).toLowerCase().includes(q) || imageOf(service).toLowerCase().includes(q);
}

/** A revision's number, as the panel writes it. */
export function revisionName(number: bigint): string {
  return `r${number}`;
}

/** A revision worth going back to: not deleted, and it didn't fail. */
export function canRollBackTo(revision: ServiceRevision): boolean {
  return !revision.deleted && !revision.deployments.some((d) => d.state === DeploymentState.FAILED);
}

/**
 * A message from the API for one app's page. The environment's ID says
 * nothing here, as the page is already that app's: "Service web in env_01M…
 * isn't valid: the server path…" reads "The server path…".
 */
export function tidyMessage(message: string): string {
  const bare = message
    .replace(/^.*?\bisn't valid: /, "")
    .replace(/\bservice (\S+) in env_[0-9A-Za-z]+/g, "$1")
    .replace(/ in env_[0-9A-Za-z]+/g, "");
  return bare.charAt(0).toUpperCase() + bare.slice(1);
}

/**
 * An image change in few characters: where only the tag moves, the name goes
 * without saying, as "v1.11 → v1.12". Otherwise both in full.
 */
export function shortImages(from: string, to: string): string {
  const repo = (image: string) => image.slice(0, Math.max(image.lastIndexOf(":"), 0));
  if (from !== "" && repo(from) !== "" && repo(from) === repo(to)) {
    return `${from.slice(repo(from).length + 1)} → ${to.slice(repo(to).length + 1)}`;
  }
  return `${from} → ${to}`;
}

/** A service's own field, from a plan's path: "app.image" reads as "image", and env[KEY] keeps its key. */
export function fieldName(path: string): string {
  return path.replace(/^app\./, "");
}

/** A name Dockwell takes for a service: a DNS label, such as "api-v2". */
export const SERVICE_NAME = /^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/;

/**
 * The name an image's app takes unless you say otherwise: the last part of
 * its name, without its tag or digest, made into a DNS label. Empty when
 * nothing of it would do.
 */
export function nameFromImage(image: string): string {
  const path = image.trim().split("@")[0] ?? "";
  const last = path.split("/").pop() ?? "";
  const bare = last.split(":")[0] ?? "";
  const label = bare
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^[^a-z]+/, "")
    .replace(/-+$/, "");
  return label.slice(0, 63).replace(/-+$/, "");
}

/** The name of a service's container's environment variables, sorted. */
export function envNames(app: Container | undefined): string[] {
  return Object.keys(app?.env ?? {}).sort((a, b) => a.localeCompare(b));
}
