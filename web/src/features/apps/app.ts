// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import {
  type Change,
  ChangeState,
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
  /** A step failed, or the change gave up before a server got it. */
  | "failed"
  /** It was live, but a server it runs on isn't connected, so its state isn't known. */
  | "offline"
  /** Its last deploy failed, and Dockwell put back what ran before. */
  | "rolled-back"
  /** No server has it: each it ran on has left the fleet. */
  | "unplaced"
  /** It deployed, and the fleet hasn't said yet whether its servers are there. */
  | "unknown";

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
  unknown: "stopped",
};

/**
 * Where an app stands. It says what Dockwell knows and no more: live means the
 * revision deployed and passed the check its step made, and that its servers
 * are there, not that the app is healthy now, which comes with rollouts (#28).
 * Until the fleet answers, it can't say that much. The fleet lists every server
 * still in it, so one it doesn't list has left.
 */
export function appState(
  service: Pick<ServiceInfo, "deployments" | "rolledBackChangeId" | "changeState">,
  servers: readonly ServerSummary[] | undefined,
): AppState {
  const placed = service.deployments;
  const listed = (d: ServiceDeployment) => servers?.find((s) => s.id === d.serverId);
  const away = (d: ServiceDeployment) => {
    const server = listed(d);
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
  // Under a change that has failed, a deployment still pending won't move again.
  if (pending.length > 0 && service.changeState === ChangeState.FAILED) return state("failed", pending);
  const stuck = pending.filter(away);
  if (stuck.length > 0) return state("waiting", stuck);
  if (pending.length > 0) return state("deploying", pending);
  if (servers === undefined) return state("unknown");
  const still = placed.filter((d) => listed(d) !== undefined);
  if (still.length === 0) return state("unplaced", placed);
  const offline = still.filter(away);
  if (offline.length > 0) return state("offline", offline);
  return state(service.rolledBackChangeId !== "" ? "rolled-back" : "live");
}

/** Work going on: nothing should go back, and what runs isn't what the latest revision says yet. */
export function isMoving(state: AppState): boolean {
  return state.kind === "deploying" || state.kind === "waiting";
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
  unknown: 6,
};

/** Which of two states is the more concerning. */
export function worse(a: AppState, b: AppState): AppState {
  return CONCERN[b.kind] < CONCERN[a.kind] ? b : a;
}

/** Whether a state is one a person should look at. Going as it should, and not knowing yet, aren't. */
export function needsALook(state: AppState): boolean {
  return state.kind !== "live" && state.kind !== "unknown" && state.kind !== "deploying";
}

/** Where a service lives, as far as the list needs to know. */
export interface PlaceOf {
  /** The project's name, to show. */
  label: string;
  /** In the hidden default project, where apps stand on their own. */
  isDefault: boolean;
}

/**
 * What the list shows as one row (ADR 0026). A Compose file's services are
 * their project's, in one row with its name that folds. An app in the default
 * project is a row of its own. A project with one service has nothing to fold,
 * so it is that service's row, under the project's name.
 */
export interface AppGroup {
  /** The environment's ID for a project's row, and the service's for one of its own. */
  key: string;
  environment: string;
  /** The project's name. Empty for an app in the default project. */
  name: string;
  /** By name. */
  services: ServiceInfo[];
  /** More than one service, so it folds. */
  stack: boolean;
}

/** What connects a service to others: the networks and volumes it uses. */
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

/** Groups services by project, in the order they come. An app whose place isn't known yet stands on its own. */
export function groupApps(
  services: readonly ServiceInfo[],
  placeOf: (environment: string) => PlaceOf | undefined,
): AppGroup[] {
  const groups = new Map<string, AppGroup>();
  for (const service of services) {
    const environment = environmentOf(service);
    const place = placeOf(environment);
    if (!place || place.isDefault) {
      groups.set(service.id, { key: service.id, environment, name: "", services: [service], stack: false });
      continue;
    }
    const group = groups.get(environment);
    if (group) {
      group.services.push(service);
      group.stack = true;
    } else {
      groups.set(environment, { key: environment, environment, name: place.label, services: [service], stack: false });
    }
  }
  const out = [...groups.values()];
  for (const group of out) group.services.sort(byName);
  return out;
}

/**
 * The services a service runs with: the rest of its project; in the default
 * project, whose apps stand on their own, those that share a network or a
 * volume with it.
 */
export function companions(
  service: ServiceInfo,
  services: readonly ServiceInfo[],
  placeOf: (environment: string) => PlaceOf | undefined,
): ServiceInfo[] {
  const environment = environmentOf(service);
  const near = services.filter((s) => s.id !== service.id && environmentOf(s) === environment);
  const place = placeOf(environment);
  if (place && !place.isDefault) return [...near].sort(byName);
  const own = new Set(links(service));
  return near.filter((s) => links(s).some((link) => own.has(link))).sort(byName);
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

/**
 * A revision worth going back to: not deleted, it didn't fail, and going back
 * would change something. While recovering, as the latest failed, one that
 * matches the record counts: what the record says isn't what runs.
 */
export function canRollBackTo(revision: ServiceRevision, recovering = false): boolean {
  return (
    !revision.deleted &&
    (recovering || !revision.matchesLatest) &&
    !revision.deployments.some((d) => d.state === DeploymentState.FAILED)
  );
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
  // A message that starts with a server's name keeps it as it is written.
  if (/^[a-z0-9-]+:\s/.test(bare)) return bare;
  return bare.charAt(0).toUpperCase() + bare.slice(1);
}

/** An image by what tells it apart on a narrow screen: the last part of its name, and its tag. */
export function shortImage(image: string): string {
  return image.slice(image.lastIndexOf("/") + 1);
}

/**
 * An image change in few characters: where only the tag moves, the name goes
 * without saying, as v1.11 to v1.12; where the rest of the name does, the last
 * part of it says enough. Otherwise both in full.
 */
export function imageChange(from: string, to: string): { from: string; to: string } {
  const repo = (image: string) => image.slice(0, Math.max(image.lastIndexOf(":"), 0));
  if (from !== "" && repo(from) !== "" && repo(from) === repo(to)) {
    return { from: from.slice(repo(from).length + 1), to: to.slice(repo(to).length + 1) };
  }
  if (from !== "" && shortImage(from) !== shortImage(to)) return { from: shortImage(from), to: shortImage(to) };
  return { from, to };
}

export function shortImages(from: string, to: string): string {
  const change = imageChange(from, to);
  return `${change.from} → ${change.to}`;
}

/** A service's own field, from a plan's path: "app.image" reads as "image", and env[KEY] keeps its key. */
export function fieldName(path: string): string {
  return path.replace(/^app\./, "");
}

/** A name Dockwell takes for a service or a project: a DNS label, such as "api-v2". */
export const SERVICE_NAME = /^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/;

/** A name made into a DNS label, as a Compose file's may need to be: my_app reads as my-app. Empty when nothing of it would do. */
export function toLabel(name: string): string {
  const label = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^[^a-z]+/, "")
    .replace(/-+$/, "");
  return label.slice(0, 63).replace(/-+$/, "");
}

/** The name Docker gives a service's first container, as Compose does (ADR 0021). */
export function containerName(service: ServiceInfo): string {
  return `${environmentOf(service).toLowerCase()}-${nameOf(service)}-1`;
}

/**
 * The name an image's app takes unless you say otherwise: the last part of
 * its name, without its tag or digest, made into a DNS label. Empty when
 * nothing of it would do.
 */
export function nameFromImage(image: string): string {
  const path = image.trim().split("@")[0] ?? "";
  const last = path.split("/").pop() ?? "";
  return toLabel(last.split(":")[0] ?? "");
}

/** Something a service runs with, by the name it sees. */
export interface ServiceValue {
  name: string;
  /** Its environment's variable or secret, or its spec's own. */
  kind: "variable" | "secret" | "own";
  /** What its environment calls it: a service can see one under another name. */
  from: string;
}

/** What a service runs with, sorted. No API response holds a secret's value. */
export function valuesOf(service: ServiceInfo): ServiceValue[] {
  const used = (kind: "variable" | "secret", names: { [name: string]: string } | undefined): ServiceValue[] =>
    Object.entries(names ?? {}).map(([name, from]) => ({ name, kind, from: from || name }));
  return [
    ...used("variable", service.spec?.variables),
    ...used("secret", service.spec?.secrets),
    ...Object.keys(appOf(service)?.env ?? {}).map((name): ServiceValue => ({ name, kind: "own", from: name })),
  ].sort((a, b) => a.name.localeCompare(b.name));
}

/**
 * A plan refused for a port something else on the server holds, in the agent's
 * words without the ones that say where it was planned.
 */
export function portRefusal(message: string): string | undefined {
  const found = /\bport \d+(?:\/udp)?(?: on \S+(?: \([^)]*\))?)? is (?:in use|wanted)\b.*$/s.exec(message)?.[0];
  return found ? found.charAt(0).toUpperCase() + found.slice(1) : undefined;
}

/** Whether anything outside its server can reach it: today, whether it publishes a port. */
export function publishes(app: Container | undefined): boolean {
  return (app?.ports.length ?? 0) > 0;
}

/**
 * The new services a plan makes that nothing outside their server will reach,
 * by name, for what is said once it applies. A plan lists the fields a new
 * service sets, so one with no ports among them publishes none.
 */
export function unreached(change: Change | undefined): string[] {
  const out: string[] = [];
  for (const planned of change?.plan?.resources ?? []) {
    if (planned.resource?.kind !== "service" || planned.baseRevision !== 0n) continue;
    if (planned.changes.some((c) => c.path.startsWith("app.ports"))) continue;
    out.push(planned.resource.name);
  }
  return out;
}
